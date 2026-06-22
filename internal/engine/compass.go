package engine

import (
	"fmt"
	"sort"
)

// Compass Draw: one EAST main single-elimination bracket, plus a single-elim
// CONSOLATION bracket per East losing round. The losers of East round r (every
// round EXCEPT the final) drop together into one consolation tree named by
// compass direction:
//
//	East round 1 losers -> West
//	East round 2 losers -> North
//	East round 3 losers -> South
//	East round 4+ losers -> a generic "East-R{n} consolation" (group east_r{n})
//
// East's winner is the champion (1st), its finalist is 2nd. Each consolation's
// winner is that direction's champion. Losing inside a consolation ends a team's
// run (placement by depth reached) — there are NO sub-sub-consolations in this
// MVP (a future extension would back-draw each consolation in turn).
//
// Works for ANY field size: East is built by GenerateBracket (byes pad to the
// next power of two), and each consolation is built by the same bye-aware token
// flow the back-draw consolation uses, so a non-power-of-two count of droppers
// collapses cleanly (a lone dropper skips its bye to its first real match).

// CompassDirection orders the consolation brackets by the East round they drop
// from. Group is the stable id stored on every match (matches.bracket_group);
// Label is a human name for the UI when a fixed compass name runs out.
func compassGroup(eastRound int) (group, label string) {
	switch eastRound {
	case 1:
		return "west", "West"
	case 2:
		return "north", "North"
	case 3:
		return "south", "South"
	default:
		return fmt.Sprintf("east_r%d", eastRound), fmt.Sprintf("East-R%d", eastRound)
	}
}

// EastGroup is the bracket_group tag of the primary (East) bracket.
const EastGroup = "east"

// CompassMatchSpec is one node of a Compass plan. Group identifies its bracket
// (east | west | north | south | east_r{n}). For East matches the winner feeds
// (FeedsRound/Slot/Team within East) and the loser drops into a consolation
// (LoserGroup + LoserRound/Slot/Team). For consolation matches only the winner
// feed is set (its loser is eliminated). FeedsRound==0 means that bracket's
// FINAL. A bye (East round 1) carries ResolvedWinner and produces no loser.
type CompassMatchSpec struct {
	Group        string
	Round, Slot  int
	Side1, Side2 []string
	ResolvedWinner []string

	// Winner feed, WITHIN the same Group (0 round = this group's final).
	FeedsRound, FeedsSlot, FeedsTeam int

	// Loser drop into a consolation (East matches only); empty group = no drop
	// (East final, or a bye that produced no loser).
	LoserGroup                   string
	LoserRound, LoserSlot, LoserTeam int
}

// CompassBracket describes one rendered bracket in the plan (East or one
// consolation direction) for callers/tests that want the bracket list.
type CompassBracket struct {
	Group  string
	Label  string
	Rounds int
}

type CompassPlan struct {
	Size     int // East bracket size (power of two)
	Brackets []CompassBracket
	Matches  []*CompassMatchSpec
}

// GenerateCompass builds the full Compass plan from best-seed-first sides.
func GenerateCompass(seededSides [][]string) CompassPlan {
	east := GenerateBracket(seededSides)

	var matches []*CompassMatchSpec
	brackets := []CompassBracket{{Group: EastGroup, Label: "East", Rounds: east.Rounds}}

	// East matches: reuse the single-elim structure, tag group=east. Winner feeds
	// stay within East; loser drops are wired below per losing round.
	eastByKey := make(map[[2]int]*CompassMatchSpec, len(east.Matches))
	for _, m := range east.Matches {
		cm := &CompassMatchSpec{
			Group: EastGroup, Round: m.Round, Slot: m.Slot,
			Side1: m.Side1, Side2: m.Side2, ResolvedWinner: m.ResolvedWinner,
		}
		if m.FeedsRound != 0 {
			cm.FeedsRound, cm.FeedsSlot, cm.FeedsTeam = m.FeedsRound, m.FeedsSlot, m.FeedsTeam
		}
		eastByKey[[2]int{m.Round, m.Slot}] = cm
		matches = append(matches, cm)
	}

	// A bye is an East round-1 match the generator already resolved (no loser to
	// drop). Only round 1 can hold byes in a single-elim draw.
	isBye := func(round, slot int) bool {
		m := eastByKey[[2]int{round, slot}]
		return m != nil && m.ResolvedWinner != nil
	}

	// For each East round EXCEPT the final, the losers of that round drop into one
	// consolation bracket. The droppers are the round's matches in slot order; a
	// bye match contributes no dropper (handled as an empty consolation slot).
	for r := 1; r < east.Rounds; r++ {
		group, label := compassGroup(r)
		cnt := east.Size / (1 << r) // East round-r match count (full structure)

		// Build this round's consolation as a single-elim tree among its droppers,
		// reusing the bye-aware token flow. dropperBye(j) reports whether East
		// round-r slot j was a bye (so it produces no loser to drop).
		dropperBye := func(j int) bool { return isBye(r, j) }
		cons, ok := buildConsolationTree(cnt, dropperBye)
		if !ok {
			continue // <2 real droppers (e.g. tiny field) — no consolation here.
		}
		brackets = append(brackets, CompassBracket{Group: group, Label: label, Rounds: cons.rounds})

		// Emit the consolation matches (group-tagged) and their winner feeds.
		for _, c := range cons.matches {
			cm := &CompassMatchSpec{Group: group, Round: c.round, Slot: c.slot}
			if c.feedsRound != 0 {
				cm.FeedsRound, cm.FeedsSlot, cm.FeedsTeam = c.feedsRound, c.feedsSlot, c.feedsTeam
			}
			matches = append(matches, cm)
		}

		// Wire each East round-r match's LOSER into the slot where that dropper
		// enters its first real consolation match.
		for _, d := range cons.drops {
			src := eastByKey[[2]int{r, d.source}]
			if src == nil {
				continue
			}
			src.LoserGroup = group
			src.LoserRound, src.LoserSlot, src.LoserTeam = d.round, d.slot, d.team
		}
	}

	return CompassPlan{Size: east.Size, Brackets: brackets, Matches: matches}
}

