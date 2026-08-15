package service

import (
	"os"
	"testing"
)

// The whole point of the separation: a webhook must be routed by PRICE, because
// the two plans are different products and one boolean can't represent both.
// Get this wrong and a coach is granted organizer Premium they never bought, or
// cancelling one plan revokes the other.
func TestSubscriptionEventRoutedByPrice(t *testing.T) {
	t.Setenv("STRIPE_COACH_PRICE_ID", "price_coach_29")
	t.Setenv("STRIPE_PREMIUM_PRICE_ID", "price_premium_15")

	if !isCoachPlanEvent("price_coach_29") {
		t.Fatal("the coach price must route to the coach plan")
	}
	if isCoachPlanEvent("price_premium_15") {
		t.Fatal("the Premium price must NOT route to the coach plan")
	}
	// An unknown price is not the coach plan either — a plan added in the Stripe
	// dashboard that this build doesn't know about must not grant anything.
	if isCoachPlanEvent("price_something_new") {
		t.Fatal("an unknown price must not route to the coach plan")
	}
}

// With no coach price configured, nothing can be mistaken for the coach plan —
// so an install that hasn't set it up behaves exactly as it did before.
func TestCoachPlanInertWhenUnconfigured(t *testing.T) {
	t.Setenv("STRIPE_COACH_PRICE_ID", "")
	if isCoachPlanEvent("") || isCoachPlanEvent("price_anything") {
		t.Fatal("no coach price configured must mean no coach-plan events")
	}
}

// While billing is off everyone is treated as subscribed, so the roster cap
// can't strand a coach on a build where the plan isn't live.
func TestCoachPlanOpenWhileSubscriptionsDisabled(t *testing.T) {
	old := os.Getenv("SUBSCRIPTIONS_ENABLED")
	t.Setenv("SUBSCRIPTIONS_ENABLED", "false")
	defer t.Setenv("SUBSCRIPTIONS_ENABLED", old)

	if !(&Service{}).CoachPlanActive("any-user") {
		t.Fatal("with subscriptions off every coach must be treated as subscribed")
	}
}

// The free tier is a real number the pricing depends on; pin it so it can't
// drift silently away from what's advertised.
func TestFreeCoachStudentLimit(t *testing.T) {
	if kFreeCoachStudents != 3 {
		t.Fatalf("free tier = %d students, advertised as 3", kFreeCoachStudents)
	}
}
