package engine

import "sort"

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
// their seats. So you only sit out after LOSING on the bottom court.
//
// FAIRNESS, HONESTLY: the bench is FIFO, so bench ORDER is fair — you are always
// the last to be re-benched. But that is not the same as fair playing TIME, and
// this comment used to claim it was. Because you re-enter onto the BOTTOM court,
// where you are most likely to lose again, sit-outs concentrate on whoever keeps
// losing. Simulated over a 3-hour night with 24 players on 4 courts and results
// that track skill, the weakest quartile sat out ~65% of rounds and the
// strongest ~3.5% — an 18× gap; with randomly-decided games the spread is nearly
// flat, so this only bites when results correlate with ability, i.e. always.
// That is a property of the RULE (Michelle's "whoever loses at the bottom court
// sits out"), not a bug in this code, and changing it is a product decision.

// RotCourt is the four players on one court, as two teams (a vs b). Each team is
// a pair of player ids (entrant ids). Court number is 1-based (1 = top).
type RotCourt struct {
	Court int
	// TeamA / TeamB are the two pairs currently playing on this court.
	TeamA [2]string
	TeamB [2]string
}

// LoserMode decides where a LOSING pair goes, chosen when the ladder is created
// and fixed for the session.
//
//	LosersDown — the classic river: losers drop one court, the bottom court's
//	             losers stay (and are the ones who rotate off to the bench).
//	LosersStay — losers hold their court, and only winners climb. The TOP court's
//	             losers fall all the way to the bottom, which is what keeps every
//	             court at four: court 1 takes its own winners plus the climbers
//	             from court 2, each middle court takes its own losers plus the
//	             climbers from below, and the bottom takes its own losers plus
//	             the pair dropped from the top.
//
// With ONE court the two modes are identical (nowhere to move), and with TWO
// "all the way to the bottom" and "down one" are the same court — so they behave
// the same there too.
type LoserMode int

const (
	LosersDown LoserMode = iota
	LosersStay
)

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

// Seat is one player waiting to be placed for round 1, with the court the
// organizer put them on. Court 0 means unplaced — seed them by rating.
type Seat struct {
	ID    string
	Court int
}

