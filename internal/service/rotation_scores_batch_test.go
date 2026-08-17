package service

import (
	"strings"
	"testing"
)

// Scoring a court used to be one HTTP request PER PLAYER, awaited in sequence:
// four round trips from the organizer's phone to save what they experience as
// typing two numbers, several times a round, on venue wifi. These cover the
// batch write that replaced it.

func batchHarness(t *testing.T) (*Service, *fakeSupabase) {
	t.Helper()
	f := newFake().
		seed("rotation_sessions",
			`[{"id":"s1","league_bracket_id":"lb1","status":"live","current_round":1,"round_minutes":12,"court_count":1}]`).
		seed("rotation_players",
			`[{"id":"p1","session_id":"s1","display_name":"Al","active":true}]`).
		seed("rotation_round_scores", `[]`).
		// Present-but-empty: the substitution guard runs first, and an absent
		// table there reads as a lookup failure rather than "nobody was subbed".
		seed("rotation_substitutions", `[]`).
		seed("rotation_round_courts",
			`[{"session_id":"s1","round":1,"court":1,"team_a_p1":"p1","team_a_p2":"p2","team_b_p1":"p3","team_b_p2":"p4"}]`)
	return newFakeSvc(t, f), f
}

func TestSetRotationScoresWritesEveryPlayer(t *testing.T) {
	s, f := batchHarness(t)
	n := 11
	m := 7
	err := s.SetRotationScores("s1", 1, []RotationScoreEntry{
		{PlayerID: "p1", Score: &n},
		{PlayerID: "p2", Score: &n},
		{PlayerID: "p3", Score: &m},
		{PlayerID: "p4", Score: &m},
	})
	if err != nil {
		t.Fatalf("batch write failed: %v", err)
	}
	// Every one of the four has to land — a court with two scores written and
	// two missing is unresolvable, and an unresolvable court silently hands the
	// round to team A.
	rows := f.writes["rotation_round_scores"]
	seen := map[string]bool{}
	for _, r := range rows {
		if id, ok := r["rotation_player_id"].(string); ok {
			seen[id] = true
		}
	}
	for _, p := range []string{"p1", "p2", "p3", "p4"} {
		if !seen[p] {
			t.Errorf("player %s was never written; wrote %v", p, rows)
		}
	}
}

func TestSetRotationScoresKeepsPerPlayerValidation(t *testing.T) {
	// The batch reuses SetRotationScore rather than reimplementing it, so the
	// rules that stop a court becoming unresolvable still apply. A player who
	// was RESTING has no game to score.
	s, _ := batchHarness(t)
	n := 11
	err := s.SetRotationScores("s1", 1, []RotationScoreEntry{
		{PlayerID: "p1", Score: &n},
		{PlayerID: "ghost", Score: &n}, // never on court
	})
	if err == nil {
		t.Fatal("expected the resting-player rule to reject this")
	}
	if !strings.Contains(err.Error(), "resting") {
		t.Fatalf("expected a resting-player error, got: %v", err)
	}
}

func TestSetRotationScoresEmptyIsANoOp(t *testing.T) {
	s, f := batchHarness(t)
	before := len(f.writes["rotation_round_scores"])
	if err := s.SetRotationScores("s1", 1, nil); err != nil {
		t.Fatalf("empty batch should do nothing, got: %v", err)
	}
	if len(f.writes["rotation_round_scores"]) != before {
		t.Fatalf("empty batch wrote something: %v", f.writes["rotation_round_scores"])
	}
}

func TestSetRotationScoresClearsWithNil(t *testing.T) {
	// A nil score clears the cell — that's how a score left on somebody who was
	// later benched or substituted gets removed.
	s, _ := batchHarness(t)
	if err := s.SetRotationScores("s1", 1, []RotationScoreEntry{
		{PlayerID: "p1", Score: nil},
		{PlayerID: "p2", Score: nil},
	}); err != nil {
		t.Fatalf("clear failed: %v", err)
	}
}
