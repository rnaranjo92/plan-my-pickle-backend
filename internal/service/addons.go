package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// teeSelfTestName is the hidden event the QA tee self-test reuses so runs don't
// pile up new events.
const teeSelfTestName = "Tee Self-Test (hidden QA)"

// TeeSelfTest exercises the event-tee presale end to end against the real DB
// (no Stripe): it configures a hidden event with a tee + sizes, registers a
// dummy player, then asserts (1) an invalid size is rejected, (2) a valid size
// is accepted, (3) the charge total = entry + tee, and (4) the size shows up in
// the orders breakdown. The dummy registration is always cleaned up. Returns a
// human-readable PASS summary, or an error whose message is the failing check.
func (s *Service) TeeSelfTest(ownerID string) (string, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return "", errors.New("not signed in")
	}
	const teePriceCents = 2500
	teeSizes := []string{"S", "M", "L", "XL"}

	// Find-or-create the hidden self-test event, tee configured.
	row, err := s.sb.SelectOne("events",
		"owner_id=eq."+store.Q(ownerID)+"&name=eq."+store.Q(teeSelfTestName)+"&select=id&limit=1")
	if err != nil {
		return "", err
	}
	var eventID string
	if row != nil {
		eventID = asStr(row, "id")
		if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID), map[string]any{
			"addon_tee_cents": teePriceCents,
			"addon_tee_name":  "Self-Test Tee",
			"addon_tee_sizes": teeSizes,
		}); err != nil {
			return "", err
		}
	} else {
		eventID, err = s.CreateEvent(model.CreateEventRequest{
			Name:                 teeSelfTestName,
			Format:               "doubles",
			TournamentFormat:     "round_robin",
			NumCourts:            1,
			PointsToWin:          11,
			WinBy:                2,
			BestOf:               1,
			RegistrationFeeCents: 100, // $1.00 entry
			Currency:             "USD",
			AddonTeeCents:        teePriceCents,
			AddonTeeName:         "Self-Test Tee",
			AddonTeeSizes:        teeSizes,
			Brackets:             []model.BracketInput{{Name: "Open", DivisionType: "open"}},
		}, ownerID)
		if err != nil {
			return "", err
		}
	}

	// Register a dummy player (trusted add skips the public gates); unique phone.
	bracketID := ""
	if bks, berr := s.GetBrackets(eventID); berr == nil && len(bks) > 0 {
		bracketID = bks[0].ID
	}
	uniq := time.Now().UnixNano() % 10000000
	reg, err := s.RegisterPlayer(eventID, model.RegisterRequest{
		FullName:   "Tee Self Test",
		Phone:      fmt.Sprintf("+1555%07d", uniq),
		BracketID:  bracketID,
		TrustedAdd: true,
	}, "")
	if err != nil {
		return "", err
	}
	// No payment/webhook is in flight, so always clean the dummy registration up.
	defer func() { _ = s.sb.Delete("registrations", "id=eq."+store.Q(reg.ID)) }()

	// (1) An invalid size must be rejected.
	if err := s.SetRegistrationAddons(reg.ID, true, false, "XXL"); err == nil {
		return "", errors.New("FAIL: an invalid tee size (\"XXL\") was accepted")
	}
	// (2) A valid size must be accepted.
	if err := s.SetRegistrationAddons(reg.ID, true, false, "M"); err != nil {
		return "", fmt.Errorf("FAIL: valid size \"M\" was rejected: %w", err)
	}
	// (3) The charge total must include the tee.
	total, _, _, err := s.registrationChargeCents(reg.ID)
	if err != nil {
		return "", err
	}
	wantTotal := 100 + teePriceCents
	if total != wantTotal {
		return "", fmt.Errorf("FAIL: charge total = %d cents, want %d (entry + tee)", total, wantTotal)
	}
	// (4) The orders breakdown must reflect our M.
	sum, err := s.TeeOrders(eventID)
	if err != nil {
		return "", err
	}
	if sum.SizeCounts["M"] < 1 {
		return "", fmt.Errorf("FAIL: tee-orders shows no size \"M\" (got %v)", sum.SizeCounts)
	}
	found := false
	for _, o := range sum.Orders {
		if o.RegistrationID == reg.ID {
			if o.Size != "M" {
				return "", fmt.Errorf("FAIL: our order size = %q, want \"M\"", o.Size)
			}
			found = true
		}
	}
	if !found {
		return "", errors.New("FAIL: our registration is missing from tee-orders")
	}

	return fmt.Sprintf(
		"PASS — invalid size rejected, \"M\" accepted, charge = $%.2f ($1.00 entry + $%.2f tee), and it shows in the size breakdown.",
		float64(wantTotal)/100, float64(teePriceCents)/100), nil
}

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
	// The registrant's entered name lives on registrations.name (players.full_name
	// is the account-profile name, a different column) — select/order by "name".
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&addon_tee=eq.true"+
			"&select=id,name,addon_tee_size,payment_status&order=name.asc")
	if err != nil {
		return model.TeeOrdersSummary{}, err
	}
	for _, r := range rows {
		size := asStr(r, "addon_tee_size")
		sum.Orders = append(sum.Orders, model.TeeOrder{
			RegistrationID: asStr(r, "id"),
			PlayerName:     asStr(r, "name"),
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
