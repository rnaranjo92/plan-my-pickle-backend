package service

import "strings"

// normalizeWaiverURL cleans up a link a club owner typed by hand, and refuses
// anything that isn't a web address.
//
// Two separate jobs, both learned from how people actually fill in a form:
//
//  1. Almost nobody types the scheme. "lifetime.life/waiver" pasted straight
//     into an href resolves RELATIVE to the current page, so the link silently
//     goes to planmypickle.com/lifetime.life/waiver — a 404 that looks like our
//     bug, on the one link that matters legally.
//
//  2. The value is club-supplied and rendered as a link on a PUBLIC page, so it
//     is untrusted input in an href. "javascript:..." in that position runs
//     script for every visitor. Allowing only http/https closes that off by
//     construction rather than by escaping.
//
// Returns "" for anything it cannot make into a safe absolute web URL — the
// caller then treats the club as having a waiver with no link, which is a real
// and supported state (plenty of clubs hand out paper at the door).
func normalizeWaiverURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	// Reject control characters and whitespace outright: a URL contains none,
	// and they are how "java\tscript:" style bypasses are smuggled past a
	// prefix check.
	for _, r := range u {
		if r <= ' ' || r == 0x7f {
			return ""
		}
	}
	lower := strings.ToLower(u)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return u
	case strings.Contains(u, ":"):
		// Some other scheme — javascript:, data:, mailto:, file:. Not a web page.
		// Checked BEFORE prefixing https:// so a scheme can never survive by
		// being wrapped in one.
		//
		// A bare "example.com:8080/waiver" also lands here and is refused. That
		// is a deliberate trade: a port is vanishingly rare on a public waiver
		// link, and guessing wrong in the other direction would render an
		// attacker's scheme.
		return ""
	default:
		return "https://" + u
	}
}
