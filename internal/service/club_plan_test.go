package service

import "testing"

// The webhook routes subscriptions to plan columns BY PRICE ID. Getting this
// wrong is how buying one product grants another — a coach subscription once
// nearly granted organizer Premium — so the routing rules are pinned here.

func TestClubPlanEventMatchesOnlyItsOwnPrice(t *testing.T) {
	t.Setenv("STRIPE_CLUB_PRICE_ID", "price_club_123")
	if !isClubPlanEvent("price_club_123") {
		t.Fatal("the club price must route to the club plan")
	}
	for _, p := range []string{"price_premium_999", "price_coach_777", ""} {
		if isClubPlanEvent(p) {
			t.Fatalf("price %q must NOT route to the club plan", p)
		}
	}
}

func TestUnconfiguredClubPriceMatchesNothing(t *testing.T) {
	t.Setenv("STRIPE_CLUB_PRICE_ID", "")
	// The classic empty==empty trap: with no club price configured, an event
	// with an empty price id must not be mistaken for a club purchase.
	if isClubPlanEvent("") {
		t.Fatal("empty price must not match an unconfigured club plan")
	}
	if isClubPlanEvent("price_anything") {
		t.Fatal("no price can be a club purchase while the plan is unconfigured")
	}
}

// ClubPlanActive is STRICT where CoachPlanActive is permissive: an
// unconfigured price grants nothing (founding access is carried by `comped`,
// not by an accident of configuration). This pins the difference so a
// well-meaning refactor doesn't "align" the two and open the tier for free.
func TestClubPlanIsNotGrantedByAnUnconfiguredPrice(t *testing.T) {
	t.Setenv("SUBSCRIPTIONS_ENABLED", "true")
	t.Setenv("STRIPE_CLUB_PRICE_ID", "")
	// A profile that is neither comped nor subscribed.
	svc := newFakeSvc(t, newFake().seed("pmp_profiles",
		`[{"user_id":"u1","comped":false,"club_plan":false}]`))
	if svc.ClubPlanActive("u1") {
		t.Fatal("an unconfigured club price must not grant the tier")
	}
}

func TestClubPlanGrantedByCompAndBySubscription(t *testing.T) {
	t.Setenv("SUBSCRIPTIONS_ENABLED", "true")
	// Founding club: comped, no subscription — must pass.
	svc := newFakeSvc(t, newFake().seed("pmp_profiles",
		`[{"user_id":"u1","comped":true,"club_plan":false}]`))
	if !svc.ClubPlanActive("u1") {
		t.Fatal("a comped founding club must hold the tier")
	}
	// Paying club: subscription, no comp — must pass.
	svc = newFakeSvc(t, newFake().seed("pmp_profiles",
		`[{"user_id":"u2","comped":false,"club_plan":true}]`))
	if !svc.ClubPlanActive("u2") {
		t.Fatal("a live club_plan subscription must hold the tier")
	}
}

func TestClubPlanFreeWhileSubscriptionsAreOff(t *testing.T) {
	t.Setenv("SUBSCRIPTIONS_ENABLED", "false")
	svc := &Service{}
	if !svc.ClubPlanActive("anyone") {
		t.Fatal("with subscriptions globally off, everything is free — parity with IsPremium")
	}
}
