package service

import "testing"

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
