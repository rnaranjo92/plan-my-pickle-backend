package service

import (
	"strings"
	"testing"
	"time"
)

// Every announcement is a push on forty phones. The cooldown exists for the
// MEMBERS, not the server — the fastest way to make a club's notifications
// worthless is to let a double-tap send the same thing four times.
func TestClubAnnounceCooldown(t *testing.T) {
	s := &Service{}
	now := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)

	if w := s.clubAnnounceWait("club-a", now); w != 0 {
		t.Errorf("a club that has never sent had to wait %v", w)
	}

	s.markClubAnnounced("club-a", now)

	if w := s.clubAnnounceWait("club-a", now.Add(time.Second)); w <= 0 {
		t.Error("a double-tap one second later got through")
	}
	if w := s.clubAnnounceWait("club-a", now.Add(clubAnnounceCooldown)); w != 0 {
		t.Errorf("still blocked after the full cooldown: %v", w)
	}

	// One club's send must never silence another's — they are different rooms
	// of different people.
	if w := s.clubAnnounceWait("club-b", now.Add(time.Second)); w != 0 {
		t.Errorf("club-b was blocked by club-a's message (%v)", w)
	}
}

// The wait is read by someone who has just been refused, so it has to sound
// like a person saying it rather than a duration being printed.
func TestRoundedWait(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "1 seconds"},
		{30 * time.Second, "31 seconds"},
		{90 * time.Second, "2 minutes"},
		{4 * time.Minute, "5 minutes"},
	}
	for _, c := range cases {
		if got := roundedWait(c.d); got != c.want {
			t.Errorf("roundedWait(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	// Just under a minute must not say "0 minutes".
	if got := roundedWait(59 * time.Second); strings.HasPrefix(got, "0") {
		t.Errorf("roundedWait(59s) = %q", got)
	}
}
