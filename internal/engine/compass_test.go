package engine

import (
	"fmt"
	"testing"
)

func cmKey(group string, r, s int) string { return fmt.Sprintf("%s:%d:%d", group, r, s) }

// assertCompassFeedsValid checks every winner feed and loser drop points at a
// real (non-collapsed) match present in the plan.
func assertCompassFeedsValid(t *testing.T, p CompassPlan) {
	t.Helper()
	byKey := map[string]*CompassMatchSpec{}
	for _, m := range p.Matches {
		byKey[cmKey(m.Group, m.Round, m.Slot)] = m
	}
	for _, m := range p.Matches {
		if m.FeedsRound != 0 {
			if byKey[cmKey(m.Group, m.FeedsRound, m.FeedsSlot)] == nil {
				t.Fatalf("%s %d:%d win-feeds missing %s %d:%d",
					m.Group, m.Round, m.Slot, m.Group, m.FeedsRound, m.FeedsSlot)
			}
		}
		if m.LoserGroup != "" {
			if byKey[cmKey(m.LoserGroup, m.LoserRound, m.LoserSlot)] == nil {
				t.Fatalf("%s %d:%d loser-drops missing %s %d:%d",
					m.Group, m.Round, m.Slot, m.LoserGroup, m.LoserRound, m.LoserSlot)
			}
		}
	}
}

// compassSim plays a Compass plan with "lower seed always wins". It returns, per
// group, the champion seed; and a per-seed count of how many distinct brackets
// each real entrant appeared in (East + at most one consolation = 2 for anyone
// who loses East before the final; 1 for the East champion and finalist).
type compassResult struct {
	champByGroup   map[string]int // group -> champion seed
	bracketsPlayed map[int]map[string]bool
	gamesPlayed    map[int]int // seed -> real matches played
}

func simulateCompass(p CompassPlan) compassResult {
	side := map[string][2]int{}
	resolved := map[string]bool{}
	for _, m := range p.Matches {
		k := cmKey(m.Group, m.Round, m.Slot)
		s := [2]int{-1, -1}
		if m.Side1 != nil && !IsBye(m.Side1) {
			s[0] = seedNum(m.Side1)
		}
		if m.Side2 != nil && !IsBye(m.Side2) {
			s[1] = seedNum(m.Side2)
		}
		side[k] = s
		if m.ResolvedWinner != nil {
			resolved[k] = true // an East round-1 bye — pre-resolved, no game
		}
	}
	put := func(group string, r, s, team, seed int) {
		k := cmKey(group, r, s)
		v := side[k]
		v[team-1] = seed
		side[k] = v
	}
	res := compassResult{champByGroup: map[string]int{}, bracketsPlayed: map[int]map[string]bool{}, gamesPlayed: map[int]int{}}
	mark := func(seed int, group string) {
		if seed < 0 {
			return
		}
		if res.bracketsPlayed[seed] == nil {
			res.bracketsPlayed[seed] = map[string]bool{}
		}
		res.bracketsPlayed[seed][group] = true
	}

	for progress := true; progress; {
		progress = false
		for _, m := range p.Matches {
			k := cmKey(m.Group, m.Round, m.Slot)
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
			mark(w, m.Group)
			mark(l, m.Group)
			res.gamesPlayed[w]++
			res.gamesPlayed[l]++
			// Winner advances within its group (or wins that group's final).
			if m.FeedsRound != 0 {
				put(m.Group, m.FeedsRound, m.FeedsSlot, m.FeedsTeam, w)
			} else {
				res.champByGroup[m.Group] = w
			}
			// East loser drops to its consolation; consolation loser is eliminated.
			if m.LoserGroup != "" {
				put(m.LoserGroup, m.LoserRound, m.LoserSlot, m.LoserTeam, l)
			}
		}
	}
	return res
}

