package service

import "testing"

// This string is interpolated into the public club page's markup, so anything
// that isn't a colour must be REFUSED rather than stored — an unvalidated value
// here is a hole straight into the stylesheet.
func TestNormalizeBrandColorRejectsAnythingThatIsNotAColour(t *testing.T) {
	for _, bad := range []string{
		"red",
		"#12345",
		"#1234567",
		"#12345g",
		"</style><script>",
		"#fff; background:url(x)",
	} {
		if got, err := normalizeBrandColor(bad); err == nil {
			t.Errorf("accepted %q as a colour (got %q)", bad, got)
		}
	}
}

// A picker hands you the value with or without the "#", and case varies. Both
// are the same colour and neither should be an error in front of an owner.
func TestNormalizeBrandColorAcceptsTheUsualSpellings(t *testing.T) {
	for _, in := range []string{"#1B4D3E", "1b4d3e", "  #1b4d3e  "} {
		got, err := normalizeBrandColor(in)
		if err != nil {
			t.Fatalf("%q was refused: %v", in, err)
		}
		if got != "#1b4d3e" {
			t.Fatalf("%q normalized to %q", in, got)
		}
	}
}

// Empty means "no branding, use the house palette" — a real choice, not an error.
func TestNormalizeBrandColorTreatsEmptyAsNone(t *testing.T) {
	got, err := normalizeBrandColor("")
	if err != nil || got != "" {
		t.Fatalf("empty should be allowed and stay empty; got %q / %v", got, err)
	}
}
