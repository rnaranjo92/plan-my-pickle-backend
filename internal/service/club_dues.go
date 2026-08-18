package service

import (
	"errors"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Club dues: a season fee, and who has paid it.
//
// A ONE-OFF season fee, not a recurring subscription — and paying is STATUS
// ONLY. Nobody is ever refused a session over a billing state, which is the
// same rule the coach cap and the season archive follow. A club chasing dues
// wants to know who to ask; it does not want the app turning somebody away at
// the door on a Tuesday over a card that expired.
//
// Everything here is inert until add_club_dues.sql runs.

// ClubDuesPeriod is one season's fee.
type ClubDuesPeriod struct {
	ID          string `json:"id"`
	ClubID      string `json:"clubId"`
	Name        string `json:"name"`
	AmountCents int    `json:"amountCents"`
	Currency    string `json:"currency"`
	CreatedAt   string `json:"createdAt,omitempty"`
	ClosedAt    string `json:"closedAt,omitempty"`
	// Paid / Of summarise collection at a glance: "24 of 31".
	Paid int `json:"paid"`
	Of   int `json:"of"`
}

// ClubDuesStatus is one member's standing for the open period.
type ClubDuesStatus struct {
	// Period is nil when the club isn't collecting dues at all, which is the
	// normal case and must not read as "you owe nothing yet".
	Period *ClubDuesPeriod `json:"period,omitempty"`
	Paid   bool            `json:"paid"`
	PaidAt string          `json:"paidAt,omitempty"`
	Method string          `json:"method,omitempty"`
}

func (s *Service) duesReady() bool {
	return s.columnReady("club_dues_periods", "id")
}

// OpenDuesPeriod returns the club's current period, or nil when it isn't
// collecting. Most clubs never will, and a club that doesn't charge should see
// nothing about money anywhere.
func (s *Service) OpenDuesPeriod(clubID string) *ClubDuesPeriod {
	if !s.duesReady() {
		return nil
	}
	row, err := s.sb.SelectOne("club_dues_periods",
		"club_id=eq."+store.Q(clubID)+"&closed_at=is.null"+
			"&select=*&order=created_at.desc&limit=1")
	if err != nil || row == nil {
		return nil
	}
	p := mapDuesPeriod(row)
	return &p
}

// OpenDuesPeriodFor is OpenDuesPeriod plus the collection counts, for the
// owner's view. Two counts rather than a list: an owner glancing at the club
// wants "24 of 31", and the names are one tap away on the roster.
func (s *Service) OpenDuesPeriodFor(clubID string) *ClubDuesPeriod {
	p := s.OpenDuesPeriod(clubID)
	if p == nil {
		return nil
	}
	p.Of = s.countRows("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id", "user_id")
	p.Paid = s.countRows("club_dues_payments",
		"period_id=eq."+store.Q(p.ID)+"&select=user_id", "user_id")
	return p
}

// SetClubDues opens a new dues period, closing any period already open.
//
// Closing rather than editing: a club that changes the fee mid-season has
// changed what it charges, and rewriting the old number would make the people
// who already paid look like they paid the new one. The old period keeps its
// payments and stops being the one anybody is asked about.
func (s *Service) SetClubDues(
	clubID, callerID, name string, amountCents int, currency string,
) (ClubDuesPeriod, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return ClubDuesPeriod{}, err
	}
	if !s.duesReady() {
		return ClubDuesPeriod{}, errors.New(
			"dues aren't enabled yet — run add_club_dues.sql")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ClubDuesPeriod{}, errors.New(
			"what's this period called? e.g. \"2026 Season\"")
	}
	if amountCents < 0 {
		return ClubDuesPeriod{}, errors.New("an amount can't be negative")
	}
	currency = strings.ToLower(strings.TrimSpace(currency))
	if currency == "" {
		currency = "usd"
	}
	if err := s.CloseClubDues(clubID, callerID); err != nil {
		return ClubDuesPeriod{}, err
	}
	rows, err := s.sb.Insert("club_dues_periods", map[string]any{
		"club_id":      clubID,
		"name":         name,
		"amount_cents": amountCents,
		"currency":     currency,
	})
	if err != nil {
		return ClubDuesPeriod{}, err
	}
	if len(rows) == 0 {
		return ClubDuesPeriod{}, errors.New("dues period insert returned no row")
	}
	return mapDuesPeriod(rows[0]), nil
}

