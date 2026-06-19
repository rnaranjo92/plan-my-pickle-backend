package engine

import (
	"fmt"
	"sort"
)

// Consolation back-draw: the first-round losers of a single-elimination main
// bracket play their own single-elim down to a consolation champion (bronze), so
// every entrant gets at least two matches (USA Pickleball Rule 12.J).
//
// The main bracket has Size/2 round-1 matches; the loser of each drops into the
// consolation tree, paired so two players from DIFFERENT main matches meet (never
// a rematch). Main round-1 matches that were BYES produce no loser, so the
// player who would have met that missing loser gets a consolation bye and
// advances — exactly as byes work in the main draw. Because a bye is stored as
// an absent team (no sentinel), this is resolved HERE at generation: a lone real
// loser is routed past the bye to its first real match, and only matches that
// will have two real players are emitted. No phantom/auto-complete matches.

// ConsolationMatchSpec is one node in the consolation tree. FeedsRound == 0 means
// the consolation FINAL (its winner is the consolation champion / bronze).
type ConsolationMatchSpec struct {
	Round, Slot                      int
	FeedsRound, FeedsSlot, FeedsTeam int
}

// LoserDrop says where a main-bracket round-1 loser ENTERS the consolation tree
// (the first real match it plays, after skipping any byes).
type LoserDrop struct {
	MainSlot          int
	Round, Slot, Team int // Team is 1 or 2
}

type ConsolationPlan struct {
	Rounds  int
	Matches []*ConsolationMatchSpec
	Drops   []LoserDrop
}

// GenerateConsolation builds the consolation tree for a main single-elim bracket
// of size (a power of two). isBye(mainSlot) reports whether main round-1 match
// mainSlot was a bye (no loser). Returns an empty plan when size < 4.
func GenerateConsolation(size int, isBye func(mainSlot int) bool) ConsolationPlan {
	entrants := size / 2 // main round-1 matches / potential losers
	if entrants < 2 {
		return ConsolationPlan{}
	}
	structuralRounds := ilog2(entrants)

	// A token flowing up the tree: a real main loser, the winner of a live
	// consolation match, or empty (a bye produced no loser).
	const (
		kindEmpty = iota
		kindLoser
		kindWinner
	)
	type token struct {
		kind     int
		loser    int    // main slot (kindLoser)
		matchKey string // live match key (kindWinner)
	}
	empty := token{kind: kindEmpty}

	cur := make([]token, entrants)
	for i := 0; i < entrants; i++ {
		if isBye(i) {
			cur[i] = empty
		} else {
			cur[i] = token{kind: kindLoser, loser: i}
		}
	}

	var matches []*ConsolationMatchSpec
	var drops []LoserDrop
	byKey := map[string]*ConsolationMatchSpec{}

	for r := 1; r <= structuralRounds; r++ {
		next := make([]token, len(cur)/2)
		for j := 0; j < len(next); j++ {
			a, b := cur[2*j], cur[2*j+1]
			aEmpty, bEmpty := a.kind == kindEmpty, b.kind == kindEmpty
			switch {
			case aEmpty && bEmpty:
				next[j] = empty
			case aEmpty:
				next[j] = b // lone real entrant skips up (consolation bye)
			case bEmpty:
				next[j] = a
			default:
				// Both real -> a match that will actually be played.
				mk := fmt.Sprintf("%d:%d", r, j)
				m := &ConsolationMatchSpec{Round: r, Slot: j}
				matches = append(matches, m)
				byKey[mk] = m
				record := func(team int, tk token) {
					switch tk.kind {
					case kindLoser:
						drops = append(drops, LoserDrop{MainSlot: tk.loser, Round: r, Slot: j, Team: team})
					case kindWinner:
						f := byKey[tk.matchKey]
						f.FeedsRound, f.FeedsSlot, f.FeedsTeam = r, j, team
					}
				}
				record(1, a)
				record(2, b)
				next[j] = token{kind: kindWinner, matchKey: mk}
			}
		}
		cur = next
	}

	// Byes can collapse (and even leave gaps in) the structural rounds. Relabel
	// the distinct emitted rounds onto a contiguous 1..N — applied identically to
	// match rounds, winner-feed rounds, and loser-drop rounds — so the bracket UI
	// renders no empty round columns. Slots within a round are untouched.
	effRounds := 0
	if len(matches) > 0 {
		seen := map[int]bool{}
		for _, m := range matches {
			seen[m.Round] = true
		}
		ordered := make([]int, 0, len(seen))
		for r := range seen {
			ordered = append(ordered, r)
		}
		sort.Ints(ordered)
		remap := make(map[int]int, len(ordered))
		for i, r := range ordered {
			remap[r] = i + 1
		}
		for _, m := range matches {
			m.Round = remap[m.Round]
			if m.FeedsRound != 0 {
				m.FeedsRound = remap[m.FeedsRound]
			}
		}
		for i := range drops {
			drops[i].Round = remap[drops[i].Round]
		}
		effRounds = len(ordered)
	}
	return ConsolationPlan{Rounds: effRounds, Matches: matches, Drops: drops}
}
