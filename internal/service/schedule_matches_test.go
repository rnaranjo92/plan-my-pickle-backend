package service

import "testing"

// EventScheduleMatches decides which games reach the Game tab. It filters by
// STAGE per tournament format, so a format missing from that switch silently
// loses its bracket games — they exist in the database and are invisible in the
// app, which is exactly what happened when round-robin gained a playoff.

func formatFake(tf string) *fakeSupabase {
	return newFake().
		seed("events", `[{"id":"e1","tournament_format":"`+tf+`"}]`).
		seed("matches", `[
			{"id":"m1","event_id":"e1","stage":"pool","status":"completed"},
			{"id":"m2","event_id":"e1","stage":"bracket","status":"scheduled"}]`)
}

// A round robin can finish with a playoff now, so its bracket games must come
// back. Without this the bracket is built and then unreachable: no games on the
// Game tab, and "Build playoff" never hides — the client decides that by looking
// for a bracket match.
func TestScheduleMatches_RoundRobinIncludesThePlayoff(t *testing.T) {
	s := newFakeSvc(t, formatFake("round_robin"))

	ms, err := s.EventScheduleMatches("e1")
	if err != nil {
		t.Fatalf("EventScheduleMatches: %v", err)
	}
	var bracket int
	for _, m := range ms {
		if m.Stage == "bracket" {
			bracket++
		}
	}
	if bracket == 0 {
		t.Fatal("a round robin's playoff games never reach the Game tab")
	}
}

// Every format that can show a bracket must return one. This is the assertion
// that would have caught the original bug when round-robin was given a playoff.
func TestScheduleMatches_EveryBracketCapableFormatReturnsIt(t *testing.T) {
	for _, tf := range []string{
		"round_robin", "pools_playoff", "single_elim", "double_elim", "compass",
	} {
		s := newFakeSvc(t, formatFake(tf))
		ms, err := s.EventScheduleMatches("e1")
		if err != nil {
			t.Fatalf("%s: %v", tf, err)
		}
		found := false
		for _, m := range ms {
			if m.Stage == "bracket" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: bracket games are filtered out of the Game tab", tf)
		}
	}
}
