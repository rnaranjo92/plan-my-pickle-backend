package service

import (
	"fmt"
	"testing"
)

// TestE2ETournamentScheduling is an end-to-end scenario over the scheduling
// engine: for each supported format it seeds a full checked-in field in one
// division, generates the schedule, and asserts the engine's core invariants —
// it produces matches, drops nobody (Unscheduled empty), and (round-robin, where
// everyone is guaranteed to play) every registered player actually appears in the
// persisted match participants. This is the regression guard that a change to the
// pairing/persist path never silently drops a player or produces an empty draw.
func TestE2ETournamentScheduling(t *testing.T) {
	cases := []struct {
		format   string
		players  int
		everyone bool // round-robin guarantees every player is scheduled
	}{
		{"round_robin", 8, true},
		{"round_robin", 6, true},
		{"single_elim", 8, false},
		{"pools_playoff", 8, false},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("%s_%dp", c.format, c.players), func(t *testing.T) {
			f := newFake().
				seed("events", fmt.Sprintf(`[{"id":"e1","name":"Scenario","owner_id":"o","tournament_format":%q,"format":"doubles","partner_mode":"rotating","num_courts":2,"points_to_win":11,"win_by":2,"best_of":1,"status":"draft"}]`, c.format)).
				seed("brackets", `[{"id":"b1","event_id":"e1","name":"Open","division_type":"open"}]`).
				seed("registrations", regRows(c.players))
			s := newFakeSvc(t, f)

			res, err := s.GenerateSchedule("e1", true, true)
			if err != nil {
				t.Fatalf("GenerateSchedule(%s) error: %v", c.format, err)
			}
			if res.Matches <= 0 {
				t.Fatalf("%s: expected matches to be scheduled, got %d", c.format, res.Matches)
			}
			if len(res.Unscheduled) != 0 {
				t.Fatalf("%s: %d players left unscheduled: %v",
					c.format, len(res.Unscheduled), res.Unscheduled)
			}
			if c.everyone {
				seen := map[string]bool{}
				for _, row := range f.written("match_participants") {
					if pid, ok := row["player_id"].(string); ok {
						seen[pid] = true
					}
				}
				for i := 1; i <= c.players; i++ {
					if pid := fmt.Sprintf("p%d", i); !seen[pid] {
						t.Fatalf("%s: player %s was never scheduled into any match", c.format, pid)
					}
				}
			}
		})
	}
}
