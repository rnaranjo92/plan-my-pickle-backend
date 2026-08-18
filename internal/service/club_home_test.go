package service

import (
	"testing"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

func evAt(name string, at *string) model.Event {
	return model.Event{Name: name, StartsAt: at}
}

func ptr(s string) *string { return &s }

// "What's on next" is the reason a member opens the club at all, so the rule
// for what appears there has to be exactly right — an empty card on a busy
// Tuesday, or a ladder listed as if it started at 7pm, both teach people to
// stop looking.
func TestUpcomingClubEvents(t *testing.T) {
	now := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *string {
		return ptr(now.Add(d).Format(time.RFC3339))
	}

	t.Run("soonest first, capped", func(t *testing.T) {
		got := upcomingClubEvents([]model.Event{
			evAt("in a month", at(30*24*time.Hour)),
			evAt("tomorrow", at(24*time.Hour)),
			evAt("next week", at(7*24*time.Hour)),
			evAt("in an hour", at(time.Hour)),
		}, now, 3)
		if len(got) != 3 {
			t.Fatalf("got %d events, want 3", len(got))
		}
		want := []string{"in an hour", "tomorrow", "next week"}
		for i, w := range want {
			if got[i].Name != w {
				t.Errorf("position %d = %q, want %q", i, got[i].Name, w)
			}
		}
	})

	t.Run("tonight's session stays up while it's running", func(t *testing.T) {
		// Dropping it at its start time empties the card exactly when the club
		// is busiest — people are mid-way through the thing.
		got := upcomingClubEvents([]model.Event{
			evAt("started two hours ago", at(-2*time.Hour)),
		}, now, 3)
		if len(got) != 1 {
			t.Fatalf("a session in progress fell off the card")
		}
	})

	t.Run("last week's session is gone", func(t *testing.T) {
		got := upcomingClubEvents([]model.Event{
			evAt("last week", at(-7*24*time.Hour)),
		}, now, 3)
		if len(got) != 0 {
			t.Errorf("got %d, want none — that already happened", len(got))
		}
	})

	t.Run("a dateless ladder is not 'on next'", func(t *testing.T) {
		// A ladder has no date by design. Listing it here would tell a member to
		// turn up for something that isn't happening on any particular evening.
		got := upcomingClubEvents([]model.Event{
			evAt("the club ladder", nil),
			evAt("unparseable", ptr("next Tuesday-ish")),
		}, now, 3)
		if len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})

	t.Run("nothing scheduled is an empty list, not nil", func(t *testing.T) {
		got := upcomingClubEvents(nil, now, 3)
		if len(got) != 0 {
			t.Errorf("got %d", len(got))
		}
	})
}
