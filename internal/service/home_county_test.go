package service

import "testing"

// pmp_profiles.county went unwritten for the life of the product — nothing set
// it, so userHomeCounty returned empty for every account and anything
// county-scoped showed nothing to anybody. The profile is now filled from the
// app, but that only helps people who open it again; the fallback is what makes
// every EXISTING account work straight away.
func TestUserHomeCountyPrefersTheProfile(t *testing.T) {
	f := newFake().seed("pmp_profiles",
		`[{"county":"San Diego","state":"CA"}]`)
	s := newFakeSvc(t, f)
	c, st := s.userHomeCounty("u1")
	if c != "San Diego" || st != "CA" {
		t.Fatalf("profile should win; got %q/%q", c, st)
	}
}

// Null profile → fall back to an event they OWN.
func TestUserHomeCountyFallsBackToAnOwnedEvent(t *testing.T) {
	f := newFake().
		seed("pmp_profiles", `[{"county":null,"state":null}]`).
		seed("events", `[{"county":"Orange","state":"CA"}]`)
	s := newFakeSvc(t, f)
	c, st := s.userHomeCounty("u1")
	if c != "Orange" || st != "CA" {
		t.Fatalf("should infer from their event; got %q/%q", c, st)
	}
}

// A blank string in the profile counts as unset — it is what an empty form
// submit leaves behind, and treating it as a real answer would pin somebody to
// nowhere forever.
func TestUserHomeCountyTreatsBlankAsUnset(t *testing.T) {
	f := newFake().
		seed("pmp_profiles", `[{"county":"   ","state":"CA"}]`).
		seed("events", `[{"county":"Orange","state":"CA"}]`)
	s := newFakeSvc(t, f)
	if c, _ := s.userHomeCounty("u1"); c != "Orange" {
		t.Fatalf("blank county should fall through; got %q", c)
	}
}

// Nothing anywhere is a real state: say so rather than guessing.
func TestUserHomeCountyEmptyWhenNothingKnown(t *testing.T) {
	s := newFakeSvc(t, newFake())
	if c, st := s.userHomeCounty("u1"); c != "" || st != "" {
		t.Fatalf("want empty, got %q/%q", c, st)
	}
}

// No user, no lookups.
func TestUserHomeCountyIgnoresEmptyUser(t *testing.T) {
	s := newFakeSvc(t, newFake())
	if c, _ := s.userHomeCounty(""); c != "" {
		t.Fatalf("want empty for no user, got %q", c)
	}
}

// A location we can't resolve must not wipe a county already stored.
func TestSetHomeLocationRejectsNullIsland(t *testing.T) {
	s := newFakeSvc(t, newFake())
	if err := s.SetHomeLocation("u1", 0, 0); err == nil {
		t.Error("0,0 is not a real location and should be refused")
	}
}

// The home feed must be able to fill itself WITHOUT a location prompt, and it
// must only claim proximity when proximity is real. Place is what the client
// reads to decide between "Happening near you" and a neutral heading, so these
// assert the label as much as the list.
func TestHomeFeedEventsNamesTheCountyWhenItKnowsOne(t *testing.T) {
	f := newFake().
		seed("pmp_profiles", `[{"county":"San Diego","state":"CA"}]`).
		seed("events", `[{"id":"e1","name":"Slam","listed":true,"format":"doubles","starts_at":"2030-01-01T18:00:00Z","county":"San Diego","state":"CA"}]`)
	s := newFakeSvc(t, f)
	out, err := s.HomeFeedEvents("u1", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("want the county's event; got %d", len(out.Events))
	}
	if out.Place != "San Diego, CA" {
		t.Fatalf("want a named place so the client can say 'near you'; got %q", out.Place)
	}
}

// No county anywhere → still return something, but do NOT call it near anyone:
// an empty Place is the client's signal to head the list neutrally.
func TestHomeFeedEventsFallsBackWithoutClaimingAPlace(t *testing.T) {
	f := newFake().
		seed("pmp_profiles", `[{"county":null,"state":null}]`).
		seed("events", `[{"id":"e1","name":"Slam","listed":true,"format":"doubles","starts_at":"2030-01-01T18:00:00Z"}]`)
	s := newFakeSvc(t, f)
	out, err := s.HomeFeedEvents("u1", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("a blank feed should still get the newest events; got %d", len(out.Events))
	}
	if out.Place != "" {
		t.Fatalf("national fallback must not claim a place; got %q", out.Place)
	}
}

// A known county with nothing listed in it must not report the place either —
// otherwise the heading says "near you" over an empty list.
func TestHomeFeedEventsWithNothingToShowReportsNoPlace(t *testing.T) {
	f := newFake().
		seed("pmp_profiles", `[{"county":"San Diego","state":"CA"}]`).
		seed("events", `[]`)
	s := newFakeSvc(t, f)
	out, err := s.HomeFeedEvents("u1", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Events) != 0 || out.Place != "" {
		t.Fatalf("want empty/unnamed; got %d events, place %q", len(out.Events), out.Place)
	}
}
