package service

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Vendor Village: booths / food trucks / sponsors an organizer attaches to an
// event. Listed publicly on the player + spectator views; create/update/delete
// are owner-gated at the handler ("vendor" in ownerKindTable). The optional
// deal push reuses the same OneSignal path as match-start notifications.

func mapVendor(r map[string]any) model.Vendor {
	return model.Vendor{
		ID:        asStr(r, "id"),
		EventID:   asStr(r, "event_id"),
		Name:      asStr(r, "name"),
		Tagline:   asStr(r, "tagline"),
		Booth:     asStr(r, "booth"),
		Promo:     asStr(r, "promo"),
		LinkURL:   asStr(r, "link_url"),
		LogoURL:   asStr(r, "logo_url"),
		SortOrder: asInt(r, "sort_order"),
	}
}

// ListVendors returns an event's Vendor Village entries in display order.
// Public — spectators see these.
func (s *Service) ListVendors(eventID string) ([]model.Vendor, error) {
	rows, err := s.sb.Select("vendors",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=sort_order.asc,created_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Vendor, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapVendor(r))
	}
	return out, nil
}

func vendorRow(req model.VendorRequest) (map[string]any, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("vendor name is required")
	}
	return map[string]any{
		"name":       name,
		"tagline":    strings.TrimSpace(req.Tagline),
		"booth":      strings.TrimSpace(req.Booth),
		"promo":      strings.TrimSpace(req.Promo),
		"link_url":   strings.TrimSpace(req.LinkURL),
		"logo_url":   strings.TrimSpace(req.LogoURL),
		"sort_order": req.SortOrder,
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
