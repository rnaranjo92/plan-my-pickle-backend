package engine

import (
	"fmt"
	"testing"
)

func deKey(tier string, r, s int) string { return fmt.Sprintf("%s:%d:%d", tier, r, s) }

func TestDoubleElimStructure(t *testing.T) {
	for _, n := range []int{2, 4, 8, 16} {
		p := GenerateDoubleElim(seeds(n))
		wb, lb, gf := 0, 0, 0
		byKey := map[string]*DEMatch{}
		for _, m := range p.Matches {
			byKey[deKey(m.Tier, m.Round, m.Slot)] = m
			switch m.Tier {
			case "winners":
				wb++
			case "losers":
				lb++
			case "grand_final":
				gf++
			}
		}
		if wb != n-1 {
			t.Errorf("n=%d: WB matches=%d want %d", n, wb, n-1)
		}
		wantLB := n - 2
		if n < 4 {
			wantLB = 0
		}
		if lb != wantLB {
			t.Errorf("n=%d: LB matches=%d want %d", n, lb, wantLB)
		}
		if gf != 1 {
			t.Errorf("n=%d: grand_final=%d want 1", n, gf)
		}
		assertFeedsValid(t, p)
	}
}

// assertFeedsValid checks every win/lose feed targets a real match in the plan.
func assertFeedsValid(t *testing.T, p DoubleElimPlan) {
	t.Helper()
	byKey := map[string]*DEMatch{}
	for _, m := range p.Matches {
		byKey[deKey(m.Tier, m.Round, m.Slot)] = m
	}
	for _, m := range p.Matches {
		if m.WinTier != "" && byKey[deKey(m.WinTier, m.WinRound, m.WinSlot)] == nil {
			t.Fatalf("%s %d:%d win-feeds missing %s %d:%d", m.Tier, m.Round, m.Slot, m.WinTier, m.WinRound, m.WinSlot)
		}
		if m.LoseTier != "" {
			if m.Tier != "winners" {
				t.Fatalf("non-WB match %s %d:%d has a loser feed", m.Tier, m.Round, m.Slot)
			}
			if byKey[deKey(m.LoseTier, m.LoseRound, m.LoseSlot)] == nil {
				t.Fatalf("%s %d:%d lose-feeds missing %s %d:%d", m.Tier, m.Round, m.Slot, m.LoseTier, m.LoseRound, m.LoseSlot)
			}
		}
	}
}

// simulateDoubleElim plays the bracket with "lower seed always wins" and returns
// the champion seed and a loss count per seed. WB byes are pre-resolved by the
// generator (winner already propagated to the next round) so they carry no loss.
// A stuck/unfillable match leaves its entrants short of the bracket and shows up
// as a seed that never reaches two losses.
func simulateDoubleElim(p DoubleElimPlan) (champ int, losses map[int]int) {
	side := map[string][2]int{}
	resolved := map[string]bool{}
	for _, m := range p.Matches {
		k := deKey(m.Tier, m.Round, m.Slot)
		s := [2]int{-1, -1}
		if m.Side1 != nil && !IsBye(m.Side1) {
			s[0] = seedNum(m.Side1)
		}
		if m.Side2 != nil && !IsBye(m.Side2) {
			s[1] = seedNum(m.Side2)
		}
		side[k] = s
		if m.ResolvedWinner != nil {
			resolved[k] = true // a WB bye: no game, no loss
		}
	}
	put := func(tier string, r, s, team, seed int) {
		k := deKey(tier, r, s)
		v := side[k]
		v[team-1] = seed
		side[k] = v
	}
	losses = map[int]int{}
	champ = -1
	for progress := true; progress; {
		progress = false
		for _, m := range p.Matches {
			k := deKey(m.Tier, m.Round, m.Slot)
			if resolved[k] {
				continue
			}
			v := side[k]
			if v[0] < 0 || v[1] < 0 {
				continue
			}
			resolved[k] = true
			progress = true
			w, l := v[0], v[1]
			if l < w {
				w, l = l, w
			}
			losses[l]++
			if m.WinTier != "" {
				put(m.WinTier, m.WinRound, m.WinSlot, m.WinTeam, w)
			} else {
				champ = w // grand final (top seed wins game 1, no reset)
			}
			if m.LoseTier != "" {
				put(m.LoseTier, m.LoseRound, m.LoseSlot, m.LoseTeam, l)
			}
		}
	}
	return champ, losses
}

// assertValidDoubleElim: top seed champions, every other real entrant out after
// EXACTLY two losses.
func assertValidDoubleElim(t *testing.T, p DoubleElimPlan, realCount int) {
	t.Helper()
	champ, losses := simulateDoubleElim(p)
	if champ != 1 {
		t.Fatalf("realCount=%d: champion = s%d, want s1", realCount, champ)
	}
	if losses[1] != 0 {
		t.Fatalf("realCount=%d: champion has %d losses, want 0", realCount, losses[1])
	}
	for s := 2; s <= realCount; s++ {
		if losses[s] != 2 {
			t.Fatalf("realCount=%d: seed %d has %d losses, want exactly 2 (double-elim)", realCount, s, losses[s])
		}
	}
}

// Power-of-two fields: full structure, no byes.
func TestDoubleElimChampionAndTwoLoss(t *testing.T) {
	for _, n := range []int{2, 4, 8, 16} {
		assertValidDoubleElim(t, GenerateDoubleElim(seeds(n)), n)
	}
}

// Non-power-of-two fields: byes collapse the entry of the losers tree. The token
// flow must still route every real entrant so it's eliminated after two losses,
// and every feed must point at a real (non-collapsed) match.
func TestDoubleElimByes(t *testing.T) {
	for _, rc := range []int{3, 5, 6, 7, 9, 11, 12, 13, 15, 22, 31} {
		p := GenerateDoubleElim(seeds(rc))
		assertFeedsValid(t, p)
		// LB rounds must be contiguous 1..N (byes can empty a whole round).
		maxR := 0
		seen := map[int]bool{}
		for _, m := range p.Matches {
			if m.Tier == "losers" {
				seen[m.Round] = true
				if m.Round > maxR {
					maxR = m.Round
				}
			}
		}
		for r := 1; r <= maxR; r++ {
			if !seen[r] {
				t.Fatalf("realCount=%d: LB round %d missing (not contiguous)", rc, r)
			}
		}
		assertValidDoubleElim(t, p, rc)
	}
}
