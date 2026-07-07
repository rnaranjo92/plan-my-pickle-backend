package service

import (
	"errors"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Paid registration add-ons (post-registration upsell): an organizer prices an
// event tee and/or overgrips per event (0 = not offered); a registrant opts in
// and the add-ons are charged with their entry fee — the Stripe checkout, the
// Zelle self-report, and the organizer's mark-paid all use the same total via
// registrationChargeCents.

// SetRegistrationAddons records which offered add-ons a registrant wants.
// Gated by the caller (owner JWT or the registration's check-in token) and
// refused once the registration is already paid — the money was collected on
// the old total, so changing the cart afterwards would desync what was charged.
func (s *Service) SetRegistrationAddons(registrationID string, tee, grips bool) error {
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
		"id=eq."+store.Q(asStr(reg, "event_id"))+"&select=addon_tee_cents,addon_grips_cents")
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
	_, err = s.sb.Update("registrations", "id=eq."+store.Q(registrationID),
		map[string]any{"addon_tee": tee, "addon_grips": grips})
	return err
}

// registrationChargeCents returns what this registration owes — entry fee plus
// any opted-in add-ons — with the event's currency and id.
func (s *Service) registrationChargeCents(registrationID string) (total int, currency, eventID string, err error) {
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=event_id,addon_tee,addon_grips")
	if err != nil {
		return 0, "", "", err
	}
	if reg == nil {
		return 0, "", "", ErrNotFound
	}
	eventID = asStr(reg, "event_id")
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+
		"&select=registration_fee_cents,addon_tee_cents,addon_grips_cents,currency")
	if err != nil {
		return 0, "", "", err
	}
	if ev == nil {
		return 0, "", "", ErrNotFound
	}
	total = asInt(ev, "registration_fee_cents")
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
