package service

import (
	"errors"
	"fmt"
	"log"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// seedRotationNames are deliberately distinct at a glance — a rotation board
// shows four names per court and a bench, and "Player 7 / Player 11" tells you
// nothing about whether the movement is right. Initials are unique too, so the
// avatar circles on the roster don't all read the same.
var seedRotationNames = []string{
	"Ada Chen", "Ben Ortiz", "Cara Diaz", "Dev Patel",
	"Elle Moore", "Finn Walsh", "Gia Romano", "Hugo Silva",
	"Iris Novak", "Jae Kim", "Kira Bloom", "Liam Fox",
	"Mara Ruiz", "Noor Aziz", "Omar Haddad", "Pia Lindqvist",
	"Quinn Foster", "Rosa Delgado", "Sami Tanaka", "Tess Okafor",
	"Uma Krishnan", "Vic Bellini", "Wren Halloran", "Xavi Moreno",
	"Yara Sadiq", "Zane Whitlock",
}

// SeedRotationLadderDemo creates a PRIVATE rotation ladder ready to start, and
// returns the league id.
//
// TWENTY-SIX players on SIX courts. The previous 14-on-3 was the *minimum*
// field that exercised the mechanics, which made it a poor test of the thing
// organizers actually run: three courts fit on one screen, so nothing about
// scrolling, court-count limits or a real bench queue was ever exercised, and a
// two-person bench empties and refills so fast that unfair court time never has
// room to appear.
//
// 26 on 6 is sized to break things instead:
//   - 24 seats fill the courts and TWO wait, so the bench is a live queue
//     rather than a formality;
//   - six courts is past what fits on a phone, so the board has to scroll;
//   - 26 is not divisible by 4, so overflow/queue handling stays live;
//   - enough rounds of movement that the fairness pass's equal-court-time
//     behaviour is observable rather than theoretical.
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
		CourtCount:   6,
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
	// Provision the ladder's ongoing event before handing the id back, so the
	// test fixture lands on the SAME screen a real one does — the event shell
	// with Feed, Players, Media, Rotation, Finances, Info and Admin — instead of
	// the thin league shell it would show until something happened to read the
	// league first. A test that opens a different page than production is how a
	// routing regression goes unnoticed.
	if _, err := s.GetLeague(leagueID); err != nil {
		// Non-fatal: the ladder itself is built and usable, and the next open
		// provisions the event anyway.
		log.Printf("seed rotation ladder: could not provision the event for %s: %v",
			leagueID, err)
	}
	return leagueID, nil
}
