package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// TestServiceCreateDeep drives the heavyweight create/import methods with
// fully-populated requests (Location left blank so bestEffortGeocode short-
// circuits without any network call). Business errors are tolerated.
func TestServiceCreateDeep(t *testing.T) {
	s := newFakeSvc(t, seededFake())

	req := model.CreateEventRequest{
		Name:                 "Test Event",
		Format:               "doubles",
		PartnerMode:          "rotating",
		TournamentFormat:     "round_robin",
		ScoringMode:          "wins",
		NumCourts:            2,
		PointsToWin:          11,
		WinBy:                2,
		BestOf:               1,
		GameDurationMinutes:  25,
		RegistrationFeeCents: 0,
		Location:             "", // blank → no geocode network call
		ContactPhone:         "5551234",
	}
	_, _ = s.CreateEvent(req, "o1")
	_ = s.UpdateEvent("e1", req)

	// Registration with a populated request exercises the player-create +
	// bracket-assignment path.
	_, _ = s.RegisterPlayer("e1", model.RegisterRequest{
		FullName: "New Player", Phone: "5559999", BracketID: "b1",
	}, "")

	// Bulk roster import.
	_, _ = s.ImportRoster("e1", model.ImportRosterRequest{
		BracketID: "b1",
		Players: []model.ImportRosterPlayer{
			{FullName: "Imp One", Phone: "5550001"},
			{FullName: "Imp Two", Phone: "5550002"},
		},
	})

	// League with explicit divisions.
	_, _ = s.CreateLeague("o1", model.CreateLeagueRequest{
		Name:       "Season",
		LeagueType: "round_robin",
		DayType:    "multi",
		Divisions:  []model.LeagueBracketInput{{Name: "Open", DivisionType: "open"}},
	})

	// Division reconciliation (edit flow).
	_, _ = s.SyncDivisions("e1", []model.BracketInput{
		{Name: "3.0-3.5", DivisionType: "open"},
		{ID: "b1", Name: "Open"},
	})
}
