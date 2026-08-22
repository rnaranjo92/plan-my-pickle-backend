package service

import (
	"strings"
	"testing"
)

// A private club takes members by INVITATION. These are the two halves of that
// sentence, and getting the second one wrong is the quiet failure: an invitation
// the invitee cannot accept is not an invitation.
func TestPrivateClubRefusesAnUninvitedJoinRequest(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"The Locals","owner_id":"owner","is_public":false}]`).
		// EMPTY on purpose. The fake ignores query filters, so ANY seeded member
		// row makes isClubMember true for everybody — and both of these tests
		// would sail through the "already in" shortcut without ever reaching the
		// rule they claim to check.
		seed("club_members", `[]`).
		seed("club_join_requests", `[]`)
	s := newFakeSvc(t, f)
	joined, err := s.RequestJoinClub("c1", "stranger")
	if err == nil {
		t.Fatal("a stranger should not be able to ask to join a private club")
	}
	if joined {
		t.Fatal("refused, yet reported as joined")
	}
}

func TestPrivateClubStillAdmitsSomeoneItInvited(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"The Locals","owner_id":"owner","is_public":false}]`).
		// EMPTY on purpose. The fake ignores query filters, so ANY seeded member
		// row makes isClubMember true for everybody — and both of these tests
		// would sail through the "already in" shortcut without ever reaching the
		// rule they claim to check.
		seed("club_members", `[]`).
		// invited=true is the club having already chosen this person.
		seed("club_join_requests", `[{"club_id":"c1","user_id":"guest","invited":true}]`)
	s := newFakeSvc(t, f)
	joined, err := s.RequestJoinClub("c1", "guest")
	if err != nil {
		t.Fatalf("an invited person must still be admitted: %v", err)
	}
	if !joined {
		t.Fatal("invited person was not admitted")
	}
}

// The flag was added later with `default true`, so a row read before the
// migration has no such key. Reading that as false would make every club on the
// platform private the moment this code deployed ahead of the SQL.
func TestMissingVisibilityColumnMeansPublic(t *testing.T) {
	if !asBoolDefaultTrue(map[string]any{}, "is_public") {
		t.Fatal("an absent flag must read as PUBLIC, matching the column default")
	}
	if !asBoolDefaultTrue(map[string]any{"is_public": nil}, "is_public") {
		t.Fatal("a null flag must read as public")
	}
	if asBoolDefaultTrue(map[string]any{"is_public": false}, "is_public") {
		t.Fatal("an explicit false must be honoured")
	}
	if !asBoolDefaultTrue(map[string]any{"is_public": true}, "is_public") {
		t.Fatal("an explicit true must be honoured")
	}
}

// Private means UNPUBLISHED, not unreachable: no crawlable page, while the
// in-app link an invitee was sent keeps working.
func TestPrivateClubHasNoCrawlablePage(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"The Locals","owner_id":"owner","is_public":false}]`)
	s := newFakeSvc(t, f)
	if _, _, _, _, err := s.PublicClubByID("c1"); err != ErrNotFound {
		t.Fatalf("a private club must not have a public page; got %v", err)
	}
}

// An appointment nobody is told about is head office believing a site is
// covered while the appointee has no idea. Removal stays silent on purpose
// (the app must not announce a revocation on the org's behalf), so the test
// pins BOTH sides of that line.
func TestOrgAppointmentNotifiesTheAppointee(t *testing.T) {
	f := newFake().
		seed("organizations", `[{"id":"o1","name":"Life Time","owner_id":"boss"}]`).
		seed("organization_members", `[]`)
	s := newFakeSvc(t, f)
	if err := s.SetOrgRole("o1", "boss", "manager", "admin"); err != nil {
		t.Fatalf("appoint: %v", err)
	}
	wrote := f.written("user_notifications")
	if len(wrote) != 1 {
		t.Fatalf("expected exactly one bell entry, got %d", len(wrote))
	}
	if got := wrote[0]["type"]; got != "org_role" {
		t.Fatalf("wrong type: %v", got)
	}
	if got, _ := wrote[0]["body"].(string); !strings.Contains(got, "Life Time") {
		t.Fatalf("the message should name the organization: %q", got)
	}
	if got := wrote[0]["link"]; got != "org:o1" {
		t.Fatalf("wrong link: %v", got)
	}
}

func TestOrgRemovalStaysSilent(t *testing.T) {
	f := newFake().
		seed("organizations", `[{"id":"o1","name":"Life Time","owner_id":"boss"}]`).
		seed("organization_members", `[{"org_id":"o1","user_id":"manager","role":"admin"}]`)
	s := newFakeSvc(t, f)
	if err := s.RemoveOrgMember("o1", "boss", "manager"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if wrote := f.written("user_notifications"); len(wrote) != 0 {
		t.Fatalf("removal must not notify; wrote %v", wrote)
	}
}

// Rename/contact were immutable and orgs undeletable. Owner-only, both.
func TestOrgUpdateAndDeleteAreOwnerOnly(t *testing.T) {
	f := newFake().
		seed("organizations", `[{"id":"o1","name":"Old Name","owner_id":"boss"}]`).
		seed("organization_members", `[]`)
	s := newFakeSvc(t, f)
	if err := s.UpdateOrganization("o1", "someone-else", "New", ""); err == nil {
		t.Fatal("a non-owner renamed the organization")
	}
	if err := s.DeleteOrganization("o1", "someone-else"); err == nil {
		t.Fatal("a non-owner deleted the organization")
	}
	if err := s.UpdateOrganization("o1", "boss", "New Name", "hq@x.com"); err != nil {
		t.Fatalf("owner rename: %v", err)
	}
	if err := s.UpdateOrganization("o1", "boss", "  ", ""); err == nil {
		t.Fatal("a blank name should be refused")
	}
	if err := s.DeleteOrganization("o1", "boss"); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
}
