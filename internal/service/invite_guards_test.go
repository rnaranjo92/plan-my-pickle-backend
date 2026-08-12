package service

import (
	"strings"
	"testing"
)

// InviteRegistrant is the one path that messages a rostered player, and invites
// are deliberately NOT gated on the SMS-consent box (they're a single
// transactional message, like League -> Members has always sent). These pin the
// guards that make that safe.

func inviteFake(playerRow string) *fakeSupabase {
	return newFake().
		seed("events", `[{"id":"e1","owner_id":"o1","name":"Fall Slam"}]`).
		seed("registrations", `[{"id":"r1","player_id":"p1"}]`).
		seed("players", "["+playerRow+"]")
}

func TestInviteRegistrant_RefusesPlaceholder(t *testing.T) {
	s := newFakeSvc(t, inviteFake(
		`{"id":"p1","full_name":"Jane Doe","phone":"+15553000001","email":""}`))

	_, _, err := s.InviteRegistrant("e1", "r1", "o1", "o@b.com")
	if err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("placeholder invite = %v, want a placeholder refusal "+
			"(it would text a number that doesn't exist)", err)
	}
}

func TestInviteRegistrant_RefusesBlockedContact(t *testing.T) {
	f := inviteFake(`{"id":"p1","full_name":"Jane Doe","phone":"(619) 555-0100","email":""}`).
		seed("blocked_contacts", `[{"id":"b1"}]`)
	s := newFakeSvc(t, f)

	_, _, err := s.InviteRegistrant("e1", "r1", "o1", "o@b.com")
	if err == nil {
		t.Fatal("invited a BLOCKED contact — the denylist is what protects opt-outs")
	}
	if strings.Contains(strings.ToLower(err.Error()), "block") {
		t.Fatalf("refusal names the block (%q) — should stay neutral", err)
	}
}

func TestInviteRegistrant_RefusesAlreadyOnTheApp(t *testing.T) {
	s := newFakeSvc(t, inviteFake(
		`{"id":"p1","full_name":"Jane Doe","phone":"(619) 555-0100","user_id":"u9"}`))

	_, _, err := s.InviteRegistrant("e1", "r1", "o1", "o@b.com")
	if err == nil || !strings.Contains(err.Error(), "already on the app") {
		t.Fatalf("invite to a linked account = %v, want an already-on-the-app refusal", err)
	}
}

func TestInviteRegistrant_RefusesNonOwner(t *testing.T) {
	s := newFakeSvc(t, inviteFake(
		`{"id":"p1","full_name":"Jane Doe","phone":"(619) 555-0100"}`))

	if _, _, err := s.InviteRegistrant("e1", "r1", "someone-else", "x@b.com"); err == nil {
		t.Fatal("a non-owner could message another organizer's roster")
	}
}
