package service

import (
	"errors"
	"strings"
	"testing"
)

// Advancing a round is where a scorecard turns into movement, and where a wrong
// answer is worst: the courts reshuffle and the round is closed, so a pair
// promoted by mistake stays promoted.

// oneCourtSession seeds a live one-court session at round 1 with the four given
// scores. The fake ignores query filters, so a single court and a single round
// is the shape it can represent faithfully.
func oneCourtSession(a1, a2, b1, b2 int) *fakeSupabase {
	return newFake().
		seed("rotation_sessions",
			`[{"id":"s1","status":"live","current_round":1,"round_minutes":12,"bench":[]}]`).
		seed("rotation_round_courts", `[{"session_id":"s1","round":1,"court":1,
			"team_a_p1":"pA1","team_a_p2":"pA2",
			"team_b_p1":"pB1","team_b_p2":"pB2","winner":""}]`).
		seed("rotation_round_scores", `[
			{"id":"s-a1","session_id":"s1","round":1,"rotation_player_id":"pA1","score":`+itoa(a1)+`},
			{"id":"s-a2","session_id":"s1","round":1,"rotation_player_id":"pA2","score":`+itoa(a2)+`},
			{"id":"s-b1","session_id":"s1","round":1,"rotation_player_id":"pB1","score":`+itoa(b1)+`},
			{"id":"s-b2","session_id":"s1","round":1,"rotation_player_id":"pB2","score":`+itoa(b2)+`}]`).
		seed("rotation_players", `[
			{"id":"pA1","session_id":"s1","display_name":"A1","active":true},
			{"id":"pA2","session_id":"s1","display_name":"A2","active":true},
			{"id":"pB1","session_id":"s1","display_name":"B1","active":true},
			{"id":"pB2","session_id":"s1","display_name":"B2","active":true}]`).
		seedRPC("advance_rotation_session", `{"advanced":true}`)
}

// Michelle's rule is that a tie is settled by a sudden-death point. Before this,
// equal totals fell through to the "nothing was reported" default of team A —
// so a 22-22 court silently promoted team A and closed the round.
func TestAdvance_RefusesATiedCourt(t *testing.T) {
	f := oneCourtSession(11, 11, 11, 11)
	s := newFakeSvc(t, f)

	err := s.AdvanceRotationSession("s1", 1)
	if err == nil {
		t.Fatal("a tied court advanced — team A would have been promoted by default")
	}
	if !strings.Contains(err.Error(), "sudden-death") {
		t.Fatalf("the refusal should name the remedy; got %q", err)
	}
	if !strings.Contains(err.Error(), "Court 1") {
		t.Fatalf("the refusal should name the court; got %q", err)
	}
	if len(f.rpcBodies("advance_rotation_session")) != 0 {
		t.Fatal("a refused advance still closed the round")
	}
}

// The refusal must be specific to a REAL tie. A decided court has to advance.
func TestAdvance_DecidedCourtStillAdvances(t *testing.T) {
	f := oneCourtSession(11, 11, 9, 9)
	s := newFakeSvc(t, f)

	if err := s.AdvanceRotationSession("s1", 1); err != nil {
		t.Fatalf("a decided court was refused: %v", err)
	}
	if len(f.rpcBodies("advance_rotation_session")) != 1 {
		t.Fatal("a decided court did not advance")
	}
}

// An all-zero court is "nobody entered anything", not a tie — that keeps the
// existing unreported behaviour rather than stalling a session on empty courts.
func TestAdvance_UnscoredCourtIsNotTreatedAsATie(t *testing.T) {
	f := oneCourtSession(0, 0, 0, 0)
	s := newFakeSvc(t, f)

	if err := s.AdvanceRotationSession("s1", 1); err != nil {
		t.Fatalf("an unscored court was treated as a tie: %v", err)
	}
}

