package api

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

// Ladders are the Premium league type; the free ones must stay free. A
// first-time organizer starts with a round-robin, and a wall there costs more in
// adoption than it earns.
func TestOnlyLadderIsPremiumLeagueType(t *testing.T) {
	for _, lt := range []string{"ladder", "LADDER", " Ladder "} {
		if !service.PremiumLeagueType(lt) {
			t.Errorf("%q should be Premium", lt)
		}
	}
	for _, lt := range []string{"round_robin", "team", "flex", ""} {
		if service.PremiumLeagueType(lt) {
			t.Errorf("%q must NOT be Premium", lt)
		}
	}
}

// "rotation" is a WIZARD-ONLY pseudo-type: the client resolves it to
// leagueType "ladder" with ladderFormat "rotation" before it ever reaches the
// API. So gating "ladder" gates the rotation ladder too — which is the intent,
// since rotation is the ladder people actually run — but it means the gate can
// never be narrowed by matching on "rotation" here. Narrowing it would need the
// ladderFormat, which is a different field.
func TestRotationArrivesAsLadderNotRotation(t *testing.T) {
	if service.PremiumLeagueType("rotation") {
		t.Fatal("the API never receives 'rotation'; matching it would be dead code")
	}
}
