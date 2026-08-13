package service

import (
	"strings"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// Someone's knee goes at round 5 and a friend takes over. The scores already on
// the board belong to the first player and must stay theirs; every round from
// here belongs to whoever actually played it. That split is the whole feature —
// which is why a substitution creates a roster row rather than renaming one.

func liveSession(round int, bench string) *fakeSupabase {
	return newFake().
		seed("rotation_sessions", `[{"id":"s1","status":"live","current_round":`+
			itoa(round)+`,"bench":`+bench+`}]`).
		seed("rotation_players",
			`[{"id":"pOut","session_id":"s1","display_name":"Ann","self_rating":4.5,"active":true}]`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}

func subReq() model.SubstituteRotationPlayerRequest {
	return model.SubstituteRotationPlayerRequest{OutPlayerID: "pOut", DisplayName: "Bea"}
}

// The substitute must be a NEW row. Renaming the outgoing player would hand
// every point already scored to someone who never played those rounds.
func TestSubstitute_CreatesANewRosterRow(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}

	var inserted map[string]any
	for _, w := range f.targetedWrites("rotation_players") {
		if w.row["display_name"] == "Bea" {
			inserted = w.row
		}
	}
	if inserted == nil {
		t.Fatal("no roster row was created for the substitute")
	}
	if inserted["session_id"] != "s1" {
		t.Errorf("substitute landed in session %v, want s1", inserted["session_id"])
	}
	// Nothing may rewrite the outgoing player's NAME — that is the exact mistake
	// this design exists to prevent.
	for _, w := range f.targetedWrites("rotation_players") {
		if idOf(w.filter) == "pOut" {
			if _, renamed := w.row["display_name"]; renamed {
				t.Fatal("the outgoing player was renamed — their score would move to the substitute")
			}
		}
	}
}

// The outgoing player has to leave the active roster, or the reconciliation on
// the next advance will seat them again alongside their own replacement.
func TestSubstitute_RetiresTheOutgoingPlayer(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}

	retired := false
	for _, w := range f.targetedWrites("rotation_players") {
		if idOf(w.filter) == "pOut" && w.row["active"] == false {
			retired = true
		}
	}
	if !retired {
		t.Fatal("the outgoing player is still active — they'd rotate back in")
	}
}

// The seat swap must cover every one of the four positions on a court: a
// substitute who is only ever checked into team A's first slot silently fails
// for three players out of four.
func TestSubstitute_TakesTheSeatInEveryPosition(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}

	want := map[string]bool{
		"team_a_p1": false, "team_a_p2": false,
		"team_b_p1": false, "team_b_p2": false,
	}
	for _, w := range f.targetedWrites("rotation_round_courts") {
		for col := range want {
			if _, set := w.row[col]; set && strings.Contains(w.filter, col+"=eq.pOut") {
				want[col] = true
			}
		}
	}
	for col, covered := range want {
		if !covered {
			t.Errorf("a player sitting in %s would never be substituted", col)
		}
	}
}

// The swap is filtered round >= current, not round = current. An advance landing
// between reading the round and writing the seat would otherwise strand the
// substitute in a round that has already finished.
func TestSubstitute_SeatSwapCoversLaterRounds(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}

	writes := f.targetedWrites("rotation_round_courts")
	if len(writes) == 0 {
		t.Fatal("no seat swap was attempted")
	}
	for _, w := range writes {
		if !strings.Contains(w.filter, "round=gte.5") {
			t.Fatalf("seat swap filtered %q — an advance mid-call would strand the substitute", w.filter)
		}
	}
}

// A waiting player's replacement inherits their PLACE in the queue. Sending them
// to the back would make the substitute wait a second full turn for a rest the
// player they replaced had already served.
func TestSubstitute_InheritsTheBenchPosition(t *testing.T) {
	f := liveSession(5, `["pX","pOut","pY"]`)
	s := newFakeSvc(t, f)

	in, err := s.SubstituteRotationPlayer("s1", subReq())
	if err != nil {
		t.Fatalf("substitution failed: %v", err)
	}

	var bench []any
	for _, w := range f.targetedWrites("rotation_sessions") {
		if b, ok := w.row["bench"].([]any); ok {
			bench = b
		}
	}
	if bench == nil {
		t.Fatal("the waiting queue was never updated")
	}
	if len(bench) != 3 {
		t.Fatalf("queue length changed to %d, want 3: %v", len(bench), bench)
	}
	if bench[1] != in.ID {
		t.Fatalf("substitute landed at %v, want position 1 (their queue slot): %v", bench[1], bench)
	}
	if bench[0] != "pX" || bench[2] != "pY" {
		t.Fatalf("the rest of the queue moved: %v", bench)
	}
}

