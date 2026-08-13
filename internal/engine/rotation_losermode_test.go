package engine

import (
	"fmt"
	"sort"
	"testing"
)

// The invariants from the edge-case spec, asserted directly. A movement rule
// that violates one of these produces a session that looks plausible and is
// quietly wrong, so they're checked exhaustively rather than by example.
//
//	I1 every court has exactly 4 players
//	I3 nobody appears twice in a round
//	I4 nobody keeps their partner between rounds
//	I5 movement is a permutation — the same people, round to round
func peopleOf(courts []RotCourt, bench []string) []string {
	out := append([]string(nil), bench...)
	for _, c := range courts {
		out = append(out, c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1])
	}
	sort.Strings(out)
	return out
}

func partnersOf(courts []RotCourt) map[string]string {
	p := map[string]string{}
	for _, c := range courts {
		p[c.TeamA[0]], p[c.TeamA[1]] = c.TeamA[1], c.TeamA[0]
		p[c.TeamB[0]], p[c.TeamB[1]] = c.TeamB[1], c.TeamB[0]
	}
	return p
}

func assertInvariants(t *testing.T, label string, before, after []RotCourt, benchBefore, benchAfter []string) {
	t.Helper()
	// I1 + I3
	seen := map[string]bool{}
	for _, c := range after {
		for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			if id == "" {
				t.Fatalf("%s: court %d has an empty seat (I1)", label, c.Court)
			}
			if seen[id] {
				t.Fatalf("%s: %s appears twice in one round (I3)", label, id)
			}
			seen[id] = true
		}
	}
	// I5 — same population, seats + bench
	b, a := peopleOf(before, benchBefore), peopleOf(after, benchAfter)
	if fmt.Sprint(b) != fmt.Sprint(a) {
		t.Fatalf("%s: population changed (I5)\n before %v\n after  %v", label, b, a)
	}
	// I4 — partners must change for anyone still on court
	was, now := partnersOf(before), partnersOf(after)
	for id, oldPartner := range was {
		if np, ok := now[id]; ok && np == oldPartner {
			t.Fatalf("%s: %s kept partner %s (I4)", label, id, oldPartner)
		}
	}
}

func mkCourts(n int) []RotCourt {
	courts := make([]RotCourt, n)
	for k := 0; k < n; k++ {
		b := k * 4
		courts[k] = RotCourt{
			Court: k + 1,
			TeamA: [2]string{fmt.Sprintf("p%d", b+1), fmt.Sprintf("p%d", b+3)},
			TeamB: [2]string{fmt.Sprintf("p%d", b+2), fmt.Sprintf("p%d", b+4)},
		}
	}
	return courts
}

// Every court count, both modes, and every combination of winners — the state
// space is small enough to cover completely rather than sample.
func TestInvariantsHoldEverywhere(t *testing.T) {
	for _, mode := range []LoserMode{LosersDown, LosersStay} {
		for n := 1; n <= 5; n++ {
			for mask := 0; mask < (1 << n); mask++ {
				courts := mkCourts(n)
				results := make([]RotResult, n)
				for k := 0; k < n; k++ {
					w := "a"
					if mask&(1<<k) != 0 {
						w = "b"
					}
					results[k] = RotResult{Court: k + 1, Winner: w}
				}
				next, nb := NextRound(courts, results, nil, mode)
				assertInvariants(t,
					fmt.Sprintf("mode=%d courts=%d mask=%b", mode, n, mask),
					courts, next, nil, nb)
			}
		}
	}
}

// The same, with a bench, so the bye swap can't break the population either.
func TestInvariantsHoldWithBench(t *testing.T) {
	for _, mode := range []LoserMode{LosersDown, LosersStay} {
		for n := 1; n <= 4; n++ {
			for benchSize := 1; benchSize <= 3; benchSize++ {
				courts := mkCourts(n)
				bench := make([]string, benchSize)
				for i := range bench {
					bench[i] = fmt.Sprintf("b%d", i+1)
				}
				results := make([]RotResult, n)
				for k := range results {
					results[k] = RotResult{Court: k + 1, Winner: "a"}
				}
				next, nb := NextRound(courts, results, bench, mode)
				assertInvariants(t,
					fmt.Sprintf("mode=%d courts=%d bench=%d", mode, n, benchSize),
					courts, next, bench, nb)
			}
		}
	}
}

// LosersStay: winners climb, losers hold, and the TOP court's losers fall all
// the way to the bottom. This is the rule that keeps every court at four.
func TestLosersStayMovement(t *testing.T) {
	courts := mkCourts(3) // p1..p12
	results := []RotResult{
		{Court: 1, Winner: "a"}, // p1,p3 win — stay; p2,p4 fall to court 3
		{Court: 2, Winner: "a"}, // p5,p7 climb to court 1; p6,p8 hold court 2
		{Court: 3, Winner: "a"}, // p9,p11 climb to court 2; p10,p12 hold court 3
	}
	next, _ := NextRound(courts, results, nil, LosersStay)

	on := func(court int) map[string]bool {
		m := map[string]bool{}
		for _, c := range next {
			if c.Court == court {
				for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
					m[id] = true
				}
			}
		}
		return m
	}
	for _, want := range []struct {
		court int
		ids   []string
	}{
		{1, []string{"p1", "p3", "p5", "p7"}},   // top winners + climbers from 2
		{2, []string{"p6", "p8", "p9", "p11"}},  // own losers + climbers from 3
		{3, []string{"p10", "p12", "p2", "p4"}}, // own losers + the top court's losers
	} {
		got := on(want.court)
		for _, id := range want.ids {
			if !got[id] {
				t.Errorf("court %d should hold %s, got %v", want.court, id, got)
			}
		}
	}
}

// The top court's winners stay AND split, so they face each other next round —
// Michelle's rule, and it must hold in both modes.
func TestTopCourtWinnersBecomeOpponents(t *testing.T) {
	for _, mode := range []LoserMode{LosersDown, LosersStay} {
		courts := mkCourts(2)
		next, _ := NextRound(courts, []RotResult{
			{Court: 1, Winner: "a"}, // p1,p3 win the top court
			{Court: 2, Winner: "a"},
		}, nil, mode)

		var top RotCourt
		for _, c := range next {
			if c.Court == 1 {
				top = c
			}
		}
		sameTeam := (top.TeamA[0] == "p1" && top.TeamA[1] == "p3") ||
			(top.TeamA[0] == "p3" && top.TeamA[1] == "p1") ||
			(top.TeamB[0] == "p1" && top.TeamB[1] == "p3") ||
			(top.TeamB[0] == "p3" && top.TeamB[1] == "p1")
		if sameTeam {
			t.Fatalf("mode=%d: top-court winners stayed partners, should be opponents: %+v",
				mode, top)
		}
	}
}

// With one court there is nowhere to move, so both modes just re-pair.
func TestSingleCourtIdenticalInBothModes(t *testing.T) {
	a, _ := NextRound(mkCourts(1), []RotResult{{Court: 1, Winner: "a"}}, nil, LosersDown)
	b, _ := NextRound(mkCourts(1), []RotResult{{Court: 1, Winner: "a"}}, nil, LosersStay)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("single court differs by mode:\n down %+v\n stay %+v", a, b)
	}
}
