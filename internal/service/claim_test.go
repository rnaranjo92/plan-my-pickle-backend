package service

import (
	"strings"
	"testing"
)

// Claiming hands someone an identity: their name on a roster, their match
// history, and access to that event's private feed. These pin the boundaries.

func claimFake(playerRow string) *fakeSupabase {
	return newFake().
		seed("events", `[{"id":"e1","owner_id":"o1"}]`).
		seed("registrations", `[{"id":"r1","event_id":"e1","player_id":"p1","claim_token":"tok-1"}]`).
		seed("players", "["+playerRow+"]")
}

// A row that already belongs to someone must never be re-claimed — that would
// hand one player's history and feed access to another.
func TestClaim_RefusesAlreadyOwnedRow(t *testing.T) {
	s := newFakeSvc(t, claimFake(`{"id":"p1","user_id":"someone-else"}`))

	_, err := s.ClaimRegistrationByToken("tok-1", "u-new", "new@b.com")
	if err == nil {
		t.Fatal("claimed a row that already belongs to another account")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("err = %q, want an already-claimed refusal", err)
	}
}

// Re-claiming your OWN row is a no-op, not an error — a double-tap or a
// re-opened link shouldn't look like a failure.
func TestClaim_OwnRowIsIdempotent(t *testing.T) {
	s := newFakeSvc(t, claimFake(`{"id":"p1","user_id":"u1"}`))

	if _, err := s.ClaimRegistrationByToken("tok-1", "u1", "a@b.com"); err != nil {
		t.Fatalf("re-claiming my own row errored: %v", err)
	}
}

func TestClaim_RefusesPlaceholderRow(t *testing.T) {
	s := newFakeSvc(t, claimFake(`{"id":"p1","phone":"+15553000001"}`))

	_, err := s.ClaimRegistrationByToken("tok-1", "u1", "a@b.com")
	if err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("claimed a placeholder row: %v", err)
	}
}

func TestClaim_UnknownTokenIsNotFound(t *testing.T) {
	f := newFake().
		seed("events", `[{"id":"e1","owner_id":"o1"}]`).
		seed("players", `[{"id":"p1"}]`)
	s := newFakeSvc(t, f)

	if _, err := s.ClaimRegistrationByToken("no-such-token", "u1", "a@b.com"); err == nil {
		t.Fatal("an unknown claim token succeeded")
	}
}

func TestClaim_AnonymousRefused(t *testing.T) {
	s := newFakeSvc(t, claimFake(`{"id":"p1"}`))

	if _, err := s.ClaimRegistrationByToken("tok-1", "", ""); err == nil {
		t.Fatal("an anonymous caller claimed a roster row")
	}
}

// The court-side "pick your name" list is for casual events only. On a league,
// standings persist for a season and every player already has a contact to match
// on, so self-serve claiming must be refused outright.
func TestClaimable_RefusedOnLeagues(t *testing.T) {
	f := newFake().seed("events", `[{"id":"e1","league_id":"lg1"}]`)
	s := newFakeSvc(t, f)

	if _, err := s.ClaimableEntries("e1"); err == nil {
		t.Fatal("a league exposed its roster to self-serve claiming")
	}
	if err := s.ClaimRegistrationByID("e1", "r1", "u1", "a@b.com"); err == nil {
		t.Fatal("a league allowed a self-serve claim")
	}
}

// Minting a link for a player who is already on the app would hand out a live
// token for a real person's identity.
func TestClaimLink_RefusedForLinkedPlayer(t *testing.T) {
	s := newFakeSvc(t, claimFake(`{"id":"p1","user_id":"u9"}`))

	if _, err := s.RegistrationClaimToken("e1", "r1", "o1", "o@b.com"); err == nil {
		t.Fatal("minted a claim link for a player already on the app")
	}
}

func TestClaimLink_RefusedForNonOwner(t *testing.T) {
	s := newFakeSvc(t, claimFake(`{"id":"p1"}`))

	if _, err := s.RegistrationClaimToken("e1", "r1", "not-the-owner", "x@b.com"); err == nil {
		t.Fatal("a non-owner minted a claim link for someone else's event")
	}
}
