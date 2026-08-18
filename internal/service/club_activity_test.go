package service

import (
	"testing"
	"time"
)

// The public page's job is to answer "is this worth turning up to". These
// claims are the answer, so a wrong one is worse than no claim: somebody who
// reads "plays most Tuesdays" and finds an empty court on Tuesday does not come
// back a second time.
func TestActivityFrom(t *testing.T) {
	now := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC) // a Monday
	// Sessions n weeks ago on the same weekday as `now`.
	weekly := func(n int) clubSession {
		return clubSession{
			id: "s", at: now.Add(time.Duration(-n) * 7 * 24 * time.Hour),
		}
	}

	t.Run("a weekly club claims its day", func(t *testing.T) {
		got := activityFrom([]clubSession{
			weekly(0), weekly(1), weekly(2), weekly(3), weekly(4),
		}, now)
		if got.UsualDay != "Monday" {
			t.Errorf("usual day = %q, want Monday", got.UsualDay)
		}
		if got.SessionsRecent != 5 {
			t.Errorf("recent = %d, want 5", got.SessionsRecent)
		}
	})

	t.Run("three scattered dates are not a habit", func(t *testing.T) {
		// One session on each of three different weekdays. There is a mode, but
		// claiming it would send somebody to a court on the strength of a
		// coincidence.
		got := activityFrom([]clubSession{
			{id: "a", at: now},
			{id: "b", at: now.Add(-3 * 24 * time.Hour)},
			{id: "c", at: now.Add(-10 * 24 * time.Hour)},
		}, now)
		if got.UsualDay != "" {
			t.Errorf("claimed %q from three scattered dates", got.UsualDay)
		}
	})

	t.Run("a club that stopped in the spring doesn't look busy", func(t *testing.T) {
		old := []clubSession{}
		for i := 20; i < 30; i++ { // all ~5-7 months ago
			old = append(old, clubSession{
				id: "old", at: now.Add(time.Duration(-i) * 7 * 24 * time.Hour),
			})
		}
		got := activityFrom(old, now)
		if got.SessionsRecent != 0 {
			t.Errorf("recent = %d, want 0 — those were months ago",
				got.SessionsRecent)
		}
		if got.UsualDay != "" {
			t.Error("advertised a rhythm it no longer has")
		}
		// Still reports WHEN it last played — honest, and different from
		// pretending nothing ever happened.
		if got.LastPlayed == "" {
			t.Error("lost the last-played date")
		}
	})

	t.Run("a brand new club says nothing rather than something wrong", func(t *testing.T) {
		got := activityFrom(nil, now)
		if got.UsualDay != "" || got.SessionsRecent != 0 || got.LastPlayed != "" {
			t.Errorf("invented activity from no sessions: %+v", got)
		}
	})
}
