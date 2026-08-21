package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Sample data for a club, so its pages can be looked at with something in them.
//
// An empty club page tells you nothing about whether a club page is any good.
// This fills one with the shape of a real club's week: a weekly league, a
// weekend tournament, an ongoing ladder, and the activity those produce.
//
// TWO DELIBERATE CHOICES:
//
// The events are UNLISTED. Realistic names are required — the club feed filters
// anything matching the demo/test pattern, so calling them "Test League" would
// seed a feed that renders empty — but a realistic name that is also PUBLIC
// would put invented events into nearby discovery and the SEO pages, where real
// people would try to register for them. Unlisted keeps them to the club.
//
// It is additive and repeatable. It never deletes, so running it twice gives
// you more sample data rather than a surprise: nothing here is worth building a
// destructive path for.

// SeedClubDemo fills a club with sample events and activity. Returns how many
// events it created.
func (s *Service) SeedClubDemo(clubID, callerID string) (int, error) {
	if !s.IsClubAdmin(clubID, callerID) {
		return 0, ErrForbidden
	}
	club, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=name,city")
	if err != nil {
		return 0, err
	}
	if club == nil {
		return 0, ErrNotFound
	}
	where := strings.TrimSpace(asStr(club, "city"))
	if where == "" {
		where = "the club"
	}

	now := time.Now().UTC()
	type sample struct {
		name    string
		format  string
		starts  time.Time
		posts   [][2]string // {type, text}
	}
	samples := []sample{
		{
			name:   "Tuesday Night League",
			format: "round_robin",
			starts: now.AddDate(0, 0, 2),
			posts: [][2]string{
				{"schedule_posted", "Round 3 is up — courts 1 through 4."},
				{"match_final", "Dana & Priya beat Marco & Sam 11-9"},
				{"match_final", "Alex & Jordan beat Rae & Tom 11-7"},
			},
		},
		{
			name:   "Summer Slam",
			format: "pools_playoff",
			starts: now.AddDate(0, 0, 9),
			posts: [][2]string{
				{"announcement", "Registration closes Friday — 6 spots left."},
				{"registered", "4 players registered"},
			},
		},
		{
			name:   "Club Ladder",
			format: "round_robin",
			starts: now.AddDate(0, 0, -3),
			posts: [][2]string{
				{"match_final", "Priya moved up to rung 3"},
				{"announcement", fmt.Sprintf("Ladder resets at the end of the month at %s.", where)},
			},
		},
	}

	made := 0
	for _, sm := range samples {
		row := map[string]any{
			"name":              sm.name,
			"owner_id":          callerID,
			"club_id":           clubID,
			"format":            "doubles",
			"tournament_format": sm.format,
			"num_courts":        4,
			"points_to_win":     11,
			"win_by":            2,
			"starts_at":         sm.starts.Format(time.RFC3339),
			// Kept OFF the public feeds — see the note above.
			"listed": false,
		}
		rows, ierr := s.sb.Insert("events", row)
		if ierr != nil || len(rows) == 0 {
			continue // one bad sample shouldn't cost the others
		}
		id := asStr(rows[0], "id")
		if id == "" {
			continue
		}
		made++
		// The event's own card, then its activity — the same order a real event
		// produces them in, so the feed reads chronologically.
		s.AddFeedItem(id, "event", sm.name, id)
		for _, p := range sm.posts {
			s.AddFeedItem(id, p[0], p[1], "")
		}
	}
	if made == 0 {
		return 0, errors.New("could not create any sample events")
	}
	return made, nil
}