// SeedPlacedCourts builds round 1 from EXPLICIT court placements, falling back
// to rating order for whoever wasn't placed.
//
// This exists because sorting the roster by court number and chunking it into
// fours -- the obvious implementation -- does not place anybody. It only ranks
// them: place two players on court 3, leave the rest unplaced, and those two
// sort to the front and open the TOP court. The number the organizer typed has
// to survive as a destination, not a sort key.
//
// `players` must arrive in rating order (strongest first); that order decides
// which unplaced player fills which remaining seat, and which placed player
// keeps their seat when a court is oversubscribed.
//
// Court count is min(maxCourts, floor(n/4)) exactly as SeedCourts, so the room
// never seats more courts than it has players for. Two consequences the caller
// should expect, both deliberate:
//
//   - A court placed BEYOND the last real court can't be honoured; those players
//     join the unplaced pool rather than vanishing.
//   - A court with more than four placed players keeps the first four by rating
//     and returns the rest to the pool. The alternative -- spilling them onto the
//     next court -- silently shifts everyone below.
func SeedPlacedCourts(players []Seat, maxCourts int) ([]RotCourt, []string) {
	c := len(players) / 4
	if maxCourts > 0 && maxCourts < c {
		c = maxCourts
	}
	if c == 0 {
		ids := make([]string, 0, len(players))
		for _, p := range players {
			ids = append(ids, p.ID)
		}
		return []RotCourt{}, ids
	}

	seats := make([][]string, c+1) // 1-based; index 0 unused
	var pool, overflow []string
	for _, p := range players {
		if p.Court >= 1 && p.Court <= c && len(seats[p.Court]) < 4 {
			seats[p.Court] = append(seats[p.Court], p.ID)
			continue
		}
		if p.Court >= 1 {
			// Placed somewhere we can't honour: a court that isn't running
			// tonight, or a court the organizer put five people on. They go
			// BEHIND the unplaced players, because the one thing we know is that
			// the organizer did NOT ask for them to be up top. Putting them in
			// the ordinary pool let an evicted or out-of-range player open court
			// 1 — the exact bug this function was written to kill, wearing a
			// different hat.
			overflow = append(overflow, p.ID)
			continue
		}
		pool = append(pool, p.ID)
	}
	pool = append(pool, overflow...)

	// Fill the gaps top court first, so the strongest UNPLACED players land
	// highest — the same intent as the rating seed they're falling back to.
	next := 0
	for k := 1; k <= c; k++ {
		for len(seats[k]) < 4 && next < len(pool) {
			seats[k] = append(seats[k], pool[next])
			next++
		}
	}

	courts := make([]RotCourt, 0, c)
	for k := 1; k <= c; k++ {
		q := seats[k]
		courts = append(courts, RotCourt{
			Court: k,
			TeamA: [2]string{q[0], q[2]},
			TeamB: [2]string{q[1], q[3]},
		})
	}
	return courts, append([]string(nil), pool[next:]...)
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
func NextRound(courts []RotCourt, results []RotResult, bench []string, mode LoserMode) ([]RotCourt, []string) {
	n := len(courts)
	// The movement math assumes contiguous courts 1..n with four real players
	// each. Violate either and it doesn't degrade — it ERASES people: courts
	// numbered [1,3] produce two courts of empty seats with all eight players
	// gone, and a single blank seat becomes a phantom that holds a slot all night
	// while a real player is benched every round. Nothing reaches here that way
	// today; this makes sure nothing ever can.
	seen := make(map[string]bool, n*4)
	for i, c := range courts {
		if c.Court != i+1 {
			return append([]RotCourt(nil), courts...), append([]string(nil), bench...)
		}
		for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			// Blank seats become permanent phantoms; a duplicated id means one
			// person on two courts and a real player silently missing. Neither
			// self-heals — a duplicate survives every subsequent round.
			if id == "" || seen[id] {
				return append([]RotCourt(nil), courts...), append([]string(nil), bench...)
			}
			seen[id] = true
		}
	}
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
		if mode == LosersStay && n > 1 {
			// Only winners climb. Losers hold their court, except at the top,
			// where they fall to the bottom.
			switch k {
			case 1:
				fromAbove = winners[1] // winners stay at the top
				fromBelow = winners[2] // climbers from court 2
			case n:
				fromAbove = losers[1]  // the top court's losers land here
				fromBelow = bottomStay // this court's own losers (after any bye swap)
			default:
				fromAbove = losers[k] // own losers hold this court
				fromBelow = winners[k+1]
			}
		} else {
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

// NextRoundFair is NextRound with an equal-court-time pass on top.
//
// THE PROBLEM IT SOLVES. In the plain river only the BOTTOM court's losers ever
// step off, so sitting out is a punishment for losing rather than a turn that
// comes round. A player who keeps winning climbs and never reaches the bottom
// court, so never sits at all; a player who keeps losing sinks and cycles the
// bench every few rounds. Measured over a long night that is an ~18x gap
// between the quartile that sits most and the quartile that sits least — on 40
// players and 4 courts, close to two hours of waiting for some and none for
// others.
//
// WHAT IT DOES. After the normal movement, up to two of the players with the
// MOST games swap seats with up to two of the players who have sat the most.
// An incoming player takes the exact seat of the player they replace, so courts
// stay full, contiguous and four-a-side — the movement math above is untouched,
// and this only exchanges identities between a seat and the bench.
//
// THE TRADE, STATED PLAINLY. This loosens the river: a rested player can come
// back onto a higher court than they left, because they take the seat of
// whoever had played most. That is the point — it is the cost of equal court
// time, and it was chosen deliberately over "winners keep playing".
//
// Bounded to two swaps per round on purpose. Equalising in one go would
// reshuffle the whole field every round and stop the night resembling a ladder
// at all; two per round matches the existing two-in/two-out cadence and still
// converges, because every swap strictly moves a more-played player off and a
// less-played player on.
//
// played is games played BEFORE this round, by player id. Players missing from
// the map count as zero, so a walk-up who just joined is treated as owed court
// time rather than as having had it.
func NextRoundFair(courts []RotCourt, results []RotResult, bench []string,
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

	// Every seat on court, most-played first. Ties keep court order, so the swap
	// is deterministic — a night that reshuffles differently on each poll would
	// be unusable.
	type seatRef struct {
		court, idx int // idx 0..3 across TeamA[0],TeamA[1],TeamB[0],TeamB[1]
		id         string
	}
	seats := make([]seatRef, 0, len(next)*4)
	for ci := range next {
		ids := [4]string{
			next[ci].TeamA[0], next[ci].TeamA[1],
			next[ci].TeamB[0], next[ci].TeamB[1],
		}
		for i, id := range ids {
			seats = append(seats, seatRef{court: ci, idx: i, id: id})
		}
	}
	sort.SliceStable(seats, func(a, b int) bool {
		return plays(seats[a].id) > plays(seats[b].id)
	})

	// Bench, longest-waiting first. Same stable ordering rule.
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

	swaps := min2(len(nextBench))
	for i := 0; i < swaps && i < len(seats) && i < len(benchOrder); i++ {
		seat := seats[i]
		bi := benchOrder[i]
		onCourt, waiting := seat.id, nextBench[bi]
		// Only swap while it actually narrows the gap. Equal counts are left
		// alone: swapping them churns the courts every round for no fairness
		// gain, and churn is what makes a night feel random.
		if plays(waiting) >= plays(onCourt) {
			break
		}
		setSeat(seat, waiting)
		nextBench[bi] = onCourt
	}
	return next, nextBench
}
