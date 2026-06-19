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
		// Every win/lose feed must target a real match.
		for _, m := range p.Matches {
			if m.WinTier != "" && byKey[deKey(m.WinTier, m.WinRound, m.WinSlot)] == nil {
				t.Fatalf("n=%d: %s %d:%d win-feeds missing %s %d:%d", n, m.Tier, m.Round, m.Slot, m.WinTier, m.WinRound, m.WinSlot)
			}
			if m.LoseTier != "" {
				if m.Tier != "winners" {
					t.Fatalf("n=%d: non-WB match has a loser feed", n)
				}
				if byKey[deKey(m.LoseTier, m.LoseRound, m.LoseSlot)] == nil {
					t.Fatalf("n=%d: %s %d:%d lose-feeds missing %s %d:%d", n, m.Tier, m.Round, m.Slot, m.LoseTier, m.LoseRound, m.LoseSlot)
				}
			}
		}
	}
}

// Simulate "lower seed always wins" and verify the structure produces a valid
// double elimination: the top seed is champion, and every other entrant is
// eliminated after exactly TWO losses.
func TestDoubleElimChampionAndTwoLoss(t *testing.T) {
	for _, n := range []int{2, 4, 8, 16} {
		p := GenerateDoubleElim(seeds(n))
		side := map[string][2]int{} // match key -> two seed numbers (-1 = empty)
		for _, m := range p.Matches {
			s := [2]int{-1, -1}
			if m.Side1 != nil && !IsBye(m.Side1) {
				s[0] = seedNum(m.Side1)
			}
			if m.Side2 != nil && !IsBye(m.Side2) {
				s[1] = seedNum(m.Side2)
			}
			side[deKey(m.Tier, m.Round, m.Slot)] = s
		}
		put := func(tier string, r, s, team, seed int) {
			k := deKey(tier, r, s)
			v := side[k]
			v[team-1] = seed
			side[k] = v
		}
		losses := map[int]int{}
		resolved := map[string]bool{}
		champ := -1
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
					champ = w // grand final (top-seed wins game 1, no reset)
				}
				if m.LoseTier != "" {
					put(m.LoseTier, m.LoseRound, m.LoseSlot, m.LoseTeam, l)
				}
			}
		}
		if champ != 1 {
			t.Fatalf("n=%d: champion = s%d, want s1", n, champ)
		}
		if losses[1] != 0 {
			t.Fatalf("n=%d: champion has %d losses, want 0", n, losses[1])
		}
		for s := 2; s <= n; s++ {
			if losses[s] != 2 {
				t.Fatalf("n=%d: seed %d has %d losses, want exactly 2 (double-elim)", n, s, losses[s])
			}
		}
	}
}
