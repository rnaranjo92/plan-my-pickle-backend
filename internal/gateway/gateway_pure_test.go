package gateway

import "testing"

// TestGatewayConstructorsAndPureHelpers covers the Stripe/PayPal/Twilio
// constructors + their pure Live()/SetMarketplace()/Paid()/snippet helpers
// without invoking any SDK network call.
func TestGatewayConstructorsAndPureHelpers(t *testing.T) {
	st := NewStripeGateway("sk_test_x", "whsec_x")
	if !st.Live() {
		t.Error("configured Stripe gateway should report Live")
	}

	pp := NewPayPalGateway("cid", "secret", "https://api-m.sandbox.paypal.com", "wh-id")
	pp.SetMarketplace("partner-merchant", "bn-code")
	if !pp.Live() {
		t.Error("configured PayPal gateway should report Live")
	}

	if NewTwilioSms("AC123", "tok", "+15125550000") == nil {
		t.Error("NewTwilioSms returned nil")
	}

	if NewMockPayment().Live() {
		t.Error("mock payment is not live")
	}

	// Pure body-snippet helpers (truncate for logging).
	_ = snippet([]byte("a reasonably long response body used to exercise truncation logic in the snippet helper"))
	_ = snippet([]byte("short"))
	_ = snippet(nil)
	_ = ppSnippet([]byte("paypal response body for snippet"))
	_ = ppSnippet(nil)

	// PayPalCapture.Paid is a pure status check.
	_ = (PayPalCapture{}).Paid()
}
