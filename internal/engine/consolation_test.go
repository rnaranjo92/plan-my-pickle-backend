package engine

import (
	"fmt"
	"testing"
)

func noByes(int) bool { return false }

func byeSet(slots ...int) func(int) bool {
	m := map[int]bool{}
	for _, s := range slots {
		m[s] = true
	}
	return func(s int) bool { return m[s] }
}

func TestConsolationStructureNoByes(t *testing.T) {
	cases := []struct {
		size, wantRounds, wantMatches, wantDrops int
	}{
		{2, 0, 0, 0}, // too small — no consolation
		{4, 1, 1, 2}, // 2 round-1 losers play once for bronze
		{8, 2, 3, 4},
		{16, 3, 7, 8},
		{32, 4, 15, 16},
	}
	for _, c := range cases {
		p := GenerateConsolation(c.size, noByes)
		if p.Rounds != c.wantRounds {
			t.Errorf("size %d: rounds=%d want %d", c.size, p.Rounds, c.wantRounds)
		}
		if len(p.Matches) != c.wantMatches {
			t.Errorf("size %d: matches=%d want %d", c.size, len(p.Matches), c.wantMatches)
		}
		if len(p.Drops) != c.wantDrops {
			t.Errorf("size %d: drops=%d want %d", c.size, len(p.Drops), c.wantDrops)
		}
	}
}

// Every consolation round-1 match must pair losers from two DIFFERENT main
// matches (so the two players have never met).
func TestConsolationNoImmediateRematch(t *testing.T) {
	for _, size := range []int{4, 8, 16, 32} {
		p := GenerateConsolation(size, noByes)
		bySlot := map[string][]int{}
		for _, d := range p.Drops {
			if d.Round != 1 {
				continue // with no byes, all entries are at round 1
			}
			k := fmt.Sprintf("%d:%d", d.Round, d.Slot)
			bySlot[k] = append(bySlot[k], d.MainSlot)
		}
		for k, mains := range bySlot {
			if len(mains) == 2 && mains[0] == mains[1] {
				t.Fatalf("size %d: consolation %s fed by same main match %d", size, k, mains[0])
			}
		}
	}
}

// Drops + feeds must form a consistent single tree converging on one final, and
// every real loser must get exactly one entry slot.
func consConsistent(t *testing.T, size int, isBye func(int) bool) ConsolationPlan {
	t.Helper()
	p := GenerateConsolation(size, isBye)
	byKey := map[string]*ConsolationMatchSpec{}
	for _, m := range p.Matches {
		byKey[fmt.Sprintf("%d:%d", m.Round, m.Slot)] = m
	}
	// each (round,slot,team) filled at most once (by a drop or a feed)
	filled := map[string]bool{}
	mark := func(r, s, team int, what string) {
		k := fmt.Sprintf("%d:%d:%d", r, s, team)
		if filled[k] {
			t.Fatalf("size %d: slot %s filled twice (%s)", size, k, what)
		}
		filled[k] = true
	}
	for _, d := range p.Drops {
		if byKey[fmt.Sprintf("%d:%d", d.Round, d.Slot)] == nil {
			t.Fatalf("size %d: drop targets missing match %d:%d", size, d.Round, d.Slot)
		}
		mark(d.Round, d.Slot, d.Team, "drop")
	}
	finals := 0
	for _, m := range p.Matches {
		if m.FeedsRound == 0 {
			finals++
			if m.Round != p.Rounds {
				t.Fatalf("size %d: final at round %d want %d", size, m.Round, p.Rounds)
			}
			continue
		}
		if byKey[fmt.Sprintf("%d:%d", m.FeedsRound, m.FeedsSlot)] == nil {
			t.Fatalf("size %d: match %d:%d feeds missing %d:%d", size, m.Round, m.Slot, m.FeedsRound, m.FeedsSlot)
		}
		mark(m.FeedsRound, m.FeedsSlot, m.FeedsTeam, "feed")
	}
	if len(p.Matches) > 0 && finals != 1 {
		t.Fatalf("size %d: want exactly 1 final, got %d", size, finals)
	}
	// every real match has both team slots filled (by a drop or a feed)
	for _, m := range p.Matches {
		for team := 1; team <= 2; team++ {
			if !filled[fmt.Sprintf("%d:%d:%d", m.Round, m.Slot, team)] {
				t.Fatalf("size %d: match %d:%d team %d never filled", size, m.Round, m.Slot, team)
			}
		}
	}
	return p
}

func TestConsolationConsistentNoByes(t *testing.T) {
	for _, size := range []int{4, 8, 16, 32} {
		consConsistent(t, size, noByes)
	}
}

