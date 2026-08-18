package service

import "testing"

func TestNormalizeWaiverURLAddsTheMissingScheme(t *testing.T) {
	// The overwhelmingly common case: someone types what they'd say out loud.
	for _, in := range []string{
		"lifetime.life/waiver",
		"www.lifetime.life/waiver",
		"  lifetime.life/waiver  ",
	} {
		if got := normalizeWaiverURL(in); !hasPrefixI(got, "https://") {
			t.Fatalf("%q: want an https link, got %q", in, got)
		}
	}
}

func TestNormalizeWaiverURLKeepsARealLinkIntact(t *testing.T) {
	for _, in := range []string{
		"https://lifetime.life/waiver",
		"http://lifetime.life/waiver",
		"HTTPS://Lifetime.Life/Waiver?v=2",
	} {
		if got := normalizeWaiverURL(in); got != in {
			t.Fatalf("%q was rewritten to %q; an already-valid link must survive untouched", in, got)
		}
	}
}

func TestNormalizeWaiverURLRefusesNonWebSchemes(t *testing.T) {
	// This value is rendered as an href on a PUBLIC page. A scheme that runs
	// script must never survive — including the tricks that try to sneak one
	// past a naive prefix check.
	for _, in := range []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"java\tscript:alert(1)",
		"java\nscript:alert(1)",
		" javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"mailto:club@lifetime.life",
		"file:///etc/passwd",
		"vbscript:msgbox(1)",
	} {
		if got := normalizeWaiverURL(in); got != "" {
			t.Fatalf("%q must be refused, got %q", in, got)
		}
	}
}

func TestNormalizeWaiverURLEmptyStaysEmpty(t *testing.T) {
	// A club with a waiver and no link is a real, supported state — paper at
	// the door. Empty must not become "https://".
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := normalizeWaiverURL(in); got != "" {
			t.Fatalf("%q must stay empty, got %q", in, got)
		}
	}
}

func hasPrefixI(s, p string) bool {
	return len(s) >= len(p) && equalFoldASCII(s[:len(p)], p)
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
