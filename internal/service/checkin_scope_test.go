package service

import (
	"strings"
	"testing"
)

// A perpetual (never-ending) league session is built ONLY from the players who
// are checked in that day — a coach with 30 members and 9 present must get a
// draw of 9, not 30. That scoping is a PostgREST filter, so the guarantee lives
// in the query string: assert it directly rather than trusting the call site,
// because a refactor that drops the flag would otherwise silently schedule
// everyone who ever registered.
func TestBracketRegsScopesToCheckedIn(t *testing.T) {
	f := newFake()
	s := newFakeSvc(t, f)

	if _, err := s.bracketRegs("e1", "b1", true); err != nil {
		t.Fatalf("bracketRegs: %v", err)
	}
	var got string
	for _, u := range f.urls {
		if strings.Contains(u, "/registrations") {
			got = u
		}
	}
	if got == "" {
		t.Fatal("no registrations read was issued")
	}
	if !strings.Contains(got, "checked_in=is.true") {
		t.Errorf("checked-in scoping missing from the query: %s", got)
	}
	if !strings.Contains(got, "bracket_id=eq.b1") {
		t.Errorf("division scoping missing from the query: %s", got)
	}
}

// The same helper must NOT scope by check-in when the caller didn't ask —
// a normal tournament drafts every registrant, present or not.
func TestBracketRegsUnscopedWhenNotAsked(t *testing.T) {
	f := newFake()
	s := newFakeSvc(t, f)

	if _, err := s.bracketRegs("e1", "b1", false); err != nil {
		t.Fatalf("bracketRegs: %v", err)
	}
	for _, u := range f.urls {
		if strings.Contains(u, "/registrations") && strings.Contains(u, "checked_in") {
			t.Errorf("must not filter by check-in when unscoped: %s", u)
		}
	}
}
