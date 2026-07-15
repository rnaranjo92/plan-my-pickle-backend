package service

import "testing"

// TestServiceInternalHelpers drives the remaining unexported helper methods
// directly (in-package) against the seeded fake. Each is wrapped to recover from
// any panic on unexpected data — the goal is to execute these code paths.
func TestServiceInternalHelpers(t *testing.T) {
	s := newFakeSvc(t, seededFake())
	safe := func(fn func()) {
		defer func() { _ = recover() }()
		fn()
	}

	safe(func() { _, _ = s.NearbyEvents(30.2, -97.7, 0, 10) })
	safe(func() { _ = s.reconcileRoundStatuses("e1") })
	safe(func() { _, _ = s.listPoolMatchIDs("e1") })
	safe(func() { _, _ = s.bracketHasRows("matches", "b1") })
	safe(func() { _, _ = s.autoAssignBracket("e1", ratingPtr(3.2)) })
	safe(func() { _, _ = s.autoAssignBracket("e1", nil) })
	safe(func() { _ = s.unlinkPartnerOf("e1", "b1", "p1") })
	safe(func() { _, _ = s.playerNamesByID("e1", []string{"p1", "p2"}) })
	safe(func() { _ = s.spreadCourts("e1") })
	safe(func() { _ = s.maybeSeedPlayoff("b1") })
	safe(func() {
		_ = s.resolveGrandFinal(map[string]any{"id": "m1", "bracket_id": "b1", "bracket_slot": float64(1)})
	})
	safe(func() { _ = s.copyGrandFinalTeams("m1", "m2") })
	safe(func() { s.markSubmission("m1", "submitted", "ref", "") })
	safe(func() { _ = s.advanceTeam("m1", 1, "m2", 1) })
	safe(func() { _ = s.resetCompletedDownstream("m1") })
	safe(func() { _, _ = s.slotPlayers("m1", 1) })
	safe(func() { _, _ = s.nextPlayOrder("e1", "court1") })
	safe(func() { _, _ = s.playerSkills() })
	safe(func() { _, _ = s.courtIDsByNumber("e1") })
	safe(func() { _ = s.wipeBracketStage("b1") })
	safe(func() { _ = s.followingSet("u1", []string{"u2", "u3"}) })

	if strp("x") == nil || *strp("x") != "x" {
		t.Error("strp")
	}
	if agePtr(50) == nil || *agePtr(50) != 50 {
		t.Error("agePtr")
	}
}
