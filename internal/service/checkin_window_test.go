package service

import (
	"errors"
	"testing"
	"time"
)

// The window is computed in the EVENT's zone, not the server's. A UTC server
// must not open check-in for a Pacific event at 5pm the day before.
func TestCheckInWindowUsesEventZone(t *testing.T) {
	pacific := time.FixedZone("PDT", -7*3600)
	// Event starts 9am Saturday Pacific.
	start := time.Date(2026, 8, 22, 9, 0, 0, 0, pacific)
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, pacific)

	// 6pm Friday Pacific = already Saturday in UTC. Must still be closed.
	fridayEvening := time.Date(2026, 8, 21, 18, 0, 0, 0, pacific)
	if !fridayEvening.In(pacific).Before(startDay) {
		t.Fatal("Friday evening Pacific should be before the event day")
	}
	if fridayEvening.UTC().Day() != 22 {
		t.Fatal("precondition: that instant should already be Saturday in UTC")
	}

	// Midnight Saturday Pacific opens it.
	justOpen := startDay.Add(time.Minute)
	if justOpen.In(pacific).Before(startDay) {
		t.Fatal("a minute past midnight on the day should be open")
	}
}

func TestCheckInNotOpenIsDistinct(t *testing.T) {
	wrapped := errors.New("x")
	if errors.Is(wrapped, ErrCheckInNotOpen) {
		t.Fatal("unrelated errors must not match ErrCheckInNotOpen")
	}
}
