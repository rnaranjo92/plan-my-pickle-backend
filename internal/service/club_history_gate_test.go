package service

import "testing"

func TestEventInLeaderboardYearAllTimeTakesEverything(t *testing.T) {
	// window 0 = all-time. Nothing may be dropped, including junk dates.
	for _, in := range []string{
		"2019-03-01T00:00:00Z", "2026-08-17T00:00:00Z", "", "not a date",
	} {
		if !eventInLeaderboardYear(in, 0) {
			t.Fatalf("all-time dropped %q", in)
		}
	}
}

func TestEventInLeaderboardYearKeepsThisYearDropsOlder(t *testing.T) {
	cases := []struct {
		startsAt string
		want     bool
	}{
		{"2026-01-01T00:00:00Z", true},  // first moment of the window
		{"2026-08-17T18:30:00Z", true},  // mid-window
		{"2027-02-01T00:00:00Z", true},  // later than the window: still counts
		{"2025-12-31T23:59:59Z", false}, // last moment before it
		{"2019-06-01T00:00:00Z", false}, // long past
	}
	for _, c := range cases {
		if got := eventInLeaderboardYear(c.startsAt, 2026); got != c.want {
			t.Errorf("eventInLeaderboardYear(%q, 2026) = %v, want %v",
				c.startsAt, got, c.want)
		}
	}
}

func TestEventInLeaderboardYearKeepsUndatedEvents(t *testing.T) {
	// A missing or unreadable date must never cost a club its records. Far more
	// likely an old import than an attempt to dodge a paywall — and the failure
	// is invisible, which is the kind that erodes trust in the whole board.
	for _, in := range []string{"", "   ", "sometime in June", "2026-13-45"} {
		if !eventInLeaderboardYear(in, 2026) {
			t.Fatalf("undated event %q was dropped from the windowed board", in)
		}
	}
}

func TestEverythingTodayIsPermanentlyExempt(t *testing.T) {
	// The promise: anything that exists before billing launches keeps its
	// history forever. Every club and league alive right now predates the
	// epoch, so this must hold for today's dates no matter what else changes.
	for _, in := range []string{
		"2026-08-17T00:00:00Z", "2026-01-01T00:00:00Z", "2024-05-05T00:00:00Z",
	} {
		if !predatesPaywall(in) {
			t.Fatalf("%q lost its exemption — an existing club would be re-gated", in)
		}
	}
}
