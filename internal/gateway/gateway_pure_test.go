package gateway

import "testing"

// TestIsNANP verifies the US/Canada gate used to keep A2P 10DLC SMS off
// international numbers (those fall back to push).
func TestIsNANP(t *testing.T) {
	cases := map[string]bool{
		"(555) 123-4567":  true,  // bare US 10-digit
		"5551234567":      true,  // bare US 10-digit
		"15551234567":     true,  // 1 + 10
		"+1 555 123 4567": true,  // explicit +1 (US/CA)
		"+15551234567":    true,  // explicit +1
		"+447911123456":   false, // UK
		"+61412345678":    false, // Australia
		"+521234567890":   false, // Mexico (+52)
		"":                false,
		"12345":           false, // too short
	}
	for in, want := range cases {
		if got := IsNANP(in); got != want {
			t.Errorf("IsNANP(%q) = %v, want %v", in, got, want)
		}
	}
}

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
