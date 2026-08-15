package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// The real 2-win block from the Women's Never-ending league, which is what
// prompted this: Amy (-19) sat above Joanie (-13) and Jenn (-4) on head-to-head,
// and nothing on the row explained why.
func ladderTie() []model.Standing {
	return []model.Standing{
		{PlayerID: "kay", FullName: "Kay Naranjo", Wins: 2, Losses: 4, PointDiff: -5, PointsFor: 47},
		{PlayerID: "amy", FullName: "Amy Robinson", Wins: 2, Losses: 5, PointDiff: -19, PointsFor: 46},
		{PlayerID: "joanie", FullName: "Joanie Volle", Wins: 2, Losses: 4, PointDiff: -13, PointsFor: 44},
		{PlayerID: "jenn", FullName: "Jenn Esh", Wins: 2, Losses: 5, PointDiff: -4, PointsFor: 55},
	}
}

// Amy beat both Joanie and Jenn; Joanie beat Jenn.
func ladderH2H() map[string]map[string]int {
	return map[string]map[string]int{
		"amy":    {"joanie": 1, "jenn": 1},
		"joanie": {"jenn": 1},
	}
}

func order(rows []model.Standing) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.PlayerID
	}
	return out
}

func TestLeagueTiebreakIsPointDifferential(t *testing.T) {
	got := order(rankStandingsDiffFirst(ladderTie(), ladderH2H(), true))
	want := []string{"jenn", "kay", "joanie", "amy"} // -4, -5, -13, -19
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("league order = %v, want %v (differential, best first)", got, want)
		}
	}
}

// Tournaments must be unchanged — USAP puts head-to-head first, and sanctioned
// events depend on it.
func TestTournamentTiebreakStillHeadToHead(t *testing.T) {
	got := order(rankStandings(ladderTie(), ladderH2H(), true))
	// Amy has 2 h2h wins in the tied group, Joanie 1, Kay and Jenn 0 — so Amy
	// leads despite the worst differential. That's the behaviour being kept.
	if got[0] != "amy" {
		t.Fatalf("tournament order = %v, want Amy first on head-to-head", got)
	}
	if got[1] != "joanie" {
		t.Fatalf("tournament order = %v, want Joanie second on head-to-head", got)
	}
}

// Differential first must not disturb the primary criterion.
func TestWinsStillOutrankDifferential(t *testing.T) {
	rows := []model.Standing{
		{PlayerID: "few-wins-great-diff", Wins: 1, PointDiff: 50},
		{PlayerID: "more-wins-poor-diff", Wins: 2, PointDiff: -30},
	}
	got := order(rankStandingsDiffFirst(rows, nil, true))
	if got[0] != "more-wins-poor-diff" {
		t.Fatalf("wins must still come first, got %v", got)
	}
}
