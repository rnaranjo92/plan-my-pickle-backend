// Package engine holds PlanMyPickle's pure tournament logic: round-robin
// scheduling and single-elimination bracket generation. No I/O, fully testable.
//
// Ported from the Python/Dart prototypes whose invariants were verified.
package engine

import (
	"sort"
	"strconv"
	"strings"
)

type PlayFormat int

const (
	Singles PlayFormat = iota
	Doubles
)

type PartnerMode int

const (
	Fixed PartnerMode = iota
	Rotating
)

// MatchSpec is one scheduled game; a side is 1 id (singles) or 2 (doubles).
type MatchSpec struct {
	Team1       []string
	Team2       []string
	CourtNumber int
	Slot        int // wave within the round when matches exceed courts
}

type RoundSpec struct {
	RoundNumber int
	Matches     []MatchSpec
}

type rawMatch struct {
	t1, t2 []string
}

const bye = "" // sentinel BYE (player ids are never empty)

// GenerateSchedule builds a full round-robin schedule from the registered ids.
// GenerateSchedule builds the pool-play round-robin. rounds is the desired count
// for rotating doubles; minRounds/maxRounds (0 = unset) bound the rounds for ALL
// formats — a full round-robin is capped to maxRounds (partial RR) and topped up
// to minRounds by repeating matchups (a guaranteed-games social cap).
func GenerateSchedule(playerIDs []string, format PlayFormat, partner PartnerMode, numCourts int, fixedPairs [][]string, rounds, minRounds, maxRounds int) []RoundSpec {
	if numCourts < 1 {
		numCourts = 1
	}
	if format == Singles {
		rr := singlesRoundRobin(playerIDs)
		return placeCourts(fitRounds(rr, boundedTarget(len(rr), minRounds, maxRounds)), numCourts)
	}
	if partner == Fixed {
		pairs := fixedPairs
		if pairs == nil {
			pairs = autoPair(playerIDs)
		}
		rr := doublesFixed(pairs)
		return placeCourts(fitRounds(rr, boundedTarget(len(rr), minRounds, maxRounds)), numCourts)
	}
	// Rotating's "rounds" IS the round count, so bound it directly before building.
	return placeCourts(doublesRotating(playerIDs, boundedTarget(rounds, minRounds, maxRounds)), numCourts)
}

// boundedTarget clamps a natural round count into the organizer's [min, max]
// window (either 0 = unset). Always at least 1.
func boundedTarget(natural, minRounds, maxRounds int) int {
	t := natural
	if maxRounds > 0 && t > maxRounds {
		t = maxRounds
	}
	if minRounds > 0 && t < minRounds {
		t = minRounds
	}
	if t < 1 {
		t = 1
	}
	return t
}

// fitRounds resizes a generated round-robin to exactly target rounds: truncate
// when there are too many, or cycle from the start (repeat matchups) to top up.
func fitRounds(rr [][]rawMatch, target int) [][]rawMatch {
	if target <= 0 || len(rr) == 0 || len(rr) == target {
		return rr
	}
	if len(rr) > target {
		return rr[:target]
	}
	out := make([][]rawMatch, 0, target)
	for i := 0; i < target; i++ {
		out = append(out, rr[i%len(rr)])
	}
	return out
}

// singles round robin via the circle method.
func singlesRoundRobin(players []string) [][]rawMatch {
	p := append([]string{}, players...)
	if len(p)%2 == 1 {
		p = append(p, bye)
	}
	n := len(p)
	var out [][]rawMatch
	if n < 2 {
		return out
	}
	arr := append([]string{}, p...)
	for r := 0; r < n-1; r++ {
		var ms []rawMatch
		for i := 0; i < n/2; i++ {
			a, b := arr[i], arr[n-1-i]
			if a != bye && b != bye {
				ms = append(ms, rawMatch{[]string{a}, []string{b}})
			}
		}
		out = append(out, ms)
		// rotate, fixing index 0: [arr0, arrLast, arr1..arrLast-1]
		next := make([]string, 0, n)
		next = append(next, arr[0], arr[n-1])
		next = append(next, arr[1:n-1]...)
		arr = next
	}
	return out
}

