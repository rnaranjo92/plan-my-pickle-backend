package engine

import "sort"

// Double elimination: a winners bracket (WB) plus a losers bracket (LB). A team
// is out only after TWO losses. The WB champion meets the LB champion in the
// grand final; if the LB champion wins game 1 a deciding reset game is played
// (both then have one loss) — that conditional reset is handled at score time,
// not in this static structure.
//
// LB construction (n = 2^k entrants, k WB rounds): LB has 2(k-1) rounds that
// alternate MINOR (LB winners meet) and MAJOR (an LB winner meets a freshly
// dropped WB loser). Rounds 2i-1 and 2i each have n/2^(i+1) matches (i=1..k-1).
// "Come-around": WB losers are dropped into the major round in REVERSED slot
// order so a dropped team doesn't immediately face someone from its own WB
// region (which would risk an early rematch).
//
// Byes (fields that aren't a power of two) only ever occur in WB round 1, so
// they collapse exactly the entry of the losers tree. They're resolved by the
// token flow below: a lone real entrant skips past a bye, and only matches with
// two real entrants are emitted — the same technique the consolation generator
// uses.

// DEMatch is one node of a double-elim plan. Win* is where the winner advances;
// Lose* is where the loser advances (WB matches only). A "" tier feed means no
// feed (the grand final's winner, or the LB has no loser feed).
type DEMatch struct {
	Tier           string // "winners" | "losers" | "grand_final"
	Round, Slot    int
	Side1, Side2   []string // seeded (WB round 1) or nil (TBD); {Bye} for a bye
	ResolvedWinner []string

	WinTier                    string
	WinRound, WinSlot, WinTeam int

	LoseTier                      string
	LoseRound, LoseSlot, LoseTeam int
}

type DoubleElimPlan struct {
	Size     int
	WBRounds int
	LBRounds int
	Matches  []*DEMatch
}

// lbMatchCount returns the number of LB matches in losers round lr (1-indexed)
// for the full power-of-two structure (before byes collapse any).
func lbMatchCount(n, lr int) int {
	i := (lr + 1) / 2 // rounds (2i-1, 2i) both have n/2^(i+1)
	return n / (1 << (i + 1))
}

// tok is a slot's occupant as entrants flow up the losers tree: a bye, an unset
// slot, or a real entrant tagged with the feed that produces it (a WB match's
// loser, or an emitted LB match's winner) so that feed can be wired on arrival.
type tok struct {
	bye    bool
	origin string // "" (unset) | "wloser" | "lbwinner"
	r, s   int    // producer coords: WB match (wloser) or emitted LB match (lbwinner)
}

func (t tok) real() bool { return !t.bye && t.origin != "" }

