package service

import (
	"testing"
	"time"
)

// The 9am push is HELD by OneSignal until 9am in each subscriber's timezone,
// but the day is claimed once per UTC day — so the job queues it at 5pm Pacific
// and it arrives the next morning. Choosing the joke on the queue day made
// every push carry the joke the card had already shown all of the previous day.
//
// Kim caught this in the wild on 2026-08-21: the 9am push read "My partner
// poached…" while the card read "The scariest sound in pickleball…".
func TestJokeDeliveryDayIsWhenItLandsNotWhenItIsQueued(t *testing.T) {
	pt, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no zoneinfo in this environment")
	}
	cases := []struct {
		name     string
		queuedAt time.Time
		wantDay  int // day-of-month it should be chosen for
	}{
		// The real schedule: first tick after 00:00 UTC is 5pm Pacific.
		{"queued 5pm, lands tomorrow",
			time.Date(2026, 8, 20, 17, 0, 0, 0, pt), 21},
		// A retry before 9am still lands today.
		{"queued 8am, lands today",
			time.Date(2026, 8, 21, 8, 0, 0, 0, pt), 21},
		// Exactly 9am has already gone out for today.
		{"queued 9am, lands tomorrow",
			time.Date(2026, 8, 21, 9, 0, 0, 0, pt), 22},
		// Across a month boundary.
		{"queued 11pm on the 31st",
			time.Date(2026, 8, 31, 23, 0, 0, 0, pt), 1},
	}
	for _, c := range cases {
		got := deliveryDayFor(c.queuedAt)
		if got.Day() != c.wantDay {
			t.Errorf("%s: chose day %d, want %d", c.name, got.Day(), c.wantDay)
		}
	}
}

// The push and the card must name the same joke for the same day, or the
// feature contradicts itself on two screens.
func TestPushAndCardAgreeOnTheDeliveryDay(t *testing.T) {
	pt, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no zoneinfo in this environment")
	}
	queued := time.Date(2026, 8, 20, 17, 0, 0, 0, pt) // 5pm, the real case
	pushed := JokeOfTheDay(deliveryDayFor(queued))
	// What the card shows the next morning, from the caller's own local date.
	card := JokeOfTheDay(time.Date(2026, 8, 21, 8, 45, 0, 0, pt))
	if pushed != card {
		t.Fatalf("push and card disagree:\n push: %q\n card: %q", pushed, card)
	}
}
