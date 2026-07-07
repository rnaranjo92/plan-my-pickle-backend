package service

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Vendor Village: booths / food trucks / sponsors an organizer attaches to an
// event. Listed publicly on the player + spectator views; create/update/delete
// are owner-gated at the handler ("vendor" in ownerKindTable). The optional
// deal push reuses the same OneSignal path as match-start notifications.

func mapVendor(r map[string]any) model.Vendor {
	status := asStr(r, "status")
	if status == "" {
		status = "approved" // pre-0052 rows have no status column
	}
	return model.Vendor{
		ID:           asStr(r, "id"),
		EventID:      asStr(r, "event_id"),
		Name:         asStr(r, "name"),
		Tagline:      asStr(r, "tagline"),
		Booth:        asStr(r, "booth"),
		Promo:        asStr(r, "promo"),
		LinkURL:      asStr(r, "link_url"),
		LogoURL:      asStr(r, "logo_url"),
		SortOrder:    asInt(r, "sort_order"),
		Status:       status,
		ContactEmail: asStr(r, "contact_email"),
		ContactPhone: asStr(r, "contact_phone"),
		Pitch:        asStr(r, "pitch"),
		FeeCents:     asInt(r, "fee_cents"),
		PaymentStatus: func() string {
			if ps := asStr(r, "payment_status"); ps != "" {
				return ps
			}
			return "unpaid"
		}(),
		PayToken:     asStr(r, "pay_token"),
		SponsorCourt: asInt(r, "sponsor_court"),
		IsSponsor:    asBool(r, "is_sponsor"),
		Clicks:       asInt(r, "clicks"),
	}
}

// ListVendors returns an event's Vendor Village entries in display order.
// Public callers (spectators, the strip) get APPROVED vendors only; the owner
// (includeAll) also sees pending applications and rejected rows, with contact
// details. The public projection strips applicant contact info regardless.
func (s *Service) ListVendors(eventID string, includeAll bool) ([]model.Vendor, error) {
	rows, err := s.sb.Select("vendors",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=sort_order.asc,created_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Vendor, 0, len(rows))
	for _, r := range rows {
		v := mapVendor(r)
		if !includeAll {
			if v.Status != "approved" {
				continue
			}
			// PII-free public shape: applicants' contact details and the pay
			// token are for the organizer only.
			v.ContactEmail, v.ContactPhone, v.Pitch, v.PayToken = "", "", "", ""
		}
		out = append(out, v)
	}
	return out, nil
}

// ApplyVendor records a PUBLIC vendor application (status pending) from the
// event's "Become a vendor" link. Rate-limited + captcha-gated at the handler.
func (s *Service) ApplyVendor(eventID string, req model.VendorApplyRequest) (model.Vendor, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return model.Vendor{}, errors.New("business name is required")
	}
	if strings.TrimSpace(req.ContactEmail) == "" &&
		strings.TrimSpace(req.ContactPhone) == "" {
		return model.Vendor{}, errors.New("an email or phone is required so the organizer can reach you")
	}
	// The event must exist and be visible; applications to a deleted event 404.
	if ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=id"); err != nil {
		return model.Vendor{}, err
	} else if ev == nil {
		return model.Vendor{}, ErrNotFound
	}
	ins, err := s.sb.Insert("vendors", map[string]any{
		"event_id":      eventID,
		"name":          name,
		"tagline":       strings.TrimSpace(req.Tagline),
		"pitch":         strings.TrimSpace(req.Pitch),
		"link_url":      strings.TrimSpace(req.LinkURL),
		"contact_email": strings.TrimSpace(req.ContactEmail),
		"contact_phone": strings.TrimSpace(req.ContactPhone),
		"status":        "pending",
	})
	if err != nil {
		return model.Vendor{}, err
	}
	if len(ins) == 0 {
		return model.Vendor{}, errors.New("vendor application insert returned no row")
	}
	return mapVendor(ins[0]), nil
}

// SetVendorStatus approves or rejects a vendor application (owner-gated
// upstream). Approving flips it straight onto the public Vendor Village strip.
func (s *Service) SetVendorStatus(vendorID, status string) (model.Vendor, error) {
	if status != "approved" && status != "rejected" && status != "pending" {
		return model.Vendor{}, fmt.Errorf("invalid vendor status %q", status)
	}
	upd, err := s.sb.Update("vendors", "id=eq."+store.Q(vendorID),
		map[string]any{"status": status})
	if err != nil {
		return model.Vendor{}, err
	}
	if len(upd) == 0 {
		return model.Vendor{}, ErrNotFound
	}
	return mapVendor(upd[0]), nil
}

func vendorRow(req model.VendorRequest) (map[string]any, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("vendor name is required")
	}
	if req.FeeCents < 0 || req.FeeCents > 1_000_000 {
		return nil, errors.New("booth fee must be between $0 and $10,000")
	}
	return map[string]any{
		"name":          name,
		"tagline":       strings.TrimSpace(req.Tagline),
		"booth":         strings.TrimSpace(req.Booth),
		"promo":         strings.TrimSpace(req.Promo),
		"link_url":      strings.TrimSpace(req.LinkURL),
		"logo_url":      strings.TrimSpace(req.LogoURL),
		"sort_order":    req.SortOrder,
		"fee_cents":     req.FeeCents,
		"is_sponsor":    req.IsSponsor,
		"sponsor_court": req.SponsorCourt,
	}, nil
}

