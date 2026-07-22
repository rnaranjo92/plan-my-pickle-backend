package engine

// Rotation ("up and down the river" / king-of-the-court) — a LIVE, timed session
// format. Players are seeded onto numbered courts (court 1 = top). Each timed
// round, the two teams on a court play; when the round ends the winning team
// moves UP one court and the losing team moves DOWN one, then everyone re-pairs
// with a new partner. Court 1 winners stay at the top; the last court's losers
// stay at the bottom. This file is the PURE movement engine (no DB/timers) so it
// can be unit-tested exhaustively; the session/timer/live-view layer calls it.

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
// self-rating) onto floor(n/4) FULL courts of 4, top court first. Up-and-down-
// the-river needs a perfect 4:1 player:court ratio, so any remainder (n mod 4)
// is NOT seated — a partial trailing court would break the up/down re-pair
// (phantom empty seats). The session layer enforces a multiple of 4 before
// calling this (the extras sit out), so in practice there's no remainder.
// Players seed as [0,2] vs [1,3] within each court for the opening round.
func SeedCourts(players []string) []RotCourt {
	courts := []RotCourt{}
	for i := 0; i+4 <= len(players); i += 4 {
		courts = append(courts, RotCourt{
			Court: len(courts) + 1,
			TeamA: [2]string{players[i], players[i+2]},
			TeamB: [2]string{players[i+1], players[i+3]},
		})
	}
	return courts
}

// NextRound applies one round's results and returns the next round's courts.
// Movement: winners go up a court, losers go down; court 1 winners and the last
// court's losers stay. Each destination court's four players re-pair so nobody
// keeps their partner (the "split + new partner" rule): the two who arrive from
// BELOW (winners moving up) each pair with one who arrives from ABOVE (losers
// moving down). Courts must be contiguous 1..N with exactly 4 players each.
func NextRound(courts []RotCourt, results []RotResult) []RotCourt {
	n := len(courts)
	if n == 0 {
		return nil
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

	next := make([]RotCourt, n)
	for k := 1; k <= n; k++ {
		// Who arrives at court k:
		//  - from ABOVE (court k-1 losers moving down) — or, at the top, court 1's
		//    own winners who stay.
		//  - from BELOW (court k+1 winners moving up) — or, at the bottom, court n's
		//    own losers who stay.
		var fromAbove, fromBelow [2]string
		if k == 1 {
			fromAbove = winners[1] // court 1 winners stay at the top
		} else {
			fromAbove = losers[k-1] // court k-1 losers drop into k
		}
		if k == n {
			fromBelow = losers[n] // last court losers stay at the bottom
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
	return next
}
