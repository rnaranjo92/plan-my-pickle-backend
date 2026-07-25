package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// canonicalTeeSizes is the master size list an organizer picks from for the
// event-tee presale. Registrants choose one of the sizes the organizer offers.
var canonicalTeeSizes = []string{"XS", "S", "M", "L", "XL", "2XL", "3X"}

// normTeeSizes keeps only recognized sizes, de-duped, in canonical order.
func normTeeSizes(in []string) []string {
	want := map[string]bool{}
	for _, s := range in {
		want[strings.ToUpper(strings.TrimSpace(s))] = true
	}
	out := make([]string, 0, len(canonicalTeeSizes))
	for _, s := range canonicalTeeSizes {
		if want[s] {
			out = append(out, s)
		}
	}
	return out
}

// teeSizeAllowed reports whether size is one of the offered sizes.
func teeSizeAllowed(offered []string, size string) bool {
	size = strings.ToUpper(strings.TrimSpace(size))
	for _, s := range offered {
		if strings.ToUpper(s) == size {
			return true
		}
	}
	return false
}

// Paid registration add-ons (post-registration upsell): an organizer prices an
// event tee and/or overgrips per event (0 = not offered); a registrant opts in
// and the add-ons are charged with their entry fee — the Stripe checkout, the
// Zelle self-report, and the organizer's mark-paid all use the same total via
// registrationChargeCents.

// SetRegistrationAddons records which offered add-ons a registrant wants.
// Gated by the caller (owner JWT or the registration's check-in token) and
// refused once the registration is already paid — the money was collected on
// the old total, so changing the cart afterwards would desync what was charged.
func (s *Service) SetRegistrationAddons(registrationID string, tee, grips bool, teeSize string) error {
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=event_id,payment_status")
	if err != nil {
		return err
	}
	if reg == nil {
		return ErrNotFound
	}
	if asStr(reg, "payment_status") == "paid" {
		return errors.New("registration is already paid — ask the organizer to adjust add-ons")
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(asStr(reg, "event_id"))+
			"&select=addon_tee_cents,addon_grips_cents,addon_tee_sizes")
	if err != nil {
		return err
	}
	if ev == nil {
		return ErrNotFound
	}
	if tee && asInt(ev, "addon_tee_cents") <= 0 {
		return errors.New("this event doesn't offer an event tee")
	}
	if grips && asInt(ev, "addon_grips_cents") <= 0 {
		return errors.New("this event doesn't offer overgrips")
	}
	// Validate the tee size against the sizes the organizer offers (when any).
	size := strings.ToUpper(strings.TrimSpace(teeSize))
	if tee {
		offered := asStrSlice(ev, "addon_tee_sizes")
		if len(offered) > 0 {
			if size == "" {
				return errors.New("please choose a tee size")
			}
			if !teeSizeAllowed(offered, size) {
				return errors.New("that tee size isn't available for this event")
			}
		}
	} else {
		size = "" // clear any stored size when the tee isn't being bought
	}
	_, err = s.sb.Update("registrations", "id=eq."+store.Q(registrationID),
		map[string]any{"addon_tee": tee, "addon_grips": grips, "addon_tee_size": orNull(size)})
	return err
}

