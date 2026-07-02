package service

import (
	"testing"
	"time"
)

func TestDueNow(t *testing.T) {
	if !dueNow("") {
		t.Error("empty next_attempt_at should be due")
	}
	if !dueNow("not-a-timestamp") {
		t.Error("unparseable next_attempt_at should be treated as due (never stuck)")
	}
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if !dueNow(past) {
		t.Errorf("past time %q should be due", past)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if dueNow(future) {
		t.Errorf("future time %q should NOT be due", future)
	}
	// PostgREST returns timestamptz with a +00:00 offset, not Z — must still parse.
	futureOffset := time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000000+00:00")
	if dueNow(futureOffset) {
		t.Errorf("future offset time %q should NOT be due", futureOffset)
	}
}

func TestDuprNextAttempt(t *testing.T) {
	// Exponential backoff 1,2,4,8,16,30(capped)… — check the delay from now.
	wants := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, want := range wants {
		attempts := i + 1
		ts := duprNextAttempt(attempts)
		at, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Fatalf("attempts=%d: unparseable %q: %v", attempts, ts, err)
		}
		got := time.Until(at)
		// Allow slack for execution time.
		if got < want-5*time.Second || got > want+5*time.Second {
			t.Errorf("attempts=%d: backoff ~%v, want ~%v", attempts, got.Round(time.Second), want)
		}
	}
}
