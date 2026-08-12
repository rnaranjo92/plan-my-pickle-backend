package service

import (
	"strings"
	"testing"
)

// An organizer may add players as bare names ("dump names in and see the
// games"), but a contactless player is invisible to themselves: nothing can be
// sent to them and no account will ever link to them. So it's bounded — and
// forbidden outright on leagues, which run for weeks and must reach people
// between sessions.

func TestContactlessRoom_LeagueRefusesAny(t *testing.T) {
	f := newFake().seed("events", `[{"id":"e1","league_id":"lg1"}]`)
	s := newFakeSvc(t, f)

	err := s.checkContactlessRoom("e1")
	if err == nil {
		t.Fatal("a league accepted a player with no phone and no email")
	}
	if !strings.Contains(err.Error(), "reachable") {
		t.Fatalf("league refusal should explain WHY; got %q", err)
	}
}

func TestContactlessRoom_NonLeagueAllowsUpToTheCap(t *testing.T) {
	// Eleven existing contactless registrations — one slot left of twelve.
	rows := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		rows = append(rows, `{"player":{"phone":"","email":""}}`)
	}
	f := newFake().
		seed("events", `[{"id":"e1"}]`).
		seed("registrations", "["+strings.Join(rows, ",")+"]")
	s := newFakeSvc(t, f)

	if err := s.checkContactlessRoom("e1"); err != nil {
		t.Fatalf("player %d of %d was refused: %v", 12, maxContactlessPlayers, err)
	}
}

func TestContactlessRoom_NonLeagueBlocksPastTheCap(t *testing.T) {
	rows := make([]string, 0, maxContactlessPlayers)
	for i := 0; i < maxContactlessPlayers; i++ {
		rows = append(rows, `{"player":{"phone":"","email":""}}`)
	}
	f := newFake().
		seed("events", `[{"id":"e1"}]`).
		seed("registrations", "["+strings.Join(rows, ",")+"]")
	s := newFakeSvc(t, f)

	if err := s.checkContactlessRoom("e1"); err == nil {
		t.Fatalf("accepted player %d with the cap at %d",
			maxContactlessPlayers+1, maxContactlessPlayers)
	}
}

// Players WITH contact details must not consume the allowance — otherwise a
// normal event stops accepting bare names after twelve ordinary registrations.
func TestContactlessRoom_ReachablePlayersDontCount(t *testing.T) {
	rows := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		rows = append(rows, `{"player":{"phone":"(619) 555-0100","email":""}}`)
	}
	f := newFake().
		seed("events", `[{"id":"e1"}]`).
		seed("registrations", "["+strings.Join(rows, ",")+"]")
	s := newFakeSvc(t, f)

	if err := s.checkContactlessRoom("e1"); err != nil {
		t.Fatalf("reachable players consumed the contactless allowance: %v", err)
	}
}

// The env override is the escape hatch for an organizer who genuinely needs more.
func TestContactlessRoom_EnvOverride(t *testing.T) {
	t.Setenv("MAX_CONTACTLESS_PLAYERS", "40")
	rows := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, `{"player":{"phone":"","email":""}}`)
	}
	f := newFake().
		seed("events", `[{"id":"e1"}]`).
		seed("registrations", "["+strings.Join(rows, ",")+"]")
	s := newFakeSvc(t, f)

	if err := s.checkContactlessRoom("e1"); err != nil {
		t.Fatalf("env override ignored: %v", err)
	}
}
