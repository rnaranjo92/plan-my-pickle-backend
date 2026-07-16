package service

import (
	"errors"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Recurring "socials" — the weekly-habit engine. An event with
// recur_interval_days > 0 is a series HEAD; MaterializeRecurringEvents (run on a
// ticker) spawns the next occurrence a week ahead and pushes the club to RSVP.
// This turns PlanMyPickle from an episodic tournament tool into a recurring
// touchpoint — the highest-leverage retention move vs. community-first rivals.

// occurrenceLookahead is how far in advance the next occurrence is created — far
// enough that players can plan/RSVP, near enough that we don't litter the
// calendar months out.
const occurrenceLookahead = 8 * 24 * time.Hour

// SetEventRecurrence turns a recurring social on/off or changes its cadence
// (organizer only). intervalDays 0 STOPS the series (no further occurrences);
// >0 (re)starts it every N days and stamps series_id so occurrences link back.
// until is an optional RFC3339 end ("" = open-ended / clears). Kept separate
// from UpdateEvent so a normal event edit never has to reference these columns
// before the add_recurring_events migration is applied.
func (s *Service) SetEventRecurrence(eventID string, intervalDays int, until string) error {
	if intervalDays < 0 {
		return errors.New("interval must be 0 or more days")
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=starts_at,series_id")
	if err != nil {
		return err
	}
	if ev == nil {
		return ErrNotFound
	}
	// Only a series HEAD (or a plain event) can define recurrence. An occurrence
	// already belongs to a series (series_id points at its head), so turning
	// repeat on here would detach it and duplicate its parent's slot.
	if sid := asStrPtr(ev, "series_id"); sid != nil && *sid != "" && *sid != eventID {
		return errors.New("this is a session of a recurring series — change the repeat on the original event")
	}
	if intervalDays > 0 {
		if hs := asStrPtr(ev, "starts_at"); hs == nil || *hs == "" {
			return errors.New("set a start date on the event before making it repeat")
		}
	}
	upd := map[string]any{"recur_interval_days": intervalDays}
	if intervalDays > 0 {
		upd["series_id"] = eventID // anchor the head to its own series
		// Only touch recur_until when the caller actually sends one — a bare
		// cadence change must not silently wipe a configured end date. Validate
		// it as RFC3339 so a malformed value can't later disable the end cap.
		if u := strings.TrimSpace(until); u != "" {
			if _, err := time.Parse(time.RFC3339, u); err != nil {
				return errors.New("end date must be a valid RFC3339 timestamp")
			}
			upd["recur_until"] = u
		}
	} else {
		// Stopping the series clears its end date and cursor.
		upd["recur_until"] = nil
		upd["series_cursor"] = nil
	}
	rows, err := s.sb.Update("events", "id=eq."+store.Q(eventID), upd)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrNotFound
	}
	return nil
}

// MaterializeRecurringEvents ensures every active series head has its next
// occurrence created once that occurrence falls within the lookahead window.
// Idempotent across ticks via a per-head series_cursor that only advances (so a
// deleted occurrence is never resurrected); missed past occurrences are
// fast-forwarded, never back-filled. Best-effort per series.
func (s *Service) MaterializeRecurringEvents() error {
	heads, err := s.sb.SelectAll("events",
		"recur_interval_days=gt.0"+
			"&select=id,recur_interval_days,recur_until,name,club_id,starts_at,ends_at,series_cursor")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	lookahead := now.Add(occurrenceLookahead)
	for _, h := range heads {
		headID := asStr(h, "id")
		interval := asInt(h, "recur_interval_days")
		if headID == "" || interval <= 0 {
			continue
		}
		// Anchor on the last slot we SPAWNED (series_cursor), falling back to the
		// head's own start (occurrence #1). Both null = an undated head with no
		// anchor — nothing to schedule from (guarded at create/set time too).
		anchor, ok := parseFirst(asStrPtr(h, "series_cursor"), asStrPtr(h, "starts_at"))
		if !ok {
			continue
		}
		// Next slot after the anchor, fast-forwarded past any missed (past) slots
		// so a dormant series resumes on its NEXT real date — never a back-fill.
		next := anchor.AddDate(0, 0, interval)
		for next.Before(now) {
			next = next.AddDate(0, 0, interval)
		}
		if ru := asStrPtr(h, "recur_until"); ru != nil && *ru != "" {
			if until, err := time.Parse(time.RFC3339, *ru); err == nil && next.After(until) {
				continue // series has ended
			}
		}
		if next.After(lookahead) {
			continue // not due yet — a later tick will create it
		}
		nextStr := next.UTC().Format(time.RFC3339)
		// Materialize the slot unless it already exists (a prior tick created it
		// but failed to advance the cursor). Clone FIRST, advance the cursor only
		// AFTER: a transient clone failure then simply retries next tick instead of
		// permanently skipping the session. Once the cursor moves past a slot, a
		// deleted occurrence is never recreated.
		if dup, _ := s.sb.SelectOne("events",
			"series_id=eq."+store.Q(headID)+"&starts_at=eq."+store.Q(nextStr)+
				"&select=id"); dup == nil {
			// Preserve the head's duration on the new occurrence.
			var newEnds time.Time
			if hs, he := asStrPtr(h, "starts_at"), asStrPtr(h, "ends_at"); hs != nil && he != nil {
				if st, err1 := time.Parse(time.RFC3339, *hs); err1 == nil {
					if en, err2 := time.Parse(time.RFC3339, *he); err2 == nil && en.After(st) {
						newEnds = next.Add(en.Sub(st))
					}
				}
			}
			newID, err := s.cloneEventOccurrence(headID, next, newEnds)
			if err != nil {
				log.Printf("recurring: clone for series %s failed: %v", headID, err)
				continue // cursor NOT advanced → retried next tick
			}
			if clubID := asStr(h, "club_id"); clubID != "" {
				s.notifySocialToClub(clubID, newID, asStr(h, "name"))
			}
			log.Printf("recurring: spawned occurrence %s of series %s for %s",
				newID, headID, nextStr)
		}
		// Advance the cursor now that the slot is materialized (or already was).
		if _, err := s.sb.Update("events", "id=eq."+store.Q(headID),
			map[string]any{"series_cursor": nextStr}); err != nil {
			log.Printf("recurring: cursor advance for series %s failed: %v", headID, err)
		}
	}
	return nil
}

// parseFirst returns the first parseable RFC3339 time among the candidates, and
// whether any parsed. Used to anchor a series on its cursor, else its start.
func parseFirst(candidates ...*string) (time.Time, bool) {
	for _, c := range candidates {
		if c != nil && *c != "" {
			if t, err := time.Parse(time.RFC3339, *c); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// eventCloneCols is the allowlist of config columns copied to a new recurring
// occurrence — mirrors CreateEvent's payload. Deliberately EXCLUDES identity
// (id/created_at), entitlement/payment state (premium_pass, stripe_*), and the
// recurrence fields (an occurrence is never itself a series head).
var eventCloneCols = []string{
	"name", "format", "partner_mode", "tournament_format", "scoring_mode",
	"num_courts", "points_to_win", "win_by", "best_of", "game_duration_minutes",
	"registration_fee_cents", "extra_division_fee_mode",
	"additional_division_fee_cents", "addon_tee_cents", "addon_grips_cents", "currency",
	"location", "contact_phone", "zelle_handle", "club_id", "dupr_sanctioned",
	"dupr_min_entitlement", "cash_prize", "cash_prize_amount", "consolation",
	"auto_adjust", "auto_start_next", "court_calls", "team_size", "admin_passcode",
	"owner_id", "listed", "player_scoring", "score_confirm_minutes", "poster_url",
	"venue_name", "venue_address", "venue_phone", "venue_website", "venue_lat",
	"venue_lng", "description", "venue_notes", "waiver_url", "min_pool_rounds",
	"max_pool_rounds", "county", "state",
	// Scheduling + presentation config the auto-scheduler/TV board rely on — must
	// carry to each occurrence or matches book through breaks/past close times and
	// the board reverts to the default theme.
	"schedule_breaks", "day_cap_minutes", "day_end_minutes", "scoreboard_theme",
	"league_id",
}

// cloneEventOccurrence deep-copies a series head into a new event dated startsAt:
// the events config row (minus identity/state), its divisions, and its courts —
// so an occurrence is a full, playable event. Registrations/matches are NOT
// copied (each session starts empty).
func (s *Service) cloneEventOccurrence(headID string, startsAt, endsAt time.Time) (string, error) {
	row, err := s.sb.SelectOne("events", "id=eq."+store.Q(headID)+"&select=*")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	// Build the occurrence from an explicit config ALLOWLIST — never copy the
	// whole row, or entitlement/payment state (premium_pass, stripe_*) would leak
	// a paid unlock onto every free auto-spawned occurrence. Missing columns
	// (older DBs) are simply skipped.
	payload := map[string]any{}
	for _, k := range eventCloneCols {
		if v, ok := row[k]; ok && v != nil {
			payload[k] = v
		}
	}
	payload["starts_at"] = startsAt.UTC().Format(time.RFC3339)
	if !endsAt.IsZero() {
		payload["ends_at"] = endsAt.UTC().Format(time.RFC3339)
	}
	payload["series_id"] = headID
	// recur_interval_days omitted → DB default 0 (an occurrence is not a head).
	payload["status"] = "open"

	ins, err := s.sb.Insert("events", payload)
	if err != nil {
		return "", err
	}
	if len(ins) == 0 {
		return "", errors.New("occurrence insert returned no row")
	}
	newID := asStr(ins[0], "id")

	// Clone divisions.
	if braw, err := s.sb.Select("brackets",
		"event_id=eq."+store.Q(headID)+"&select=*&order=sort_order"); err == nil {
		clones := make([]map[string]any, 0, len(braw))
		for _, b := range braw {
			delete(b, "id")
			delete(b, "created_at")
			b["event_id"] = newID
			clones = append(clones, b)
		}
		if len(clones) > 0 {
			if _, err := s.sb.Insert("brackets", clones); err != nil {
				log.Printf("recurring: clone brackets for %s failed: %v", newID, err)
			}
		}
	}
	// Clone courts.
	if craw, err := s.sb.Select("courts",
		"event_id=eq."+store.Q(headID)+"&select=*&order=court_number"); err == nil {
		clones := make([]map[string]any, 0, len(craw))
		for _, c := range craw {
			delete(c, "id")
			delete(c, "created_at")
			c["event_id"] = newID
			clones = append(clones, c)
		}
		if len(clones) > 0 {
			if _, err := s.sb.Insert("courts", clones); err != nil {
				log.Printf("recurring: clone courts for %s failed: %v", newID, err)
			}
		}
	}
	// Publish to the feed like a normal create.
	s.ensureEventPosts([]string{newID})
	return newID, nil
}

// notifySocialToClub push-notifies a club's members that a new session is up,
// deep-linking to the event so they can RSVP. Push-only (linked accounts) —
// the recurring habit is for app users, and it costs nothing.
func (s *Service) notifySocialToClub(clubID, eventID, name string) {
	members, err := s.sb.SelectAll("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id")
	if err != nil {
		return
	}
	seen := map[string]bool{}
	var uids []string
	for _, m := range members {
		if uid := asStr(m, "user_id"); uid != "" && !seen[uid] {
			seen[uid] = true
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return
	}
	if name == "" {
		name = "New session"
	}
	// No per-event timezone is stored, so we don't assert a calendar day here
	// (server-UTC formatting could name the wrong local day for evening slots) —
	// the deep-linked event page shows the correct local date/time.
	url := "https://app.planmypickle.com/?event=" + eventID
	_ = s.sendPush(uids, name, "A new session is up — tap to RSVP", url)
}
