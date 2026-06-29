package gateway

import "testing"

// TestStripeVerifyWebhookRejectsBadSig exercises Stripe webhook verification's
// failure path — it's local crypto (no network); a bogus signature must error.
func TestStripeVerifyWebhookRejectsBadSig(t *testing.T) {
	g := NewStripeGateway("sk_test_x", "whsec_test_x")
	if _, err := g.VerifyWebhook([]byte(`{"id":"evt_1","type":"x"}`), "t=1,v1=deadbeef"); err == nil {
		t.Error("expected a verification error for a bad signature")
	}
	if _, err := g.VerifyWebhook([]byte(`{}`), ""); err == nil {
		t.Error("expected a verification error for an empty signature")
	}
}

// TestPayPalAuthAssertion covers the pure PayPal-Auth-Assertion builder (a
// base64 JWT header.payload), which has no network dependency.
func TestPayPalAuthAssertion(t *testing.T) {
	g := NewPayPalGateway("client-id", "secret", "https://api-m.sandbox.paypal.com", "wh-id")
	g.SetMarketplace("partner-merchant", "bn-code")
	if got := g.authAssertion("merchant-123"); got == "" {
		t.Error("authAssertion should produce a non-empty assertion")
	}
	_ = g.authAssertion("") // exercise the empty-seller branch too
}
