package service

import (
	"strings"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// The unsubscribe link is public and unauthenticated -- it's clicked from an
// email client, often on a device that isn't signed in. The signature is the
// only thing stopping someone walking the endpoint and opting other people out.
func TestDigestUnsubscribeRefusesAForgedToken(t *testing.T) {
	f := newFake()
	s := newFakeSvc(t, f)
	if err := s.DigestUnsubscribe("u1", "not-the-token"); err != ErrForbidden {
		t.Fatalf("a forged token was accepted: %v", err)
	}
	if err := s.DigestUnsubscribe("u1", ""); err != ErrForbidden {
		t.Fatalf("an empty token was accepted: %v", err)
	}
	if len(f.written("pmp_profiles")) != 0 {
		t.Fatal("a refused unsubscribe still wrote to the profile")
	}
}

// ...and the real link works, or the email is legally a problem.
func TestDigestUnsubscribeAcceptsItsOwnLink(t *testing.T) {
	f := newFake()
	s := newFakeSvc(t, f)
	if err := s.DigestUnsubscribe("u1", digestToken("u1")); err != nil {
		t.Fatalf("our own token was refused: %v", err)
	}
	wrote := f.written("pmp_profiles")
	if len(wrote) == 0 || wrote[0]["digest_opt_out"] != true {
		t.Fatalf("unsubscribe didn't set the opt-out: %+v", wrote)
	}
}

// A token for one account must not unsubscribe another.
func TestDigestTokenIsPerAccount(t *testing.T) {
	if digestToken("u1") == digestToken("u2") {
		t.Fatal("two accounts share an unsubscribe token")
	}
}

// Every email carries a working unsubscribe. This is CAN-SPAM, not polish.
func TestDigestEmailAlwaysCarriesAnUnsubscribeLink(t *testing.T) {
	name := "Kim Naranjo"
	starts := "2030-01-04T18:00:00Z"
	evs := []model.PublicEvent{{ID: "e1", Name: "Tuesday Night League", StartsAt: &starts}}
	subject, htmlBody, text := digestEmail(name, "San Diego, CA", evs, "u1")
	if !strings.Contains(htmlBody, digestToken("u1")) {
		t.Fatal("the HTML part has no signed unsubscribe link")
	}
	if !strings.Contains(text, "Unsubscribe") {
		t.Fatal("the plain-text part has no unsubscribe link")
	}
	// Greeted by first name only: this is a note, not a letter.
	if !strings.Contains(htmlBody, "Kim,") {
		t.Fatalf("expected a first-name greeting, got: %s", htmlBody)
	}
	if !strings.Contains(subject, "1 pickleball event near you") {
		t.Fatalf("subject doesn't say what's inside: %q", subject)
	}
}

// THE BUG THAT WOULD HAVE REACHED NOBODY.
//
// The recipient query used to require a stored county. pmp_profiles.county went
// years with nothing writing it -- which is the whole reason userHomeCounty has
// a fallback to the events somebody owns or plays in -- so that filter threw the
// fallback away and left the digest with almost no recipients.
//
// The fake ignores query filters, so this asserts the RESOLUTION: a profile with
// a blank county still gets placed, from an event they own.
func TestDigestRecipientsFallBackToWhereTheyPlay(t *testing.T) {
	f := newFake().
		seed("pmp_profiles", `[{"user_id":"u1","full_name":"Kim Naranjo","county":"","state":""}]`).
		seed("players", `[{"user_id":"u1","email":"kim@example.com"}]`).
		seed("events", `[{"county":"San Diego","state":"CA","owner_id":"u1"}]`)
	s := newFakeSvc(t, f)
	got := s.digestRecipients()
	if len(got) != 1 {
		t.Fatalf("a blank county should fall back, not disqualify; got %d", len(got))
	}
	if got[0].place != "San Diego, CA" {
		t.Fatalf("place not resolved from their events: %q", got[0].place)
	}
}

// Somebody we genuinely cannot place is not a recipient -- better no email than
// one headed "near ," listing events from a county it never names.
func TestDigestRecipientsSkipAnyoneUnplaceable(t *testing.T) {
	f := newFake().
		seed("pmp_profiles", `[{"user_id":"u1","full_name":"Nobody","county":"","state":""}]`).
		seed("players", `[{"user_id":"u1","email":"n@example.com"}]`).
		seed("events", `[]`).
		seed("registrations", `[]`)
	s := newFakeSvc(t, f)
	if got := s.digestRecipients(); len(got) != 0 {
		t.Fatalf("an unplaceable person became a recipient: %+v", got)
	}
}
