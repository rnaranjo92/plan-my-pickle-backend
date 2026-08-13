package service

import "testing"

// poolProgress is the single gate on "Build playoff". What counts as
// outstanding work has to match what the app tells an organizer to do about a
// game that never happened.

func poolFake(rows string) *fakeSupabase {
	return newFake().seed("matches", rows)
}

// The reported dead end: a game nobody turned up for is marked "not played" —
// the app's own remedy, offered in the match menu — and the playoff was STILL
// blocked, while the round couldn't be deleted because it held real results.
func TestPoolProgress_NotPlayedIsNotOutstandingWork(t *testing.T) {
	f := poolFake(`[
		{"status":"completed"},
		{"status":"completed"},
		{"status":"canceled"}]`)
	s := newFakeSvc(t, f)

	total, open, err := s.poolProgress("b1")
	if err != nil {
		t.Fatalf("poolProgress: %v", err)
	}
	if open != 0 {
		t.Fatalf("%d matches still count as unplayed — the playoff stays blocked "+
			"after the organizer did exactly what the app suggested", open)
	}
	if total != 2 {
		t.Fatalf("total %d, want 2 — a called-off game is not a result either", total)
	}
}

// Both spellings appear in this codebase; the gate must not depend on which.
func TestPoolProgress_AcceptsEitherSpellingOfCancelled(t *testing.T) {
	f := poolFake(`[{"status":"completed"},{"status":"cancelled"}]`)
	s := newFakeSvc(t, f)

	_, open, _ := s.poolProgress("b1")
	if open != 0 {
		t.Fatalf("'cancelled' still counted as outstanding (open=%d)", open)
	}
}

// A genuinely unplayed game must still block — this is the guard that stops a
// playoff being seeded off half-finished standings.
func TestPoolProgress_ScheduledMatchStillBlocks(t *testing.T) {
	f := poolFake(`[{"status":"completed"},{"status":"scheduled"}]`)
	s := newFakeSvc(t, f)

	total, open, _ := s.poolProgress("b1")
	if open != 1 || total != 2 {
		t.Fatalf("want 1 open of 2, got %d of %d", open, total)
	}
}

// If every pool game was called off there is nothing to seed from, so this must
// read as "no schedule" rather than "complete" — otherwise the playoff would be
// built off all-zero standings.
func TestPoolProgress_AllCalledOffReadsAsNoSchedule(t *testing.T) {
	f := poolFake(`[{"status":"canceled"},{"status":"canceled"}]`)
	s := newFakeSvc(t, f)

	total, open, _ := s.poolProgress("b1")
	if total != 0 || open != 0 {
		t.Fatalf("want 0 of 0 (no schedule), got %d of %d", open, total)
	}
}