// doubles, fixed partners: round-robin among the pair-units.
func doublesFixed(pairs [][]string) [][]rawMatch {
	unitIDs := make([]string, len(pairs))
	for i := range pairs {
		unitIDs[i] = strconv.Itoa(i)
	}
	rr := singlesRoundRobin(unitIDs)
	var out [][]rawMatch
	for _, round := range rr {
		var ms []rawMatch
		for _, m := range round {
			a, _ := strconv.Atoi(m.t1[0])
			b, _ := strconv.Atoi(m.t2[0])
			ms = append(ms, rawMatch{pairs[a], pairs[b]})
		}
		out = append(out, ms)
	}
	return out
}

// doubles, rotating partners: greedy social mixer.
func doublesRotating(players []string, numRounds int) [][]rawMatch {
	partnerCount := map[string]int{}
	oppCount := map[string]int{}
	games := map[string]int{}
	for _, p := range players {
		games[p] = 0
	}
	key := func(a, b string) string {
		if a <= b {
			return a + "|" + b
		}
		return b + "|" + a
	}
	pc := func(a, b string) int { return partnerCount[key(a, b)] }
	oc := func(a, b string) int { return oppCount[key(a, b)] }

	var out [][]rawMatch
	for r := 0; r < numRounds; r++ {
		pool := append([]string{}, players...)
		sort.Slice(pool, func(i, j int) bool {
			if games[pool[i]] != games[pool[j]] {
				return games[pool[i]] < games[pool[j]]
			}
			return pool[i] < pool[j]
		})
		sit := len(pool) % 4
		active := pool[:len(pool)-sit]
		if len(active) < 4 {
			out = append(out, nil)
			continue
		}

		// form teams minimizing repeat partnerships
		remaining := append([]string{}, active...)
		var teams [][]string
		for len(remaining) > 0 {
			a := remaining[0]
			remaining = remaining[1:]
			best := 0
			for i := range remaining {
				if lessTriple(pc(a, remaining[i]), oc(a, remaining[i]), remaining[i],
					pc(a, remaining[best]), oc(a, remaining[best]), remaining[best]) {
					best = i
				}
			}
			b := remaining[best]
			remaining = append(remaining[:best], remaining[best+1:]...)
			teams = append(teams, []string{a, b})
		}

		// pair teams minimizing repeat opponents
		oppCost := func(t1, t2 []string) int {
			s := 0
			for _, x := range t1 {
				for _, y := range t2 {
					s += oc(x, y)
				}
			}
			return s
		}
		var ms []rawMatch
		trem := teams
		for len(trem) > 0 {
			t1 := trem[0]
			trem = trem[1:]
			best := 0
			bestCost := oppCost(t1, trem[0])
			for i, t2 := range trem {
				c := oppCost(t1, t2)
				if c < bestCost || (c == bestCost && join(t2) < join(trem[best])) {
					best = i
					bestCost = c
				}
			}
			t2 := trem[best]
			trem = append(trem[:best], trem[best+1:]...)
			ms = append(ms, rawMatch{t1, t2})
			partnerCount[key(t1[0], t1[1])]++
			partnerCount[key(t2[0], t2[1])]++
			for _, x := range t1 {
				for _, y := range t2 {
					oppCount[key(x, y)]++
				}
			}
			for _, x := range append(append([]string{}, t1...), t2...) {
				games[x]++
			}
		}
		out = append(out, ms)
	}
	return out
}

func lessTriple(pcA, ocA int, candA string, pcB, ocB int, candB string) bool {
	if pcA != pcB {
		return pcA < pcB
	}
	if ocA != ocB {
		return ocA < ocB
	}
	return candA < candB
}

func autoPair(players []string) [][]string {
	var pairs [][]string
	for i := 0; i+1 < len(players); i += 2 {
		pairs = append(pairs, []string{players[i], players[i+1]})
	}
	return pairs
}

func placeCourts(rawRounds [][]rawMatch, numCourts int) []RoundSpec {
	var out []RoundSpec
	roundNo := 0
	for _, raw := range rawRounds {
		roundNo++
		if len(raw) == 0 {
			out = append(out, RoundSpec{RoundNumber: roundNo})
			continue
		}
		var ms []MatchSpec
		for i, m := range raw {
			ms = append(ms, MatchSpec{
				Team1:       m.t1,
				Team2:       m.t2,
				Slot:        i / numCourts,
				CourtNumber: i%numCourts + 1,
			})
		}
		out = append(out, RoundSpec{RoundNumber: roundNo, Matches: ms})
	}
	return out
}

func join(s []string) string { return strings.Join(s, "") }
