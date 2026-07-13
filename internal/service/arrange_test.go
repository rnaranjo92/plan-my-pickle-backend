package service

import "testing"

// TestArrangePlacementsStrictCourts locks in per-division court assignment: a
// division only plays on its assigned courts, disjoint divisions run in
// parallel, shared courts sequence by division order, and players are never
// double-booked in a slot.
func TestArrangePlacementsStrictCourts(t *testing.T) {
	// unique player per match (no cross-division conflicts unless we say so)
	mp := func(ids ...string) map[string][]string {
		m := map[string][]string{}
		for _, id := range ids {
			m[id] = []string{id + "-p"}
		}
		return m
	}
	inSet := func(v int, set ...int) bool {
		for _, s := range set {
			if v == s {
				return true
			}
		}
		return false
	}

	t.Run("disjoint courts run in parallel and never spill", func(t *testing.T) {
		divRounds := map[string][][]string{
			"A": {{"a1", "a2", "a3", "a4"}},
			"B": {{"b1", "b2", "b3", "b4"}},
		}
		divCourts := map[string][]int{"A": {1, 2}, "B": {3, 4}}
		players := mp("a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4")
		pl, n := arrangePlacements([]string{"A", "B"}, divRounds, divCourts,
			[]int{1, 2, 3, 4}, players, 8, 0, true)
		if n != 8 {
			t.Fatalf("scheduled=%d want 8", n)
		}
		for _, id := range []string{"a1", "a2", "a3", "a4"} {
			if !inSet(pl[id].court, 1, 2) {
				t.Errorf("%s on court %d, want 1 or 2", id, pl[id].court)
			}
		}
		for _, id := range []string{"b1", "b2", "b3", "b4"} {
			if !inSet(pl[id].court, 3, 4) {
				t.Errorf("%s on court %d, want 3 or 4", id, pl[id].court)
			}
		}
		if pl["a1"].slot != 0 || pl["b1"].slot != 0 {
			t.Errorf("divisions should both start at slot 0; a1=%d b1=%d", pl["a1"].slot, pl["b1"].slot)
		}
	})

	t.Run("shared court sequences divisions (earlier order first)", func(t *testing.T) {
		divRounds := map[string][][]string{"A": {{"a1", "a2"}}, "B": {{"b1", "b2"}}}
		divCourts := map[string][]int{"A": {1}, "B": {1}}
		pl, n := arrangePlacements([]string{"A", "B"}, divRounds, divCourts,
			[]int{1}, mp("a1", "a2", "b1", "b2"), 4, 0, true)
		if n != 4 {
			t.Fatalf("scheduled=%d want 4", n)
		}
		for _, id := range []string{"a1", "a2", "b1", "b2"} {
			if pl[id].court != 1 {
				t.Errorf("%s court=%d want 1", id, pl[id].court)
			}
		}
		if !(pl["a1"].slot < pl["b1"].slot && pl["a2"].slot < pl["b1"].slot) {
			t.Errorf("A should fully precede B on the shared court: a=%d,%d b=%d,%d",
				pl["a1"].slot, pl["a2"].slot, pl["b1"].slot, pl["b2"].slot)
		}
	})

	t.Run("no player double-booked across divisions in one slot", func(t *testing.T) {
		players := map[string][]string{"a1": {"p"}, "b1": {"p"}} // shared player p
		pl, _ := arrangePlacements([]string{"A", "B"},
			map[string][][]string{"A": {{"a1"}}, "B": {{"b1"}}},
			map[string][]int{"A": {1}, "B": {2}}, []int{1, 2}, players, 2, 0, true)
		if pl["a1"].slot == pl["b1"].slot {
			t.Errorf("player double-booked: a1 and b1 both at slot %d", pl["a1"].slot)
		}
	})

	t.Run("sequential mode honors division courts", func(t *testing.T) {
		pl, n := arrangePlacements([]string{"A"},
			map[string][][]string{"A": {{"a1", "a2", "a3"}}},
			map[string][]int{"A": {1, 2}}, []int{1, 2, 3, 4}, mp("a1", "a2", "a3"), 3, 0, false)
		if n != 3 {
			t.Fatalf("scheduled=%d want 3", n)
		}
		for _, id := range []string{"a1", "a2", "a3"} {
			if !inSet(pl[id].court, 1, 2) {
				t.Errorf("%s court=%d want 1 or 2", id, pl[id].court)
			}
		}
	})
}