// A substitution that lands while an advance is in flight leaves the court rows
// naming someone who has gone home. The bench prune only cleans the QUEUE, so
// without a seat remap the next round is written with the departed player on a
// court — for the rest of the night, and unrecoverably.
func TestAdvance_RemapsSeatsThroughSubstitutions(t *testing.T) {
	f := oneCourtSession(11, 11, 9, 9).
		seed("rotation_substitutions",
			`[{"session_id":"s1","round":1,"out_player":"pA1","in_player":"pSub"}]`).
		seed("rotation_players", `[
			{"id":"pA2","session_id":"s1","display_name":"A2","active":true},
			{"id":"pB1","session_id":"s1","display_name":"B1","active":true},
			{"id":"pB2","session_id":"s1","display_name":"B2","active":true},
			{"id":"pSub","session_id":"s1","display_name":"Sub","active":true},
			{"id":"pA1","session_id":"s1","display_name":"A1","active":false}]`)
	s := newFakeSvc(t, f)

	if err := s.AdvanceRotationSession("s1", 1); err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	bodies := f.rpcBodies("advance_rotation_session")
	if len(bodies) != 1 {
		t.Fatalf("want one advance, got %d", len(bodies))
	}
	sent := string(bodies[0])
	if strings.Contains(sent, "pA1") {
		t.Fatalf("the next round was written with a player who was substituted out: %s", sent)
	}
	if !strings.Contains(sent, "pSub") {
		t.Fatalf("the substitute never inherited the seat: %s", sent)
	}
}

// A chain (A -> B -> C) must resolve all the way to the player actually on court,
// not stop at the first hop.
func TestAdvance_RemapFollowsASubstitutionChain(t *testing.T) {
	f := oneCourtSession(11, 11, 9, 9).
		seed("rotation_substitutions", `[
			{"session_id":"s1","round":1,"out_player":"pA1","in_player":"pMid"},
			{"session_id":"s1","round":1,"out_player":"pMid","in_player":"pLast"}]`).
		seed("rotation_players", `[
			{"id":"pA2","session_id":"s1","display_name":"A2","active":true},
			{"id":"pB1","session_id":"s1","display_name":"B1","active":true},
			{"id":"pB2","session_id":"s1","display_name":"B2","active":true},
			{"id":"pLast","session_id":"s1","display_name":"Last","active":true},
			{"id":"pA1","session_id":"s1","display_name":"A1","active":false},
			{"id":"pMid","session_id":"s1","display_name":"Mid","active":false}]`)
	s := newFakeSvc(t, f)

	if err := s.AdvanceRotationSession("s1", 1); err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	sent := string(f.rpcBodies("advance_rotation_session")[0])
	if strings.Contains(sent, "pA1") || strings.Contains(sent, "pMid") {
		t.Fatalf("the chain stopped short of the player actually on court: %s", sent)
	}
	if !strings.Contains(sent, "pLast") {
		t.Fatalf("the last substitute never took the seat: %s", sent)
	}
}

// A player who is sat out mid-session must lose their SEAT, not just a flag.
// Flipping active alone left them on court every round for the rest of the
// night: collecting games, never scoring, and so keeping the scorecard
// permanently incomplete — which silently stopped auto-advance dead.
func TestAdvance_RefillsTheSeatOfAPlayerWhoLeft(t *testing.T) {
	f := oneCourtSession(11, 11, 9, 9).
		seed("rotation_sessions",
			`[{"id":"s1","status":"live","current_round":1,"round_minutes":12,
			   "bench":["pWait"]}]`).
		seed("rotation_players", `[
			{"id":"pA2","session_id":"s1","display_name":"A2","active":true},
			{"id":"pB1","session_id":"s1","display_name":"B1","active":true},
			{"id":"pB2","session_id":"s1","display_name":"B2","active":true},
			{"id":"pWait","session_id":"s1","display_name":"Wait","active":true},
			{"id":"pA1","session_id":"s1","display_name":"Gone","active":false}]`)
	s := newFakeSvc(t, f)

	if err := s.AdvanceRotationSession("s1", 1); err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	sent := string(f.rpcBodies("advance_rotation_session")[0])
	if strings.Contains(sent, "pA1") {
		t.Fatalf("a player who left is still seated next round: %s", sent)
	}
	if !strings.Contains(sent, "pWait") {
		t.Fatalf("the waiting player never took the free seat: %s", sent)
	}
}