// CloseClubDues stops collecting, keeping every payment recorded against the
// period. Safe to call when nothing is open.
func (s *Service) CloseClubDues(clubID, callerID string) error {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	if !s.duesReady() {
		return nil
	}
	_, err := s.sb.Update("club_dues_periods",
		"club_id=eq."+store.Q(clubID)+"&closed_at=is.null",
		map[string]any{"closed_at": time.Now().UTC().Format(time.RFC3339)})
	return err
}

// RecordDuesPayment marks a member as paid for the open period.
//
// Manual on purpose: most small clubs take cash, Zelle or a bank transfer, and
// a dues feature that only understands card payments would be recording a
// fiction. Idempotent — marking someone paid twice is a double tap, not a
// second payment.
func (s *Service) RecordDuesPayment(
	clubID, callerID, targetUserID, method, note string,
) error {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	period := s.OpenDuesPeriod(clubID)
	if period == nil {
		return errors.New("this club isn't collecting dues right now")
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return errors.New("which member?")
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "manual"
	}
	_, err := s.sb.Upsert("club_dues_payments", "period_id,user_id",
		map[string]any{
			"period_id":    period.ID,
			"user_id":      targetUserID,
			"method":       method,
			"amount_cents": period.AmountCents,
			"note":         orNull(strings.TrimSpace(note)),
			"recorded_by":  callerID,
		})
	return err
}

// UnrecordDuesPayment undoes a payment recorded by mistake.
//
// Necessary rather than tidy: money marked against the wrong member is worse
// than money not marked at all — one person is chased for what they paid while
// another is thanked for what they didn't.
func (s *Service) UnrecordDuesPayment(clubID, callerID, targetUserID string) error {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	period := s.OpenDuesPeriod(clubID)
	if period == nil {
		return errors.New("this club isn't collecting dues right now")
	}
	return s.sb.Delete("club_dues_payments",
		"period_id=eq."+store.Q(period.ID)+
			"&user_id=eq."+store.Q(strings.TrimSpace(targetUserID)))
}

// MyDuesStatus is what a member sees about their own standing.
func (s *Service) MyDuesStatus(clubID, userID string) ClubDuesStatus {
	out := ClubDuesStatus{}
	if strings.TrimSpace(userID) == "" {
		return out
	}
	period := s.OpenDuesPeriod(clubID)
	if period == nil {
		return out
	}
	out.Period = period
	row, err := s.sb.SelectOne("club_dues_payments",
		"period_id=eq."+store.Q(period.ID)+"&user_id=eq."+store.Q(userID)+
			"&select=paid_at,method")
	if err != nil || row == nil {
		return out
	}
	out.Paid = true
	out.PaidAt = asStr(row, "paid_at")
	out.Method = asStr(row, "method")
	return out
}

// duesPaidUserIDs returns everyone who has paid the open period, for the roster.
func (s *Service) duesPaidUserIDs(periodID string) map[string]bool {
	out := map[string]bool{}
	if periodID == "" {
		return out
	}
	rows, err := s.sb.SelectAll("club_dues_payments",
		"period_id=eq."+store.Q(periodID)+"&select=user_id")
	if err != nil {
		return out
	}
	for _, r := range rows {
		if u := strings.TrimSpace(asStr(r, "user_id")); u != "" {
			out[u] = true
		}
	}
	return out
}

func mapDuesPeriod(r map[string]any) ClubDuesPeriod {
	return ClubDuesPeriod{
		ID:          asStr(r, "id"),
		ClubID:      asStr(r, "club_id"),
		Name:        asStr(r, "name"),
		AmountCents: asInt(r, "amount_cents"),
		Currency:    asStr(r, "currency"),
		CreatedAt:   asStr(r, "created_at"),
		ClosedAt:    asStr(r, "closed_at"),
	}
}
