package engine

// Bye is the sentinel for an empty bracket slot a real side advances past.
const Bye = "__BYE__"

func IsBye(side []string) bool { return len(side) == 1 && side[0] == Bye }

// BracketMatchSpec is one node in a single-elimination bracket. A side is the
// side's player ids (nil = TBD, {Bye} = a bye). FeedsRound == 0 means no feed
// (the FINAL).
type BracketMatchSpec struct {
	Round, Slot                      int
	Side1, Side2                     []string
	FeedsRound, FeedsSlot, FeedsTeam int
	ResolvedWinner                   []string
}

type BracketPlan struct {
	Size    int // bracket size (power of 2)
	Rounds  int // number of rounds, FINAL included
	Matches []*BracketMatchSpec
}

// GenerateBracket builds a seeded single-elimination bracket. seededSides is
// ordered best-seed-first; each entry is a side's player ids.
func GenerateBracket(seededSides [][]string) BracketPlan {
	realCount := len(seededSides)
	n := nextPow2(max(2, realCount))
	totalRounds := ilog2(n)
	order := seedOrder(n)

	sideForSeed := func(s int) []string {
		if s-1 < realCount {
			return seededSides[s-1]
		}
		return []string{Bye}
	}

	rounds := map[int][]*BracketMatchSpec{}
	var r1 []*BracketMatchSpec
	for i := 0; i < n/2; i++ {
		m := &BracketMatchSpec{Round: 1, Slot: i}
		m.Side1 = sideForSeed(order[2*i])
		m.Side2 = sideForSeed(order[2*i+1])
		r1 = append(r1, m)
	}
	rounds[1] = r1
	for r := 2; r <= totalRounds; r++ {
		cnt := n / (1 << r)
		var rs []*BracketMatchSpec
		for i := 0; i < cnt; i++ {
			rs = append(rs, &BracketMatchSpec{Round: r, Slot: i})
		}
		rounds[r] = rs
	}

	// feed pointers
	for r := 1; r < totalRounds; r++ {
		for _, m := range rounds[r] {
			m.FeedsRound = r + 1
			m.FeedsSlot = m.Slot / 2
			m.FeedsTeam = m.Slot%2 + 1
		}
	}

	// resolve round-1 byes (real side auto-advances)
	for _, m := range rounds[1] {
		if IsBye(m.Side1) && m.Side2 != nil && !IsBye(m.Side2) {
			advance(rounds, m, m.Side2)
		} else if IsBye(m.Side2) && m.Side1 != nil && !IsBye(m.Side1) {
			advance(rounds, m, m.Side1)
		}
	}

	var all []*BracketMatchSpec
	for r := 1; r <= totalRounds; r++ {
		all = append(all, rounds[r]...)
	}
	return BracketPlan{Size: n, Rounds: totalRounds, Matches: all}
}

func advance(rounds map[int][]*BracketMatchSpec, m *BracketMatchSpec, winner []string) {
	m.ResolvedWinner = winner
	if m.FeedsRound == 0 {
		return
	}
	target := rounds[m.FeedsRound][m.FeedsSlot]
	if m.FeedsTeam == 1 {
		target.Side1 = winner
	} else {
		target.Side2 = winner
	}
	if IsBye(target.Side1) && target.Side2 != nil && !IsBye(target.Side2) {
		advance(rounds, target, target.Side2)
	} else if IsBye(target.Side2) && target.Side1 != nil && !IsBye(target.Side1) {
		advance(rounds, target, target.Side1)
	}
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p *= 2
	}
	return p
}

func ilog2(n int) int {
	k := 0
	for v := n; v > 1; v >>= 1 {
		k++
	}
	return k
}

func seedOrder(n int) []int {
	order := []int{1}
	for len(order) < n {
		length := len(order) * 2
		var next []int
		for _, r := range order {
			next = append(next, r, length+1-r)
		}
		order = next
	}
	return order
}
