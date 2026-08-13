package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// Someone's knee goes at round 5 and a friend takes over. The scores already on
// the board belong to the first player and must stay theirs; every round from
// here belongs to whoever actually played it.
//
// The SPLIT ITSELF now happens inside the rotation_substitute RPC, under the
// session's row lock — five separate writes from Go left gaps that could seat a
// phantom player or clobber a concurrent advance's queue. That means the
// interesting invariants (which round the swap takes effect, seat swap, queue
// position, retiring the outgoing player) are enforced in SQL and are NOT
// reachable from this fake, which returns canned responses. What Go still owns,
// and what these tests cover, is: validation before the call, the payload, and
// turning each refusal into something an organizer can act on.

func liveSession(round int, bench string) *fakeSupabase {
	return newFake().
		seed("rotation_sessions", `[{"id":"s1","status":"live","current_round":`+
			itoa(round)+`,"bench":`+bench+`}]`).
		seed("rotation_players",
			`[{"id":"pOut","session_id":"s1","display_name":"Ann","self_rating":4.5,"active":true}]`).
		seedRPC("rotation_substitute",
			`{"ok":true,"round":5,"player":{"id":"pIn","session_id":"s1","display_name":"Bea","active":true}}`)
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

// rpcPayload returns the body sent to the substitution RPC.
func rpcPayload(t *testing.T, f *fakeSupabase) map[string]any {
	t.Helper()
	for _, b := range f.rpcBodies("rotation_substitute") {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unparseable RPC body: %v", err)
		}
		return m
	}
	t.Fatal("the substitution RPC was never called")
	return nil
}

