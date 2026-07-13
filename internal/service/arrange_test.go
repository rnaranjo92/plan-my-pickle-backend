package service

import (
	"reflect"
	"testing"
)

// TestDivisionCourtMapFrom locks in filtering (drop nonexistent courts),
// de-duplication, and the empty/all-invalid -> all-courts default.
func TestDivisionCourtMapFrom(t *testing.T) {
	courtByNum := map[int]string{1: "c1", 2: "c2", 3: "c3", 4: "c4"}
	courtNums := []int{1, 2, 3, 4}
	brackets := []map[string]any{
		{"id": "keep", "courts": []any{3.0, 4.0}},          // valid subset
		{"id": "dup", "courts": []any{1.0, 1.0, 2.0}},      // duplicates -> deduped
		{"id": "bad", "courts": []any{5.0, 6.0}},           // all invalid -> default all
		{"id": "mixed", "courts": []any{2.0, 9.0}},         // drop 9 -> [2]
		{"id": "empty"},                                    // none -> default all
	}
	got := divisionCourtMapFrom(brackets, courtByNum, courtNums)
	want := map[string][]int{
		"keep":  {3, 4},
		"dup":   {1, 2},
		"bad":   {1, 2, 3, 4},
		"mixed": {2},
		"empty": {1, 2, 3, 4},
	}
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("bracket %q: got %v want %v", k, got[k], w)
		}
	}
}

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

	t.Run("unknown/missing division courts fall back to all courts (no skip/panic)", func(t *testing.T) {
		// divCourts has no entry for div "A" (simulates an orphan bracket_id).
		pl, n := arrangePlacements([]string{"A"},
			map[string][][]string{"A": {{"a1", "a2"}}},
			map[string][]int{}, []int{1, 2, 3}, mp("a1", "a2"), 2, 0, true)
		if n != 2 {
			t.Fatalf("scheduled=%d want 2 (must not skip on missing court set)", n)
		}
		// sequential path too (would index a nil slice without the guard)
		pl2, n2 := arrangePlacements([]string{"A"},
			map[string][][]string{"A": {{"a1", "a2"}}},
			map[string][]int{}, []int{1, 2, 3}, mp("a1", "a2"), 2, 0, false)
		if n2 != 2 {
			t.Fatalf("sequential scheduled=%d want 2", n2)
		}
		_ = pl
		_ = pl2
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
