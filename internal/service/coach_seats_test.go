package service

import "testing"

// Sponsorship must never become the thing that gates a coach when the checks
// before it were supposed to. These pin the ORDER of CoachPlanActive's answers,
// which is what keeps a comped or paying coach from ever reaching a club
// lookup that could fail closed.
func TestCoachPlanShortCircuitsBeforeSponsorship(t *testing.T) {
	// Subscriptions off: everyone is active, and nothing else is consulted.
	t.Setenv("SUBSCRIPTIONS_ENABLED", "false")
	if !(&Service{}).CoachPlanActive("anyone") {
		t.Fatal("with subscriptions off every coach is active")
	}

	// On, but the plan cannot be bought: still active, still no club lookup.
	// A nil-store Service would panic if either path actually queried.
	t.Setenv("SUBSCRIPTIONS_ENABLED", "true")
	t.Setenv("STRIPE_COACH_PRICE_ID", "")
	if !(&Service{}).CoachPlanActive("anyone") {
		t.Fatal("no purchasable price means nobody is capped")
	}

	// And an empty user id is answered without touching anything.
	if (&Service{}).clubSponsoredCoach("") {
		t.Fatal("an empty user id was treated as sponsored")
	}
	if (&Service{}).clubSponsoredCoach("   ") {
		t.Fatal("whitespace was treated as a user id")
	}
}

// The seat follows the STAFF relationship, not membership. A club with forty
// members must not be buying forty coach plans it never agreed to.
func TestSponsorshipUsesTheCoOwnerRole(t *testing.T) {
	// The query is built from this constant; if the role string ever drifts
	// from what SetClubRole writes, sponsorship silently stops working and
	// coaches get capped with no explanation.
	if ClubRoleCoOwner != "co_owner" {
		t.Fatalf("co-owner role is %q — SetClubRole and the sponsorship lookup "+
			"must agree", ClubRoleCoOwner)
	}
}
