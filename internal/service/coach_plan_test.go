package service

import (
	"os"
	"strings"
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

// SUBSCRIPTIONS_ENABLED is already true in production (organizer Premium runs on
// it), so the coach cap must NOT switch itself on the moment the migration runs.
// Without a purchasable price a capped coach has no way to pay — blocked with no
// upgrade path is strictly worse than free.
func TestCoachCapOffWhenPlanIsNotPurchasable(t *testing.T) {
	t.Setenv("SUBSCRIPTIONS_ENABLED", "true")
	t.Setenv("STRIPE_COACH_PRICE_ID", "")
	if !(&Service{}).CoachPlanActive("some-coach") {
		t.Fatal("with no coach price configured nobody may be capped")
	}
}

func TestCompedCoachEmailsParsing(t *testing.T) {
	t.Setenv("COMPED_COACH_EMAILS", " Austen@Example.com ,, second@example.com,")
	got := compedCoachEmails()
	// Two from the env, plus the hardcoded founding coaches. Blanks dropped.
	if want := 2 + len(foundingCoachEmails); len(got) != want {
		t.Fatalf("parsed %d emails, want %d (2 env + %d founding, blanks dropped)",
			len(got), want, len(foundingCoachEmails))
	}
	// Lower-cased and trimmed, so the env value doesn't have to be exact.
	if !got["austen@example.com"] || !got["second@example.com"] {
		t.Fatalf("emails should be normalised, got %v", got)
	}
	// An unset env must still comp the founding coaches — that's the point of
	// hardcoding them.
	t.Setenv("COMPED_COACH_EMAILS", "")
	base := compedCoachEmails()
	if len(base) != len(foundingCoachEmails) {
		t.Fatalf("unset env comped %d, want the %d founding coaches",
			len(base), len(foundingCoachEmails))
	}
}

// Austen is the first coach on the platform and must never be billed by an
// accident of configuration.
func TestFoundingCoachIsCompedWithoutAnyEnv(t *testing.T) {
	t.Setenv("COMPED_COACH_EMAILS", "")
	if !compedCoachEmails()["asveom@lt.life"] {
		t.Fatal("Austen must be comped with no env set")
	}
	// And case/whitespace in the stored email must not defeat it.
	if !compedCoachEmails()[strings.ToLower(strings.TrimSpace(" ASVEOM@LT.LIFE "))] {
		t.Fatal("comp lookup must be case-insensitive")
	}
}
