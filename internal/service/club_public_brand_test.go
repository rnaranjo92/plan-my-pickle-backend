package service

// The public /club/{id} page carries the club's brand colour as its header
// accent. The colour is club-supplied and the page is crawlable markup, so the
// value is re-normalized on the way OUT: a row written before validation
// existed (or by hand in SQL) must degrade to "no accent", never reach HTML.

import "testing"

func TestPublicClubCarriesItsBrandColor(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"The Locals","owner_id":"o","brand_color":"#0F4299"}]`)
	s := newFakeSvc(t, f)
	club, _, _, _, err := s.PublicClubByID("c1")
	if err != nil {
		t.Fatalf("public club: %v", err)
	}
	if club.BrandColor != "#0f4299" {
		t.Fatalf("brand colour should be exposed lowercased, got %q", club.BrandColor)
	}
}

func TestPublicClubRefusesAGarbageStoredColor(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"The Locals","owner_id":"o","brand_color":"</style><script>"}]`)
	s := newFakeSvc(t, f)
	club, _, _, _, err := s.PublicClubByID("c1")
	if err != nil {
		t.Fatalf("public club: %v", err)
	}
	if club.BrandColor != "" {
		t.Fatalf("a non-colour must never leave the service layer, got %q", club.BrandColor)
	}
}