func TestSubstitute_CallsTheAtomicRPC(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	in, err := s.SubstituteRotationPlayer("s1", subReq())
	if err != nil {
		t.Fatalf("substitution failed: %v", err)
	}
	if in.ID != "pIn" || in.DisplayName != "Bea" {
		t.Fatalf("returned the wrong player: %+v", in)
	}
	p := rpcPayload(t, f)
	if p["p_session"] != "s1" || p["p_out"] != "pOut" || p["p_name"] != "Bea" {
		t.Fatalf("wrong payload: %v", p)
	}
	// Doing any of this from Go would reintroduce the partial-write races the
	// RPC exists to remove.
	if len(f.written("rotation_players")) != 0 ||
		len(f.written("rotation_round_courts")) != 0 ||
		len(f.written("rotation_substitutions")) != 0 {
		t.Fatal("the substitution wrote directly to tables instead of going through the RPC")
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
	if got := rpcPayload(t, f)["p_rating"]; got != 4.5 {
		t.Fatalf("substitute rated %v, want the 4.5 they inherited", got)
	}
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
	if got := rpcPayload(t, f)["p_rating"]; got != 3.0 {
		t.Fatalf("substitute rated %v, want the requested 3.0", got)
	}
}

// Covering a walk-off with someone who is already resting is the natural move,
// and it would give one human two roster rows — seated on two courts at once,
// splitting their own points.
func TestSubstitute_RefusesSomeoneAlreadyPlaying(t *testing.T) {
	f := liveSession(5, `[]`).
		seed("rotation_players", `[
			{"id":"pOut","session_id":"s1","display_name":"Ann","active":true},
			{"id":"pRest","session_id":"s1","display_name":"Bea","active":true}]`)
	s := newFakeSvc(t, f)

	_, err := s.SubstituteRotationPlayer("s1", subReq())
	if err == nil {
		t.Fatal("substituting in a player who is already in the session was allowed")
	}
	if !strings.Contains(err.Error(), "already playing") {
		t.Fatalf("the refusal should say why; got %q", err)
	}
	if len(f.rpcBodies("rotation_substitute")) != 0 {
		t.Fatal("a refused substitution still called the RPC")
	}
}

// Case and stray spacing are how the same person gets typed differently.
func TestSubstitute_DuplicateCheckIgnoresCaseAndSpacing(t *testing.T) {
	f := liveSession(5, `[]`).
		seed("rotation_players", `[
			{"id":"pOut","session_id":"s1","display_name":"Ann","active":true},
			{"id":"pRest","session_id":"s1","display_name":"  bea  ","active":true}]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err == nil {
		t.Fatal("'Bea' was accepted while '  bea  ' is already playing")
	}
}

// A player who already left is not a duplicate — their name must be reusable,
// which is what makes a player returning later work.
func TestSubstitute_RetiredNameIsNotADuplicate(t *testing.T) {
	f := liveSession(5, `[]`).
		seed("rotation_players", `[
			{"id":"pOut","session_id":"s1","display_name":"Ann","active":true},
			{"id":"pGone","session_id":"s1","display_name":"Bea","active":false}]`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != nil {
		t.Fatalf("a departed player's name was treated as a duplicate: %v", err)
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
	if len(f.rpcBodies("rotation_substitute")) != 0 {
		t.Fatal("a nameless substitution still called the RPC")
	}
}

// Newly callable mid-session, so an accidental paste shouldn't reach the live
// board and the TV scoreboard.
func TestSubstitute_CapsAbsurdNames(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	req := subReq()
	req.DisplayName = strings.Repeat("x", 5000)
	if _, err := s.SubstituteRotationPlayer("s1", req); err != nil {
		t.Fatalf("substitution failed: %v", err)
	}
	if got, _ := rpcPayload(t, f)["p_name"].(string); len(got) != 80 {
		t.Fatalf("name reached the datastore at %d chars", len(got))
	}
}

// Each refusal has to tell the organizer what to do instead — they are standing
// on a court with people waiting.
func TestSubstitute_RefusalsAreActionable(t *testing.T) {
	cases := []struct{ reason, want string }{
		{"not_started", "remove"},
		{"finished", "finished"},
		{"out_not_active", "already out"},
		{"already_substituted", "already taken over"},
	}
	for _, c := range cases {
		f := liveSession(5, `[]`).
			seedRPC("rotation_substitute", `{"ok":false,"reason":"`+c.reason+`"}`)
		s := newFakeSvc(t, f)

		_, err := s.SubstituteRotationPlayer("s1", subReq())
		if err == nil {
			t.Errorf("%s: was accepted", c.reason)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: message %q doesn't mention %q", c.reason, err, c.want)
		}
	}
}

func TestSubstitute_MissingSessionIsNotFound(t *testing.T) {
	f := liveSession(5, `[]`).
		seedRPC("rotation_substitute", `{"ok":false,"reason":"no_session"}`)
	s := newFakeSvc(t, f)

	if _, err := s.SubstituteRotationPlayer("s1", subReq()); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
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
	if len(f.rpcBodies("rotation_substitute")) != 0 {
		t.Fatal("a rename triggered a substitution")
	}
}

func TestSetName_RejectsBlank(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerName("pOut", "   "); err == nil {
		t.Fatal("a blank name was accepted")
	}
}

// A player rolls an ankle at round 4 and nobody can replace them. Without a live
// sit-out the organizer has to invent a fake substitute, or leave a ghost
// holding a seat and being scored all night.
func TestSetActive_SittingOutIsAllowedMidSession(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerActive("pOut", false); err != nil {
		t.Fatalf("sitting a player out mid-session was refused: %v", err)
	}
	writes := f.written("rotation_players")
	if len(writes) != 1 || writes[0]["active"] != false {
		t.Fatalf("wrote %v", writes)
	}
}

func TestSetActive_BringingBackIsAllowedMidSession(t *testing.T) {
	f := liveSession(5, `[]`)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerActive("pOut", true); err != nil {
		t.Fatalf("bringing a player back mid-session was refused: %v", err)
	}
}

func TestSetActive_UnknownPlayerIsNotFound(t *testing.T) {
	f := newFake().seed("rotation_sessions", `[{"id":"s1","status":"live"}]`)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerActive("nope", false); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
