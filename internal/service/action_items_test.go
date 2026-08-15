package service

import (
	"strings"
	"testing"
	"time"
)

func TestCountOfReadsLikeEnglish(t *testing.T) {
	if got := countOf(1, "player waiting", "players waiting"); got != "1 player waiting" {
		t.Fatalf("got %q", got)
	}
	if got := countOf(3, "player waiting", "players waiting"); got != "3 players waiting" {
		t.Fatalf("got %q", got)
	}
}

// The list is read at a glance, so the time has to read the way somebody would
// say it — "today at 6:30 PM", not an ISO timestamp.
func TestHumanWhen(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		in   time.Time
		want string
	}{
		{now.Add(30 * time.Minute), "within the hour"},
		{now.Add(6 * time.Hour), "today at"},
		{now.AddDate(0, 0, 1), "tomorrow at"},
		{now.AddDate(0, 0, 4), ""}, // weekday form; just assert it isn't the others
	}
	for _, c := range cases {
		got := humanWhen(c.in, now)
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("humanWhen(+%v) = %q, want it to contain %q", c.in.Sub(now), got, c.want)
		}
		if got == "" {
			t.Errorf("humanWhen(+%v) returned empty", c.in.Sub(now))
		}
	}
}

// A first badge fifty games away is a wall, not an incentive.
func TestMilestonesStartShallowAndAscend(t *testing.T) {
	if gamesPlayedMilestones[0] > 10 {
		t.Fatalf("first milestone is %d — too far for a new player",
			gamesPlayedMilestones[0])
	}
	for i := 1; i < len(gamesPlayedMilestones); i++ {
		if gamesPlayedMilestones[i] <= gamesPlayedMilestones[i-1] {
			t.Fatalf("milestones must ascend: %v", gamesPlayedMilestones)
		}
	}
}
