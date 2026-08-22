package service

import "testing"

// Phone numbers live behind this endpoint, so the gate IS the feature: only
// someone who runs the club gets an answer at all.
func TestClubMemberProfileIsAdminOnly(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"LT Test","owner_id":"boss"}]`).
		seed("club_members", `[]`)
	s := newFakeSvc(t, f)
	if _, err := s.ClubMemberProfileFor("c1", "someone", "stranger"); err == nil {
		t.Fatal("a non-admin read a member's profile")
	}
}

func TestClubMemberProfileCarriesContactForTheAdmin(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"LT Test","owner_id":"boss"}]`).
		seed("club_members", `[{"club_id":"c1","user_id":"m1","role":"member","created_at":"2026-08-01T00:00:00Z"}]`).
		seed("players", `[{"id":"p1","user_id":"m1","email":"m@x.com","phone":"5551234","dupr_rating":3.5}]`).
		seed("events", `[]`).
		seed("registrations", `[]`).
		seed("match_participants", `[]`)
	s := newFakeSvc(t, f)
	// The fake ignores filters, so "boss" passes OwnsClub via the seeded club.
	prof, err := s.ClubMemberProfileFor("c1", "m1", "boss")
	if err != nil {
		t.Fatalf("admin read failed: %v", err)
	}
	if prof.Email != "m@x.com" || prof.Phone != "5551234" {
		t.Fatalf("contact missing: %+v", prof)
	}
	if prof.DoublesRating == nil || *prof.DoublesRating != 3.5 {
		t.Fatalf("dupr rating missing: %+v", prof.DoublesRating)
	}
	if prof.JoinedAt == "" {
		t.Fatal("joined date missing")
	}
}
