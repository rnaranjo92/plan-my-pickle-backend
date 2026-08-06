package service

import (
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Perpetual leagues — a recurring/"forever" league runs as ONE ongoing event
// (the normal tournament interface: Feed/Players/Game/Standings…) instead of
// spawning a fresh session event every week. Standings + games accumulate
// season-long; only CHECK-INS reset each day so the organizer re-takes
// attendance each session. This file holds the adoption (mark the league's one
// event perpetual + stop cloning) and the daily check-in reset.

// perpetualCheckinWindow is how long a check-in stays "current". A perpetual
// league plays one session per day, so a check-in older than this is from a
// previous session and is cleared — the roster shows everyone un-checked-in for
// the new day. Comfortably longer than any single session, shorter than a day.
const perpetualCheckinWindow = 12 * time.Hour

// ensurePerpetualLeagueEvent adopts a recurring league into the single-ongoing-
// event model: it finds the league's one ongoing event, marks it `perpetual`,
// and STOPS its weekly cloning (recur_interval_days -> 0). Idempotent — once an
// event is perpetual it just returns that id without writing. Returns the
// ongoing event id (nil if the league has no event yet). Pass the league's
// events (already loaded by the caller); the perpetual flag is set in place.
func (s *Service) ensurePerpetualLeagueEvent(events []model.Event) *string {
	// Already adopted → return the perpetual event.
	for i := range events {
		if events[i].Perpetual {
			id := events[i].ID
			return &id
		}
	}
	// Otherwise adopt the "current session": the series head (the event the
	// recurrence was set on), else the earliest event.
	var head *model.Event
	for i := range events {
		e := &events[i]
		if e.RecurIntervalDays > 0 || (e.SeriesID != nil && *e.SeriesID == e.ID) {
			head = e
			break
		}
	}
	if head == nil && len(events) > 0 {
		head = &events[0]
	}
	if head == nil {
		return nil // no event yet — nothing to adopt
	}
	// Mark perpetual and stop the series so no more weekly clones spawn. Clearing
	// the cursor/until is belt-and-suspenders (recur_interval_days=0 already halts
	// the materializer).
	if _, err := s.sb.Update("events", "id=eq."+store.Q(head.ID), map[string]any{
		"perpetual":           true,
		"recur_interval_days": 0,
		"recur_until":         nil,
		"series_cursor":       nil,
	}); err != nil {
		// Adoption failed — report no ongoing id so the client falls back to the
		// session list rather than pointing at an unconverted event.
		return nil
	}
	head.Perpetual = true
	id := head.ID
	return &id
}

// resetStaleCheckins clears check-ins on a perpetual event that are older than
// the session window, so each new day/session starts with everyone un-checked-in
// (standings + games are untouched). Best-effort; a no-op when nothing's stale.
func (s *Service) resetStaleCheckins(eventID string) {
	if eventID == "" {
		return
	}
	cutoff := time.Now().UTC().Add(-perpetualCheckinWindow).Format(time.RFC3339)
	_, _ = s.sb.Update("registrations",
		"event_id=eq."+store.Q(eventID)+"&checked_in=eq.true&checked_in_at=lt."+store.Q(cutoff),
		map[string]any{"checked_in": false, "checked_in_at": nil})
}

// ResetPerpetualCheckins runs on the recurring ticker: for every perpetual event
// it clears check-ins from a prior session so the flag flips over without anyone
// having to open the event. Best-effort per event.
func (s *Service) ResetPerpetualCheckins() error {
	rows, err := s.sb.SelectAll("events", "perpetual=eq.true&select=id")
	if err != nil {
		return err
	}
	for _, r := range rows {
		s.resetStaleCheckins(asStr(r, "id"))
	}
	return nil
}
