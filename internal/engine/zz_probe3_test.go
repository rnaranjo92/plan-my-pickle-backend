package engine

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// variantA = NextRoundFair, but the swapped-off player goes to the BACK of the
// bench instead of into the incoming player's slot.
func variantA(courts []RotCourt, results []RotResult, bench []string,
	mode LoserMode, played map[string]int) ([]RotCourt, []string) {
	next, nextBench := NextRound(courts, results, bench, mode)
	if len(nextBench) == 0 || len(next) == 0 {
		return next, nextBench
	}
	plays := func(id string) int {
		if played == nil {
			return 0
		}
		return played[id]
	}
	type seatRef struct {
		court, idx int
		id         string
	}
	seats := make([]seatRef, 0, len(next)*4)
	for ci := range next {
		ids := [4]string{next[ci].TeamA[0], next[ci].TeamA[1], next[ci].TeamB[0], next[ci].TeamB[1]}
		for i, id := range ids {
			seats = append(seats, seatRef{ci, i, id})
		}
	}
	sort.SliceStable(seats, func(a, b int) bool { return plays(seats[a].id) > plays(seats[b].id) })
	benchOrder := make([]int, len(nextBench))
	for i := range benchOrder {
		benchOrder[i] = i
	}
	sort.SliceStable(benchOrder, func(a, b int) bool {
		return plays(nextBench[benchOrder[a]]) < plays(nextBench[benchOrder[b]])
	})
	setSeat := func(s seatRef, id string) {
		switch s.idx {
		case 0:
			next[s.court].TeamA[0] = id
		case 1:
			next[s.court].TeamA[1] = id
		case 2:
			next[s.court].TeamB[0] = id
		case 3:
			next[s.court].TeamB[1] = id
		}
	}
	removed := map[int]bool{}
	var offs []string
	swaps := min2(len(nextBench))
	for i := 0; i < swaps && i < len(seats) && i < len(benchOrder); i++ {
		seat := seats[i]
		bi := benchOrder[i]
		if plays(nextBench[bi]) >= plays(seat.id) {
			break
		}
		setSeat(seat, nextBench[bi])
		removed[bi] = true
		offs = append(offs, seat.id)
	}
	nb := make([]string, 0, len(nextBench))
	for i, id := range nextBench {
		if !removed[i] {
			nb = append(nb, id)
		}
	}
	nb = append(nb, offs...)
	return next, nb
}

// variantB = NextRoundFair, but bench candidates are taken in FIFO order (front
// of queue) rather than by fewest games.
func variantB(courts []RotCourt, results []RotResult, bench []string,
	mode LoserMode, played map[string]int) ([]RotCourt, []string) {
	next, nextBench := NextRound(courts, results, bench, mode)
	if len(nextBench) == 0 || len(next) == 0 {
		return next, nextBench
	}
	plays := func(id string) int {
		if played == nil {
			return 0
		}
		return played[id]
	}
	type seatRef struct {
		court, idx int
		id         string
	}
	seats := make([]seatRef, 0, len(next)*4)
	for ci := range next {
		ids := [4]string{next[ci].TeamA[0], next[ci].TeamA[1], next[ci].TeamB[0], next[ci].TeamB[1]}
		for i, id := range ids {
			seats = append(seats, seatRef{ci, i, id})
		}
	}
	sort.SliceStable(seats, func(a, b int) bool { return plays(seats[a].id) > plays(seats[b].id) })
	setSeat := func(s seatRef, id string) {
		switch s.idx {
		case 0:
			next[s.court].TeamA[0] = id
		case 1:
			next[s.court].TeamA[1] = id
		case 2:
			next[s.court].TeamB[0] = id
		case 3:
			next[s.court].TeamB[1] = id
		}
	}
	removed := map[int]bool{}
	var offs []string
	swaps := min2(len(nextBench))
	for i := 0; i < swaps && i < len(seats) && i < len(nextBench); i++ {
		seat := seats[i]
		if plays(nextBench[i]) >= plays(seat.id) {
			break
		}
		setSeat(seat, nextBench[i])
		removed[i] = true
		offs = append(offs, seat.id)
	}
	nb := make([]string, 0, len(nextBench))
	for i, id := range nextBench {
		if !removed[i] {
			nb = append(nb, id)
		}
	}
	nb = append(nb, offs...)
	return next, nb
}

type nextFn func([]RotCourt, []RotResult, []string, LoserMode, map[string]int) ([]RotCourt, []string)

func runFn(t *testing.T, n, courts, rounds int, seed int64, fn nextFn) (int, int, int, int) {
	t.Helper()
	seats := make([]Seat, n)
	skill := map[string]int{}
	for i := range seats {
		seats[i] = Seat{ID: fmt.Sprintf("p%02d", i)}
		skill[seats[i].ID] = i
	}
	cs, bench := SeedPlacedCourts(seats, courts)
	played := map[string]int{}
	streak := map[string]int{}
	worst := map[string]int{}
	rng := rand.New(rand.NewSource(seed))
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
		cs, bench = fn(cs, res, bench, LosersDown, played)
	}
	gLo, gHi := spread(played)
	_, wHi := spread(worst)
	return gLo, gHi, wHi, 0
}

func plainFn(c []RotCourt, r []RotResult, b []string, m LoserMode, _ map[string]int) ([]RotCourt, []string) {
	return NextRound(c, r, b, m)
}

func TestProbe3_WhichLine(t *testing.T) {
	for _, sh := range [][2]int{{40, 1}, {30, 1}, {24, 1}, {40, 2}, {40, 4}, {24, 2}} {
		n, c := sh[0], sh[1]
		type row struct {
			name string
			fn   nextFn
		}
		for _, v := range []row{
			{"plain    ", plainFn},
			{"shipped  ", NextRoundFair},
			{"A back   ", variantA},
			{"B fifo   ", variantB},
		} {
			lo, hi, w, _ := runFn(t, n, c, 200, 5, v.fn)
			t.Logf("n=%2d c=%d %s games %3d-%3d  worst-consecutive-sitout %3d", n, c, v.name, lo, hi, w)
		}
		t.Log("---")
	}
}
