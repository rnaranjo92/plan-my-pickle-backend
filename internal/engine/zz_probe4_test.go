package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// Trace exactly what happens to the player who ends up with the worst wait.
func TestProbe4_Trace(t *testing.T) {
	n, courts, rounds := 40, 1, 200
	seats := make([]Seat, n)
	skill := map[string]int{}
	for i := range seats {
		seats[i] = Seat{ID: fmt.Sprintf("p%02d", i)}
		skill[seats[i].ID] = i
	}
	cs, bench := SeedPlacedCourts(seats, courts)
	played := map[string]int{}
	rng := rand.New(rand.NewSource(5))

	streak := map[string]int{}
	worst := map[string]int{}
	// "phantom turn": the bye swap seated you, the fairness pass un-seated you in
	// the same call, so you never played.
	phantom := map[string]int{}
	// queue index right after each round's rotation
	idxOf := func(b []string, id string) int {
		for i, x := range b {
			if x == id {
				return i
			}
		}
		return -1
	}
	type ev struct {
		round      int
		id         string
		fromIdx    int
		toIdx      int
		benchLen   int
	}
	var evs []ev

	for r := 0; r < rounds; r++ {
		on := map[string]bool{}
		res := make([]RotResult, 0, len(cs))
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
				if streak[s.ID] > worst[s.ID] {
					worst[s.ID] = streak[s.ID]
				}
			}
		}
		benchBefore := append([]string(nil), bench...)
		plainC, _ := NextRound(cs, res, bench, LosersDown)
		fairC, fairB := NextRoundFair(cs, res, bench, LosersDown, played)
		plainOn := idsOf(plainC)
		fairOn := idsOf(fairC)
		for _, id := range benchBefore {
			_, p := plainOn[id]
			_, f := fairOn[id]
			if p && !f {
				phantom[id]++
				evs = append(evs, ev{r, id, idxOf(benchBefore, id), idxOf(fairB, id), len(fairB)})
			}
		}
		cs, bench = fairC, fairB
	}

	// worst waiter
	wid, wv := "", -1
	for id, v := range worst {
		if v > wv {
			wid, wv = id, v
		}
	}
	t.Logf("worst waiter %s sat %d consecutive rounds; phantom turns (seated by the "+
		"bye swap then un-seated by the fairness pass in the same call): %d", wid, wv, phantom[wid])
	total := 0
	for _, v := range phantom {
		total += v
	}
	t.Logf("phantom turns across the night: %d over %d rounds", total, rounds)
	for _, e := range evs {
		if e.id == wid {
			t.Logf("  r%3d %s: queue idx %2d -> %2d (bench len %d)  == turn consumed, sent back",
				e.round, e.id, e.fromIdx, e.toIdx, e.benchLen)
		}
	}
}
