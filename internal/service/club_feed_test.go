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

// A private club's chatter must not be readable by anyone holding the link.
// Club POSTS follow the join-button rule: members always, strangers only when
// the club is public. (Event-derived items keep their own event visibility.)
func TestPrivateClubPostsAreHiddenFromStrangers(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"The Locals","owner_id":"owner","is_public":false}]`).
		seed("club_members", `[]`).
		seed("events", `[]`).
		seed("feed_items", `[{"id":"p1","club_id":"c1","type":"post","text":"secret plans","author_id":"m1","created_at":"2026-08-22T00:00:00Z"}]`)
	s := newFakeSvc(t, f)
	items, err := s.ClubFeed("c1", "stranger")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, it := range items {
		if it.Text == "secret plans" {
			t.Fatal("a stranger read a private club's post")
		}
	}
}

func TestClubPostRequiresMembership(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"The Locals","owner_id":"owner"}]`).
		seed("club_members", `[]`).
		seed("feed_items", `[{"club_id":"present"}]`)
	s := newFakeSvc(t, f)
	if _, err := s.CreateClubPost("c1", "stranger", "", "hi", "", ""); err == nil {
		t.Fatal("a non-member posted to the club")
	}
}

func TestClubPostCarriesTheClubNameForContext(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","name":"LT Test","owner_id":"owner"}]`).
		seed("club_members", `[{"club_id":"c1","user_id":"m1","role":"member"}]`).
		seed("feed_items", `[{"club_id":"present"}]`)
	s := newFakeSvc(t, f)
	item, err := s.CreateClubPost("c1", "m1", "m@x.com", "great night", "", "")
	if err != nil {
		t.Fatalf("member post failed: %v", err)
	}
	// The context cue: wherever this item travels, the card can say whose
	// wall it lives on.
	if item.ClubName != "LT Test" {
		t.Fatalf("club name should ride in meta; got %q", item.ClubName)
	}
	wrote := f.written("feed_items")
	if len(wrote) == 0 {
		t.Fatal("nothing written")
	}
	if got := wrote[len(wrote)-1]["club_id"]; got != "c1" {
		t.Fatalf("post not tied to the club: %v", got)
	}
}
