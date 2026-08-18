package service

import (
	"testing"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// Where these lines fall decides who gets a message and who gets left alone, so
// they are a judgement worth pinning rather than a threshold worth tuning.
func TestClubMemberStatus(t *testing.T) {
	cases := []struct {
		name       string
		everPlayed bool
		missedRun  int
		want       string
	}{
		{"at the last session", true, 0, ClubStatusActive},
		{"missed one — a normal life", true, 1, ClubStatusActive},
		{"missed two — still nothing to act on", true, 2, ClubStatusActive},

		// The only actionable band, and the whole reason for the view: a
		// message here brings people back; the same message a month later
		// reads as an accusation.
		{"missed three — about a month away", true, 3, ClubStatusSlipping},
		{"missed five", true, 5, ClubStatusSlipping},

		{"missed six — gone for now", true, 6, ClubStatusLapsed},
		{"missed twenty", true, 20, ClubStatusLapsed},

		// A different problem with a different fix: the club never got them
		// through the door once. Calling them lapsed hides both.
		{"joined and never came", false, 0, ClubStatusNeverAlong},
		{"joined long ago, never came", false, 12, ClubStatusNeverAlong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clubMemberStatus(c.everPlayed, c.missedRun); got != c.want {
				t.Errorf("status = %q, want %q", got, c.want)
			}
		})
	}
}

// The list is a to-do. Its top should be the people a message would still
// reach in time — not the most-gone, and not the most active.
func TestRosterOrderPutsTheReachableFirst(t *testing.T) {
	if rosterOrder(ClubStatusSlipping) >= rosterOrder(ClubStatusLapsed) {
		t.Error("the long-gone outrank the nearly-gone — that buries the only " +
			"people an owner can still do something about")
	}
	if rosterOrder(ClubStatusSlipping) >= rosterOrder(ClubStatusActive) {
		t.Error("regulars are above people slipping away")
	}
	if rosterOrder(ClubStatusNeverAlong) >= rosterOrder(ClubStatusLapsed) {
		t.Error("someone who never came should outrank someone long gone — " +
			"they are one invitation from being a member")
	}
}

func TestPastSessions(t *testing.T) {
	now := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *string {
		s := now.Add(d).Format(time.RFC3339)
		return &s
	}
	events := []model.Event{
		{ID: "next-week", StartsAt: at(7 * 24 * time.Hour)},
		{ID: "last-week", StartsAt: at(-7 * 24 * time.Hour)},
		{ID: "yesterday", StartsAt: at(-24 * time.Hour)},
		{ID: "the-ladder", StartsAt: nil},
	}
	got := pastSessions(events, now, 8)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (the future one and the ladder are "+
			"not sessions that happened)", len(got))
	}
	if got[0].id != "yesterday" {
		t.Errorf("newest first broken: got %q", got[0].id)
	}
	if capped := pastSessions(events, now, 1); len(capped) != 1 {
		t.Errorf("window not applied: got %d", len(capped))
	}
}
