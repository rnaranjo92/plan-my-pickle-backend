package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
)

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


// The finding that prompted these: checkout.session.completed carries NO line
// items, so the router used to see PriceID=="" and fall into the premium
// column — a Club (or coach) purchase granted $15 Premium instead of the
// product paid for. The fix stamps price_id into session metadata and reads it
// back; these pin the routing on both sides of that seam.
func TestClubCheckoutEventRoutesToClubColumns(t *testing.T) {
	t.Setenv("STRIPE_CLUB_PRICE_ID", "price_club_123")
	t.Setenv("STRIPE_PREMIUM_PRICE_ID", "price_premium_999")
	f := newFake()
	svc := newFakeSvc(t, f)
	if err := svc.applySubscriptionEvent(gateway.SubscriptionEvent{
		UserID: "buyer-1", PriceID: "price_club_123",
		SubscriptionID: "sub_club", Status: "active", Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	rows := f.written("pmp_profiles")
	if len(rows) == 0 {
		t.Fatal("nothing written")
	}
	row := rows[len(rows)-1]
	if v, ok := row["club_plan"].(bool); !ok || !v {
		t.Fatalf("club purchase must set club_plan, wrote: %v", row)
	}
	if _, hasPremium := row["premium"]; hasPremium {
		t.Fatalf("a club purchase must NEVER touch the premium column, wrote: %v", row)
	}
}

func TestPremiumCheckoutStillRoutesToPremium(t *testing.T) {
	t.Setenv("STRIPE_CLUB_PRICE_ID", "price_club_123")
	t.Setenv("STRIPE_PREMIUM_PRICE_ID", "price_premium_999")
	f := newFake()
	svc := newFakeSvc(t, f)
	if err := svc.applySubscriptionEvent(gateway.SubscriptionEvent{
		UserID: "buyer-2", PriceID: "price_premium_999",
		Status: "active", Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	rows := f.written("pmp_profiles")
	row := rows[len(rows)-1]
	if v, ok := row["premium"].(bool); !ok || !v {
		t.Fatalf("premium purchase must set premium, wrote: %v", row)
	}
	if _, hasClub := row["club_plan"]; hasClub {
		t.Fatalf("a premium purchase must not touch club_plan, wrote: %v", row)
	}
}
