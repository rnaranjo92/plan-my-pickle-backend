package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// The fairness pass exists because of a measured problem, so it is tested by
// measuring: simulate whole nights and compare how much court time the players
// who sit MOST get against the players who sit LEAST. Asserting "a swap
// happened" would pass while the night stayed as lopsided as before.

// simulate plays `rounds` rounds with `n` players on `courts` courts and returns
// each player's games-played count. strongBias makes better-ranked players win
// more often, which is what produces the skew in the first place — with coin
// flips everybody drifts and even the unfair engine looks acceptable.
func simulate(t *testing.T, n, courts, rounds int, fair bool, seed int64) []int {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))

	ids := make([]string, n)
	skill := map[string]int{}
	for i := range ids {
		ids[i] = fmt.Sprintf("p%02d", i)
		skill[ids[i]] = i // lower index = stronger
	}
	seats := make([]Seat, len(ids))
	for i, id := range ids {
		seats[i] = Seat{ID: id}
	}
	cs, bench := SeedPlacedCourts(seats, courts)

	played := map[string]int{}
	for r := 0; r < rounds; r++ {
		results := make([]RotResult, 0, len(cs))
		for _, c := range cs {
			for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
				played[id]++
			}
			// The stronger PAIR usually wins: sum the two skills, lower is better.
			a := skill[c.TeamA[0]] + skill[c.TeamA[1]]
			b := skill[c.TeamB[0]] + skill[c.TeamB[1]]
			w := "a"
			if b < a {
				w = "b"
			}
			if rng.Intn(100) < 25 { // upsets happen
				if w == "a" {
					w = "b"
				} else {
					w = "a"
				}
			}
			results = append(results, RotResult{Court: c.Court, Winner: w})
		}
		if fair {
			cs, bench = NextRoundFair(cs, results, bench, LosersDown, played)
		} else {
			cs, bench = NextRound(cs, results, bench, LosersDown)
		}
	}
	out := make([]int, 0, n)
	for _, id := range ids {
		out = append(out, played[id])
	}
	return out
}

func minMax(v []int) (int, int) {
	lo, hi := v[0], v[0]
	for _, x := range v {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return lo, hi
}

// The headline: 40 players on 4 courts is the shape that produced ~2 hours of
// waiting. Fairness must close the gap between the busiest and the idlest.
func TestFairRotation_ClosesTheCourtTimeGap(t *testing.T) {
	const n, courts, rounds = 40, 4, 60

	unfair := simulate(t, n, courts, rounds, false, 7)
	fair := simulate(t, n, courts, rounds, true, 7)

	uLo, uHi := minMax(unfair)
	fLo, fHi := minMax(fair)
	uSpread := uHi - uLo
	fSpread := fHi - fLo

	t.Logf("plain river : least %d games, most %d games (spread %d)", uLo, uHi, uSpread)
	t.Logf("fair pass   : least %d games, most %d games (spread %d)", fLo, fHi, fSpread)

	if fSpread >= uSpread {
		t.Fatalf("fairness did not narrow the gap: spread %d vs %d", fSpread, uSpread)
	}
	// Nobody should be frozen out. In the plain river the idlest players get a
	// small fraction of the busiest; the whole point is that everyone plays a
	// comparable amount.
	if fLo == 0 {
		t.Fatalf("a player never got on court across %d rounds", rounds)
	}
	if ratio := float64(fHi) / float64(fLo); ratio > 1.75 {
		t.Errorf("busiest player got %.2fx the court time of the idlest "+
			"(least %d, most %d) — that is not equal court time", ratio, fLo, fHi)
	}
}

// Population is the invariant that matters most: the pass moves people between
// a seat and the bench, so a bug here loses or clones players.
func TestFairRotation_NobodyIsLostOrCloned(t *testing.T) {
	for n := 5; n <= 23; n++ {
		for courts := 1; courts <= 4; courts++ {
			ids := make([]Seat, n)
			for i := range ids {
				ids[i] = Seat{ID: fmt.Sprintf("p%d", i)}
			}
			cs, bench := SeedPlacedCourts(ids, courts)
			if len(cs) == 0 {
				continue
			}
			played := map[string]int{}
			rng := rand.New(rand.NewSource(int64(n*100 + courts)))
			for r := 0; r < 40; r++ {
				res := make([]RotResult, 0, len(cs))
				for _, c := range cs {
					for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
						played[id]++
					}
					w := "a"
					if rng.Intn(2) == 0 {
						w = "b"
					}
					res = append(res, RotResult{Court: c.Court, Winner: w})
				}
				cs, bench = NextRoundFair(cs, res, bench, LosersDown, played)

				seen := map[string]int{}
				for _, c := range cs {
					for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
						if id == "" {
							t.Fatalf("n=%d courts=%d round=%d: empty seat", n, courts, r)
						}
						seen[id]++
					}
				}
				for _, b := range bench {
					seen[b]++
				}
				if len(seen) != n {
					t.Fatalf("n=%d courts=%d round=%d: %d distinct players, want %d",
						n, courts, r, len(seen), n)
				}
				for id, times := range seen {
					if times != 1 {
						t.Fatalf("n=%d courts=%d round=%d: %s appears %d times",
							n, courts, r, id, times)
					}
				}
			}
		}
	}
}

