package service

import (
	"strings"
	"testing"
	"time"
)

func TestRotationRoundOverdue(t *testing.T) {
	now := time.Date(2026, 8, 17, 19, 30, 0, 0, time.UTC)
	grace := 75 * time.Second

	cases := []struct {
		name   string
		endsAt time.Time
		want   bool
	}{
		{"still running", now.Add(3 * time.Minute), false},
		{"just hit zero — the organizer's device gets first refusal",
			now, false},
		{"inside the grace window", now.Add(-60 * time.Second), false},
		{"exactly at the grace boundary is not yet overdue",
			now.Add(-grace), false},
		{"past the grace window", now.Add(-90 * time.Second), true},
		{"long abandoned", now.Add(-40 * time.Minute), true},
		{"no deadline at all is never overdue", time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rotationRoundOverdue(c.endsAt, now, grace); got != c.want {
				t.Errorf("overdue = %v, want %v", got, c.want)
			}
		})
	}
}

// The filter is the safety argument. A sweep that advanced a PAUSED night would
// undo the pause the organizer just took, and one that advanced a MANUAL night
// would rotate a room whose organizer is deliberately holding it — so both
// exclusions are pinned here rather than trusted to survive an edit.
func TestRotationSweepFilterExcludesPausedAndManual(t *testing.T) {
	q := rotationSweepFilter("2026-08-17T19:30:00Z")

	if !strings.Contains(q, "status=eq.live") {
		t.Error("missing status=eq.live — a paused session could be advanced")
	}
	if !strings.Contains(q, "auto_advance=is.true") {
		t.Error("missing auto_advance filter — manual mode could be overruled")
	}
	if !strings.Contains(q, "round_ends_at=lt.2026-08-17T19:30:00Z") {
		t.Errorf("cutoff not applied: %s", q)
	}
	// Without a bound, one stuck session could make every tick a full scan.
	if !strings.Contains(q, "limit=") {
		t.Error("unbounded sweep query")
	}
	// current_round is what makes the advance idempotent against the
	// organizer's device; without it the sweep can't pass an expected round.
	if !strings.Contains(q, "current_round") {
		t.Error("current_round not selected — the advance can't be made idempotent")
	}
}

// The backstop must stay a backstop. If the grace ever drops near zero the
// server starts racing the organizer's device for every round.
func TestRotationSweepGraceIsABackstop(t *testing.T) {
	if rotationSweepGrace < 30*time.Second {
		t.Errorf("grace %v is too short — the server would race the client",
			rotationSweepGrace)
	}
	if rotationSweepGrace > 3*time.Minute {
		t.Errorf("grace %v is too long — the gym is standing around",
			rotationSweepGrace)
	}
}

// A session that is merely paused for dinner must not be mistaken for one the
// organizer walked away from — and a session in setup (no deadline at all) must
// never be swept, because that's the night being prepared right now.
func TestRotationSessionAbandoned(t *testing.T) {
	now := time.Date(2026, 8, 17, 21, 0, 0, 0, time.UTC)
	fmtT := func(d time.Duration) string {
		return now.Add(d).Format(time.RFC3339)
	}
	cases := []struct {
		name   string
		endsAt string
		want   bool
	}{
		{"round still running", fmtT(5 * time.Minute), false},
		{"finished ten minutes ago", fmtT(-10 * time.Minute), false},
		{"a long dinner break", fmtT(-3 * time.Hour), false},
		{"paused all evening", fmtT(-11 * time.Hour), false},
		{"left running overnight", fmtT(-13 * time.Hour), true},
		{"left running since last week", fmtT(-7 * 24 * time.Hour), true},
		{"never started — no deadline", "", false},
		{"unparseable deadline", "not a timestamp", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rotationSessionAbandoned(c.endsAt, now); got != c.want {
				t.Errorf("abandoned = %v, want %v", got, c.want)
			}
		})
	}
}