// consNode is one emitted consolation match (round/slot within its tree) and its
// winner feed (feedsRound==0 = that tree's final).
type consNode struct {
	round, slot                      int
	feedsRound, feedsSlot, feedsTeam int
}

// consDrop says where the dropper from source index `source` enters the tree.
type consDrop struct {
	source            int // index into the round's matches (0-based, slot order)
	round, slot, team int
}

type consTree struct {
	rounds  int
	matches []*consNode
	drops   []consDrop
}

// buildConsolationTree builds a single-elim tree among `count` potential
// droppers (one per source slot, in order), where isBye(j) reports a source that
// produces no dropper. It mirrors GenerateConsolation's bye-aware token flow: a
// lone real dropper skips past byes to its first real match, and only matches
// that will have two real players are emitted. Rounds (and every feed/drop that
// targets them) are relabeled to a contiguous 1..N so the UI draws no gap
// columns. Returns ok=false when fewer than two real droppers exist.
func buildConsolationTree(count int, isBye func(j int) bool) (consTree, bool) {
	if count < 2 {
		return consTree{}, false
	}
	// Pad the source lane to a power of two so the pairing tree is uniform; the
	// padding slots are byes (no dropper), collapsed by the token flow.
	lane := nextPow2(count)
	structuralRounds := ilog2(lane)

	const (
		kindEmpty = iota
		kindLoser
		kindWinner
	)
	type token struct {
		kind     int
		source   int
		matchKey string
	}
	empty := token{kind: kindEmpty}

	cur := make([]token, lane)
	real := 0
	for i := 0; i < lane; i++ {
		if i < count && !isBye(i) {
			cur[i] = token{kind: kindLoser, source: i}
			real++
		} else {
			cur[i] = empty
		}
	}
	if real < 2 {
		return consTree{}, false
	}

	var nodes []*consNode
	var drops []consDrop
	byKey := map[string]*consNode{}

	for r := 1; r <= structuralRounds; r++ {
		next := make([]token, len(cur)/2)
		for j := 0; j < len(next); j++ {
			a, b := cur[2*j], cur[2*j+1]
			aEmpty, bEmpty := a.kind == kindEmpty, b.kind == kindEmpty
			switch {
			case aEmpty && bEmpty:
				next[j] = empty
			case aEmpty:
				next[j] = b // lone real dropper skips up (consolation bye)
			case bEmpty:
				next[j] = a
			default:
				mk := fmt.Sprintf("%d:%d", r, j)
				node := &consNode{round: r, slot: j}
				nodes = append(nodes, node)
				byKey[mk] = node
				record := func(team int, tk token) {
					switch tk.kind {
					case kindLoser:
						drops = append(drops, consDrop{source: tk.source, round: r, slot: j, team: team})
					case kindWinner:
						f := byKey[tk.matchKey]
						f.feedsRound, f.feedsSlot, f.feedsTeam = r, j, team
					}
				}
				record(1, a)
				record(2, b)
				next[j] = token{kind: kindWinner, matchKey: mk}
			}
		}
		cur = next
	}

	// Relabel emitted rounds onto a contiguous 1..N (byes can empty whole rounds),
	// applied identically to match rounds, winner-feed rounds, and drop rounds.
	effRounds := 0
	if len(nodes) > 0 {
		seen := map[int]bool{}
		for i := range nodes {
			seen[nodes[i].round] = true
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
		for i := range nodes {
			nodes[i].round = remap[nodes[i].round]
			if nodes[i].feedsRound != 0 {
				nodes[i].feedsRound = remap[nodes[i].feedsRound]
			}
		}
		for i := range drops {
			drops[i].round = remap[drops[i].round]
		}
		effRounds = len(ordered)
	}
	return consTree{rounds: effRounds, matches: nodes, drops: drops}, true
}
