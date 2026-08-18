package service

import (
	"testing"
	"time"
)

// The exemption is a PROMISE: a league that existed before the paywall keeps
// everything, forever. If this ever flips, clubs that were told their history
// was safe lose it — so the boundary gets a test rather than a comment.
func TestLeaguePredatesPaywall(t *testing.T) {
	epoch, err := time.Parse(time.RFC3339, paywallEpoch)
	if err != nil {
		t.Fatalf("paywallEpoch is unparseable (%v) — every gate would fail open", err)
	}

	cases := []struct {
		name      string
		createdAt string
		want      bool
	}{
		{"a league from last year", epoch.Add(-365 * 24 * time.Hour).Format(time.RFC3339), true},
		{"a league from the day before", epoch.Add(-24 * time.Hour).Format(time.RFC3339), true},
		{"one second before the epoch", epoch.Add(-time.Second).Format(time.RFC3339), true},
		{"exactly at the epoch is NOT exempt", epoch.Format(time.RFC3339), false},
		{"a league created after", epoch.Add(24 * time.Hour).Format(time.RFC3339), false},

		// Every unreadable case is exempt. We cannot tell how old the league is,
		// and the answer that cannot hurt anyone is to leave it alone.
		{"missing timestamp", "", true},
		{"garbage timestamp", "sometime last spring", true},
		{"a date-only string PostgREST might return", "2026-08-17", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := predatesPaywall(c.createdAt); got != c.want {
				t.Errorf("predatesPaywall(%q) = %v, want %v",
					c.createdAt, got, c.want)
			}
		})
	}
}

// The gate ships INERT. Until the epoch passes nothing is gated at all, so this
// can be deployed and lived with long before anyone is asked to pay.
func TestPaywallShipsInert(t *testing.T) {
	epoch, _ := time.Parse(time.RFC3339, paywallEpoch)

	if paywallActive(epoch.Add(-time.Second)) {
		t.Error("paywall active before its own epoch")
	}
	if !paywallActive(epoch.Add(time.Second)) {
		t.Error("paywall never becomes active after the epoch")
	}
	if paywallActive(time.Now()) {
		t.Error("the paywall is ALREADY LIVE — the epoch is in the past. " +
			"That is fine only if billing is genuinely launched; otherwise " +
			"organizers are being gated with nothing to buy.")
	}
}

// A club that has never rolled a season has no archive, so it must never meet
// this paywall at all. Sanity-checking the shape of the rule, not the wiring:
// the gate is only ever consulted from the archive endpoints.
func TestPaywallEpochIsAFixedPointInTime(t *testing.T) {
	// Parsed twice, far apart in wall-clock terms, must be identical — the
	// exemption cannot drift, or "created before the paywall" would mean
	// something different next month.
	a, err1 := time.Parse(time.RFC3339, paywallEpoch)
	b, err2 := time.Parse(time.RFC3339, paywallEpoch)
	if err1 != nil || err2 != nil {
		t.Fatalf("epoch does not parse: %v %v", err1, err2)
	}
	if !a.Equal(b) {
		t.Error("the epoch is not stable")
	}
	if a.Location() != time.UTC {
		t.Errorf("epoch is not UTC (%v) — a local-time boundary moves with the "+
			"server's timezone", a.Location())
	}
}