func TestCompassStructure(t *testing.T) {
	for _, n := range []int{4, 6, 8, 16} {
		p := GenerateCompass(seeds(n))
		assertCompassFeedsValid(t, p)

		// East must be present and be a full single-elim (Size-1 nodes incl. byes).
		eastCount := 0
		for _, m := range p.Matches {
			if m.Group == EastGroup {
				eastCount++
			}
		}
		if eastCount != p.Size-1 {
			t.Fatalf("n=%d: east matches=%d want %d", n, eastCount, p.Size-1)
		}
		// West (East round-1 losers) must exist for any field of 4+.
		hasWest := false
		for _, b := range p.Brackets {
			if b.Group == "west" {
				hasWest = true
			}
		}
		if !hasWest {
			t.Fatalf("n=%d: expected a West consolation bracket", n)
		}
	}
}

// Non-power-of-two AND power-of-two fields: East crowns the top seed; every real
// entrant plays East; and a team is routed into the consolation for its
// East-losing round whenever that round actually formed one. (A round whose
// losers are fewer than two — thinned by byes — forms no consolation, so that
// loser legitimately plays only East: compass placement is depth-based, it does
// NOT guarantee two brackets the way double-elim guarantees two losses.)
func TestCompassProgression(t *testing.T) {
	for _, n := range []int{6, 8, 5, 7, 12} {
		p := GenerateCompass(seeds(n))
		assertCompassFeedsValid(t, p)

		// Which East round does each real (non-bye) East match belong to, and which
		// consolation group does that round drop into (empty = round formed none)?
		consGroupForEastRound := map[int]string{}
		for _, m := range p.Matches {
			if m.Group == EastGroup && m.LoserGroup != "" {
				consGroupForEastRound[m.Round] = m.LoserGroup
			}
		}

		res := simulateCompass(p)
		if res.champByGroup[EastGroup] != 1 {
			t.Fatalf("n=%d: East champion = s%d, want s1", n, res.champByGroup[EastGroup])
		}

		eastLostRound := simulateEastLossRounds(p)
		for s := 1; s <= n; s++ {
			played := res.bracketsPlayed[s]
			if !played[EastGroup] {
				t.Fatalf("n=%d: seed %d never played in East", n, s)
			}
			if s == 1 {
				if len(played) != 1 {
					t.Fatalf("n=%d: East champion seed 1 played %d brackets %v, want East only", n, len(played), played)
				}
				continue
			}
			lostR := eastLostRound[s]
			wantCons := consGroupForEastRound[lostR] // "" if that round formed none
			if wantCons == "" {
				if len(played) != 1 {
					t.Fatalf("n=%d: seed %d lost East r%d (no consolation) yet played %v", n, s, lostR, played)
				}
			} else if !played[wantCons] {
				// Sideways feeding means a team can chain deeper (East -> West ->
				// South -> Southeast), so only the FIRST drop is asserted here —
				// the chain's later hops are the sideways drops validated by the
				// feeds check + the direction champions below.
				t.Fatalf("n=%d: seed %d lost East r%d but didn't play its consolation %q (played %v)", n, s, lostR, wantCons, played)
			}
		}

		// Every consolation bracket in the plan must crown a champion when played
		// out (no stuck/unfillable tree).
		for _, b := range p.Brackets {
			if b.Group == EastGroup {
				continue
			}
			if _, ok := res.champByGroup[b.Group]; !ok {
				t.Fatalf("n=%d: consolation %q never crowned a champion", n, b.Group)
			}
		}
	}
}

