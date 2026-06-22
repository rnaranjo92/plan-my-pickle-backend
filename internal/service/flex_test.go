package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// TestFlexPairingsToCreate exercises the PURE round-robin generator behind
// GenerateFlexSchedule: n teams must yield exactly n*(n-1)/2 UNIQUE unordered
// pairs (no team paired with itself, no pair twice in either order), and a
// re-run with the full schedule already present must add NONE (idempotent).
func TestFlexPairingsToCreate(t *testing.T) {
	team := func(id string) model.Team { return model.Team{ID: id, Name: id} }

	// pairsAsMatchups turns the helper's [a,b] output into existing matchup rows,
	// so we can feed a generated schedule back in to test idempotency.
	pairsAsMatchups := func(pairs [][2]string) []model.FlexMatchup {
		out := make([]model.FlexMatchup, 0, len(pairs))
		for _, p := range pairs {
			out = append(out, model.FlexMatchup{TeamAID: p[0], TeamBID: p[1], Status: "pending"})
		}
		return out
	}

	t.Run("n teams -> n*(n-1)/2 unique pairs, no dupes", func(t *testing.T) {
		for _, n := range []int{0, 1, 2, 3, 4, 5, 6} {
			teams := make([]model.Team, 0, n)
			for i := 0; i < n; i++ {
				teams = append(teams, team(string(rune('a'+i))))
			}
			got := flexPairingsToCreate(teams, nil)

			want := n * (n - 1) / 2
			if len(got) != want {
				t.Fatalf("n=%d: got %d pairs, want %d (%v)", n, len(got), want, got)
			}
			// Every pair distinct (order-independent) and never a self-pairing.
			seen := map[string]bool{}
			for _, p := range got {
				if p[0] == p[1] {
					t.Fatalf("n=%d: self-pairing %v", n, p)
				}
				k := pairKey(p[0], p[1])
				if seen[k] {
					t.Fatalf("n=%d: duplicate pair %v", n, p)
				}
				seen[k] = true
			}
			if len(seen) != want {
				t.Fatalf("n=%d: %d unique pairs, want %d", n, len(seen), want)
			}
		}
	})

	t.Run("idempotent re-run adds none", func(t *testing.T) {
		teams := []model.Team{team("a"), team("b"), team("c"), team("d")}
		first := flexPairingsToCreate(teams, nil)
		if len(first) != 6 { // C(4,2)
			t.Fatalf("first run = %d pairs, want 6", len(first))
		}
		// Feed the generated schedule back as existing — a second run must add 0.
		second := flexPairingsToCreate(teams, pairsAsMatchups(first))
		if len(second) != 0 {
			t.Fatalf("idempotent re-run added %d pairs, want 0 (%v)", len(second), second)
		}
	})

	t.Run("re-run after adding a team fills only the missing pairs", func(t *testing.T) {
		teams := []model.Team{team("a"), team("b"), team("c")}
		existing := pairsAsMatchups(flexPairingsToCreate(teams, nil)) // 3 pairs among a,b,c

		// A fourth team joins → only its 3 new pairings (d-a, d-b, d-c) are added.
		teams = append(teams, team("d"))
		got := flexPairingsToCreate(teams, existing)
		if len(got) != 3 {
			t.Fatalf("after adding a 4th team, got %d new pairs, want 3 (%v)", len(got), got)
		}
		for _, p := range got {
			if p[0] != "d" && p[1] != "d" {
				t.Fatalf("new pair %v does not involve the added team d", p)
			}
		}
	})

	t.Run("existing pair blocks the reverse order too", func(t *testing.T) {
		teams := []model.Team{team("a"), team("b")}
		// Existing matchup stored as b-vs-a must still block generating a-vs-b.
		existing := []model.FlexMatchup{{TeamAID: "b", TeamBID: "a", Status: "completed"}}
		got := flexPairingsToCreate(teams, existing)
		if len(got) != 0 {
			t.Fatalf("reverse-order existing pair not deduped: got %v", got)
		}
	})
}
