package service

import "testing"

// Guards the bug class that broke substitute re-entry: the app stores a
// FORMATTED phone, so any SQL match on a normalized value misses.
func TestNormPhoneMatchesFormattedStorage(t *testing.T) {
	typed := "6195550100"      // what a lookup normalizes to
	stored := "(619) 555-0100" // what players.phone actually holds
	if normPhone(stored) != normPhone(typed) {
		t.Fatalf("normPhone mismatch: stored=%q -> %q, typed=%q -> %q",
			stored, normPhone(stored), typed, normPhone(typed))
	}
	// The old/broken approaches, asserted as broken so nobody reintroduces them.
	if containsSub(stored, "6195550100") {
		t.Fatal("exact-digits substring unexpectedly present in formatted phone")
	}
	if containsSub(stored, "5550100") {
		t.Fatal("last-7-digit substring unexpectedly present in formatted phone")
	}
	// +1 and international forms normalize together too.
	if normPhone("+1 (619) 555-0100") != normPhone(stored) {
		t.Fatal("+1 prefix should normalize to the same 10 digits")
	}
}

func containsSub(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