// GenerateDoubleElim builds a double-elimination plan from best-seed-first sides.
func GenerateDoubleElim(seededSides [][]string) DoubleElimPlan {
	wb := GenerateBracket(seededSides)
	n := wb.Size
	k := wb.Rounds
	lbRounds := 0
	if k >= 2 {
		lbRounds = 2 * (k - 1)
	}

	wbByKey := make(map[[2]int]*DEMatch)
	var matches []*DEMatch

	// --- Winners bracket: reuse the single-elim structure, tag tier=winners,
	// route the WB final winner to the grand final. Loser drops are wired by the
	// token flow below. ---
	for _, m := range wb.Matches {
		de := &DEMatch{
			Tier: "winners", Round: m.Round, Slot: m.Slot,
			Side1: m.Side1, Side2: m.Side2, ResolvedWinner: m.ResolvedWinner,
		}
		if m.FeedsRound != 0 {
			de.WinTier, de.WinRound, de.WinSlot, de.WinTeam = "winners", m.FeedsRound, m.FeedsSlot, m.FeedsTeam
		} else {
			de.WinTier, de.WinRound, de.WinSlot, de.WinTeam = "grand_final", 1, 0, 1
		}
		wbByKey[[2]int{m.Round, m.Slot}] = de
		matches = append(matches, de)
	}

	// n=2: no losers bracket — the only match's loser drops straight to the grand
	// final (and can still win it via the reset).
	if k == 1 {
		if m := wbByKey[[2]int{1, 0}]; m != nil && m.ResolvedWinner == nil {
			m.LoseTier, m.LoseRound, m.LoseSlot, m.LoseTeam = "grand_final", 1, 0, 2
		}
		matches = append(matches, &DEMatch{Tier: "grand_final", Round: 1, Slot: 0})
		return DoubleElimPlan{Size: n, WBRounds: k, LBRounds: 0, Matches: matches}
	}

	// --- Losers bracket via bye-aware token flow. ---
	w1bye := func(slot int) bool {
		m := wbByKey[[2]int{1, slot}]
		return m != nil && m.ResolvedWinner != nil
	}
	wloserTok := func(round, slot int) tok {
		if round == 1 && w1bye(slot) {
			return tok{bye: true} // a bye has no loser to drop
		}
		return tok{origin: "wloser", r: round, s: slot}
	}

	incoming := make(map[[2]int][2]tok)
	setIn := func(lr, slot, team int, t tok) {
		key := [2]int{lr, slot}
		v := incoming[key]
		v[team-1] = t
		incoming[key] = v
	}
	// Seed LB round 1 from W1 losers, and pre-inject every major round's dropped
	// WB loser (team 2, reversed for come-around). WB rounds >= 2 never have byes.
	for lr := 1; lr <= lbRounds; lr++ {
		cnt := lbMatchCount(n, lr)
		if lr == 1 {
			for j := 0; j < cnt; j++ {
				setIn(1, j, 1, wloserTok(1, 2*j))
				setIn(1, j, 2, wloserTok(1, 2*j+1))
			}
		} else if lr%2 == 0 {
			r := lr/2 + 1 // major round 2(r-1) takes W r losers
			for j := 0; j < cnt; j++ {
				// Come-around: alternate the drop permutation per major round —
				// REVERSED on even WB rounds, HALF-SHIFTED on odd — instead of
				// reversing every time. Reversing twice in a row lands a WB loser
				// back on the LB path carrying its own earlier victims; the
				// alternation sends it to the quarter it has met least (measured:
				// halves the deterministic-sim rematch count at 16+ fields).
				src := cnt - 1 - j
				if r%2 == 1 && cnt > 1 {
					src = (j + cnt/2) % cnt
				}
				setIn(lr, j, 2, wloserTok(r, src))
			}
		}
	}

	lbEmitted := make(map[[2]int]*DEMatch)
	// wire connects a token's producing feed to a target slot.
	wire := func(t tok, tier string, round, slot, team int) {
		switch t.origin {
		case "wloser":
			if m := wbByKey[[2]int{t.r, t.s}]; m != nil {
				m.LoseTier, m.LoseRound, m.LoseSlot, m.LoseTeam = tier, round, slot, team
			}
		case "lbwinner":
			if m := lbEmitted[[2]int{t.r, t.s}]; m != nil {
				m.WinTier, m.WinRound, m.WinSlot, m.WinTeam = tier, round, slot, team
			}
		}
	}

	for lr := 1; lr <= lbRounds; lr++ {
		cnt := lbMatchCount(n, lr)
		for j := 0; j < cnt; j++ {
			in := incoming[[2]int{lr, j}]
			t1, t2 := in[0], in[1]
			var out tok
			switch {
			case t1.real() && t2.real():
				de := &DEMatch{Tier: "losers", Round: lr, Slot: j}
				lbEmitted[[2]int{lr, j}] = de
				matches = append(matches, de)
				wire(t1, "losers", lr, j, 1)
				wire(t2, "losers", lr, j, 2)
				out = tok{origin: "lbwinner", r: lr, s: j}
			case t1.real():
				out = t1 // lone entrant skips the bye
			case t2.real():
				out = t2
			default:
				out = tok{bye: true}
			}
			// Route the match's output to its destination.
			switch {
			case lr == lbRounds:
				if out.real() {
					wire(out, "grand_final", 1, 0, 2) // LB champion -> GF team 2
				}
			case lr%2 == 1: // minor winner -> next (major) round, same slot, team 1
				setIn(lr+1, j, 1, out)
			default: // major winner -> next (minor) round, paired
				setIn(lr+1, j/2, j%2+1, out)
			}
		}
	}

	matches = append(matches, &DEMatch{Tier: "grand_final", Round: 1, Slot: 0})

	// Byes can leave whole LB rounds empty — renumber emitted LB rounds to a
	// contiguous 1..N so the bracket UI draws no gap columns.
	renumberLBRounds(matches)

	return DoubleElimPlan{Size: n, WBRounds: k, LBRounds: lbRoundCount(matches), Matches: matches}
}

// renumberLBRounds compacts the emitted losers rounds (and every feed that
// targets the losers tier) to a contiguous 1..N.
func renumberLBRounds(matches []*DEMatch) {
	present := map[int]bool{}
	for _, m := range matches {
		if m.Tier == "losers" {
			present[m.Round] = true
		}
	}
	if len(present) == 0 {
		return
	}
	rounds := make([]int, 0, len(present))
	for r := range present {
		rounds = append(rounds, r)
	}
	sort.Ints(rounds)
	remap := make(map[int]int, len(rounds))
	for i, r := range rounds {
		remap[r] = i + 1
	}
	for _, m := range matches {
		if m.Tier == "losers" {
			m.Round = remap[m.Round]
		}
		if m.WinTier == "losers" {
			m.WinRound = remap[m.WinRound]
		}
		if m.LoseTier == "losers" {
			m.LoseRound = remap[m.LoseRound]
		}
	}
}

func lbRoundCount(matches []*DEMatch) int {
	max := 0
	for _, m := range matches {
		if m.Tier == "losers" && m.Round > max {
			max = m.Round
		}
	}
	return max
}