// With nobody waiting there is nothing to equalise, and the fair pass must be
// exactly the plain river — otherwise turning fairness on would change every
// night, including the ones it has no business touching.
func TestFairRotation_NoBenchMeansNoChange(t *testing.T) {
	seats := make([]Seat, 8)
	for i := range seats {
		seats[i] = Seat{ID: fmt.Sprintf("p%d", i)}
	}
	cs, bench := SeedPlacedCourts(seats, 2)
	if len(bench) != 0 {
		t.Fatalf("expected a full field, got bench %v", bench)
	}
	res := []RotResult{{Court: 1, Winner: "a"}, {Court: 2, Winner: "b"}}
	played := map[string]int{"p0": 99} // lopsided on purpose

	wantC, wantB := NextRound(cs, res, bench, LosersDown)
	gotC, gotB := NextRoundFair(cs, res, bench, LosersDown, played)

	if len(gotB) != len(wantB) {
		t.Fatalf("bench differs: %v vs %v", gotB, wantB)
	}
	for i := range wantC {
		if gotC[i] != wantC[i] {
			t.Fatalf("court %d differs: %+v vs %+v", i+1, gotC[i], wantC[i])
		}
	}
}

// A walk-up who just joined has no history. They must read as OWED court time,
// not as having had it — otherwise the newest player is the last one on.
func TestFairRotation_UnknownPlayerCountsAsZero(t *testing.T) {
	seats := make([]Seat, 6)
	for i := range seats {
		seats[i] = Seat{ID: fmt.Sprintf("p%d", i)}
	}
	cs, bench := SeedPlacedCourts(seats, 1)
	if len(bench) == 0 {
		t.Skip("need a bench for this case")
	}
	// Everyone on court has played a lot; the bench pair is absent from the map.
	played := map[string]int{}
	for _, c := range cs {
		for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			played[id] = 50
		}
	}
	res := []RotResult{{Court: 1, Winner: "a"}}
	_, gotB := NextRoundFair(cs, res, bench, LosersDown, played)

	// The long-waiting unknowns should now be ON court, so the bench should hold
	// players who HAVE played.
	for _, id := range gotB {
		if played[id] == 0 {
			t.Fatalf("a player with no court time (%s) is still benched: %v", id, gotB)
		}
	}
}

