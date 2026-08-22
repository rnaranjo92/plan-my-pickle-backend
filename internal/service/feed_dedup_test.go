package service

import "testing"

// Kay Naranjo saw the same event card twice in her feed (2026-08-20).
//
// MyFeed collects one row from several directions — events you play in, posts
// by people you follow, cards for events your followees play in, your own posts
// — and only the LAST of those checked whether the row had already been added.
// Following the organizer of an event you're registered in was enough to print
// the identical card twice.
//
// The fake ignores query filters, so every feed_items read returns this one row:
// that is precisely the shape of the bug (one row, many paths).
func TestMyFeedShowsOnePostOnceEvenWhenSeveralPathsReachIt(t *testing.T) {
	f := newFake().
		seed("events", `[{"id":"e1","name":"Slam","owner_id":"u1","listed":true,"format":"doubles","starts_at":"2030-01-01T18:00:00Z"}]`).
		seed("follows", `[{"followee_id":"org1"}]`).
		seed("feed_items", `[{"id":"f1","event_id":"e1","type":"event","text":"Slam","author_id":"org1","created_at":"2026-08-20T00:00:00Z"}]`)
	s := newFakeSvc(t, f)
	items, err := s.MyFeed("u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("one row must render one card; got %d", len(items))
	}
	if items[0].ID != "f1" {
		t.Fatalf("wrong item: %q", items[0].ID)
	}
}

// Kim saw the same event card twice on one account and once on another
// (2026-08-22). The row-id dedup above could not catch it: these are two
// DIFFERENT rows for the same event, written by ensureEventPosts — a
// read-then-insert with no unique constraint, which only runs for events you
// OWN. Hence one account doubling and the other not.
func TestMyFeedShowsOneCardPerEventWhenTheTableHasTwo(t *testing.T) {
	f := newFake().
		seed("events", `[{"id":"e1","name":"Slam","owner_id":"u1","listed":true,"format":"doubles","starts_at":"2030-01-01T18:00:00Z"}]`).
		seed("feed_items", `[
			{"id":"f1","event_id":"e1","type":"event","text":"Slam","author_id":"u1","created_at":"2026-08-20T00:00:00Z"},
			{"id":"f2","event_id":"e1","type":"event","text":"Slam","author_id":"u1","created_at":"2026-08-21T00:00:00Z"}
		]`)
	s := newFakeSvc(t, f)
	items, err := s.MyFeed("u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("one event must render one card; got %d", len(items))
	}
	// The OLDEST row wins: reactions and comments point at its id, so keeping
	// the newer one would show the card with its conversation missing.
	if items[0].ID != "f1" {
		t.Fatalf("expected the original card f1, got %q", items[0].ID)
	}
}
