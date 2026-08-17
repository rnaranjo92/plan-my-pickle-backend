package service

import "testing"

// A ladder has no dates, so the app files every one under "Now" for its owner
// to find — which badged them all LIVE every day of the week, forever. These
// cover the flag that lets LIVE mean live.

func liveFake() *fakeSupabase {
	return newFake().
		seed("leagues", `[
      {"id":"l1","owner_id":"o1","name":"Rotation night","league_type":"ladder","ladder_format":"rotation","created_at":"2026-06-20T00:00:00Z"},
      {"id":"l2","owner_id":"o1","name":"Quiet ladder","league_type":"ladder","ladder_format":"rotation","created_at":"2026-06-20T00:00:00Z"}
    ]`).
		seed("league_brackets", `[
      {"id":"lb1","league_id":"l1"},
      {"id":"lb2","league_id":"l2"}
    ]`)
}

func TestLeagueSessionLiveOnlyWhenARoundIsRunning(t *testing.T) {
	f := liveFake().
		// The fake ignores filters, so a live row here stands for "the
		// live/paused query returned something".
		seed("rotation_sessions", `[{"id":"s1","league_bracket_id":"lb1","status":"live"}]`)
	out, err := newFakeSvc(t, f).ListLeagues("o1")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 leagues, got %d", len(out))
	}
	var l1 bool
	for _, l := range out {
		if l.ID == "l1" {
			l1 = l.SessionLive
		}
	}
	if !l1 {
		t.Error("l1 has a live session and was not marked")
	}
}

func TestLeagueSessionLiveIsFalseWithNoSessions(t *testing.T) {
	// The common case: a standing ladder nobody is playing on right now. It
	// still belongs under "Now" — it just isn't LIVE.
	f := liveFake().seed("rotation_sessions", `[]`)
	out, err := newFakeSvc(t, f).ListLeagues("o1")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	for _, l := range out {
		if l.SessionLive {
			t.Errorf("%s claimed a live session with none running", l.ID)
		}
	}
}

func TestLeagueListSurvivesASessionLookupFailure(t *testing.T) {
	// Nobody's league list should fail to load because a badge couldn't be
	// computed. No rotation_sessions seed at all = the lookup errors.
	f := liveFake()
	out, err := newFakeSvc(t, f).ListLeagues("o1")
	if err != nil {
		t.Fatalf("a badge lookup must never fail the list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 leagues, got %d", len(out))
	}
	for _, l := range out {
		if l.SessionLive {
			t.Errorf("%s claimed live off a failed lookup", l.ID)
		}
	}
}

func TestNonRotationLeaguesAreNeverProbed(t *testing.T) {
	// Only rotation ladders have sessions; a round-robin must not be marked.
	f := newFake().seed("leagues",
		`[{"id":"l9","owner_id":"o1","name":"Fall","league_type":"round_robin","created_at":"2026-06-20T00:00:00Z"}]`)
	out, err := newFakeSvc(t, f).ListLeagues("o1")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(out) != 1 || out[0].SessionLive {
		t.Fatalf("a non-rotation league was marked live: %+v", out)
	}
}
