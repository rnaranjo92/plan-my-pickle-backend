package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// TestComputeTeamStandings exercises the pure Team-League standings computation:
// it tallies each team's wins/losses from the recorded fixtures, computes win %,
// and orders by wins DESC then win % DESC then name ASC. It covers a clean W-L
// spread, a win-% tie that wins must break, a wins tie that win % must break,
// and a team with NO fixtures (must appear at 0-0, 0%).
func TestComputeTeamStandings(t *testing.T) {
	team := func(id, name string) model.Team {
		return model.Team{ID: id, Name: name}
	}
	fixture := func(a, b, winner string) model.TeamFixture {
		return model.TeamFixture{TeamAID: a, TeamBID: b, WinnerTeamID: winner}
	}

	t.Run("wins ordering and win pct", func(t *testing.T) {
		teams := []model.Team{
			team("a", "Aces"),
			team("b", "Bandits"),
			team("c", "Crushers"),
		}
		// A beats B, A beats C, B beats C  →  A 2-0, B 1-1, C 0-2.
		fixtures := []model.TeamFixture{
			fixture("a", "b", "a"),
			fixture("a", "c", "a"),
			fixture("b", "c", "b"),
		}
		got := computeTeamStandings(teams, fixtures)

		wantOrder := []string{"a", "b", "c"}
		if len(got) != len(wantOrder) {
			t.Fatalf("got %d standings, want %d", len(got), len(wantOrder))
		}
		for i, id := range wantOrder {
			if got[i].TeamID != id {
				t.Fatalf("position %d = %q, want %q (order %+v)", i, got[i].TeamID, id, got)
			}
		}
		// A: 2-0 (100%).
		if got[0].Wins != 2 || got[0].Losses != 0 || got[0].Played != 2 || got[0].WinPct != 1.0 {
			t.Fatalf("A standing = %+v, want 2-0 played 2 winPct 1.0", got[0])
		}
		// B: 1-1 (50%).
		if got[1].Wins != 1 || got[1].Losses != 1 || got[1].Played != 2 || got[1].WinPct != 0.5 {
			t.Fatalf("B standing = %+v, want 1-1 played 2 winPct 0.5", got[1])
		}
		// C: 0-2 (0%).
		if got[2].Wins != 0 || got[2].Losses != 2 || got[2].Played != 2 || got[2].WinPct != 0.0 {
			t.Fatalf("C standing = %+v, want 0-2 played 2 winPct 0.0", got[2])
		}
	})

	t.Run("team with no fixtures appears 0-0", func(t *testing.T) {
		teams := []model.Team{
			team("a", "Aces"),
			team("b", "Bandits"),
			team("z", "Zephyrs"), // never plays
		}
		fixtures := []model.TeamFixture{
			fixture("a", "b", "a"), // A 1-0, B 0-1
		}
		got := computeTeamStandings(teams, fixtures)

		// A (1 win) leads; then the two 0-win teams ordered by name: Bandits, Zephyrs.
		if got[0].TeamID != "a" {
			t.Fatalf("leader = %q, want a", got[0].TeamID)
		}
		var z *model.TeamStanding
		for i := range got {
			if got[i].TeamID == "z" {
				z = &got[i]
			}
		}
		if z == nil {
			t.Fatalf("team z (no fixtures) missing from standings: %+v", got)
		}
		if z.Wins != 0 || z.Losses != 0 || z.Played != 0 || z.WinPct != 0.0 {
			t.Fatalf("z standing = %+v, want 0-0 played 0 winPct 0.0", *z)
		}
		// Among the two 0-win teams, name asc → Bandits (b) before Zephyrs (z).
		if got[1].TeamID != "b" || got[2].TeamID != "z" {
			t.Fatalf("0-win order = [%q,%q], want [b,z]", got[1].TeamID, got[2].TeamID)
		}
	})

	t.Run("win pct breaks a wins tie? no — wins dominates; win pct breaks equal wins", func(t *testing.T) {
		// Two teams with EQUAL wins (1 each) but different played counts → the
		// higher win % ranks first. h: 1-0 (100%), k: 1-1 (50%).
		teams := []model.Team{
			team("h", "Hawks"),
			team("k", "Kings"),
			team("m", "Mallards"),
		}
		fixtures := []model.TeamFixture{
			fixture("h", "k", "h"), // h 1-0, k 0-1
			fixture("k", "m", "k"), // k 1-1, m 0-1
		}
		got := computeTeamStandings(teams, fixtures)
		// h and k both have 1 win; h's win% (1.0) > k's (0.5) → h first.
		if got[0].TeamID != "h" || got[1].TeamID != "k" {
			t.Fatalf("tie order = [%q,%q], want [h,k] (%+v)", got[0].TeamID, got[1].TeamID, got)
		}
		if got[0].WinPct != 1.0 || got[1].WinPct != 0.5 {
			t.Fatalf("winPct = [%v,%v], want [1.0,0.5]", got[0].WinPct, got[1].WinPct)
		}
	})

	t.Run("exact win pct tie broken by name", func(t *testing.T) {
		// Two teams each 1-1 (50%) → ordered by name asc.
		teams := []model.Team{
			team("p", "Pumas"),
			team("o", "Otters"),
			team("x", "eXtras"),
		}
		fixtures := []model.TeamFixture{
			fixture("p", "x", "p"), // p 1-0
			fixture("o", "p", "o"), // o 1-0, p 1-1
			fixture("o", "x", "x"), // o 1-1, x 1-1
		}
		// Records: p 1-1, o 1-1, x 1-1 → all 50%. Name asc: Otters(o), Pumas(p), eXtras(x).
		got := computeTeamStandings(teams, fixtures)
		wantOrder := []string{"o", "p", "x"}
		for i, id := range wantOrder {
			if got[i].TeamID != id {
				t.Fatalf("name-tiebreak position %d = %q, want %q (%+v)", i, got[i].TeamID, id, got)
			}
		}
	})

	t.Run("empty inputs", func(t *testing.T) {
		got := computeTeamStandings(nil, nil)
		if len(got) != 0 {
			t.Fatalf("empty standings = %+v, want []", got)
		}
	})
}
