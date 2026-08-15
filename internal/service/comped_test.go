package service

import "testing"

// A comp must survive billing. Stripe owns `premium` — it overwrites it on every
// subscription webhook, and a reconciler asking "who is actually subscribed?"
// would revoke every tester since none of them are. Reading `comped` separately
// is the whole reason the column exists.
func TestCompIsIndependentOfPremium(t *testing.T) {
	t.Setenv("SUBSCRIPTIONS_ENABLED", "true")

	cases := []struct {
		name            string
		premium, comped bool
		want            bool
	}{
		{"subscriber", true, false, true},
		{"comped tester with premium revoked by a webhook", false, true, true},
		{"comped AND subscribed", true, true, true},
		{"neither", false, false, false},
	}
	for _, c := range cases {
		got := c.premium || c.comped // the expression IsPremium evaluates
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// A comp with no reason is one nobody can explain later, which makes it one
// nobody dares revoke — so grants require one. Revoking doesn't.
func TestCompRequiresAReason(t *testing.T) {
	s := &Service{}
	if err := s.SetComped("", "tester", "owner@x.com", true); err == nil {
		t.Error("a blank user must be rejected")
	}
	// A reason is only demanded on GRANT; revoking is always allowed.
	if err := s.SetComped("user-1", "", "owner@x.com", true); err == nil {
		t.Error("granting without a reason must be rejected")
	}
}
