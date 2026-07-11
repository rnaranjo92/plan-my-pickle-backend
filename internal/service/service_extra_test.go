package service

import "testing"

// TestServiceFeedAndHelpers covers the feed-text builders and a handful of
// internal helper methods against the seeded fake. Errors are tolerated.
func TestServiceFeedAndHelpers(t *testing.T) {
	s := newFakeSvc(t, seededFake())

	// Feed-text builders (read DB, build a string; no external calls).
	_, _ = s.ChampionFeedText("m1")
	_, _ = s.CheckinFeedText("r1")
	_, _ = s.MatchFeedText("m1", true)
	_, _ = s.MatchFeedText("m1", false)
	s.PostChampionFeed("e1", "m1", "Champions crowned!")

	// Internal helpers (in-package access).
	_ = s.countRows("registrations", "event_id=eq.e1", "id")
	_, _ = s.completedMatchCount("e1")
	_, _ = s.courtIDsByNumber("e1")
	_, _ = s.bracketRegs("e1", "b1")
	_, _ = s.bracketStarted("b1")
	_, _ = s.clubOwner("cl1")
	_, _ = s.PlayerProfile("p1", "")
	_, _ = s.PlayoffSeedTeams("b1", "points")
}
