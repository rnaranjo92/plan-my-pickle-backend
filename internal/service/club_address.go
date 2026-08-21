package service

import "strings"

// A club's ADDRESS, and the city derived from it.
//
// Clubs were only ever asked for a city, which is fine for grouping and useless
// for getting there: a member looking at a club page can't put "Chula Vista"
// into a map. The address field answers that.
//
// The city column STAYS, and stays a city. It slugs the public directory pages
// (/pickleball-clubs/{city}) and heads the City column of the organization
// report — a street address in there would produce
// "1650-main-street-w-chula-vista-ca-91913" as a URL and break the directory.
// So the address is stored alongside it, and the city is derived from the
// address rather than asked for twice.

// cityFromAddress pulls the city out of a typed or picked address.
//
// Place suggestions come back comma-separated and end with the region:
// "1650 Main Street W, Chula Vista, CA 91913, USA". The city is the part before
// the state/ZIP chunk, so this walks in from the right past a country and past
// the "CA 91913" piece rather than counting from the left — street addresses
// have a wildly varying number of leading parts and counting forward gets
// "Suite 200" as often as it gets a city.
//
// Returns "" when it can't tell. A wrong city is worse than none: it would file
// the club under a town it isn't in.
func cityFromAddress(addr string) string {
	parts := []string{}
	for _, p := range strings.Split(addr, ",") {
		if t := strings.TrimSpace(p); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) < 2 {
		return "" // "Chula Vista" alone is already a city, not an address
	}
	i := len(parts) - 1
	// Drop a trailing country.
	if isCountry(parts[i]) {
		i--
	}
	// Drop the "CA 91913" / "CA" chunk.
	if i >= 0 && looksLikeStateZip(parts[i]) {
		i--
	}
	if i < 1 {
		return "" // nothing left before it that could be a city
	}
	return parts[i]
}

func isCountry(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "USA", "US", "UNITED STATES", "CANADA", "CA.", "PHILIPPINES", "PH":
		return true
	}
	return false
}

// looksLikeStateZip matches "CA", "CA 91913", "California 91913" — the chunk a
// US address carries between the city and the country.
func looksLikeStateZip(s string) bool {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 || len(f) > 3 {
		return false
	}
	// A trailing all-digit ZIP is the strongest signal.
	last := f[len(f)-1]
	if len(last) >= 5 && digitsOnly(last) == last {
		return true
	}
	// Otherwise a bare two-letter state code.
	return len(f) == 1 && len(f[0]) == 2 && strings.ToUpper(f[0]) == f[0]
}

// clubCityFor picks the city to file a club under: whatever the caller sent, or
// the city read out of the address when they sent none.
//
// An explicit city WINS. Derivation is a convenience for the common case where
// the organizer only typed an address; it must never overrule someone who told
// us plainly where their club is.
func clubCityFor(city, address string) string {
	if c := strings.TrimSpace(city); c != "" {
		return c
	}
	return cityFromAddress(address)
}
