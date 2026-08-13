package service

import "testing"

// A league's win margin is the rule its sessions are born with. The danger is
// not the setting itself but what happens to sessions that have ALREADY been
// played: changing the rule under them means the standings describe games under
// one rule and the board enforces another.
//
// The contract these tests pin down:
//   - a session with a recorded result is held back and REPORTED, not silently
//     changed and not silently ignored;
//   - the organizer can then apply it deliberately (includePlayed), which is the
//     only way a PERPETUAL league — one long-running event that has certainly
//     been played in — can ever adopt the setting at all;
//   - nothing about a played game is rewritten in either case. That holds
//     because winning_team is stored when the score is entered and read back
//     verbatim; win_by is consulted only while a score is being typed.

func leagueWithSession(winBy string, played bool) *fakeSupabase {
	f := newFake().
		seed("leagues", `[{"id":"l1","owner_id":"o1","win_by":2}]`).
		seed("events", `[{"id":"e1","league_id":"l1","win_by":`+winBy+`}]`)
	if played {
		f.seed("matches", `[{"id":"m1","event_id":"e1","winning_team":1}]`)
	} else {
		f.seed("matches", `[]`)
	}
	return f
}

func TestLeagueWinBy_PlayedSessionIsHeldBackAndReported(t *testing.T) {
	f := leagueWithSession("2", true)
	s := newFakeSvc(t, f)

	updated, skipped, err := s.SetLeagueWinBy("l1", "o1", 1, true, false)
	if err != nil {
		t.Fatalf("setting the win margin failed: %v", err)
	}
	if updated != 0 || skipped != 1 {
		t.Fatalf("updated=%d skipped=%d, want 0/1 — a played session must be "+
			"held back and counted", updated, skipped)
	}
	// The league default still saves; only the SESSION is left alone.
	if len(f.written("events")) != 0 {
		t.Fatal("a session with a scored game had its win margin rewritten")
	}
	if len(f.written("leagues")) != 1 {
		t.Fatalf("the league default should still be saved; wrote %v",
			f.written("leagues"))
	}
}

// Without this, a perpetual league could never adopt the setting: it IS one
// long-running event, so it is always "played".
func TestLeagueWinBy_IncludePlayedAppliesDeliberately(t *testing.T) {
	f := leagueWithSession("2", true)
	s := newFakeSvc(t, f)

	updated, _, err := s.SetLeagueWinBy("l1", "o1", 1, true, true)
	if err != nil {
		t.Fatalf("setting the win margin failed: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated=%d, want 1 — includePlayed must reach a played session",
			updated)
	}
	w := f.written("events")
	if len(w) != 1 {
		t.Fatalf("want one session write, got %d", len(w))
	}
	if got := w[0]["win_by"]; got != float64(1) && got != 1 {
		t.Fatalf("wrote win_by=%v, want 1", got)
	}
	// The write must carry ONLY the margin — a score, status or winner riding
	// along here is how a played game would get restated.
	if len(w[0]) != 1 {
		t.Fatalf("the update touched more than the win margin: %v", w[0])
	}
}

func TestLeagueWinBy_UnplayedSessionNeedsNoConfirmation(t *testing.T) {
	f := leagueWithSession("2", false)
	s := newFakeSvc(t, f)

	updated, skipped, err := s.SetLeagueWinBy("l1", "o1", 1, true, false)
	if err != nil {
		t.Fatalf("setting the win margin failed: %v", err)
	}
	if updated != 1 || skipped != 0 {
		t.Fatalf("updated=%d skipped=%d, want 1/0", updated, skipped)
	}
}

// Re-applying the same value must not be reported as a change — otherwise the
// confirmation dialog appears for a no-op.
func TestLeagueWinBy_AlreadyCorrectSessionIsNotCounted(t *testing.T) {
	f := leagueWithSession("1", true)
	s := newFakeSvc(t, f)

	updated, skipped, err := s.SetLeagueWinBy("l1", "o1", 1, true, false)
	if err != nil {
		t.Fatalf("setting the win margin failed: %v", err)
	}
	if updated != 0 || skipped != 0 {
		t.Fatalf("updated=%d skipped=%d, want 0/0 for a session already at "+
			"the target", updated, skipped)
	}
}

// The league default alone — the create-time behaviour — must never walk the
// sessions.
func TestLeagueWinBy_DefaultOnlyLeavesSessionsAlone(t *testing.T) {
	f := leagueWithSession("2", true)
	s := newFakeSvc(t, f)

	if _, _, err := s.SetLeagueWinBy("l1", "o1", 1, false, false); err != nil {
		t.Fatalf("setting the win margin failed: %v", err)
	}
	if len(f.written("events")) != 0 {
		t.Fatal("applyToSessions=false still rewrote a session")
	}
}

func TestLeagueWinBy_RejectsAnythingButOneOrTwo(t *testing.T) {
	for _, v := range []int{0, 3, -1} {
		f := leagueWithSession("2", false)
		s := newFakeSvc(t, f)
		if _, _, err := s.SetLeagueWinBy("l1", "o1", v, true, false); err == nil {
			t.Errorf("win margin %d was accepted", v)
		}
	}
}

func TestLeagueWinBy_OnlyTheOwnerMayChangeIt(t *testing.T) {
	f := leagueWithSession("2", false)
	s := newFakeSvc(t, f)

	if _, _, err := s.SetLeagueWinBy("l1", "someone-else", 1, true, false); err != ErrForbidden {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if len(f.written("leagues")) != 0 || len(f.written("events")) != 0 {
		t.Fatal("a non-owner's request still wrote")
	}
}