// The critical one from the ladder review. NextRound signals "this layout is
// corrupt" by returning it UNCHANGED, and the service reads that no-op
// (sameLayout) to stop the session. NextRoundFair used to swap seats on that
// refusal — so the layout differed, the tripwire never fired, and a duplicated
// or blank seat ran for the rest of the night with a player both seated and on
// the bench.
//
// Fairness is a preference. The tripwire is a safety net. A preference must
// never be able to disarm one.
func TestFairRotation_NeverTouchesARefusedLayout(t *testing.T) {
	bench := []string{"b1", "b2", "b3"}
	played := map[string]int{"p1": 99, "p2": 99, "p3": 99, "p4": 99}
	res := []RotResult{{Court: 1, Winner: "a"}}

	cases := []struct {
		name   string
		courts []RotCourt
	}{
		{
			// The exact shape the substitution remap can produce — the service's
			// own comment measured it in 15,091 of 40,000 simulated nights.
			name: "a player seated twice",
			courts: []RotCourt{{
				Court: 1,
				TeamA: [2]string{"p1", "p2"},
				TeamB: [2]string{"p1", "p4"}, // p1 twice
			}},
		},
		{
			name: "a blank seat",
			courts: []RotCourt{{
				Court: 1,
				TeamA: [2]string{"p1", ""},
				TeamB: [2]string{"p3", "p4"},
			}},
		},
		{
			name: "courts not contiguous from 1",
			courts: []RotCourt{
				{Court: 1, TeamA: [2]string{"p1", "p2"}, TeamB: [2]string{"p3", "p4"}},
				{Court: 3, TeamA: [2]string{"p5", "p6"}, TeamB: [2]string{"p7", "p8"}},
			},
		},
	}

	for _, c := range cases {
		gotC, _ := NextRoundFair(c.courts, res, bench, LosersDown, played)
		if len(gotC) != len(c.courts) {
			t.Errorf("%s: court count changed (%d -> %d)", c.name, len(c.courts), len(gotC))
			continue
		}
		for i := range c.courts {
			if gotC[i] != c.courts[i] {
				t.Errorf("%s: the fairness pass MUTATED a refused layout.\n"+
					"  court %d before: %+v\n  court %d after : %+v\n"+
					"  the service's sameLayout tripwire would no longer fire",
					c.name, i+1, c.courts[i], i+1, gotC[i])
			}
		}
	}
}

// The same corruption arriving from the other side: a bench holding somebody who
// is already seated. Swapping them in seats one person twice.
func TestFairRotation_RefusesABenchThatOverlapsTheCourts(t *testing.T) {
	courts := []RotCourt{{
		Court: 1,
		TeamA: [2]string{"p1", "p2"},
		TeamB: [2]string{"p3", "p4"},
	}}
	res := []RotResult{{Court: 1, Winner: "a"}}
	// "p3" is on court AND on the bench.
	bench := []string{"p3", "b2"}
	played := map[string]int{"p1": 99, "p2": 99, "p3": 99, "p4": 99}

	gotC, gotB := NextRoundFair(courts, res, bench, LosersDown, played)
	wantC, wantB := NextRound(courts, res, bench, LosersDown)

	for i := range wantC {
		if gotC[i] != wantC[i] {
			t.Fatalf("fairness ran on an overlapping bench: court %d %+v, want %+v",
				i+1, gotC[i], wantC[i])
		}
	}
	if len(gotB) != len(wantB) {
		t.Fatalf("bench differs: %v vs %v", gotB, wantB)
	}
}

// A blank id on the bench must not be swapped onto a court.
func TestFairRotation_RefusesABlankOnTheBench(t *testing.T) {
	courts := []RotCourt{{
		Court: 1,
		TeamA: [2]string{"p1", "p2"},
		TeamB: [2]string{"p3", "p4"},
	}}
	res := []RotResult{{Court: 1, Winner: "a"}}
	bench := []string{"", "b2"}
	played := map[string]int{"p1": 99, "p2": 99, "p3": 99, "p4": 99}

	gotC, _ := NextRoundFair(courts, res, bench, LosersDown, played)
	for _, c := range gotC {
		for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			if id == "" {
				t.Fatalf("a blank bench entry was seated: %+v", c)
			}
		}
	}
}
