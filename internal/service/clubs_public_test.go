package service

import "testing"

// The crawlable surface is where a mistake outlives the deploy that made it: an
// indexed demo club is a search result pointing strangers at "Test Club", and a
// wrongly-filtered real club is invisible to search with nobody ever finding
// out.
func TestIsDemoClubName(t *testing.T) {
	blocked := []string{
		"Test Club", "test club", "  TEST  ", "Demo Pickleball",
		"Sample Club", "QA Club", "My Demo Club", "Dummy club",
		"Club (test)", "Testing Club",
	}
	for _, n := range blocked {
		if !isDemoClubName(n) {
			t.Errorf("%q would be indexed", n)
		}
	}

	// The substring version blocked every one of these. A filter that hides
	// real clubs from search is worse than one that lets a demo through.
	allowed := []string{
		"Chula Vista Pickleball Club",
		"Life Time Pickleball",
		"Protest Park Paddlers",       // contains "test"
		"Contest City Club",           // contains "test"
		"The Greatest Club",           // contains "test"
		"Demolition Derby Pickleball", // contains "demo"
		"Aqaba Pickleball",            // contains "qa"
	}
	for _, n := range allowed {
		if isDemoClubName(n) {
			t.Errorf("%q was wrongly kept out of search", n)
		}
	}
}

// The sitemap is a recommendation to a search engine, and thin pages don't fail
// quietly — they lower the standing of the pages around them. But a brand-new
// club still needs its page to WORK for anyone holding the link, which is the
// distinction this draws.
func TestWorthIndexing(t *testing.T) {
	if (PublicClub{EventCount: 1}).WorthIndexing() != true {
		t.Error("a club that has run an event should be indexed")
	}
	if (PublicClub{EventCount: 0}).WorthIndexing() != false {
		t.Error("a club with nothing on was offered to a crawler")
	}
	// Deliberately NOT gated on members: a club recruiting its first members
	// has nothing to show yet either, and "has run something" is the honest
	// signal. This pins that choice.
	if (PublicClub{MemberCount: 50, EventCount: 0}).WorthIndexing() {
		t.Error("members alone made a club indexable — it still has no page " +
			"content to rank")
	}
}
