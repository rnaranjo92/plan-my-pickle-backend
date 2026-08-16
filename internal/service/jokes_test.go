package service

import (
	"strings"
	"testing"
	"time"
)

// Same day, same joke, for everyone — otherwise it isn't a joke of the DAY and
// two people on the same court are reading different ones.
func TestJokeIsStableWithinADayAndChangesBetween(t *testing.T) {
	d := time.Date(2026, 8, 15, 6, 0, 0, 0, time.UTC)
	morning := JokeOfTheDay(d)
	evening := JokeOfTheDay(d.Add(15 * time.Hour))
	if morning != evening {
		t.Fatal("the joke must not change during the day")
	}
	if JokeOfTheDay(d.AddDate(0, 0, 1)) == morning {
		t.Fatal("the joke must change the next day")
	}
}

// Cycling is fine; repeating within a fortnight is not.
func TestNoRepeatsAcrossTheRotation(t *testing.T) {
	seen := map[string]int{}
	d := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < JokeCount(); i++ {
		j := JokeOfTheDay(d.AddDate(0, 0, i))
		if first, dup := seen[j]; dup {
			t.Fatalf("day %d repeats day %d: %q", i, first, j)
		}
		seen[j] = i
	}
}

func TestJokesAreWellFormed(t *testing.T) {
	if JokeCount() < 100 {
		t.Fatalf("only %d jokes — the rotation is too short", JokeCount())
	}
	for i, j := range pickleballJokes {
		if strings.TrimSpace(j) == "" {
			t.Fatalf("joke %d is empty", i)
		}
		// Push notifications truncate; a punchline nobody sees isn't one.
		if len(j) > 160 {
			t.Fatalf("joke %d is %d chars — too long for a notification: %q",
				i, len(j), j)
		}
	}
}

// Every joke distinct in the source, not just across one pass of the cycle.
func TestNoDuplicateJokes(t *testing.T) {
	seen := map[string]bool{}
	for i, j := range pickleballJokes {
		if seen[j] {
			t.Fatalf("joke %d is a duplicate: %q", i, j)
		}
		seen[j] = true
	}
}
