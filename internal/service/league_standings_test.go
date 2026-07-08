package service

import "testing"

// LeagueStandings must aggregate each session's per-event standings into one
// cumulative table (sums, not last-write) and order it by record. The fake
// returns the same pmp_standings body for every event, so with two league
// sessions each player's line must come back exactly doubled.
func TestLeagueStandingsAggregatesAcrossSessions(t *testing.T) {
	f := newFake().
		// Two sessions in the league.
		seed("events", `[{"id":"e1"},{"id":"e2"}]`).
		// Per-session standings (unordered on purpose — Bo first).
		seedRPC("pmp_standings", `[
			{"player_id":"p2","full_name":"Bo","games_played":3,"wins":1,"losses":2,"points_for":20,"points_against":30,"point_diff":-10},
			{"player_id":"p1","full_name":"Al","games_played":3,"wins":2,"losses":1,"points_for":30,"points_against":20,"point_diff":10}
		]`)
	s := newFakeSvc(t, f)

	out, err := s.LeagueStandings("l1")
	if err != nil {
		t.Fatalf("LeagueStandings: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("rows = %d, want 2", len(out))
	}
	// Ordered by cumulative record: Al (4W) above Bo (2W).
	if out[0].PlayerID != "p1" || out[1].PlayerID != "p2" {
		t.Fatalf("order = [%s %s], want [p1 p2]", out[0].PlayerID, out[1].PlayerID)
	}
	al := out[0]
	if al.Wins != 4 || al.Losses != 2 || al.GamesPlayed != 6 {
		t.Fatalf("Al record = %dW %dL %dGP, want 4W 2L 6GP (two sessions summed)",
			al.Wins, al.Losses, al.GamesPlayed)
	}
	if al.PointsFor != 60 || al.PointsAgainst != 40 || al.PointDiff != 20 {
		t.Fatalf("Al points = %d/%d diff %d, want 60/40 diff 20",
			al.PointsFor, al.PointsAgainst, al.PointDiff)
	}
	if bo := out[1]; bo.Wins != 2 || bo.PointDiff != -20 {
		t.Fatalf("Bo = %dW diff %d, want 2W diff -20", bo.Wins, bo.PointDiff)
	}
}
