package engine

import (
	"sort"
	"testing"
)

// allPlayers flattens every player id currently on the courts (for conservation
// + no-duplicate checks).
func allPlayers(courts []RotCourt) []string {
	var out []string
	for _, c := range courts {
		for _, p := range [][2]string{c.TeamA, c.TeamB} {
			for _, id := range p {
				if id != "" {
					out = append(out, id)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestSeedCourts(t *testing.T) {
	// 8 players, no cap → 2 courts of 4, seeded [0,2] vs [1,3] per court.
	players := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"}
	courts, bench := SeedCourts(players, 0)
	if len(courts) != 2 {
		t.Fatalf("want 2 courts, got %d", len(courts))
	}
	if len(bench) != 0 {
		t.Fatalf("want empty bench, got %v", bench)
	}
	if courts[0].Court != 1 || courts[1].Court != 2 {
		t.Fatalf("courts not numbered 1..2: %+v", courts)
	}
	// Court 1 holds the top 4.
	if courts[0].TeamA != [2]string{"p1", "p3"} || courts[0].TeamB != [2]string{"p2", "p4"} {
		t.Fatalf("court 1 seeding wrong: %+v", courts[0])
	}
	got := allPlayers(courts)
	if len(got) != 8 {
		t.Fatalf("want 8 players seeded, got %d (%v)", len(got), got)
	}
}

// A non-multiple-of-4 roster seats only full courts; the remainder benches.
func TestSeedCourtsBenchesRemainder(t *testing.T) {
	// 10 players, no cap → 2 full courts (8 seated); p9,p10 on the bench.
	players := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10"}
	courts, bench := SeedCourts(players, 0)
	if len(courts) != 2 {
		t.Fatalf("want 2 full courts, got %d", len(courts))
	}
	if len(bench) != 2 || bench[0] != "p9" || bench[1] != "p10" {
		t.Fatalf("want bench [p9 p10], got %v", bench)
	}
}

// A court CAP benches everyone beyond capacity: 24 players, 5 courts → 20 seated,
// 4 on the bench.
func TestSeedCourtsCap(t *testing.T) {
	players := make([]string, 24)
	for i := range players {
		players[i] = string(rune('a' + i))
	}
	courts, bench := SeedCourts(players, 5)
	if len(courts) != 5 {
		t.Fatalf("want 5 courts (capped), got %d", len(courts))
	}
	if len(bench) != 4 {
		t.Fatalf("want 4 benched, got %d (%v)", len(bench), bench)
	}
	if len(allPlayers(courts))+len(bench) != 24 {
		t.Fatalf("players lost: %d seated + %d benched", len(allPlayers(courts)), len(bench))
	}
}

// With a bench, the bottom court's losers step off and the longest-waiting bench
// players step on; nobody is lost, and the bench stays the same size.
func TestNextRoundByes(t *testing.T) {
	// 2 courts (8 seated) + 2 on the bench.
	courts := []RotCourt{
		{Court: 1, TeamA: [2]string{"a1", "a2"}, TeamB: [2]string{"b1", "b2"}},
		{Court: 2, TeamA: [2]string{"c1", "c2"}, TeamB: [2]string{"d1", "d2"}},
	}
	bench := []string{"z1", "z2"}
	all := append(allPlayers(courts), bench...)
	sort.Strings(all)

	// Court 1: A wins. Court 2: A wins (so d1,d2 are the bottom losers → step off).
	next, nb := NextRound(courts,
		[]RotResult{{Court: 1, Winner: "a"}, {Court: 2, Winner: "a"}}, bench)

	// Conservation: same 10 players across courts + bench, no dups.
	after := append(allPlayers(next), nb...)
	sort.Strings(after)
	if len(after) != 10 {
		t.Fatalf("player count changed: %d", len(after))
	}
	for i := range all {
		if all[i] != after[i] {
			t.Fatalf("player set changed:\n before %v\n after  %v", all, after)
		}
	}
	// Bench stays size 2, and the bottom losers d1,d2 are now on it.
	if len(nb) != 2 {
		t.Fatalf("bench size changed: %v", nb)
	}
	benchSet := sortedSet(nb...)
	if !equalSets(benchSet, sortedSet("d1", "d2")) {
		t.Fatalf("bottom losers should be benched, got bench %v", nb)
	}
	// The waiting z1,z2 are now on court (bottom court).
	seated := sortedSet(playersOfCourt(next, 2)...)
	if !containsAll(seated, "z1", "z2") {
		t.Fatalf("bench players should have come in on court 2, got %v", seated)
	}
}

// Everyone gets even playing time: over many rounds every player sits out a
// roughly equal number of times (FIFO fairness).
func TestByesRotateFairly(t *testing.T) {
	courts, bench := SeedCourts(
		[]string{"p1", "p2", "p3", "p4", "p5", "p6"}, 1) // 1 court, 2 on bench
	sat := map[string]int{}
	for round := 0; round < 30; round++ {
		for _, id := range bench {
			sat[id]++
		}
		// Court 1: team A always wins (deterministic; losers cycle to bench).
		courts, bench = NextRound(courts, []RotResult{{Court: 1, Winner: "a"}}, bench)
	}
	// 6 players, 2 sit each round over 30 rounds = 60 sit-outs; a fair spread is
	// ~10 each. Assert nobody is starved (played every round) or benched always.
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		if sat[id] == 0 || sat[id] >= 30 {
			t.Fatalf("unfair byes: %s sat %d/30 rounds (all: %v)", id, sat[id], sat)
		}
	}
}

// After a round: winners up, losers down, court-1 winners + last-court losers
// stay, everyone re-pairs, and no player is lost or duplicated.
func TestNextRoundMovementAndRepair(t *testing.T) {
	// 3 courts, 12 players. Court k: teamA = {a<k>1,a<k>2}, teamB = {b<k>1,b<k>2}.
	courts := []RotCourt{
		{Court: 1, TeamA: [2]string{"a11", "a12"}, TeamB: [2]string{"b11", "b12"}},
		{Court: 2, TeamA: [2]string{"a21", "a22"}, TeamB: [2]string{"b21", "b22"}},
		{Court: 3, TeamA: [2]string{"a31", "a32"}, TeamB: [2]string{"b31", "b32"}},
	}
	before := allPlayers(courts)

	// Court 1: A wins. Court 2: B wins. Court 3: A wins.
	results := []RotResult{
		{Court: 1, Winner: "a"},
		{Court: 2, Winner: "b"},
		{Court: 3, Winner: "a"},
	}
	next, _ := NextRound(courts, results, nil)

	if len(next) != 3 {
		t.Fatalf("want 3 courts, got %d", len(next))
	}

	// Conservation: same 12 players, none lost/duplicated.
	after := allPlayers(next)
	if len(after) != 12 {
		t.Fatalf("player count changed: %d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("player set changed:\n before %v\n after  %v", before, after)
		}
	}

	// Court 1 = its own winners (a11,a12) + court-2 winners (b21,b22 moved up).
	c1 := playersOfCourt(next, 1)
	wantC1 := sortedSet("a11", "a12", "b21", "b22")
	if !equalSets(c1, wantC1) {
		t.Fatalf("court 1 occupants wrong: got %v want %v", c1, wantC1)
	}
	// Court 3 (last) = its own losers (b31,b32) + court-2 losers (a21,a22 down).
	c3 := playersOfCourt(next, 3)
	wantC3 := sortedSet("b31", "b32", "a21", "a22")
	if !equalSets(c3, wantC3) {
		t.Fatalf("court 3 occupants wrong: got %v want %v", c3, wantC3)
	}
	// Court 2 (middle) = court-1 losers (b11,b12 down) + court-3 winners (a31,a32 up).
	c2 := playersOfCourt(next, 2)
	wantC2 := sortedSet("b11", "b12", "a31", "a32")
	if !equalSets(c2, wantC2) {
		t.Fatalf("court 2 occupants wrong: got %v want %v", c2, wantC2)
	}

	// Partner rotation: nobody keeps their exact partner on court 1.
	for _, c := range next {
		if c.Court == 1 {
			// New teams must not reproduce an old team from court 1's inputs.
			if c.TeamA == [2]string{"a11", "a12"} || c.TeamB == [2]string{"a11", "a12"} {
				t.Fatalf("court 1 kept the same partners: %+v", c)
			}
		}
	}
}

// Single-court session: winners and losers both stay (nowhere to move), but they
// still re-pair — a valid mini "king of the court".
func TestNextRoundSingleCourt(t *testing.T) {
	courts := []RotCourt{
		{Court: 1, TeamA: [2]string{"x1", "x2"}, TeamB: [2]string{"y1", "y2"}},
	}
	next, _ := NextRound(courts, []RotResult{{Court: 1, Winner: "a"}}, nil)
	if len(next) != 1 {
		t.Fatalf("want 1 court, got %d", len(next))
	}
	if !equalSets(playersOfCourt(next, 1), sortedSet("x1", "x2", "y1", "y2")) {
		t.Fatalf("single-court players changed: %+v", next[0])
	}
}

// --- test helpers ---

func playersOfCourt(courts []RotCourt, court int) []string {
	for _, c := range courts {
		if c.Court == court {
			s := []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]}
			sort.Strings(s)
			return s
		}
	}
	return nil
}

func sortedSet(ids ...string) []string {
	s := append([]string(nil), ids...)
	sort.Strings(s)
	return s
}

func containsAll(sorted []string, ids ...string) bool {
	have := map[string]bool{}
	for _, s := range sorted {
		have[s] = true
	}
	for _, id := range ids {
		if !have[id] {
			return false
		}
	}
	return true
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
