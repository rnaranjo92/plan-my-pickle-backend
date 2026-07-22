package engine

// Rotation ("up and down the river" / king-of-the-court) — a LIVE, timed session
// format. Players are seeded onto numbered courts (court 1 = top). Each timed
// round, the two teams on a court play; when the round ends the winning team
// moves UP one court and the losing team moves DOWN one, then everyone re-pairs
// with a new partner. Court 1 winners stay at the top; the last court's losers
// stay at the bottom. This file is the PURE movement engine (no DB/timers) so it
// can be unit-tested exhaustively; the session/timer/live-view layer calls it.
//
// COURT CAP + BYES: a venue has a fixed number of courts. When there are more
// players than seats (courts×4), the extras wait on a BENCH (a FIFO queue). Byes
// rotate through the BOTTOM court and are game-driven: the bottom court's losers
// step off to the back of the bench, and the longest-waiting bench players take
// their seats. So you only sit out after LOSING on the bottom court, and you're
// always the last to be re-benched — fair playing time over the session.

// RotCourt is the four players on one court, as two teams (a vs b). Each team is
// a pair of player ids (entrant ids). Court number is 1-based (1 = top).
type RotCourt struct {
	Court int
	// TeamA / TeamB are the two pairs currently playing on this court.
	TeamA [2]string
	TeamB [2]string
}

// RotResult reports which team won a court's round: "a", "b", or "tie". A tie is
// resolved by a single point in real life; the caller passes the point winner as
// "a"/"b" (never "tie" reaches the engine as a final outcome).
type RotResult struct {
	Court  int
	Winner string // "a" | "b"
}

// SeedCourts places players (already ordered strongest→weakest, e.g. by
// self-rating) onto up to maxCourts FULL courts of 4, top court first, and
// returns any remainder as the initial bench (they wait, then rotate in). The
// court count is min(maxCourts, floor(n/4)); maxCourts <= 0 means "no cap" (seat
// every full court). Players seed as [0,2] vs [1,3] within each court for the
// opening round; the bench keeps its (rating) order so the lowest-seeded wait
// first, then FIFO rotation evens it out.
func SeedCourts(players []string, maxCourts int) ([]RotCourt, []string) {
	full := len(players) / 4
	c := full
	if maxCourts > 0 && maxCourts < c {
		c = maxCourts
	}
	seats := c * 4
	courts := make([]RotCourt, 0, c)
	for k := 0; k < c; k++ {
		i := k * 4
		courts = append(courts, RotCourt{
			Court: k + 1,
			TeamA: [2]string{players[i], players[i+2]},
			TeamB: [2]string{players[i+1], players[i+3]},
		})
	}
	bench := append([]string(nil), players[seats:]...)
	return courts, bench
}

// NextRound applies one round's results and returns the next round's courts.
// Movement: winners go up a court, losers go down; court 1 winners and the last
// court's losers stay. Each destination court's four players re-pair so nobody
// keeps their partner (the "split + new partner" rule): the two who arrive from
// BELOW (winners moving up) each pair with one who arrives from ABOVE (losers
// moving down). Courts must be contiguous 1..N with exactly 4 players each.
//
// BYES: when `bench` is non-empty, the bottom court's losers step OFF (to the
// back of the bench) and the same number of longest-waiting bench players step
// ON, filling the bottom court. Up to 2 rotate per round (a losing pair's worth).
// Returns the next courts and the next bench (FIFO). An empty bench reproduces
// the classic no-bye behavior exactly.
func NextRound(courts []RotCourt, results []RotResult, bench []string) ([]RotCourt, []string) {
	n := len(courts)
	if n == 0 {
		return nil, append([]string(nil), bench...)
	}
	win := map[int]string{} // court → winning result ("a"/"b")
	for _, r := range results {
		win[r.Court] = r.Winner
	}

	// winnersUp[k]   = the pair that WON on court k (they move up to k-1, or stay if k==1)
	// losersDown[k]  = the pair that LOST on court k (they move down to k+1, or stay if k==n)
	winners := map[int][2]string{}
	losers := map[int][2]string{}
	for _, c := range courts {
		w := win[c.Court]
		if w == "b" {
			winners[c.Court] = c.TeamB
			losers[c.Court] = c.TeamA
		} else {
			// default + "a": team A won (also the safe default if unreported)
			winners[c.Court] = c.TeamA
			losers[c.Court] = c.TeamB
		}
	}

	// Bye swap on the bottom court: k = how many losers step off (0..2, capped by
	// the bench size). The stepped-off players go to the back of the bench; the
	// front-of-bench players fill the bottom court's "stay" slots.
	nextBench := append([]string(nil), bench...)
	bottomStay := losers[n] // who would normally stay at the bottom
	if k := min2(len(bench)); k > 0 {
		comingIn := append([]string(nil), bench[:k]...) // longest-waiting (FIFO front)
		// New bench = the rest, then the newly-benched bottom losers (back).
		nb := append([]string(nil), bench[k:]...)
		for j := 0; j < k; j++ {
			nb = append(nb, losers[n][j])
		}
		nextBench = nb
		// Rebuild the bottom "stay" pair: incoming bench players, then any bottom
		// loser who didn't step off.
		var stay [2]string
		copy(stay[:], comingIn)
		for j := k; j < 2; j++ {
			stay[j] = losers[n][j]
		}
		bottomStay = stay
	}

	next := make([]RotCourt, n)
	for k := 1; k <= n; k++ {
		// Who arrives at court k:
		//  - from ABOVE (court k-1 losers moving down) — or, at the top, court 1's
		//    own winners who stay.
		//  - from BELOW (court k+1 winners moving up) — or, at the bottom, the
		//    "stay" pair (bottom losers, possibly swapped with bench players).
		var fromAbove, fromBelow [2]string
		if k == 1 {
			fromAbove = winners[1] // court 1 winners stay at the top
		} else {
			fromAbove = losers[k-1] // court k-1 losers drop into k
		}
		if k == n {
			fromBelow = bottomStay // bottom stayers (after any bye swap)
		} else {
			fromBelow = winners[k+1] // court k+1 winners climb into k
		}
		// Re-pair so partners change: each "from below" pairs with one "from above".
		next[k-1] = RotCourt{
			Court: k,
			TeamA: [2]string{fromBelow[0], fromAbove[0]},
			TeamB: [2]string{fromBelow[1], fromAbove[1]},
		}
	}
	return next, nextBench
}

// min2 caps a bench size to the at-most-2 players that rotate per round.
func min2(n int) int {
	if n < 2 {
		return n
	}
	return 2
}