// simulateEastLossRounds replays ONLY the East bracket ("lower seed wins") and
// returns, per seed, the East round in which it lost (0 = never lost, i.e. the
// East champion).
func simulateEastLossRounds(p CompassPlan) map[int]int {
	side := map[[2]int][2]int{}
	resolved := map[[2]int]bool{}
	var maxRound int
	for _, m := range p.Matches {
		if m.Group != EastGroup {
			continue
		}
		if m.Round > maxRound {
			maxRound = m.Round
		}
		k := [2]int{m.Round, m.Slot}
		s := [2]int{-1, -1}
		if m.Side1 != nil && !IsBye(m.Side1) {
			s[0] = seedNum(m.Side1)
		}
		if m.Side2 != nil && !IsBye(m.Side2) {
			s[1] = seedNum(m.Side2)
		}
		side[k] = s
		if m.ResolvedWinner != nil {
			resolved[k] = true
		}
	}
	feed := map[[2]int][3]int{} // east match -> [feedRound, feedSlot, feedTeam]
	for _, m := range p.Matches {
		if m.Group == EastGroup && m.FeedsRound != 0 {
			feed[[2]int{m.Round, m.Slot}] = [3]int{m.FeedsRound, m.FeedsSlot, m.FeedsTeam}
		}
	}
	lost := map[int]int{}
	for progress := true; progress; {
		progress = false
		for _, m := range p.Matches {
			if m.Group != EastGroup {
				continue
			}
			k := [2]int{m.Round, m.Slot}
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
			lost[l] = m.Round
			if f, ok := feed[k]; ok {
				tk := [2]int{f[0], f[1]}
				cur := side[tk]
				cur[f[2]-1] = w
				side[tk] = cur
			}
		}
	}
	return lost
}

// The canonical 8-team compass: East r1 losers -> West (4 droppers -> 3
// matches), East r2 losers -> North (2 -> 1 match), West r1 losers -> South
// (2 -> 1 match). No NE/SW/NW/SE at 8 (their feeding rounds are finals or too
// thin), matching the published 8-player layout.
func TestCompass8TeamShape(t *testing.T) {
	p := GenerateCompass(seeds(8))
	count := map[string]int{}
	for _, m := range p.Matches {
		count[m.Group]++
	}
	want := map[string]int{EastGroup: 7, "west": 3, "north": 1, "south": 1}
	for g, w := range want {
		if count[g] != w {
			t.Fatalf("%s matches=%d want %d", g, count[g], w)
		}
	}
	for _, g := range []string{"northeast", "southwest", "northwest", "southeast"} {
		if count[g] != 0 {
			t.Fatalf("%s matches=%d want 0 at an 8 draw", g, count[g])
		}
	}
	// The canonical 8-compass guarantees every entrant 3 matches.
	res := simulateCompass(p)
	for s := 1; s <= 8; s++ {
		if res.gamesPlayed[s] < 3 {
			t.Fatalf("seed %d played %d games, want >=3 (8-compass guarantee)", s, res.gamesPlayed[s])
		}
	}
}

// The canonical full 16-team compass: all 8 directions with the authoritative
// feeding (E r1->W, E r2->N, E r3->NE, W r1->S, W r2->SW, N r1->NW, S r1->SE),
// and the format's selling point — every entrant is guaranteed 4 matches.
func TestCompass16FullShape(t *testing.T) {
	p := GenerateCompass(seeds(16))
	assertCompassFeedsValid(t, p)
	count := map[string]int{}
	for _, m := range p.Matches {
		count[m.Group]++
	}
	want := map[string]int{
		EastGroup: 15, "west": 7, "north": 3, "northeast": 1,
		"south": 3, "southwest": 1, "northwest": 1, "southeast": 1,
	}
	for g, w := range want {
		if count[g] != w {
			t.Fatalf("%s matches=%d want %d", g, count[g], w)
		}
	}
	res := simulateCompass(p)
	if res.champByGroup[EastGroup] != 1 {
		t.Fatalf("East champion = s%d, want s1", res.champByGroup[EastGroup])
	}
	for _, g := range []string{"west", "north", "northeast", "south", "southwest", "northwest", "southeast"} {
		if _, ok := res.champByGroup[g]; !ok {
			t.Fatalf("direction %q never crowned a champion", g)
		}
	}
	for s := 1; s <= 16; s++ {
		if res.gamesPlayed[s] < 4 {
			t.Fatalf("seed %d played %d games, want >=4 (16-compass guarantee)", s, res.gamesPlayed[s])
		}
	}
}
