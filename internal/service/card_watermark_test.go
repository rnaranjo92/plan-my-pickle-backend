package service

import "testing"

// A card is the PLAYER'S. Wearing a club's mark has to be opt-in AND limited to
// clubs they belong to — otherwise anyone can stamp any organisation's logo on
// their own identity, which is the whole reason this is gated server-side
// rather than just hidden in the picker.
func TestSetCardStyleRefusesAClubYouDoNotBelongTo(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","owner_id":"someone-else"}]`).
		seed("club_members", `[]`)
	s := newFakeSvc(t, f)
	other := "c1"
	if err := s.SetCardStyle("stranger", nil, nil, nil, &other); err == nil {
		t.Fatal("a stranger was allowed to wear a club's mark")
	}
	if len(f.written("pmp_profiles")) != 0 {
		t.Fatal("a rejected watermark still wrote to the profile")
	}
}

// A member may wear their own club's mark.
func TestSetCardStyleAcceptsYourOwnClub(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","owner_id":"owner1"}]`).
		seed("club_members", `[{"club_id":"c1","user_id":"u1"}]`).
		seed("pmp_profiles", `[{"user_id":"u1"}]`)
	s := newFakeSvc(t, f)
	mine := "c1"
	if err := s.SetCardStyle("u1", nil, nil, nil, &mine); err != nil {
		t.Fatalf("a member was refused their own club: %v", err)
	}
}

// THE POINTER TRAP, restated for this axis. "" means "take the mark off" and is
// a real choice; nil means "this request isn't about the watermark". Conflating
// them would make turning it off impossible — the same bug the theme/font/
// pattern fields were written as pointers to avoid.
func TestSetCardStyleTreatsEmptyAsTakingTheMarkOff(t *testing.T) {
	f := newFake().seed("pmp_profiles", `[{"user_id":"u1"}]`)
	s := newFakeSvc(t, f)
	off := ""
	if err := s.SetCardStyle("u1", nil, nil, nil, &off); err != nil {
		t.Fatalf("turning the watermark off was refused: %v", err)
	}
	wrote := f.written("pmp_profiles")
	if len(wrote) == 0 {
		t.Fatal(`"" must be WRITTEN — that is how the mark comes off`)
	}
	if got, ok := wrote[0]["card_club_watermark"]; !ok || got != "" {
		t.Fatalf("want an empty watermark written, got %v", wrote[0])
	}
}
