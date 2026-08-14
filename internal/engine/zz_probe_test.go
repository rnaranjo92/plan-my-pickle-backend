package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestProbe_StayModePopulation(t *testing.T) {
	for _, mode := range []LoserMode{LosersDown, LosersStay} {
		for n := 5; n <= 25; n++ {
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
				for r := 0; r < 60; r++ {
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
					cs, bench = NextRoundFair(cs, res, bench, mode, played)
					seen := map[string]int{}
					for _, c := range cs {
						for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
							seen[id]++
						}
					}
					for _, b := range bench {
						seen[b]++
					}
					if len(seen) != n {
						t.Fatalf("mode=%d n=%d courts=%d r=%d: distinct %d want %d courts=%+v bench=%v", mode, n, courts, r, len(seen), n, cs, bench)
					}
					for id, k := range seen {
						if k != 1 {
							t.Fatalf("mode=%d n=%d courts=%d r=%d: %s x%d", mode, n, courts, r, id, k)
						}
					}
				}
			}
		}
	}
}

// Does the fair pass ever bench someone two rounds in a row while another plays every round?
func TestProbe_ConsecutiveSits(t *testing.T) {
	n, courts := 24, 4
	ids := make([]Seat, n)
	for i := range ids {
		ids[i] = Seat{ID: fmt.Sprintf("p%02d", i)}
	}
	cs, bench := SeedPlacedCourts(ids, courts)
	played := map[string]int{}
	rng := rand.New(rand.NewSource(3))
	sitStreak := map[string]int{}
	worst := 0
	for r := 0; r < 200; r++ {
		res := make([]RotResult, 0, len(cs))
		for _, c := range cs {
			for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
				played[id]++
				sitStreak[id] = 0
			}
			w := "a"
			if rng.Intn(2) == 0 {
				w = "b"
			}
			res = append(res, RotResult{Court: c.Court, Winner: w})
		}
		for _, b := range bench {
			sitStreak[b]++
			if sitStreak[b] > worst {
				worst = sitStreak[b]
			}
		}
		cs, bench = NextRoundFair(cs, res, bench, LosersDown, played)
	}
	t.Logf("worst consecutive sit streak = %d", worst)
}