// With byes, a lone loser skips deeper; only real matches are emitted; every real
// loser still gets an entry and the tree stays consistent.
func TestConsolationWithByes(t *testing.T) {
	// size 8, main slot 0 is a bye -> loser L1 has no r1 opponent, enters the final.
	p := consConsistent(t, 8, byeSet(0))
	if len(p.Matches) != 2 {
		t.Fatalf("size8 bye0: want 2 real matches, got %d", len(p.Matches))
	}
	var l1 *LoserDrop
	for i := range p.Drops {
		if p.Drops[i].MainSlot == 1 {
			l1 = &p.Drops[i]
		}
		if p.Drops[i].MainSlot == 0 {
			t.Fatalf("bye main slot 0 must not produce a loser drop")
		}
	}
	if l1 == nil || l1.Round != 2 {
		t.Fatalf("L1 should enter at round 2 (after a consolation bye), got %+v", l1)
	}

	// Heavier bye load stays consistent + drops exactly the real losers.
	for _, tc := range []struct {
		size int
		byes []int
	}{
		{8, []int{0, 1}},     // two byes
		{8, []int{0, 2}},     // non-adjacent byes
		{16, []int{0, 3, 7}}, // 3 byes
		{16, []int{0, 1, 2, 3}},
	} {
		p := consConsistent(t, tc.size, byeSet(tc.byes...))
		byeMap := byeSet(tc.byes...)
		realLosers := 0
		for i := 0; i < tc.size/2; i++ {
			if !byeMap(i) {
				realLosers++
			}
		}
		if len(p.Drops) != realLosers {
			t.Fatalf("size %d byes %v: drops=%d want %d real losers", tc.size, tc.byes, len(p.Drops), realLosers)
		}
	}
}

// Emitted rounds must be contiguous 1..Rounds (no gaps) even when byes collapse
// the lowest consolation rounds — otherwise the bracket UI draws empty columns.
func TestConsolationContiguousRounds(t *testing.T) {
	cases := []struct {
		size int
		byes []int
	}{
		{8, nil},
		{8, []int{0}},
		{8, []int{0, 2}}, // collapses the whole lowest round -> single round-1 final
		{16, []int{0, 3, 7}},
		{16, []int{0, 1, 2, 3}},
		{32, []int{0, 2, 4, 6, 8}},
	}
	for _, tc := range cases {
		p := GenerateConsolation(tc.size, byeSet(tc.byes...))
		seen := map[int]bool{}
		for _, m := range p.Matches {
			if m.Round < 1 || m.Round > p.Rounds {
				t.Fatalf("size %d byes %v: round %d outside 1..%d", tc.size, tc.byes, m.Round, p.Rounds)
			}
			seen[m.Round] = true
		}
		for r := 1; r <= p.Rounds; r++ {
			if !seen[r] {
				t.Fatalf("size %d byes %v: missing round %d (not contiguous, Rounds=%d)", tc.size, tc.byes, r, p.Rounds)
			}
		}
		for _, d := range p.Drops {
			if d.Round < 1 || d.Round > p.Rounds {
				t.Fatalf("size %d byes %v: drop round %d outside 1..%d", tc.size, tc.byes, d.Round, p.Rounds)
			}
		}
	}
}

// Simulate (lowest main-slot wins) and confirm one champion emerges via the feeds.
func TestConsolationChampion(t *testing.T) {
	for _, size := range []int{4, 8, 16} {
		p := GenerateConsolation(size, noByes)
		slot := map[string][2]int{}
		for _, m := range p.Matches {
			slot[fmt.Sprintf("%d:%d", m.Round, m.Slot)] = [2]int{-1, -1}
		}
		for _, d := range p.Drops {
			k := fmt.Sprintf("%d:%d", d.Round, d.Slot)
			s := slot[k]
			s[d.Team-1] = d.MainSlot
			slot[k] = s
		}
		champ := -1
		for r := 1; r <= p.Rounds; r++ {
			for _, m := range p.Matches {
				if m.Round != r {
					continue
				}
				pair := slot[fmt.Sprintf("%d:%d", m.Round, m.Slot)]
				if pair[0] == -1 || pair[1] == -1 {
					t.Fatalf("size %d: %d:%d under-filled %v", size, m.Round, m.Slot, pair)
				}
				w := pair[0]
				if pair[1] < w {
					w = pair[1]
				}
				if m.FeedsRound == 0 {
					champ = w
					continue
				}
				tk := fmt.Sprintf("%d:%d", m.FeedsRound, m.FeedsSlot)
				ts := slot[tk]
				ts[m.FeedsTeam-1] = w
				slot[tk] = ts
			}
		}
		if champ != 0 {
			t.Fatalf("size %d: champion main slot = %d, want 0", size, champ)
		}
	}
}
