package engine

import (
	"fmt"
	"sort"
)

// Compass Draw: one EAST main single-elimination bracket plus directional
// consolation brackets, with SIDEWAYS feeding per the authoritative compass
// layout (compassdraw.com / USTA): losing moves you to a new direction, so
// everyone keeps playing.
//
//	East round 1 losers -> West        West round 1 losers  -> South
//	East round 2 losers -> North       West round 2 losers  -> Southwest
//	East round 3 losers -> Northeast   North round 1 losers -> Northwest
//	East round 4+ losers -> generic    South round 1 losers -> Southeast
//	  "East-R{n}" (group east_r{n})
//
// East's winner is the champion (1st), its finalist 2nd; each direction's
// winner is that direction's champion. A direction is only formed when its
// feeding round has 2+ real losers (byes thin the droppers), and the terminal
// directions (Northeast/Southwest/Northwest/Southeast + generics) end a run.
// On a full 16 draw this is the classic 8-bracket compass where every entrant
// is guaranteed 4 matches; an 8 draw forms the canonical East/West/North/South.
//
// Works for ANY field size: East is built by GenerateBracket (byes pad to the
// next power of two), and each direction is built by the same bye-aware token
// flow the back-draw consolation uses, so a non-power-of-two count of droppers
// collapses cleanly (a lone dropper skips its bye to its first real match).

// compassGroup names the direction fed by East round r losers. Group is the
// stable id stored on every match (matches.bracket_group); Label is the human
// name for the UI when a fixed compass name runs out.
func compassGroup(eastRound int) (group, label string) {
	switch eastRound {
	case 1:
		return "west", "West"
	case 2:
		return "north", "North"
	case 3:
		return "northeast", "Northeast"
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
	Group          string
	Round, Slot    int
	Side1, Side2   []string
	ResolvedWinner []string

	// Winner feed, WITHIN the same Group (0 round = this group's final).
	FeedsRound, FeedsSlot, FeedsTeam int

	// Loser drop into a consolation (East matches only); empty group = no drop
	// (East final, or a bye that produced no loser).
	LoserGroup                       string
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

	// emitted is a built direction bracket, keyed by STRUCTURAL (pre-relabel)
	// coordinates so its own rounds can feed child directions the same bye-aware
	// way East's rounds feed it.
	type emitted struct {
		byStruct     map[[2]int]*CompassMatchSpec
		structRounds int
	}

	// feedFrom builds direction `group` from the losers of a parent round:
	// parentAt(j) returns the parent's real match at source slot j (nil = bye /
	// collapsed, so no dropper), cnt is the parent round's structural slot count.
	// Each parent match's LOSER is wired into its entry in the new tree.
	feedFrom := func(group, label string, cnt int, parentAt func(j int) *CompassMatchSpec) *emitted {
		cons, ok := buildConsolationTree(cnt, func(j int) bool { return parentAt(j) == nil })
		if !ok {
			return nil // <2 real droppers — direction not formed
		}
		brackets = append(brackets, CompassBracket{Group: group, Label: label, Rounds: cons.rounds})
		e := &emitted{byStruct: map[[2]int]*CompassMatchSpec{}, structRounds: cons.structRounds}
		for _, c := range cons.matches {
			cm := &CompassMatchSpec{Group: group, Round: c.round, Slot: c.slot}
			if c.feedsRound != 0 {
				cm.FeedsRound, cm.FeedsSlot, cm.FeedsTeam = c.feedsRound, c.feedsSlot, c.feedsTeam
			}
			matches = append(matches, cm)
			e.byStruct[[2]int{c.sround, c.sslot}] = cm
		}
		for _, d := range cons.drops {
			if src := parentAt(d.source); src != nil {
				src.LoserGroup = group
				src.LoserRound, src.LoserSlot, src.LoserTeam = d.round, d.slot, d.team
			}
		}
		return e
	}

	// East round r as a parent: a real match is one that exists and isn't a
	// resolved bye (only round 1 can hold byes in a single-elim draw).
	eastAt := func(r int) func(j int) *CompassMatchSpec {
		return func(j int) *CompassMatchSpec {
			m := eastByKey[[2]int{r, j}]
			if m == nil || m.ResolvedWinner != nil {
				return nil
			}
			return m
		}
	}
	// A direction's structural round r as a parent for its child direction.
	secAt := func(e *emitted, r int) (func(j int) *CompassMatchSpec, int) {
		cnt := (1 << e.structRounds) / (1 << r)
		return func(j int) *CompassMatchSpec { return e.byStruct[[2]int{r, j}] }, cnt
	}

	// East feeds: r=1 -> West, r=2 -> North, r=3 -> Northeast, r>=4 -> generic.
	// Only rounds BEFORE the East final drop (the finalist takes 2nd place).
	var west, north *emitted
	for r := 1; r < east.Rounds; r++ {
		group, label := compassGroup(r)
		e := feedFrom(group, label, east.Size/(1<<r), eastAt(r))
		switch r {
		case 1:
			west = e
		case 2:
			north = e
		}
	}
	// Sideways feeds (the compass's whole point): West r1 -> South, West r2 ->
	// Southwest, North r1 -> Northwest, South r1 -> Southeast. A direction only
	// feeds a child from rounds BEFORE its own final (structRounds > r), and the
	// child forms only with 2+ real droppers — both fall out of feedFrom.
	var south *emitted
	if west != nil && west.structRounds > 1 {
		pf, cnt := secAt(west, 1)
		south = feedFrom("south", "South", cnt, pf)
	}
	if west != nil && west.structRounds > 2 {
		pf, cnt := secAt(west, 2)
		feedFrom("southwest", "Southwest", cnt, pf)
	}
	if north != nil && north.structRounds > 1 {
		pf, cnt := secAt(north, 1)
		feedFrom("northwest", "Northwest", cnt, pf)
	}
	if south != nil && south.structRounds > 1 {
		pf, cnt := secAt(south, 1)
		feedFrom("southeast", "Southeast", cnt, pf)
	}

	return CompassPlan{Size: east.Size, Brackets: brackets, Matches: matches}
}

// consNode is one emitted consolation match (round/slot within its tree) and its
// winner feed (feedsRound==0 = that tree's final). sround/sslot preserve the
// STRUCTURAL (pre-relabel) coordinates so a tree's own rounds can feed child
// directions (the compass's sideways drops) even after bye-collapsed rounds are
// renumbered away.
type consNode struct {
	round, slot                      int
	sround, sslot                    int
	feedsRound, feedsSlot, feedsTeam int
}

// consDrop says where the dropper from source index `source` enters the tree.
type consDrop struct {
	source            int // index into the round's matches (0-based, slot order)
	round, slot, team int
}

type consTree struct {
	rounds       int // emitted (relabeled) round count
	structRounds int // full structural rounds of the padded lane
	matches      []*consNode
	drops        []consDrop
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
				node := &consNode{round: r, slot: j, sround: r, sslot: j}
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
	return consTree{rounds: effRounds, structRounds: structuralRounds, matches: nodes, drops: drops}, true
}
