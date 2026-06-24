package engine

import (
	"fmt"
	"testing"
)

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("p%d", i)
	}
	return out
}

func playersOf(m MatchSpec) []string {
	return append(append([]string{}, m.Team1...), m.Team2...)
}

func TestSinglesRoundRobin(t *testing.T) {
	rounds := GenerateSchedule(ids(5), Singles, Rotating, 2, nil, 0, 0, 0) // odd -> byes
	met := map[string]bool{}
	for _, r := range rounds {
		seen := map[string]bool{}
		for _, m := range r.Matches {
			for _, p := range playersOf(m) {
				if seen[p] {
					t.Fatalf("player %s twice in round %d", p, r.RoundNumber)
				}
				seen[p] = true
			}
			key := m.Team1[0]
			other := m.Team2[0]
			if key > other {
				key, other = other, key
			}
			pair := key + "-" + other
			if met[pair] {
				t.Fatalf("pair %s repeated", pair)
			}
			met[pair] = true
		}
	}
	if len(met) != 10 { // C(5,2)
		t.Fatalf("want 10 unique pairs, got %d", len(met))
	}
}

func TestCourtAssignmentNoDoubleBooking(t *testing.T) {
	rounds := GenerateSchedule(ids(8), Singles, Rotating, 2, nil, 0, 0, 0)
	for _, r := range rounds {
		bySlot := map[int][]MatchSpec{}
		for _, m := range r.Matches {
			bySlot[m.Slot] = append(bySlot[m.Slot], m)
		}
		for slot, ms := range bySlot {
			courts := map[int]bool{}
			ppl := map[string]bool{}
			for _, m := range ms {
				if courts[m.CourtNumber] {
					t.Fatalf("court reused in slot %d", slot)
				}
				courts[m.CourtNumber] = true
				for _, p := range playersOf(m) {
					if ppl[p] {
						t.Fatalf("player on two courts in slot %d", slot)
					}
					ppl[p] = true
				}
			}
		}
	}
}

func TestPoolRoundBounds(t *testing.T) {
	// 8 singles players → a full round-robin is 7 rounds.
	full := GenerateSchedule(ids(8), Singles, Rotating, 2, nil, 0, 0, 0)
	if len(full) != 7 {
		t.Fatalf("full RR: want 7 rounds, got %d", len(full))
	}
	// max caps it (partial round-robin).
	capped := GenerateSchedule(ids(8), Singles, Rotating, 2, nil, 0, 0, 3)
	if len(capped) != 3 {
		t.Fatalf("max=3: want 3 rounds, got %d", len(capped))
	}
	// min tops it up past the natural length by repeating matchups.
	topped := GenerateSchedule(ids(8), Singles, Rotating, 2, nil, 0, 10, 0)
	if len(topped) != 10 {
		t.Fatalf("min=10: want 10 rounds, got %d", len(topped))
	}
	// Rounds renumber 1..N after fitting.
	for i, r := range topped {
		if r.RoundNumber != i+1 {
			t.Fatalf("round %d mislabeled %d", i, r.RoundNumber)
		}
	}
}

func TestRotatingDoubles8(t *testing.T) {
	rounds := GenerateSchedule(ids(8), Doubles, Rotating, 2, nil, 6, 0, 0)
	games := map[string]int{}
	partner := map[string]int{}
	maxPartner := 0
	for _, r := range rounds {
		seen := map[string]bool{}
		for _, m := range r.Matches {
			if len(m.Team1) != 2 || len(m.Team2) != 2 {
				t.Fatalf("doubles team must have 2 players")
			}
			for _, p := range playersOf(m) {
				if seen[p] {
					t.Fatalf("double-booked %s", p)
				}
				seen[p] = true
				games[p]++
			}
			for _, team := range [][]string{m.Team1, m.Team2} {
				a, b := team[0], team[1]
				if a > b {
					a, b = b, a
				}
				k := a + "-" + b
				partner[k]++
				if partner[k] > maxPartner {
					maxPartner = partner[k]
				}
			}
		}
	}
	for _, g := range games {
		if g != 6 {
			t.Fatalf("every player should play 6 games, got %d", g)
		}
	}
	if maxPartner > 2 {
		t.Fatalf("partnership repeated %d times (>2)", maxPartner)
	}
}

func TestRotatingDoubles6FairSitOuts(t *testing.T) {
	rounds := GenerateSchedule(ids(6), Doubles, Rotating, 1, nil, 6, 0, 0)
	games := map[string]int{}
	for _, p := range ids(6) {
		games[p] = 0
	}
	for _, r := range rounds {
		var ppl []string
		for _, m := range r.Matches {
			ppl = append(ppl, playersOf(m)...)
		}
		if len(ppl) != 4 {
			t.Fatalf("exactly 4 should play each round, got %d", len(ppl))
		}
		for _, p := range ppl {
			games[p]++
		}
	}
	lo, hi := 1<<30, 0
	for _, g := range games {
		if g < lo {
			lo = g
		}
		if g > hi {
			hi = g
		}
	}
	if hi-lo > 1 {
		t.Fatalf("unfair sit-outs: spread %d", hi-lo)
	}
}
