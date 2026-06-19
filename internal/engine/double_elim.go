package engine

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
// NOTE: this first cut targets a full power-of-two field (no WB byes). Byes are
// a follow-up (they leave LB slots unfilled and must be routed past, like the
// consolation generator).

// DEMatch is one node of a double-elim plan. Win* is where the winner advances;
// Lose* is where the loser advances (WB matches only). A "" tier feed means no
// feed (the grand final's winner, or the LB has no loser feed).
type DEMatch struct {
	Tier         string // "winners" | "losers" | "grand_final"
	Round, Slot  int
	Side1, Side2 []string // seeded (WB round 1) or nil (TBD); {Bye} for a bye
	ResolvedWinner []string

	WinTier              string
	WinRound, WinSlot, WinTeam int

	LoseTier               string
	LoseRound, LoseSlot, LoseTeam int
}

type DoubleElimPlan struct {
	Size    int
	WBRounds int
	LBRounds int
	Matches []*DEMatch
}

// lbMatchCount returns the number of LB matches in losers round lr (1-indexed).
func lbMatchCount(n, lr int) int {
	i := (lr + 1) / 2 // rounds (2i-1, 2i) both have n/2^(i+1)
	return n / (1 << (i + 1))
}

// GenerateDoubleElim builds a double-elimination plan from best-seed-first sides.
func GenerateDoubleElim(seededSides [][]string) DoubleElimPlan {
	wb := GenerateBracket(seededSides)
	n := wb.Size
	k := wb.Rounds
	lbRounds := 0
	if k >= 2 {
		lbRounds = 2 * (k - 1)
	}

	var matches []*DEMatch

	// --- Winners bracket: reuse the single-elim structure, tag tier=winners,
	// route the WB final winner to the grand final, and add loser drops. ---
	for _, m := range wb.Matches {
		de := &DEMatch{
			Tier: "winners", Round: m.Round, Slot: m.Slot,
			Side1: m.Side1, Side2: m.Side2, ResolvedWinner: m.ResolvedWinner,
		}
		if m.FeedsRound != 0 {
			de.WinTier, de.WinRound, de.WinSlot, de.WinTeam = "winners", m.FeedsRound, m.FeedsSlot, m.FeedsTeam
		} else {
			// WB final winner -> grand final team 1.
			de.WinTier, de.WinRound, de.WinSlot, de.WinTeam = "grand_final", 1, 0, 1
		}
		// Loser drop.
		switch {
		case k == 1:
			// n=2: no losers bracket — the only match's loser goes straight to
			// the grand final (and can still win it via the reset).
			de.LoseTier, de.LoseRound, de.LoseSlot, de.LoseTeam = "grand_final", 1, 0, 2
		case m.Round == 1:
			// W1 slot s loser -> LB round 1, match s/2, team s%2+1.
			de.LoseTier, de.LoseRound, de.LoseSlot, de.LoseTeam = "losers", 1, m.Slot/2, m.Slot%2+1
		default:
			// Wr (r>=2) loser -> major LB round 2(r-1), team 2, reversed slot.
			lr := 2 * (m.Round - 1)
			cnt := lbMatchCount(n, lr)
			de.LoseTier, de.LoseRound, de.LoseSlot, de.LoseTeam = "losers", lr, cnt-1-m.Slot, 2
		}
		matches = append(matches, de)
	}

	// --- Losers bracket. ---
	for lr := 1; lr <= lbRounds; lr++ {
		cnt := lbMatchCount(n, lr)
		for slot := 0; slot < cnt; slot++ {
			de := &DEMatch{Tier: "losers", Round: lr, Slot: slot}
			if lr == lbRounds {
				// LB final winner -> grand final team 2.
				de.WinTier, de.WinRound, de.WinSlot, de.WinTeam = "grand_final", 1, 0, 2
			} else if lr%2 == 1 {
				// Minor round winner -> next (major) round, same slot, team 1.
				de.WinTier, de.WinRound, de.WinSlot, de.WinTeam = "losers", lr+1, slot, 1
			} else {
				// Major round winner -> next (minor) round, paired.
				de.WinTier, de.WinRound, de.WinSlot, de.WinTeam = "losers", lr+1, slot/2, slot%2+1
			}
			matches = append(matches, de)
		}
	}

	// --- Grand final (single game here; the reset is created at score time if
	// the LB champion wins). ---
	if k >= 1 {
		matches = append(matches, &DEMatch{Tier: "grand_final", Round: 1, Slot: 0})
	}

	return DoubleElimPlan{Size: n, WBRounds: k, LBRounds: lbRounds, Matches: matches}
}
