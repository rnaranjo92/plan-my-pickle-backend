package service

import "testing"

// namesLooselyMatch guards the phone-based claim paths. A phone can be shared
// (a couple on one number), so the name is what stops one person from claiming
// the other's guest registrations — and their private event feeds. It must
// tolerate how people actually type names without becoming a rubber stamp.
func TestNamesLooselyMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		why  string
	}{
		// Same person, typed differently.
		{"Kay Naranjo", "kay naranjo", true, "case only"},
		{"Kay  Naranjo", "Kay Naranjo", true, "extra whitespace"},
		{"O'Brien, Anne", "Anne OBrien", true, "punctuation and order-insensitive tokens"},
		{"Kay", "Kay Naranjo", true, "given name vs full name"},
		{"Kay Naranjo", "Kay", true, "argument order must not matter"},

		// Different people — these MUST stay false or the phone guard is gone.
		{"Kay", "Kim Naranji", false, "different first names on a shared phone"},
		{"Anne Manalili", "Anne Smith", false, "same first name, different surname"},
		{"Myles Coloso", "Kay Naranjo", false, "unrelated"},
		{"", "Kay Naranjo", false, "empty never matches"},
		{"Kay Naranjo", "   ", false, "blank never matches"},
	}
	for _, c := range cases {
		if got := namesLooselyMatch(c.a, c.b); got != c.want {
			t.Errorf("namesLooselyMatch(%q, %q) = %v, want %v — %s",
				c.a, c.b, got, c.want, c.why)
		}
	}
}

// The full-name comparison is token-order insensitive, so "Anne O'Brien" and
// "O'Brien Anne" match. Verify that does NOT let two different people through
// just because they share tokens with different surnames.
func TestNamesLooselyMatch_SurnameStillCounts(t *testing.T) {
	if namesLooselyMatch("Anne Manalili", "Anne Coloso") {
		t.Fatal("two people sharing only a first name matched on a full-name compare")
	}
}
