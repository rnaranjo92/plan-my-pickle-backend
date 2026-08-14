package service

import (
	"errors"
	"fmt"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// seedRotationNames are deliberately distinct at a glance — a rotation board
// shows four names per court and a bench, and "Player 7 / Player 11" tells you
// nothing about whether the movement is right.
var seedRotationNames = []string{
	"Ada Chen", "Ben Ortiz", "Cara Diaz", "Dev Patel",
	"Elle Moore", "Finn Walsh", "Gia Romano", "Hugo Silva",
	"Iris Novak", "Jae Kim", "Kira Bloom", "Liam Fox",
	"Mara Ruiz", "Noor Aziz",
}

// SeedRotationLadderDemo creates a PRIVATE rotation ladder ready to start, and
// returns the league id.
//
// Fourteen players on three courts is chosen, not arbitrary: twelve seats fill
// the courts and TWO wait. That is the smallest field that exercises every
// interesting path on the very first round —
//   - a bench exists, so players rotate in;
//   - the bench is exactly the fairness pass's two-swap width, so equal court
//     time is visible within a few rounds rather than after forty;
//   - 14 is not divisible by 4, so the overflow/queue handling is live.
//
// Left in 'setup' on purpose. Start is where the random opening draw happens,
// so a seeder that auto-started would hide the thing most worth watching.
func (s *Service) SeedRotationLadderDemo(ownerID string) (string, error) {
	if ownerID == "" {
		return "", errors.New("not signed in")
	}
	// Private: CreateLeague only writes `listed` when opting in, so omitting it
	// leaves the league undiscoverable — which is what a test fixture should be.
	// It must never turn up on the public "leagues near you" pages.
	leagueID, err := s.CreateLeague(ownerID, model.CreateLeagueRequest{
		Name:         "TEST Private Rotation Ladder",
		LeagueType:   "ladder",
		LadderFormat: "rotation",
		Location:     "Test Courts",
		Divisions:    []model.LeagueBracketInput{{Name: "Open", DivisionType: "open"}},
	})
	if err != nil {
		return "", fmt.Errorf("could not create the test ladder: %w", err)
	}

	// The division (league_bracket) the ladder and its sessions hang off.
	div, err := s.sb.SelectOne("league_brackets",
		"league_id=eq."+store.Q(leagueID)+"&select=id&order=sort_order.asc")
	if err != nil {
		return "", err
	}
	if div == nil {
		return "", errors.New("the test ladder has no division")
	}
	divID := asStr(div, "id")

	for _, name := range seedRotationNames {
		if _, err := s.AddLadderEntrant(divID,
			model.AddLadderEntrantRequest{DisplayName: name}); err != nil {
			return "", fmt.Errorf("could not add %s: %w", name, err)
		}
	}

	sess, err := s.CreateRotationSession(divID, model.CreateRotationSessionRequest{
		Name:         "Test session",
		CourtCount:   3,
		RoundMinutes: 8,
	})
	if err != nil {
		return "", fmt.Errorf("could not create the session: %w", err)
	}
	// Pull the ladder onto the session roster so the session is ready to start.
	// Start would do this itself for an empty roster, but doing it here means the
	// roster is visible and editable BEFORE starting — which is where an
	// organizer would sit players out or place someone on a court.
	if _, err := s.ImportLadderEntrantsToSession(sess.ID); err != nil {
		return "", fmt.Errorf("could not import the ladder: %w", err)
	}
	return leagueID, nil
}
