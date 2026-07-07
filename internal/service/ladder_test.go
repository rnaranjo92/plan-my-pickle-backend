package service

import (
	"reflect"
	"testing"
)

// TestLeapfrogReorder exercises the pure leapfrog rule that mirrors the
// apply_ladder_result() plpgsql function: when a LOWER-ranked entrant beats a
// HIGHER-ranked one the winner takes the loser's slot and everyone between
// shifts down one; when the higher-ranked entrant wins the order is unchanged.
func TestLeapfrogReorder(t *testing.T) {
	// A 5-entrant ladder, top → bottom.
	base := func() []string { return []string{"A", "B", "C", "D", "E"} }

	cases := []struct {
		name          string
		order         []string
		winner, loser string
		want          []string
	}{
		{
			// Adjacent swap: #2 beats #1 → they swap, nothing else moves.
			name:   "adjacent swap lower beats higher",
			order:  base(),
			winner: "B", loser: "A",
			want: []string{"B", "A", "C", "D", "E"},
		},
		{
			// Lower beats higher across MULTIPLE positions: #5 (E) beats #2 (B).
			// E jumps to slot 2; B, C, D each shift down one.
			name:   "lower beats higher across multiple positions",
			order:  base(),
			winner: "E", loser: "B",
			want: []string{"A", "E", "B", "C", "D"},
		},
		{
			// Bottom beats top: #5 (E) beats #1 (A) → E to the top, all shift down.
			name:   "bottom beats top",
			order:  base(),
			winner: "E", loser: "A",
			want: []string{"E", "A", "B", "C", "D"},
		},
		{
			// Higher-ranked WINS → no change (the favourite held serve).
			name:   "higher wins no change",
			order:  base(),
			winner: "A", loser: "D",
			want: []string{"A", "B", "C", "D", "E"},
		},
		{
			// Higher-ranked wins, adjacent → still no change.
			name:   "higher wins adjacent no change",
			order:  base(),
			winner: "C", loser: "D",
			want: []string{"A", "B", "C", "D", "E"},
		},
		{
			// Middle leapfrog: #4 (D) beats #2 (B). D to slot 2; B, C shift down.
			name:   "middle leapfrog",
			order:  base(),
			winner: "D", loser: "B",
			want: []string{"A", "D", "B", "C", "E"},
		},
		{
			// Winner == loser is a no-op (defensive).
			name:   "same entrant no-op",
			order:  base(),
			winner: "C", loser: "C",
			want: []string{"A", "B", "C", "D", "E"},
		},
		{
			// Unknown entrant leaves the order untouched (defensive).
			name:   "unknown entrant no-op",
			order:  base(),
			winner: "Z", loser: "A",
			want: []string{"A", "B", "C", "D", "E"},
		},
		{
			// Two-entrant ladder, lower beats higher.
			name:   "two entrant swap",
			order:  []string{"X", "Y"},
			winner: "Y", loser: "X",
			want: []string{"Y", "X"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := leapfrogReorder(c.order, c.winner, c.loser)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("leapfrogReorder(%v, %q, %q) = %v, want %v",
					c.order, c.winner, c.loser, got, c.want)
			}
			// The result must always be a permutation of the input (no lost/dup'd
			// entrants) and the same length.
			if len(got) != len(c.order) {
				t.Fatalf("length changed: got %d, want %d", len(got), len(c.order))
			}
			seen := map[string]int{}
			for _, id := range got {
				seen[id]++
			}
			for _, id := range c.order {
				if seen[id] != 1 {
					t.Fatalf("entrant %q appears %d times in result %v (want exactly 1)",
						id, seen[id], got)
				}
			}
		})
	}
}

// TestLeapfrogReorderDoesNotMutateInput verifies the function returns a fresh
// slice and never reorders the caller's input in place (the service relies on
// this when it logs / compares before-and-after).
func TestLeapfrogReorderDoesNotMutateInput(t *testing.T) {
	in := []string{"A", "B", "C", "D"}
	_ = leapfrogReorder(in, "D", "A")
	want := []string{"A", "B", "C", "D"}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("input was mutated: got %v, want %v", in, want)
	}
}
