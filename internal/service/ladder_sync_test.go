package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// Registering somebody and putting them on the ladder are separate acts, so an
// organizer can register twenty people, open the Rotation tab to start a night
// and find nobody to pick from. These cover the query that reports that gap and
// the action that closes it.

func syncFake(entrants, regs string) *fakeSupabase {
	return newFake().
		seed("league_brackets", `[{"id":"lb1","league_id":"l1","name":"Open"}]`).
		seed("events", `[{"id":"e1","league_id":"l1"}]`).
		seed("ladder_entrants", entrants).
		seed("registrations", regs)
}

func TestLadderSyncCountsWhoIsMissing(t *testing.T) {
	f := syncFake(
		`[{"id":"en1","league_bracket_id":"lb1","player_id":"p1","display_name":"Ada","position":1}]`,
		`[{"player_id":"p1","player":{"full_name":"Ada"}},
		  {"player_id":"p2","player":{"full_name":"Ben"}}]`)
	got, err := newFakeSvc(t, f).LadderSync("lb1")
	if err != nil {
		t.Fatalf("sync status failed: %v", err)
	}
	if got.Missing != 1 {
		t.Errorf("expected 1 missing (Ben), got %d", got.Missing)
	}
	if got.OnLadder != 1 {
		t.Errorf("expected 1 on the ladder (Ada), got %d", got.OnLadder)
	}
}

func TestLadderSyncIgnoresPlayersAlreadyOn(t *testing.T) {
	// Everyone registered is already an entrant — pressing the button should
	// have nothing to do, which is what makes it safe to press twice.
	f := syncFake(
		`[{"id":"en1","league_bracket_id":"lb1","player_id":"p1","display_name":"Ada","position":1}]`,
		`[{"player_id":"p1","player":{"full_name":"Ada"}}]`)
	got, err := newFakeSvc(t, f).LadderSync("lb1")
	if err != nil {
		t.Fatalf("sync status failed: %v", err)
	}
	if got.Missing != 0 {
		t.Errorf("expected nobody missing, got %d", got.Missing)
	}
}

func TestLadderSyncSkipsNamelessRegistrations(t *testing.T) {
	// An entrant with no name would render blank on the board, in the standings
	// and in the spoken court calls — skip rather than create one to hunt down.
	f := syncFake(`[]`, `[{"player_id":"p9","player":{"full_name":""}}]`)
	got, err := newFakeSvc(t, f).LadderSync("lb1")
	if err != nil {
		t.Fatalf("sync status failed: %v", err)
	}
	if got.Missing != 0 {
		t.Errorf("a nameless registration was counted: %d", got.Missing)
	}
}

func TestAddRegisteredToLadderAddsTheMissing(t *testing.T) {
	f := syncFake(`[]`, `[{"player_id":"p2","player":{"full_name":"Ben"}}]`)
	s := newFakeSvc(t, f)
	n, err := s.AddRegisteredToLadder("lb1")
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 added, got %d", n)
	}
	rows := f.writes["ladder_entrants"]
	if len(rows) != 1 {
		t.Fatalf("expected one entrant written, got %v", rows)
	}
	if rows[0]["display_name"] != "Ben" {
		t.Errorf("wrong name written: %v", rows[0])
	}
	// Linked to the player, not a bare name — otherwise the entrant can never be
	// recognised as that account's, and their standings won't follow them.
	if rows[0]["player_id"] != "p2" {
		t.Errorf("entrant not linked to the player: %v", rows[0])
	}
}

func TestAddRegisteredToLadderIsANoOpWhenNobodyIsMissing(t *testing.T) {
	f := syncFake(
		`[{"id":"en1","league_bracket_id":"lb1","player_id":"p1","display_name":"Ada","position":1}]`,
		`[{"player_id":"p1","player":{"full_name":"Ada"}}]`)
	s := newFakeSvc(t, f)
	n, err := s.AddRegisteredToLadder("lb1")
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected nothing added, got %d", n)
	}
	if len(f.writes["ladder_entrants"]) != 0 {
		t.Fatalf("wrote entrants when none were missing: %v", f.writes["ladder_entrants"])
	}
}

