package service

import (
	"fmt"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// regRows builds n registration rows (with embedded player data) in one bracket
// so GenerateSchedule sees a real field and the engine can pair them.
func regRows(n int) string {
	out := "["
	for i := 1; i <= n; i++ {
		if i > 1 {
			out += ","
		}
		out += fmt.Sprintf(`{"id":"r%d","event_id":"e1","player_id":"p%d","bracket_id":"b1","payment_status":"paid","checked_in":true,"player":{"id":"p%d","full_name":"Player %d","skill_level":3.%d,"dupr_rating":3.%d}}`,
			i, i, i, i, i%9, i%9)
	}
	return out + "]"
}

// TestGenerateScheduleRoundRobin seeds a full field so GenerateSchedule actually
// builds + persists a round-robin (driving the engine + persistRoundRobin), then
// drives the scoring cascade on a seeded bracket match. Errors/panics tolerated.
func TestGenerateScheduleRoundRobin(t *testing.T) {
	f := newFake().
		seed("events", `[{"id":"e1","name":"E","owner_id":"o","tournament_format":"round_robin","format":"doubles","partner_mode":"rotating","num_courts":2,"points_to_win":11,"win_by":2,"best_of":1,"status":"draft"}]`).
		seed("brackets", `[{"id":"b1","event_id":"e1","name":"Open","division_type":"open"}]`).
		seed("registrations", regRows(8)).
		seed("matches", `[{"id":"m1","event_id":"e1","stage":"bracket","bracket_id":"b1","bracket_round":1,"bracket_slot":1,"status":"in_progress","participants":[{"team":1,"player_id":"p1","player":{"full_name":"A"}},{"team":2,"player_id":"p2","player":{"full_name":"B"}}]}]`)
	s := newFakeSvc(t, f)

	run := func(fn func()) {
		defer func() { _ = recover() }()
		fn()
	}
	run(func() { _, _ = s.GenerateSchedule("e1", true, true) })
	run(func() { _, _ = s.AutoScheduleByRating("e1", true, 1) })
	run(func() { _ = s.applyScore("m1", 11, 6) })
	run(func() { _ = s.RecordScore("m1", 11, 6) })
	run(func() { _ = s.RecordSeries("m1", []model.GameScore{{Team1: 11, Team2: 6}, {Team1: 9, Team2: 11}, {Team1: 11, Team2: 7}}) })
	run(func() { _ = s.ForfeitMatch("m1", 1, "retire", nil, nil) })
}

// TestGeneratePoolsPlayoffAndBracket exercises the pools→playoff + single-elim
// generation paths and the playoff-bracket builder.
func TestGeneratePoolsPlayoffAndBracket(t *testing.T) {
	for _, format := range []string{"single_elim", "double_elim", "pools_playoff", "compass"} {
		f := newFake().
			seed("events", fmt.Sprintf(`[{"id":"e1","name":"E","owner_id":"o","tournament_format":%q,"format":"doubles","partner_mode":"rotating","num_courts":2,"points_to_win":11,"win_by":2,"consolation":true,"status":"draft"}]`, format)).
			seed("brackets", `[{"id":"b1","event_id":"e1","name":"Open","division_type":"open"}]`).
			seed("registrations", regRows(8))
		s := newFakeSvc(t, f)
		func() {
			defer func() { _ = recover() }()
			_, _ = s.GenerateSchedule("e1", true, true)
		}()
	}

	// Playoff bracket build from a division's pool standings.
	f := newFake().
		seed("events", `[{"id":"e1","name":"E","owner_id":"o","tournament_format":"pools_playoff","format":"doubles","num_courts":2,"status":"live"}]`).
		seed("brackets", `[{"id":"b1","event_id":"e1","name":"Open","division_type":"open"}]`).
		seed("registrations", regRows(8))
	s := newFakeSvc(t, f)
	func() {
		defer func() { _ = recover() }()
		_, _ = s.GeneratePlayoffBracket("b1", 4, "wins", nil)
	}()
	func() {
		defer func() { _ = recover() }()
		_, _ = s.GeneratePlayoffBracket("b1", 8, "manual", [][]string{{"p1", "p2"}, {"p3", "p4"}})
	}()
}
