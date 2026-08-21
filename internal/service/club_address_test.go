package service

import "testing"

// The city column slugs the public directory (/pickleball-clubs/{city}) and
// heads the organization report. Deriving it wrong files a club under a town it
// isn't in; deriving it from the left would pick up "Suite 200" as often as a
// city, which is why this walks in from the right.
func TestCityFromAddress(t *testing.T) {
	cases := map[string]string{
		// The shape place suggestions actually return.
		"1650 Main Street W, Chula Vista, CA 91913, USA": "Chula Vista",
		"1650 Main Street W, Chula Vista, CA 91913":      "Chula Vista",
		"2800 Olympic Pkwy, Chula Vista, CA":             "Chula Vista",
		// Suites and unit numbers must not become the city.
		"123 Court Rd, Suite 200, Austin, TX 78701, USA": "Austin",
		// A bare city is already a city, not an address — nothing to derive.
		"Chula Vista": "",
		"":            "",
	}
	for in, want := range cases {
		if got := cityFromAddress(in); got != want {
			t.Errorf("cityFromAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// An explicit city WINS. Derivation is a convenience for someone who only typed
// an address; it must never overrule someone who said plainly where they are.
func TestClubCityForPrefersWhatTheCallerSent(t *testing.T) {
	got := clubCityFor("Bonita", "1650 Main Street W, Chula Vista, CA 91913")
	if got != "Bonita" {
		t.Fatalf("an explicit city was overruled: got %q", got)
	}
	if got := clubCityFor("", "1650 Main Street W, Chula Vista, CA 91913"); got != "Chula Vista" {
		t.Fatalf("no city sent should derive one: got %q", got)
	}
	// Nothing derivable leaves it empty rather than guessing.
	if got := clubCityFor("", "somewhere"); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}
