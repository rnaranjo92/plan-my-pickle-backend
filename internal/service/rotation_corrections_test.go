package service

import "testing"

func winCourt(round, c int, winner, a1, a2, b1, b2 string) map[string]any {
	return map[string]any{
		"round": round, "court": c, "winner": winner,
		"team_a_p1": a1, "team_a_p2": a2,
		"team_b_p1": b1, "team_b_p2": b2,
	}
}

// The counting rule behind every correction. If this disagrees with the tally
// RPC, a night that gets corrected ends up with different standings than an
// identical night that doesn't — which is worse than the bug it fixes.
func TestRotationWinsFromCourts(t *testing.T) {
	t.Run("both winners on the winning team are credited", func(t *testing.T) {
		got := rotationWinsFromCourts([]map[string]any{
			winCourt(1, 1, "a", "amy", "ben", "cal", "dee"),
		})
		for _, id := range []string{"amy", "ben"} {
			if got[id] != 1 {
				t.Errorf("%s = %d wins, want 1", id, got[id])
			}
		}
		for _, id := range []string{"cal", "dee"} {
			if got[id] != 0 {
				t.Errorf("%s = %d wins, want 0", id, got[id])
			}
		}
	})

	t.Run("wins accumulate across rounds and courts", func(t *testing.T) {
		got := rotationWinsFromCourts([]map[string]any{
			winCourt(1, 1, "a", "amy", "ben", "cal", "dee"),
			winCourt(1, 2, "b", "eve", "fay", "gus", "hal"),
			winCourt(2, 1, "b", "amy", "cal", "ben", "dee"),
		})
		if got["amy"] != 1 {
			t.Errorf("amy = %d, want 1 (won r1, lost r2)", got["amy"])
		}
		if got["ben"] != 2 {
			t.Errorf("ben = %d, want 2 (won both)", got["ben"])
		}
		if got["gus"] != 1 || got["hal"] != 1 {
			t.Errorf("team b of court 2 = %d/%d, want 1 each", got["gus"], got["hal"])
		}
		if got["eve"] != 0 {
			t.Errorf("eve = %d, want 0", got["eve"])
		}
	})

	t.Run("an undecided court credits nobody", func(t *testing.T) {
		// Unreported, tied, abandoned — all of them arrive here as a winner
		// that is neither 'a' nor 'b', and the tally RPC ignores them too.
		for _, w := range []string{"", "  ", "tie", "A", "x"} {
			got := rotationWinsFromCourts([]map[string]any{
				winCourt(1, 1, w, "amy", "ben", "cal", "dee"),
			})
			if len(got) != 0 {
				t.Errorf("winner %q credited %v, want nobody", w, got)
			}
		}
	})

	t.Run("a flipped result moves the win, it does not add one", func(t *testing.T) {
		// The correction case: the same court, re-decided. Because wins are
		// DERIVED rather than incremented, the loser cannot keep the old win —
		// which is exactly what went wrong when the tally only ever added.
		before := rotationWinsFromCourts([]map[string]any{
			winCourt(3, 2, "a", "amy", "ben", "cal", "dee"),
		})
		after := rotationWinsFromCourts([]map[string]any{
			winCourt(3, 2, "b", "amy", "ben", "cal", "dee"),
		})
		if before["amy"] != 1 || after["amy"] != 0 {
			t.Errorf("amy before=%d after=%d, want 1 then 0",
				before["amy"], after["amy"])
		}
		if after["cal"] != 1 || after["dee"] != 1 {
			t.Errorf("cal/dee after = %d/%d, want 1 each", after["cal"], after["dee"])
		}
	})

	t.Run("a three-player court credits only the seats that exist", func(t *testing.T) {
		// Substitutions and departures can leave a seat empty. An empty id must
		// never become a player with wins.
		got := rotationWinsFromCourts([]map[string]any{
			winCourt(1, 1, "a", "amy", "", "cal", "dee"),
		})
		if got["amy"] != 1 {
			t.Errorf("amy = %d, want 1", got["amy"])
		}
		if _, ok := got[""]; ok {
			t.Error("the empty seat was credited a win")
		}
		if len(got) != 1 {
			t.Errorf("credited %v, want only amy", got)
		}
	})

	t.Run("no courts is an empty tally, not a nil map", func(t *testing.T) {
		got := rotationWinsFromCourts(nil)
		if got == nil {
			t.Fatal("nil map — callers index it directly")
		}
		if got["anyone"] != 0 {
			t.Errorf("unknown player = %d, want 0", got["anyone"])
		}
	})
}
