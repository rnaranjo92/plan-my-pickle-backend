package service

import "testing"

// The club page is open to anyone with the link, so the feed's visibility has
// to be the EVENT'S, not the club's. Reading the club's events through
// ClubEventsFor is what keeps a private league's scores off a public page --
// this test is here so nobody "simplifies" it into a direct club_id query.
func TestClubFeedShowsNothingWhenTheClubHasNoVisibleEvents(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","owner_id":"owner1"}]`).
		seed("club_members", `[]`).
		seed("events", `[]`).
		seed("feed_items", `[{"id":"f1","event_id":"e1","type":"match_final","text":"11-9"}]`)
	s := newFakeSvc(t, f)
	items, err := s.ClubFeed("c1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("no visible events must mean no feed; got %d items", len(items))
	}
}

// A club that ran a test event shouldn't narrate it to its members forever --
// the same QA filter the other feeds apply.
func TestClubFeedSkipsTestEvents(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","owner_id":"owner1"}]`).
		seed("club_members", `[{"club_id":"c1","user_id":"u1"}]`).
		seed("events", `[{"id":"e1","name":"MLP Demo","club_id":"c1","listed":true}]`).
		seed("feed_items", `[{"id":"f1","event_id":"e1","type":"match_final","text":"11-9","created_at":"2026-08-21T10:00:00Z"}]`)
	s := newFakeSvc(t, f)
	items, err := s.ClubFeed("c1", "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("a demo event's activity leaked into the club feed: %d items", len(items))
	}
}

// The happy path: a real event's activity reaches the club's page, named.
func TestClubFeedCarriesTheEventName(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","owner_id":"owner1"}]`).
		seed("club_members", `[{"club_id":"c1","user_id":"u1"}]`).
		seed("events", `[{"id":"e1","name":"Tuesday Night League","club_id":"c1","listed":true}]`).
		seed("feed_items", `[{"id":"f1","event_id":"e1","type":"match_final","text":"11-9","created_at":"2026-08-21T10:00:00Z"}]`)
	s := newFakeSvc(t, f)
	items, err := s.ClubFeed("c1", "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if items[0].EventName != "Tuesday Night League" {
		t.Fatalf("item didn't carry its event's name: %q", items[0].EventName)
	}
}