// CreateVendor adds a Vendor Village entry to the event (owner-gated upstream).
func (s *Service) CreateVendor(eventID string, req model.VendorRequest) (model.Vendor, error) {
	row, err := vendorRow(req)
	if err != nil {
		return model.Vendor{}, err
	}
	row["event_id"] = eventID
	ins, err := s.sb.Insert("vendors", row)
	if err != nil {
		return model.Vendor{}, err
	}
	if len(ins) == 0 {
		return model.Vendor{}, errors.New("vendor insert returned no row")
	}
	return mapVendor(ins[0]), nil
}

// UpdateVendor replaces a vendor's editable fields (owner-gated upstream).
func (s *Service) UpdateVendor(vendorID string, req model.VendorRequest) (model.Vendor, error) {
	row, err := vendorRow(req)
	if err != nil {
		return model.Vendor{}, err
	}
	upd, err := s.sb.Update("vendors", "id=eq."+store.Q(vendorID), row)
	if err != nil {
		return model.Vendor{}, err
	}
	if len(upd) == 0 {
		return model.Vendor{}, ErrNotFound
	}
	return mapVendor(upd[0]), nil
}

// DeleteVendor removes a Vendor Village entry (owner-gated upstream).
func (s *Service) DeleteVendor(vendorID string) error {
	return s.sb.Delete("vendors", "id=eq."+store.Q(vendorID))
}

// NotifyVendorDeal pushes an organizer-composed vendor deal to every player in
// the event with a linked account, and drops it on the event feed so attendees
// without push (or who missed it) still see the deal. Returns the number of
// push recipients. Best-effort like all pushes — a missing OneSignal key means
// 0 recipients, not an error, and the feed post still lands.
func (s *Service) NotifyVendorDeal(vendorID string, req model.VendorNotifyRequest) (int, error) {
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		return 0, errors.New("message is required")
	}
	v, err := s.sb.SelectOne("vendors", "id=eq."+store.Q(vendorID)+"&select=*")
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, ErrNotFound
	}
	vendor := mapVendor(v)

	// Every registered player with a linked account (user_id), de-duped.
	regs, err := s.sb.SelectAll("registrations",
		"event_id=eq."+store.Q(vendor.EventID)+"&select=player:players!player_id(user_id)")
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	var userIDs []string
	for _, r := range regs {
		if p := asMap(r, "player"); p != nil {
			if uid := asStr(p, "user_id"); uid != "" && !seen[uid] {
				seen[uid] = true
				userIDs = append(userIDs, uid)
			}
		}
	}

	heading := vendor.Name
	if vendor.Booth != "" {
		heading = fmt.Sprintf("%s · %s", vendor.Name, vendor.Booth)
	}
	// Tapping the push opens the event's player view.
	url := "https://app.planmypickle.com/?event=" + vendor.EventID
	_ = s.sendPush(userIDs, heading, msg, url)

	// Feed post so the deal outlives the notification tray.
	s.AddFeedItem(vendor.EventID, "announcement",
		fmt.Sprintf("%s: %s", vendor.Name, msg), vendorID)

	return len(userIDs), nil
}

// AuthorizeVendorAction verifies a public pay-page caller: the vendor's
// pay_token (vendors have no accounts) OR the event owner's JWT. Mirrors
// AuthorizeRegistrationAction's IDOR stance.
func (s *Service) AuthorizeVendorAction(vendorID, token, callerUserID string) (bool, error) {
	v, err := s.sb.SelectOne("vendors",
		"id=eq."+store.Q(vendorID)+"&select=event_id,pay_token")
	if err != nil {
		return false, err
	}
	if v == nil {
		return false, ErrNotFound
	}
	if token != "" && subtle.ConstantTimeCompare(
		[]byte(token), []byte(asStr(v, "pay_token"))) == 1 {
		return true, nil
	}
	if callerUserID != "" {
		if owner, err := s.OwnerOf("event", asStr(v, "event_id")); err == nil &&
			owner == callerUserID {
			return true, nil
		}
	}
	return false, nil
}

// VendorPayInfo is the public pay page's view of a booth fee (token-gated).
type VendorPayInfo struct {
	VendorName    string `json:"vendorName"`
	EventName     string `json:"eventName"`
	FeeCents      int    `json:"feeCents"`
	Currency      string `json:"currency"`
	PaymentStatus string `json:"paymentStatus"`
	Status        string `json:"status"`
}

