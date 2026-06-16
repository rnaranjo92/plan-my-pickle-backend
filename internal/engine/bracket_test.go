package engine

import (
	"fmt"
	"strconv"
	"testing"
)

func seeds(n int) [][]string {
	out := make([][]string, n)
	for i := range out {
		out[i] = []string{fmt.Sprintf("s%d", i+1)}
	}
	return out
}

func seedNum(side []string) int {
	v, _ := strconv.Atoi(side[0][1:])
	return v
}

// simulate "top seed always wins" and return the champion.
func champion(plan BracketPlan) string {
	byKey := map[string]*BracketMatchSpec{}
	for _, m := range plan.Matches {
		byKey[fmt.Sprintf("%d:%d", m.Round, m.Slot)] = m
	}
	adv := func(m *BracketMatchSpec, w []string) {
		m.ResolvedWinner = w
		if m.FeedsRound == 0 {
			return
		}
		t := byKey[fmt.Sprintf("%d:%d", m.FeedsRound, m.FeedsSlot)]
		if m.FeedsTeam == 1 {
			t.Side1 = w
		} else {
			t.Side2 = w
		}
	}
	for r := 1; r <= plan.Rounds; r++ {
		for _, m := range plan.Matches {
			if m.Round != r {
				continue
			}
			if m.ResolvedWinner != nil {
				adv(m, m.ResolvedWinner)
				continue
			}
			if m.Side1 == nil || m.Side2 == nil {
				continue
			}
			var w []string
			switch {
			case IsBye(m.Side1):
				w = m.Side2
			case IsBye(m.Side2):
				w = m.Side1
			case seedNum(m.Side1) <= seedNum(m.Side2):
				w = m.Side1
			default:
				w = m.Side2
			}
			adv(m, w)
		}
	}
	fin := byKey[fmt.Sprintf("%d:%d", plan.Rounds, 0)]
	if fin.ResolvedWinner == nil {
		return ""
	}
	return fin.ResolvedWinner[0]
}

func TestBracketStructureAndChampion(t *testing.T) {
	for _, n := range []int{2, 4, 5, 6, 8, 11, 16} {
		plan := GenerateBracket(seeds(n))

		var r1 []*BracketMatchSpec
		for _, m := range plan.Matches {
			if m.Round == 1 {
				r1 = append(r1, m)
			}
		}
		if len(r1) != plan.Size/2 {
			t.Fatalf("N=%d: round1 count = %d, want %d", n, len(r1), plan.Size/2)
		}
		finals := 0
		for _, m := range plan.Matches {
			if m.Round == plan.Rounds {
				finals++
			}
		}
		if finals != 1 {
			t.Fatalf("N=%d: want exactly 1 final, got %d", n, finals)
		}
		seen := map[string]bool{}
		for _, m := range r1 {
			for _, s := range [][]string{m.Side1, m.Side2} {
				if s != nil && !IsBye(s) {
					seen[s[0]] = true
				}
			}
		}
		if len(seen) != n {
			t.Fatalf("N=%d: want %d seeded sides, got %d", n, n, len(seen))
		}
		if c := champion(plan); c != "s1" {
			t.Fatalf("N=%d: top seed should win, got %q", n, c)
		}
	}
}

func TestSeed1And2OppositeHalves(t *testing.T) {
	plan := GenerateBracket(seeds(8))
	slotOf := func(id string) int {
		for _, m := range plan.Matches {
			if m.Round != 1 {
				continue
			}
			if m.Side1 != nil && !IsBye(m.Side1) && m.Side1[0] == id {
				return m.Slot * 2
			}
			if m.Side2 != nil && !IsBye(m.Side2) && m.Side2[0] == id {
				return m.Slot*2 + 1
			}
		}
		return -1
	}
	if (slotOf("s1") < 4) == (slotOf("s2") < 4) {
		t.Fatalf("seed 1 and 2 should be in opposite halves")
	}
}
