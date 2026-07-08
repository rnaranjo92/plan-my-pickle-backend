package service

import (
	"strings"
	"testing"
)

// LeagueStandings must aggregate each session's per-event standings into one
// cumulative table (sums, not last-write) and order it by CUMULATIVE record.
// The fixture is built so the cumulative leader (Al) differs from session 1's
// leader (Bo) AND from first-seen aggregation order — so the test fails if the
// league-level sort is removed (per-event order would leave Bo first) or if
// aggregation keeps only one session's numbers.
func TestLeagueStandingsAggregatesAcrossSessions(t *testing.T) {
	e1 := `[
		{"player_id":"p2","full_name":"Bo","games_played":3,"wins":2,"losses":1,"points_for":25,"points_against":20,"point_diff":5},
		{"player_id":"p1","full_name":"Al","games_played":3,"wins":1,"losses":2,"points_for":20,"points_against":25,"point_diff":-5}
	]`
	e2 := `[
		{"player_id":"p1","full_name":"Al","games_played":3,"wins":3,"losses":0,"points_for":33,"points_against":10,"point_diff":23},
		{"player_id":"p2","full_name":"Bo","games_played":3,"wins":0,"losses":3,"points_for":10,"points_against":33,"point_diff":-23}
	]`
	f := newFake().
		// Two sessions in the league. Session 1: Bo leads. Session 2: Al sweeps.
		seed("events", `[{"id":"e1"},{"id":"e2"}]`).
		seedRPCFn("pmp_standings", func(body []byte) string {
			if strings.Contains(string(body), "e2") {
				return e2
			}
			return e1
		})
	s := newFakeSvc(t, f)

	out, err := s.LeagueStandings("l1")
	if err != nil {
		t.Fatalf("LeagueStandings: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("rows = %d, want 2", len(out))
	}
	// Cumulative record: Al 4W beats Bo 2W — even though Bo led session 1 and
	// was first-seen during aggregation. This is the league-level sort working.
	if out[0].PlayerID != "p1" || out[1].PlayerID != "p2" {
		t.Fatalf("order = [%s %s], want [p1 p2] (cumulative, not per-session)",
			out[0].PlayerID, out[1].PlayerID)
	}
	al := out[0]
	if al.Wins != 4 || al.Losses != 2 || al.GamesPlayed != 6 {
		t.Fatalf("Al record = %dW %dL %dGP, want 4W 2L 6GP (sessions summed)",
			al.Wins, al.Losses, al.GamesPlayed)
	}
	if al.PointsFor != 53 || al.PointsAgainst != 35 || al.PointDiff != 18 {
		t.Fatalf("Al points = %d/%d diff %d, want 53/35 diff 18",
			al.PointsFor, al.PointsAgainst, al.PointDiff)
	}
	if bo := out[1]; bo.Wins != 2 || bo.Losses != 4 || bo.PointDiff != -18 {
		t.Fatalf("Bo = %dW %dL diff %d, want 2W 4L diff -18", bo.Wins, bo.Losses, bo.PointDiff)
	}
}