// The sudden-death point IS the resolution. Refusing even after a winner was
// tapped would leave the organizer editing score cells to escape a hang.
func TestAdvance_ReportedWinnerBreaksATie(t *testing.T) {
	f := oneCourtSession(11, 11, 11, 11).
		seed("rotation_round_courts", `[{"session_id":"s1","round":1,"court":1,
			"team_a_p1":"pA1","team_a_p2":"pA2",
			"team_b_p1":"pB1","team_b_p2":"pB2","winner":"b"}]`)
	s := newFakeSvc(t, f)

	if err := s.AdvanceRotationSession("s1", 1); err != nil {
		t.Fatalf("a tie resolved by a sudden-death point still blocked: %v", err)
	}
}

// The only floor left: a session needs four people to be a session. Below that
// there is no layout to write, so say so rather than shrinking to nothing.
func TestSetActive_RefusesBelowFourPlayers(t *testing.T) {
	f := oneCourtSession(0, 0, 0, 0) // exactly four on the roster
	s := newFakeSvc(t, f)

	err := s.SetRotationPlayerActive("pA1", false)
	if err == nil {
		t.Fatal("a session was allowed to drop below four players")
	}
	if !strings.Contains(err.Error(), "end the session") {
		t.Fatalf("the refusal should name the way out; got %q", err)
	}
}

// Above that floor it is always allowed — the advance settles the empty seat,
// shrinking the room if nobody is waiting. Refusing here (the previous
// behaviour) left a full house with NO exit at all.
func TestSetActive_AllowedOnAFullHouseWithNobodyWaiting(t *testing.T) {
	f := oneCourtSession(0, 0, 0, 0).
		seed("rotation_players", `[
			{"id":"pA1","session_id":"s1","active":true},
			{"id":"pA2","session_id":"s1","active":true},
			{"id":"pB1","session_id":"s1","active":true},
			{"id":"pB2","session_id":"s1","active":true},
			{"id":"pE1","session_id":"s1","active":true}]`)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerActive("pA1", false); err != nil {
		t.Fatalf("a full house with an empty queue had no way to let someone go: %v", err)
	}
}

// Bringing back someone who was SUBSTITUTED out seats their replacement twice —
// the advance remap resolves the returning player onto the id that took over.
func TestSetActive_RefusesToResurrectASubstitutedPlayer(t *testing.T) {
	f := oneCourtSession(0, 0, 0, 0).
		seed("rotation_substitutions",
			`[{"session_id":"s1","round":1,"out_player":"pA1","in_player":"pSub"}]`)
	s := newFakeSvc(t, f)

	err := s.SetRotationPlayerActive("pA1", true)
	if err == nil {
		t.Fatal("a substituted-out player was brought back — their replacement would be seated twice")
	}
	if !strings.Contains(err.Error(), "took over") {
		t.Fatalf("the refusal should explain; got %q", err)
	}
}

// The grid still shows a departed player's row, so typing tonight's score into
// it is the natural mistake — and it orphans the score from the court.
func TestSetScore_RejectsRoundsAfterAHandover(t *testing.T) {
	f := oneCourtSession(0, 0, 0, 0).
		seed("rotation_substitutions",
			`[{"session_id":"s1","round":5,"out_player":"pA1","in_player":"pSub"}]`)
	s := newFakeSvc(t, f)

	v := 11
	if err := s.SetRotationScore("s1", 6, "pA1", &v); err == nil {
		t.Fatal("scored a round the player had already been substituted out of")
	}
	if err := s.SetRotationScore("s1", 4, "pSub", &v); err == nil {
		t.Fatal("scored a round before the substitute came on")
	}
}

// The client has to tell "a human must fix this" from "the server had a bad
// moment" — they need opposite handling, and every unclassified error leaves
// this layer as a 400, so the distinction has to be carried explicitly.
func TestAdvance_TieIsAHumanRefusal(t *testing.T) {
	f := oneCourtSession(11, 11, 11, 11)
	s := newFakeSvc(t, f)

	err := s.AdvanceRotationSession("s1", 1)
	if !errors.Is(err, ErrRoundBlocked) {
		t.Fatalf("a tie should be a refusal a human clears; got %v", err)
	}
	if errors.Is(err, ErrUpstream) {
		t.Fatal("a tie was classed as a transient failure — it would be retried forever")
	}
}