// --- fixes from the multi-agent review ---------------------------------------

func TestLadderSyncSkipsPlaceholderRegistrants(t *testing.T) {
	// Placeholder rows (the +1555 test players) must never be swept onto a real
	// ladder — they'd render on the public TV board and in the standings.
	f := syncFake(`[]`,
		`[{"player_id":"p7","player":{"full_name":"Placeholder Pat","phone":"+15553001111"}}]`)
	got, err := newFakeSvc(t, f).LadderSync("lb1")
	if err != nil {
		t.Fatalf("sync status failed: %v", err)
	}
	if got.Missing != 0 {
		t.Errorf("a +1555 placeholder was counted as missing: %d", got.Missing)
	}
}

func TestLadderSyncDedupesAcrossDivisions(t *testing.T) {
	// A player on ANY of the league's ladders is not "missing" — checking only
	// the pressed division meant a two-division league reported division B's
	// players as missing from division A, and Add would have double-laddered
	// the entire league. (The fake echoes seeds regardless of filters, so this
	// asserts the union check, not per-division scoping.)
	f := newFake().
		seed("league_brackets", `[{"id":"lb1","league_id":"l1"},{"id":"lb2","league_id":"l1"}]`).
		seed("events", `[{"id":"e1","league_id":"l1"}]`).
		seed("ladder_entrants",
			`[{"id":"en9","league_bracket_id":"lb2","player_id":"p1","display_name":"Ada","position":1}]`).
		seed("registrations", `[{"player_id":"p1","player":{"full_name":"Ada"}}]`)
	got, err := newFakeSvc(t, f).LadderSync("lb1")
	if err != nil {
		t.Fatalf("sync status failed: %v", err)
	}
	if got.Missing != 0 {
		t.Errorf("player on another division's ladder counted as missing: %d", got.Missing)
	}
}

func TestAddRegisteredToLadderAddsAlphabetically(t *testing.T) {
	// Map iteration used to append the missing players to the bottom of the
	// ladder in a different random order every press — and bottom-of-ladder
	// position is a real thing on a ladder.
	f := syncFake(`[]`, `[
	  {"player_id":"pz","player":{"full_name":"Zed"}},
	  {"player_id":"pa","player":{"full_name":"Amy"}}
	]`)
	s := newFakeSvc(t, f)
	n, err := s.AddRegisteredToLadder("lb1")
	if err != nil || n != 2 {
		t.Fatalf("expected 2 added, got %d (err %v)", n, err)
	}
	rows := f.writes["ladder_entrants"]
	if len(rows) != 2 || rows[0]["display_name"] != "Amy" || rows[1]["display_name"] != "Zed" {
		t.Fatalf("expected Amy then Zed, got %v", rows)
	}
}

func TestAddLadderEntrantRefusesToDuplicateALinkedPlayer(t *testing.T) {
	// The check lives INSIDE AddLadderEntrant now, so every caller (organizer
	// add, self-join, sync — even racing each other) gets it. A re-add returns
	// the existing entrant and writes nothing.
	f := newFake().
		seed("ladder_entrants",
			`[{"id":"en1","league_bracket_id":"lb1","player_id":"p1","display_name":"Ada","position":1}]`)
	s := newFakeSvc(t, f)
	pid := "p1"
	got, err := s.AddLadderEntrant("lb1", model.AddLadderEntrantRequest{
		DisplayName: "Ada Again", PlayerID: &pid,
	})
	if err != nil {
		t.Fatalf("re-add errored: %v", err)
	}
	if got.ID != "en1" {
		t.Errorf("expected the existing entrant back, got %+v", got)
	}
	if len(f.writes["ladder_entrants"]) != 0 {
		t.Errorf("a duplicate entrant was written: %v", f.writes["ladder_entrants"])
	}
}
