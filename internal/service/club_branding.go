package service

import (
	"errors"
	"strings"
)

// Club branding.
//
// A club owner is renting a platform, and every screen their members open says
// PlanMyPickle at them. The point of this is that a member walking into Life
// Time sees Life Time — the club's mark and the club's colour on the pages the
// club owns.
//
// PlanMyPickle stays present but SECONDARY: the club's colour leads, and we
// sign the bottom of the page rather than the top. That's the reversible
// choice. Removing our name entirely is easy to do later and very hard to
// undo — a club that has shown its members an unbranded product for a season
// will not enjoy being told it reappears.

// normalizeBrandColor validates a "#RRGGBB" colour and returns it lowercased,
// or "" for none.
//
// Rejects anything else rather than storing it: this string is interpolated
// into the public club page's markup, and an unvalidated one is a hole straight
// into the stylesheet. Accepting the missing "#" is not laxness — it's what a
// colour picker hands you half the time.
func normalizeBrandColor(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", nil
	}
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return "", errors.New("a brand colour looks like #1B4D3E")
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", errors.New("a brand colour looks like #1B4D3E")
		}
	}
	return "#" + s, nil
}
