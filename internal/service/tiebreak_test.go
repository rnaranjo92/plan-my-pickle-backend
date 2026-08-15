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

func TestLeagueRanksByWinPctThenDifferential(t *testing.T) {
	got := order(rankStandingsDiffFirst(ladderTie(), ladderH2H(), true))
	// Win% splits the old "2 wins" block by games played:
	//   Kay 2-4 = .333 (-5)   Joanie 2-4 = .333 (-13)
	//   Jenn 2-5 = .286 (-4)  Amy 2-5 = .286 (-19)
	// so the .333 pair sit above the .286 pair, and differential orders within
	// each. Amy ends last: worst record AND worst differential.
	want := []string{"kay", "joanie", "jenn", "amy"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("league order = %v, want %v (win%% then differential)", got, want)
		}
	}
}

// The criterion this whole change exists for: playing more must not, by itself,
// rank you higher.
func TestMoreGamesDoesNotBeatABetterRecord(t *testing.T) {
	rows := []model.Standing{
		{PlayerID: "grinder", Wins: 2, Losses: 8, PointDiff: 0}, // .200
		{PlayerID: "casual", Wins: 1, Losses: 1, PointDiff: 0},  // .500
	}
	if got := order(rankStandingsDiffFirst(rows, nil, true)); got[0] != "casual" {
		t.Fatalf("got %v, want the 1-1 player above the 2-8 player", got)
	}
	// Tournaments keep matches-won, where an equal schedule makes it equivalent.
	if got := order(rankStandings(rows, nil, true)); got[0] != "grinder" {
		t.Fatalf("tournament order = %v, want wins-first unchanged", got)
	}
}

// WinPct is derived in the ranking funnel, so every producer gets it.
func TestWinPctIsPopulated(t *testing.T) {
	rows := rankStandingsDiffFirst([]model.Standing{
		{PlayerID: "a", Wins: 3, Losses: 1},
		{PlayerID: "b"}, // no matches — must not divide by zero
	}, nil, true)
	for _, r := range rows {
		switch r.PlayerID {
		case "a":
			if r.WinPct != 0.75 {
				t.Fatalf("a WinPct = %v, want 0.75", r.WinPct)
			}
		case "b":
			if r.WinPct != 0 {
				t.Fatalf("b WinPct = %v, want 0 for no matches", r.WinPct)
			}
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

// Record still outranks differential — it's just measured as a RATE now, so a
// blowout margin can't lift a losing record above a winning one.
func TestRecordStillOutranksDifferential(t *testing.T) {
	rows := []model.Standing{
		{PlayerID: "losing-but-huge-margins", Wins: 1, Losses: 3, PointDiff: 50}, // .250
		{PlayerID: "winning-but-narrow", Wins: 3, Losses: 1, PointDiff: -30},     // .750
	}
	if got := order(rankStandingsDiffFirst(rows, nil, true)); got[0] != "winning-but-narrow" {
		t.Fatalf("win%% must outrank differential, got %v", got)
	}
}

// Small samples: an undefeated 1-0 ties an undefeated 6-0 on win%, and
// differential separates them. Worth pinning because it's the known cost of a
// rate-based rule — one lucky match reads as a perfect record.
func TestUndefeatedPlayersSeparateOnDifferential(t *testing.T) {
	rows := []model.Standing{
		{PlayerID: "one-and-oh", Wins: 1, Losses: 0, PointDiff: 5},
		{PlayerID: "six-and-oh", Wins: 6, Losses: 0, PointDiff: 43},
	}
	if got := order(rankStandingsDiffFirst(rows, nil, true)); got[0] != "six-and-oh" {
		t.Fatalf("got %v, want the 6-0 player first on differential", got)
	}
}
