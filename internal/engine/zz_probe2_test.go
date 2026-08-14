package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

type simCfg struct {
	n, courts, rounds int
	fair              bool
	mode              LoserMode
	seed              int64
	stale             bool // feed counts that exclude the round just played
}

type simOut struct {
	games    map[string]int
	maxWait  map[string]int
	flipFlop map[string]int // times a player went on->off->on (or off->on->off) in 2 rounds
}

func run(t *testing.T, cfg simCfg) simOut {
	t.Helper()
	seats := make([]Seat, cfg.n)
	skill := map[string]int{}
	for i := range seats {
		seats[i] = Seat{ID: fmt.Sprintf("p%02d", i)}
		skill[seats[i].ID] = i
	}
	cs, bench := SeedPlacedCourts(seats, cfg.courts)
	played := map[string]int{}
	rng := rand.New(rand.NewSource(cfg.seed))
	out := simOut{games: map[string]int{}, maxWait: map[string]int{}, flipFlop: map[string]int{}}
	streak := map[string]int{}
	var prevOn, prevPrevOn map[string]bool

	for r := 0; r < cfg.rounds; r++ {
		on := map[string]bool{}
		res := make([]RotResult, 0, len(cs))
		snapshot := map[string]int{}
		for k, v := range played {
			snapshot[k] = v
		}
		for _, c := range cs {
			for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
				played[id]++
				on[id] = true
			}
			a := skill[c.TeamA[0]] + skill[c.TeamA[1]]
			b := skill[c.TeamB[0]] + skill[c.TeamB[1]]
			w := "a"
			if b < a {
				w = "b"
			}
			if rng.Intn(100) < 25 {
				if w == "a" {
					w = "b"
				} else {
					w = "a"
				}
			}
			res = append(res, RotResult{Court: c.Court, Winner: w})
		}
		for _, s := range seats {
			if on[s.ID] {
				streak[s.ID] = 0
			} else {
				streak[s.ID]++
				if streak[s.ID] > out.maxWait[s.ID] {
					out.maxWait[s.ID] = streak[s.ID]
				}
			}
		}
		if prevPrevOn != nil {
			for _, s := range seats {
				if prevPrevOn[s.ID] != prevOn[s.ID] && prevOn[s.ID] != on[s.ID] {
					out.flipFlop[s.ID]++
				}
			}
		}
		prevPrevOn, prevOn = prevOn, on

		counts := played
		if cfg.stale {
			counts = snapshot
		}
		if cfg.fair {
			cs, bench = NextRoundFair(cs, res, bench, cfg.mode, counts)
		} else {
			cs, bench = NextRound(cs, res, bench, cfg.mode)
		}
		// invariant check
		seen := map[string]int{}
		for _, c := range cs {
			for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
				if id == "" {
					t.Fatalf("cfg=%+v round=%d: EMPTY SEAT", cfg, r)
				}
				seen[id]++
			}
		}
		for _, id := range bench {
			seen[id]++
		}
		if len(seen) != cfg.n {
			t.Fatalf("cfg=%+v round=%d: %d distinct, want %d", cfg, r, len(seen), cfg.n)
		}
		for id, k := range seen {
			if k != 1 {
				t.Fatalf("cfg=%+v round=%d: %s x%d", cfg, r, id, k)
			}
		}
	}
	out.games = played
	return out
}

func spread(m map[string]int) (int, int) {
	lo, hi := 1<<30, -1
	for _, v := range m {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return lo, hi
}

func sum(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

func TestProbe2_Shapes(t *testing.T) {
	for _, n := range []int{17, 18, 20, 24, 30, 40} {
		for _, courts := range []int{1, 2, 3, 4} {
			for _, mode := range []LoserMode{LosersDown, LosersStay} {
				if n/4 == 0 {
					continue
				}
				pf := run(t, simCfg{n: n, courts: courts, rounds: 150, fair: false, mode: mode, seed: 5})
				ff := run(t, simCfg{n: n, courts: courts, rounds: 150, fair: true, mode: mode, seed: 5})
				pLo, pHi := spread(pf.games)
				fLo, fHi := spread(ff.games)
				_, pW := spread(pf.maxWait)
				_, fW := spread(ff.maxWait)
				t.Logf("n=%2d c=%d mode=%d | games plain %3d-%3d fair %3d-%3d | maxwait plain %2d fair %2d | flipflops plain %3d fair %3d",
					n, courts, mode, pLo, pHi, fLo, fHi, pW, fW, sum(pf.flipFlop), sum(ff.flipFlop))
			}
		}
	}
}

// The service reads play counts BEFORE the advance RPC tallies the round just
// played, so every on-court player is one game light. Does that break it?
func TestProbe2_StaleCounts(t *testing.T) {
	for _, n := range []int{18, 24, 40} {
		fresh := run(t, simCfg{n: n, courts: 4, rounds: 150, fair: true, mode: LosersDown, seed: 5})
		stale := run(t, simCfg{n: n, courts: 4, rounds: 150, fair: true, mode: LosersDown, seed: 5, stale: true})
		aLo, aHi := spread(fresh.games)
		bLo, bHi := spread(stale.games)
		t.Logf("n=%d fresh %d-%d (flip %d) | stale %d-%d (flip %d)",
			n, aLo, aHi, sum(fresh.flipFlop), bLo, bHi, sum(stale.flipFlop))
	}
}
