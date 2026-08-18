package service

import (
	"testing"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

func sess(id string, at *string) model.Event {
	return model.Event{ID: id, StartsAt: at}
}

// "6 of the last 8" is a sentence a member reads about themselves, so the
// counting has to match what they remember doing.
func TestAttendanceFrom(t *testing.T) {
	now := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	week := func(n int) *string {
		s := now.Add(time.Duration(-n) * 7 * 24 * time.Hour).Format(time.RFC3339)
		return &s
	}
	// Eight weekly sessions, most recent first when sorted.
	events := []model.Event{
		sess("w1", week(1)), sess("w2", week(2)), sess("w3", week(3)),
		sess("w4", week(4)), sess("w5", week(5)), sess("w6", week(6)),
		sess("w7", week(7)), sess("w8", week(8)),
	}

	t.Run("counts the window and the streak from the most recent", func(t *testing.T) {
		got := attendanceFrom(events, map[string]bool{
			"w1": true, "w2": true, "w3": true, "w5": true, "w6": true, "w7": true,
		}, now, 8)
		if got.Played != 6 || got.Of != 8 {
			t.Errorf("got %d of %d, want 6 of 8", got.Played, got.Of)
		}
		// Missed w4, so the streak is w1..w3 — a gap ends the streak but not
		// the count.
		if got.Streak != 3 {
			t.Errorf("streak = %d, want 3", got.Streak)
		}
	})

	t.Run("missing the latest session zeroes the streak, not the count", func(t *testing.T) {
		got := attendanceFrom(events, map[string]bool{"w2": true, "w3": true}, now, 8)
		if got.Streak != 0 {
			t.Errorf("streak = %d, want 0 — they missed the last one", got.Streak)
		}
		if got.Played != 2 {
			t.Errorf("played = %d, want 2", got.Played)
		}
	})

	t.Run("a scheduled session is not a missed one", func(t *testing.T) {
		next := now.Add(3 * 24 * time.Hour).Format(time.RFC3339)
		got := attendanceFrom(
			append([]model.Event{sess("next", &next)}, events...),
			map[string]bool{"w1": true}, now, 8)
		if got.Of != 8 {
			t.Errorf("of = %d, want 8 — next Tuesday hasn't happened", got.Of)
		}
		if got.Streak != 1 {
			t.Errorf("streak = %d, want 1 — the upcoming one must not break it",
				got.Streak)
		}
	})

	t.Run("a young club counts only what it has played", func(t *testing.T) {
		got := attendanceFrom(events[:3], map[string]bool{"w1": true, "w2": true},
			now, 8)
		if got.Of != 3 {
			t.Errorf("of = %d, want 3 — don't invent sessions to pad the window",
				got.Of)
		}
	})

	t.Run("dateless events are not sessions", func(t *testing.T) {
		got := attendanceFrom([]model.Event{sess("ladder", nil)},
			map[string]bool{"ladder": true}, now, 8)
		if got.Of != 0 {
			t.Errorf("of = %d, want 0 — a ladder isn't a Tuesday", got.Of)
		}
	})
}