// GetVendorPayInfo returns what the pay page shows (authorization happens at
// the handler via AuthorizeVendorAction).
func (s *Service) GetVendorPayInfo(vendorID string) (VendorPayInfo, error) {
	v, err := s.sb.SelectOne("vendors", "id=eq."+store.Q(vendorID)+"&select=*")
	if err != nil {
		return VendorPayInfo{}, err
	}
	if v == nil {
		return VendorPayInfo{}, ErrNotFound
	}
	vendor := mapVendor(v)
	info := VendorPayInfo{
		VendorName:    vendor.Name,
		FeeCents:      vendor.FeeCents,
		Currency:      "usd",
		PaymentStatus: vendor.PaymentStatus,
		Status:        vendor.Status,
	}
	if ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(vendor.EventID)+"&select=name,currency"); err == nil && ev != nil {
		info.EventName = asStr(ev, "name")
		if c := strings.ToLower(asStr(ev, "currency")); c != "" {
			info.Currency = c
		}
	}
	return info, nil
}

// CreateVendorCheckoutSession opens a Stripe Checkout for a vendor's booth fee
// — the same Connect destination charge as entry fees: funds settle to the
// organizer's connected account, the platform takes min(5%, $5).
func (s *Service) CreateVendorCheckoutSession(vendorID, successURL, cancelURL string) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	v, err := s.sb.SelectOne("vendors", "id=eq."+store.Q(vendorID)+"&select=*")
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", ErrNotFound
	}
	vendor := mapVendor(v)
	if vendor.Status != "approved" {
		return "", errors.New("this application hasn't been approved yet")
	}
	if vendor.PaymentStatus == "paid" {
		return "", errors.New("this booth fee is already paid")
	}
	if vendor.FeeCents <= 0 {
		return "", errors.New("no booth fee is set for this vendor")
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(vendor.EventID)+"&select=name,currency,owner_id")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	currency := strings.ToLower(asStr(ev, "currency"))
	if currency == "" {
		currency = "usd"
	}
	orow, err := s.organizerPaymentRow(asStr(ev, "owner_id"))
	if err != nil {
		return "", err
	}
	if orow == nil || asStr(orow, "stripe_account_id") == "" ||
		!asBool(orow, "charges_enabled") {
		return "", ErrOrganizerNotConnected
	}
	name := strings.TrimSpace(asStr(ev, "name"))
	if name == "" {
		name = "Tournament"
	}
	return gw.CreateCheckoutSession(gateway.CheckoutParams{
		VendorID:    vendorID,
		AmountCents: vendor.FeeCents,
		Currency:    currency,
		ProductName: name + func() string {
			if vendor.IsSponsor {
				return " — event sponsorship (" + vendor.Name + ")"
			}
			return " — vendor booth (" + vendor.Name + ")"
		}(),
		DestinationAccount: asStr(orow, "stripe_account_id"),
		// Sponsor slots are B2B — flat 10%, uncapped. Booth fees keep the
		// player-fee formula min(5%, $5).
		ApplicationFeeCents: func() int {
			if vendor.IsSponsor {
				return vendor.FeeCents / 10
			}
			return platformFeeCents(vendor.FeeCents)
		}(),
		SuccessURL: successURL,
		CancelURL:  cancelURL,
	})
}

// MarkVendorPaid flips a vendor's booth fee to paid (Stripe webhook, or the
// organizer's manual confirm for cash/Zelle) and posts the confirmation on the
// event feed.
func (s *Service) MarkVendorPaid(vendorID string) error {
	upd, err := s.sb.Update("vendors", "id=eq."+store.Q(vendorID),
		map[string]any{"payment_status": "paid"})
	if err != nil {
		return err
	}
	if len(upd) == 0 {
		return ErrNotFound
	}
	v := mapVendor(upd[0])
	s.AddFeedItem(v.EventID, "announcement",
		v.Name+" is confirmed for the Vendor Village!", vendorID)
	return nil
}

// RecordVendorClick bumps a vendor's tap-through counter (public, best-effort
// — the organizer's proof a booth reached people). Rate-limited upstream.
func (s *Service) RecordVendorClick(vendorID string) {
	// Atomic increment via RPC would be ideal; a read-then-write is fine for
	// this best-effort counter (undercounting a race is acceptable).
	v, err := s.sb.SelectOne("vendors", "id=eq."+store.Q(vendorID)+"&select=clicks,status")
	if err != nil || v == nil || asStr(v, "status") != "approved" {
		return
	}
	_, _ = s.sb.Update("vendors", "id=eq."+store.Q(vendorID),
		map[string]any{"clicks": asInt(v, "clicks") + 1})
}

// CourtSponsors maps court number -> vendor name for an event's approved
// court-sponsoring vendors ("Court 3 · presented by X"). Public.
func (s *Service) CourtSponsors(eventID string) (map[int]string, error) {
	rows, err := s.sb.Select("vendors",
		"event_id=eq."+store.Q(eventID)+"&status=eq.approved&sponsor_court=gt.0"+
			"&select=name,sponsor_court&order=sort_order.asc")
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, r := range rows {
		c := asInt(r, "sponsor_court")
		if _, taken := out[c]; !taken {
			out[c] = asStr(r, "name")
		}
	}
	return out, nil
}
