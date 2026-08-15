package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// The home screen's "what needs me" list.
//
// Home used to be six tiles of TOTALS — games played, clubs, DUPR — which tell
// you how you're doing and nothing about what to do. Opening the app and
// finding a scoreboard is why people close it again.
//
// These are the things that are actually waiting: players to approve, matches
// nobody scored, a game this evening. Plus one achievement in progress, because
// "two wins from a badge" is itself a reason to go play.
//
// ONE endpoint, not six calls. Six round-trips on app open is how a home screen
// gets slow, and slow beats bad layout for driving people away.

// ActionItem is one row in that list.
type ActionItem struct {
	// Kind lets the client pick an icon and route without parsing the title.
	Kind  string `json:"kind"` // approvals | unscored | upcoming | achievement
	Title string `json:"title"`
	// Subtitle is the supporting line — the event name, the date.
	Subtitle string `json:"subtitle,omitempty"`
	// Count drives a badge where a number means something (3 players waiting).
	Count int `json:"count,omitempty"`
	// EventID, when tapping should open an event.
	EventID string `json:"eventId,omitempty"`
	// Urgent items sort first and render in the alert colour. Reserved for
	// things with a deadline attached, never for "you could do this".
	Urgent bool `json:"urgent,omitempty"`
}

// countOf renders "1 player waiting" / "3 players waiting". The api package has
// its own plural(); duplicated rather than exported because the dependency runs
// api -> service, and a helper this small isn't worth inverting it.
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// gamesPlayedMilestones are the badges a player is working toward.
//
// Deliberately shallow at the start (5, then 10) — a first badge that takes
// fifty games is not an incentive, it's a wall. They widen once the habit
// exists.
var gamesPlayedMilestones = []int{5, 10, 25, 50, 100, 250, 500}

// ActionItems returns everything waiting on this user, most urgent first.
//
// Every section is best-effort: one failing query drops its own rows rather
// than emptying the list. A home screen that shows three of four things is
// useful; one that shows an error is not.
func (s *Service) ActionItems(userID, email string) ([]ActionItem, error) {
	if strings.TrimSpace(userID) == "" {
		return []ActionItem{}, nil
	}
	out := make([]ActionItem, 0, 6)
	now := time.Now().UTC()

	// --- Organizer: events I run that aren't finished.
	owned, _ := s.sb.Select("events",
		"owner_id=eq."+store.Q(userID)+"&status=neq.completed"+
			"&select=id,name&limit=25")
	ids := make([]string, 0, len(owned))
	nameByID := map[string]string{}
	for _, e := range owned {
		if id := asStr(e, "id"); id != "" {
			ids = append(ids, id)
			nameByID[id] = asStr(e, "name")
		}
	}

	if len(ids) > 0 {
		// Players waiting on approval. Grouped by event: "3 waiting" across two
		// events is two different jobs, and a combined count hides which.
		if regs, err := s.sb.Select("registrations",
			"event_id="+store.In(ids)+"&approved=is.false&select=event_id&limit=500"); err == nil {
			perEvent := map[string]int{}
			for _, r := range regs {
				perEvent[asStr(r, "event_id")]++
			}
			for id, n := range perEvent {
				out = append(out, ActionItem{
					Kind:     "approvals",
					Title:    countOf(n, "player waiting", "players waiting"),
					Subtitle: nameByID[id],
					Count:    n,
					EventID:  id,
					Urgent:   true, // somebody is waiting on a human
				})
			}
		}

		// Matches with no score. These block standings, so they're the thing an
		// organizer most often forgets and most needs reminding of.
		if ms, err := s.sb.Select("matches",
			"event_id="+store.In(ids)+"&status=eq.scheduled&select=event_id&limit=500"); err == nil {
			perEvent := map[string]int{}
			for _, m := range ms {
				perEvent[asStr(m, "event_id")]++
			}
			for id, n := range perEvent {
				out = append(out, ActionItem{
					Kind:     "unscored",
					Title:    countOf(n, "match to score", "matches to score"),
					Subtitle: nameByID[id],
					Count:    n,
					EventID:  id,
				})
			}
		}
	}

	// --- Player: my next game, if it's soon.
	//
	// Only within a week. A tournament two months out is not an action item, and
	// putting it here would make the list something people learn to ignore.
	if evs, err := s.MyEvents(userID, email); err == nil {
		var soonest *ActionItem
		var soonestAt time.Time
		for _, e := range evs {
			if e.StartsAt == nil {
				continue
			}
			t, perr := time.Parse(time.RFC3339, *e.StartsAt)
			if perr != nil || t.Before(now) || t.After(now.AddDate(0, 0, 7)) {
				continue
			}
			if soonest == nil || t.Before(soonestAt) {
				soonestAt = t
				soonest = &ActionItem{
					Kind:     "upcoming",
					Title:    "You're playing " + humanWhen(t, now),
					Subtitle: e.Name,
					EventID:  e.ID,
					Urgent:   t.Sub(now) < 24*time.Hour,
				}
			}
		}
		if soonest != nil {
			out = append(out, *soonest)
		}
	}

	// --- One achievement in progress.
	//
	// Only the NEXT one, and only when it's close. A list of everything you
	// haven't done is a chore list; "2 more games" is a nudge.
	if n := s.gamesPlayedFor(userID); n > 0 {
		for _, m := range gamesPlayedMilestones {
			if n < m {
				if left := m - n; left <= 5 {
					out = append(out, ActionItem{
						Kind:  "achievement",
						Title: countOf(left, "game", "games") + fmt.Sprintf(" from your %d-game badge", m),
						Count: left,
					})
				}
				break
			}
		}
	}

	// Urgent first, then by kind so the order is stable between refreshes —
	// a list that reshuffles on every poll is one nobody trusts.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Urgent != out[j].Urgent {
			return out[i].Urgent
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

// gamesPlayedFor reads the player's completed-match count, or 0.
func (s *Service) gamesPlayedFor(userID string) int {
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=games_played")
	if err != nil || row == nil {
		return 0
	}
	return asInt(row, "games_played")
}

// humanWhen renders a nearby time the way a person says it.
func humanWhen(t, now time.Time) string {
	d := t.Sub(now)
	switch {
	case d < time.Hour:
		return "within the hour"
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "today at " + t.Local().Format("3:04 PM")
	case t.YearDay() == now.AddDate(0, 0, 1).YearDay():
		return "tomorrow at " + t.Local().Format("3:04 PM")
	default:
		return t.Local().Format("Mon") + " at " + t.Local().Format("3:04 PM")
	}
}
