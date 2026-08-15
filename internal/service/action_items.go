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
	// StartsAt (RFC3339) for time-based items, so the CLIENT can render it in
	// the device's timezone.
	//
	// The server must not format this. It runs in UTC on Railway, so a 6:30 PM
	// San Diego game rendered here says "1:30 AM" — and worse, the today/
	// tomorrow boundary is computed against the wrong day entirely.
	StartsAt string `json:"startsAt,omitempty"`
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

// maxActionItems bounds the home list. See the cap in ActionItems.
const maxActionItems = 5

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
		//
		// not.is.true, NOT is.false: a pending registration can have `approved`
		// NULL as well as false — Registrations() treats anything that isn't
		// explicitly true as pending — and is.false would silently miss every
		// NULL row. Guarded on the column existing at all, because it postdates
		// some installs.
		if s.columnReady("registrations", "approved") {
			if regs, err := s.sb.Select("registrations",
				"event_id="+store.In(ids)+"&approved=not.is.true&select=event_id&limit=500"); err == nil {
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
					Kind: "upcoming",
					// No time in the title — the client appends it in local time.
					Title:    "You're playing",
					Subtitle: e.Name,
					EventID:  e.ID,
					StartsAt: *e.StartsAt,
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

	// Urgent first, then kind, then EVENT ID.
	//
	// The event id is not decoration: the approvals and unscored rows are built
	// by ranging a Go map, whose iteration order is randomised per run. Without a
	// final deterministic key, two events with pending approvals swap places on
	// every refresh — the exact reshuffling this ordering exists to prevent.
	sortActionItems(out)
	// Cap AFTER sorting, so what survives is the most urgent.
	//
	// An organizer running 25 events with pending approvals and unscored matches
	// would otherwise get 50 rows — a wall that pushes the feed off the screen
	// entirely, which is the opposite of the point. Five is about as many things
	// as anyone acts on in one sitting, and the rest are still waiting inside
	// their events.
	if len(out) > maxActionItems {
		out = out[:maxActionItems]
	}
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

// sortActionItems applies the display order. Split out so it can be tested
// directly — the randomised map iteration it defends against is invisible in a
// single run.
func sortActionItems(out []ActionItem) {
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Urgent != b.Urgent {
			return a.Urgent
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.EventID < b.EventID
	})
}