// A player who was on a court, not waiting, must not be written into the queue.
func TestSubstitute_PlayingPlayerLeavesTheQueueAlone(t *testing.T) {
	f := liveSession(5, `["pX","pY"]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}
	for _, w := range f.targetedWrites("rotation_sessions") {
		if _, touched := w.row["bench"]; touched {
			t.Fatal("substituting a PLAYING player rewrote the waiting queue")
		}
	}
}

// Rating carries over. Defaulting to 3.0 would misplace the substitute the
// moment the courts reseed — they're stepping onto the court they inherited.
func TestSubstitute_InheritsTheRating(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}
	for _, w := range f.targetedWrites("rotation_players") {
		if w.row["display_name"] == "Bea" {
			if got, _ := w.row["self_rating"].(float64); got != 4.5 {
				t.Fatalf("substitute rated %v, want the 4.5 they inherited", got)
			}
			return
		}
	}
	t.Fatal("no substitute row was written")
}

// An explicit rating beats the inherited one — the organizer knows who walked in.
func TestSubstitute_ExplicitRatingWins(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	req := subReq()
	req.SelfRating = 3.0
	if _, err := s.SubstituteRotationPlayer("s1", req); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}
	for _, w := range f.targetedWrites("rotation_players") {
		if w.row["display_name"] == "Bea" {
			if got, _ := w.row["self_rating"].(float64); got != 3.0 {
				t.Fatalf("substitute rated %v, want the requested 3.0", got)
			}
			return
		}
	}
	t.Fatal("no substitute row was written")
}

// Nothing has been played in setup, so there is no record to split. Allowing it
// would leave a phantom "subbed out at round 0" in the history.
func TestSubstitute_RefusedBeforeTheSessionStarts(t *testing.T) {
	f := liveSession(0, `[]`).
		seed("rotation_sessions", `[{"id":"s1","status":"setup","current_round":0,"bench":[]}]`)
	s := newFakeSvc(t, f)

	_, err := s.SubstituteRotationPlayer("s1", subReq())
	if err == nil {
		t.Fatal("a substitution was accepted before the session started")
	}
	if !strings.Contains(err.Error(), "remove") {
		t.Fatalf("the refusal should point at remove/add; got %q", err)
	}
	if len(f.written("rotation_players")) != 0 {
		t.Fatal("a refused substitution still wrote a roster row")
	}
}

func TestSubstitute_RefusedOnAFinishedSession(t *testing.T) {
	f := liveSession(5, `[]`).
		seed("rotation_sessions", `[{"id":"s1","status":"done","current_round":5,"bench":[]}]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err == nil {
		t.Fatal("a finished session accepted a substitution")
	}
}

// Substituting someone who already left would retire a row twice and record a
// swap for a seat nobody holds.
func TestSubstitute_RefusedForAnAlreadyRetiredPlayer(t *testing.T) {
	f := liveSession(5, `[]`).
		seed("rotation_players",
			`[{"id":"pOut","session_id":"s1","display_name":"Ann","active":false}]`)
	s := newFakeSvc(t, f)

	_, err := s.SubstituteRotationPlayer("s1", subReq())
	if err == nil {
		t.Fatal("a player who already left was substituted again")
	}
	if !strings.Contains(err.Error(), "Ann") {
		t.Fatalf("the refusal should name the player; got %q", err)
	}
}

func TestSubstitute_RequiresAName(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	req := subReq()
	req.DisplayName = "   "
	if _, err := s.SubstituteRotationPlayer("s1", req); err == nil {
		t.Fatal("a nameless substitute was accepted")
	}
}

// The swap is recorded, and at the round it took effect — that round is the
// boundary between the two players' records.
func TestSubstitute_RecordsTheSwapAtTheCurrentRound(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	in, err := s.SubstituteRotationPlayer("s1", subReq())
	if err != nil {
		t.Fatalf("substitution failed: %v", err)
	}
	rows := f.written("rotation_substitutions")
	if len(rows) != 1 {
		t.Fatalf("want 1 recorded swap, got %d", len(rows))
	}
	if rows[0]["round"] != float64(5) {
		t.Errorf("swap recorded at round %v, want 5", rows[0]["round"])
	}
	if rows[0]["out_player"] != "pOut" || rows[0]["in_player"] != in.ID {
		t.Errorf("swap recorded the wrong pair: %v", rows[0])
	}
}

// A chain (Ann -> Bea -> Cal) needs no special handling: the substitute is an
// ordinary roster player, so subbing THEM out is the same call. What must hold
// is that each link is recorded separately, so the history can be walked.
func TestSubstitute_ChainsWithoutSpecialCasing(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("first substitution failed: %v", err)
	}
	// Bea is now an ordinary active roster row; sub her out for Cal at round 8.
	f.seed("rotation_sessions", `[{"id":"s1","status":"live","current_round":8,"bench":[]}]`).
		seed("rotation_players",
			`[{"id":"pBea","session_id":"s1","display_name":"Bea","active":true}]`)
	if _, err := s.SubstituteRotationPlayer("s1",
		model.SubstituteRotationPlayerRequest{OutPlayerID: "pBea", DisplayName: "Cal"}); err != nil {
		t.Fatalf("second substitution failed: %v", err)
	}

	rows := f.written("rotation_substitutions")
	if len(rows) != 2 {
		t.Fatalf("want 2 links in the chain, got %d", len(rows))
	}
	if rows[0]["out_player"] != "pOut" || rows[1]["out_player"] != "pBea" {
		t.Fatalf("the chain didn't record both links in order: %v", rows)
	}
	if rows[0]["round"] != float64(5) || rows[1]["round"] != float64(8) {
		t.Fatalf("chain rounds wrong, want 5 then 8: %v", rows)
	}
}

// Renaming is a DIFFERENT operation: same person, fixed spelling, record intact.
func TestSetName_RenamesWithoutTouchingTheRecord(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerName("pOut", "Ann Marie"); err != nil {
		t.Fatalf("rename failed: %v", err)
	}
	writes := f.written("rotation_players")
	if len(writes) != 1 {
		t.Fatalf("want exactly one write, got %d", len(writes))
	}
	if writes[0]["display_name"] != "Ann Marie" {
		t.Fatalf("wrote %v", writes[0])
	}
	if _, touched := writes[0]["active"]; touched {
		t.Fatal("a rename retired the player — that's a substitution, not a rename")
	}
	if len(f.written("rotation_substitutions")) != 0 {
		t.Fatal("a rename recorded a substitution")
	}
}

func TestSetName_RejectsBlank(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerName("pOut", "   "); err == nil {
		t.Fatal("a blank name was accepted")
	}
}
