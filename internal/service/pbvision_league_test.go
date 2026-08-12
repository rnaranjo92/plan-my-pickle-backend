package service

import (
	"os"
	"testing"
)

// InPBVisionLeague decides whether someone outside the email beta allowlist may
// run a PB Vision analysis. Every run bills our PB Vision key, so the interesting
// property is that it stays CLOSED in every ambiguous case.

func TestInPBVisionLeague_GrantedLeaguePlayerAllowed(t *testing.T) {
	t.Setenv("PBVISION_LEAGUE_ALLOWLIST", "lg-granted")
	f := newFake().
		seed("league_members", `[{"id":"lm1","league_id":"lg-granted","user_id":"u1"}]`)
	s := newFakeSvc(t, f)

	if !s.InPBVisionLeague("u1", "a@b.com") {
		t.Fatal("a player in the granted league was refused")
	}
}

func TestInPBVisionLeague_OtherLeagueRefused(t *testing.T) {
	t.Setenv("PBVISION_LEAGUE_ALLOWLIST", "lg-granted")
	f := newFake().
		seed("league_members", `[{"id":"lm1","league_id":"lg-somewhere-else","user_id":"u1"}]`)
	s := newFakeSvc(t, f)

	if s.InPBVisionLeague("u1", "a@b.com") {
		t.Fatal("a player from an UNGRANTED league was allowed — this bills our key")
	}
}

func TestInPBVisionLeague_NoLeaguesRefused(t *testing.T) {
	t.Setenv("PBVISION_LEAGUE_ALLOWLIST", "lg-granted")
	s := newFakeSvc(t, newFake())

	if s.InPBVisionLeague("u1", "a@b.com") {
		t.Fatal("a caller with no leagues was allowed")
	}
}

// Emptying the env is the documented kill switch ("set it to a bogus value to
// revoke instantly"). An empty list must revoke, NOT silently fall back to the
// built-in default — otherwise the switch doesn't switch anything off.
func TestInPBVisionLeague_BogusEnvRevokes(t *testing.T) {
	t.Setenv("PBVISION_LEAGUE_ALLOWLIST", "none")
	f := newFake().
		seed("league_members", `[{"id":"lm1","league_id":"`+pbVisionLeagueGrants+`","user_id":"u1"}]`)
	s := newFakeSvc(t, f)

	if s.InPBVisionLeague("u1", "a@b.com") {
		t.Fatal("the env kill switch did not revoke the built-in grant")
	}
}

// With no env set the built-in grant applies, so the feature works in prod
// without anyone having to remember to set a variable.
func TestInPBVisionLeague_DefaultGrantApplies(t *testing.T) {
	os.Unsetenv("PBVISION_LEAGUE_ALLOWLIST")
	f := newFake().
		seed("league_members", `[{"id":"lm1","league_id":"`+pbVisionLeagueGrants+`","user_id":"u1"}]`)
	s := newFakeSvc(t, f)

	if !s.InPBVisionLeague("u1", "a@b.com") {
		t.Fatal("the built-in league grant did not apply with no env set")
	}
}