// TeeOrders returns the event's tee-add-on purchases plus a size breakdown for
// the organizer's printer. Owner-gated by the route.
func (s *Service) TeeOrders(eventID string) (model.TeeOrdersSummary, error) {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+
		"&select=addon_tee_name,addon_tee_cents,currency,addon_tee_front_url,addon_tee_back_url")
	if err != nil {
		return model.TeeOrdersSummary{}, err
	}
	if ev == nil {
		return model.TeeOrdersSummary{}, ErrNotFound
	}
	cur := asStr(ev, "currency")
	if cur == "" {
		cur = "usd"
	}
	sum := model.TeeOrdersSummary{
		Name:       asStr(ev, "addon_tee_name"),
		PriceCents: asInt(ev, "addon_tee_cents"),
		Currency:   cur,
		FrontURL:   asStr(ev, "addon_tee_front_url"),
		BackURL:    asStr(ev, "addon_tee_back_url"),
		SizeCounts: map[string]int{},
		Orders:     []model.TeeOrder{},
	}
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&addon_tee=eq.true"+
			"&select=id,full_name,addon_tee_size,payment_status&order=full_name.asc")
	if err != nil {
		return model.TeeOrdersSummary{}, err
	}
	for _, r := range rows {
		size := asStr(r, "addon_tee_size")
		sum.Orders = append(sum.Orders, model.TeeOrder{
			RegistrationID: asStr(r, "id"),
			PlayerName:     asStr(r, "full_name"),
			Size:           size,
			Paid:           asStr(r, "payment_status") == "paid",
		})
		key := size
		if key == "" {
			key = "—"
		}
		sum.SizeCounts[key]++
		sum.Total++
	}
	return sum, nil
}

// normExtraDivMode validates/defaults the multi-division fee mode.
func normExtraDivMode(s string) string {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "free":
		return "free"
	case "full":
		return "full"
	default:
		return "discount"
	}
}

// registrationChargeCents returns what this registration owes — entry fee plus
// any opted-in add-ons — with the event's currency and id.
func (s *Service) registrationChargeCents(registrationID string) (total int, currency, eventID string, err error) {
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+
			"&select=event_id,player_id,created_at,addon_tee,addon_grips")
	if err != nil {
		return 0, "", "", err
	}
	if reg == nil {
		return 0, "", "", ErrNotFound
	}
	eventID = asStr(reg, "event_id")
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+
		"&select=registration_fee_cents,addon_tee_cents,addon_grips_cents,currency,"+
		"extra_division_fee_mode,additional_division_fee_cents")
	if err != nil {
		return 0, "", "", err
	}
	if ev == nil {
		return 0, "", "", ErrNotFound
	}
	fee := asInt(ev, "registration_fee_cents")
	// Multi-division pricing: only the player's FIRST (earliest) registration in
	// this event pays the full entry fee. Additional divisions follow the event's
	// mode — 'full' charges full again, 'free' is $0, 'discount' charges the set
	// additional-division fee. Default is discount (per-event field).
	if fee > 0 {
		mode := asStr(ev, "extra_division_fee_mode")
		if mode == "" {
			mode = "discount"
		}
		if mode != "full" && s.isAdditionalDivision(
			eventID, asStr(reg, "player_id"), registrationID, asStr(reg, "created_at")) {
			if mode == "free" {
				fee = 0
			} else {
				fee = asInt(ev, "additional_division_fee_cents")
			}
		}
	}
	total = fee
	if asBool(reg, "addon_tee") {
		total += asInt(ev, "addon_tee_cents")
	}
	if asBool(reg, "addon_grips") {
		total += asInt(ev, "addon_grips_cents")
	}
	currency = asStr(ev, "currency")
	if currency == "" {
		currency = "usd"
	}
	return total, currency, eventID, nil
}

// isAdditionalDivision reports whether this registration is NOT the player's
// primary one in the event — i.e. another of their registrations sorts before it
// (earlier created_at, or equal created_at with a smaller id as a stable
// tiebreak). The earliest registration is the "primary" that pays full fee.
func (s *Service) isAdditionalDivision(eventID, playerID, thisRegID, thisCreatedAt string) bool {
	if playerID == "" {
		return false
	}
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&player_id=eq."+store.Q(playerID)+
			"&select=id,created_at")
	if err != nil {
		return false
	}
	for _, r := range rows {
		id := asStr(r, "id")
		if id == thisRegID {
			continue
		}
		ca := asStr(r, "created_at")
		if ca < thisCreatedAt || (ca == thisCreatedAt && id < thisRegID) {
			return true
		}
	}
	return false
}
