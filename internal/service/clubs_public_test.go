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
