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
	// 8 players → 2 courts of 4, seeded [0,2] vs [1,3] per court.
	players := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"}
	courts := SeedCourts(players)
	if len(courts) != 2 {
		t.Fatalf("want 2 courts, got %d", len(courts))
	}
	if courts[0].Court != 1 || courts[1].Court != 2 {
		t.Fatalf("courts not numbered 1..2: %+v", courts)
	}
	// Court 1 holds the top 4.
	if courts[0].TeamA != [2]string{"p1", "p3"} || courts[0].TeamB != [2]string{"p2", "p4"} {
		t.Fatalf("court 1 seeding wrong: %+v", courts[0])
	}
	// Conservation: all 8 present, no dups.
	got := allPlayers(courts)
	if len(got) != 8 {
		t.Fatalf("want 8 players seeded, got %d (%v)", len(got), got)
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
	next := NextRound(courts, results)

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
	next := NextRound(courts, []RotResult{{Court: 1, Winner: "a"}})
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
