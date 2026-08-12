package service

import (
	"fmt"
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
		rows = append(rows, fmt.Sprintf(
			`{"player_id":"p%d","player":{"phone":"","email":""}}`, i))
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
		rows = append(rows, fmt.Sprintf(
			`{"player_id":"p%d","player":{"phone":"","email":""}}`, i))
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
		rows = append(rows, fmt.Sprintf(
			`{"player_id":"p%d","player":{"phone":"(619) 555-0100","email":""}}`, i))
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
		rows = append(rows, fmt.Sprintf(
			`{"player_id":"p%d","player":{"phone":"","email":""}}`, i))
	}
	f := newFake().
		seed("events", `[{"id":"e1"}]`).
		seed("registrations", "["+strings.Join(rows, ",")+"]")
	s := newFakeSvc(t, f)

	if err := s.checkContactlessRoom("e1"); err != nil {
		t.Fatalf("env override ignored: %v", err)
	}
}

// A player entered in TWO divisions has two registration rows. Counting rows
// instead of people would trip a 12-player cap at six real players.
func TestContactlessRoom_SamePlayerInTwoDivisionsCountsOnce(t *testing.T) {
	rows := make([]string, 0, 24)
	for i := 0; i < 12; i++ {
		// Same twelve people, each entered in two divisions => 24 rows.
		for d := 0; d < 2; d++ {
			rows = append(rows, fmt.Sprintf(
				`{"player_id":"p%d","player":{"phone":"","email":""}}`, i))
		}
	}
	f := newFake().
		seed("events", `[{"id":"e1"}]`).
		seed("registrations", "["+strings.Join(rows, ",")+"]")
	s := newFakeSvc(t, f)

	// Twelve DISTINCT people is exactly the cap, so the 13th must be refused —
	// but for the right reason (twelve people), not because 24 rows were counted.
	if err := s.checkContactlessRoom("e1"); err == nil {
		t.Fatal("expected the cap to be reached at twelve distinct players")
	}

	// Six people across two divisions is 12 ROWS but only 6 people — must pass.
	rows = rows[:12]
	f2 := newFake().
		seed("events", `[{"id":"e1"}]`).
		seed("registrations", "["+strings.Join(rows, ",")+"]")
	s2 := newFakeSvc(t, f2)
	if err := s2.checkContactlessRoom("e1"); err != nil {
		t.Fatalf("six people in two divisions (12 rows) was refused: %v", err)
	}
}
