package service

import (
	"errors"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
)

// The reconciler can take away paid or granted access, so these tests are about
// one thing: proving it CAN'T, except in the single case where it should.

func TestRevokeStepProtectsComps(t *testing.T) {
	// A comp has no Stripe subscription behind it. Stripe not knowing about it
	// is its normal state — never evidence to revoke on.
	if got := revokeStep(true, "", false); got != revokeProtected {
		t.Fatalf("comped row with no subscription: got %v, want protected", got)
	}
	// Even a comp that somehow carries a subscription id, and is absent from the
	// active list, stays untouched. Comps are simply out of scope for this pass.
	if got := revokeStep(true, "sub_123", false); got != revokeProtected {
		t.Fatalf("comped row with a dead subscription: got %v, want protected", got)
	}
}

func TestRevokeStepProtectsHandGrants(t *testing.T) {
	// premium=true with no subscription id: a hand-set flag, or a grant made
	// before comps had their own column. Stripe never gave it, so Stripe can't
	// take it. This is the case that would have silently revoked the testers.
	if got := revokeStep(false, "", false); got != revokeProtected {
		t.Fatalf("hand-granted row: got %v, want protected", got)
	}
}

func TestRevokeStepLeavesActiveSubscribersAlone(t *testing.T) {
	if got := revokeStep(false, "sub_123", true); got != revokeLeaveAlone {
		t.Fatalf("active subscriber: got %v, want leave alone", got)
	}
}

func TestRevokeStepConfirmsBeforeRevoking(t *testing.T) {
	// The ONLY path toward revocation, and even it only asks a question.
	// Absence from the list is never a conclusion on its own — a truncated page
	// would otherwise revoke every subscriber past the cut.
	if got := revokeStep(false, "sub_123", false); got != revokeConfirmFirst {
		t.Fatalf("stale-looking row: got %v, want confirm first", got)
	}
}

func TestRevokeConfirmedRequiresASuccessfulInactiveRead(t *testing.T) {
	cases := []struct {
		name string
		ev   gateway.SubscriptionEvent
		err  error
		want bool
	}{
		{
			name: "confirmed cancelled — the one case that revokes",
			ev:   gateway.SubscriptionEvent{Active: false, Status: "canceled"},
			want: true,
		},
		{
			name: "still active — the list was stale, the direct read wins",
			ev:   gateway.SubscriptionEvent{Active: true, Status: "active"},
			want: false,
		},
		{
			// An unreachable Stripe means we know nothing. If this returned true,
			// one API outage would revoke every subscriber at once.
			name: "stripe unreachable — knowing nothing is not knowing 'cancelled'",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			// Defensive: an error alongside a zero-value (Active=false) event must
			// still not revoke. The error is checked first for exactly this reason.
			name: "error with an empty event",
			ev:   gateway.SubscriptionEvent{},
			err:  errors.New("timeout"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := revokeConfirmed(tc.ev, tc.err); got != tc.want {
				t.Fatalf("revokeConfirmed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanColumnsDontOverlap guards the one way this pass could corrupt data
// rather than just be wrong: premium and the coach plan writing each other's
// columns. They share a customer id (same Stripe customer) but must not share
// a flag, a status, or a subscription id — otherwise cancelling one plan
// revokes the other, which is the bug the separate columns exist to prevent.
func TestPlanColumnsDontOverlap(t *testing.T) {
	p, c := premiumColumns(), coachColumns()
	if p.flag == c.flag {
		t.Errorf("premium and coach share the flag column %q", p.flag)
	}
	if p.status == c.status {
		t.Errorf("premium and coach share the status column %q", p.status)
	}
	if p.subID == c.subID {
		t.Errorf("premium and coach share the subscription id column %q", p.subID)
	}
}

// TestReconcileNoopWhenSubscriptionsOff: while subscriptions are OFF everyone is
// Premium by policy (see IsPremium), so a pass that wrote `premium=false` from
// Stripe's opinion would be writing rows nothing reads — and would leave them
// wrong for the day subscriptions are switched on.
func TestReconcileNoopWhenSubscriptionsOff(t *testing.T) {
	t.Setenv("SUBSCRIPTIONS_ENABLED", "")
	if SubscriptionsEnabled() {
		t.Skip("subscriptions enabled by another env var in this environment")
	}
	// A nil-store Service is safe here precisely BECAUSE the guard returns before
	// touching anything; if the guard regresses, this panics rather than passing.
	s := &Service{}
	if got := s.ReconcileSubscriptions(); got != nil {
		t.Fatalf("expected no passes while subscriptions are off, got %v", got)
	}
}
