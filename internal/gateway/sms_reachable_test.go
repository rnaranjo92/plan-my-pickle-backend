package gateway

import "testing"

// SmsReachable gates outbound A2P SMS by the SMS_COUNTRIES allowlist. With the
// default ("1") it must behave exactly like the old NANP-only gate; setting the
// env opens specific countries (US/CA keep working) while others stay push-only.
func TestSmsReachable(t *testing.T) {
	// Default allowlist (env unset) == NANP-only.
	t.Run("default is NANP-only", func(t *testing.T) {
		t.Setenv("SMS_COUNTRIES", "")
		cases := map[string]bool{
			"5551234567":       true,  // bare US 10-digit
			"15551234567":      true,  // 1 + 10
			"+1 555 123 4567":  true,  // explicit +1
			"+44 7911 123456":  false, // UK — push-only by default
			"+61 400 123 456":  false, // AU
			"+63 917 123 4567": false, // PH
			"":                 false,
		}
		for in, want := range cases {
			if got := SmsReachable(in); got != want {
				t.Errorf("SmsReachable(%q)=%v, want %v", in, got, want)
			}
		}
	})

	// Opening UK + AU + PH must let those through while US/CA still work and an
	// un-listed country (Germany +49) stays blocked.
	t.Run("allowlist opens specific countries", func(t *testing.T) {
		t.Setenv("SMS_COUNTRIES", "1,44,61,63")
		cases := map[string]bool{
			"+1 555 123 4567":  true,  // US/CA still on
			"+44 7911 123456":  true,  // UK now on
			"+61 400 123 456":  true,  // AU now on
			"+63 917 123 4567": true,  // PH now on
			"+49 151 12345678": false, // DE not listed → push-only
		}
		for in, want := range cases {
			if got := SmsReachable(in); got != want {
				t.Errorf("SmsReachable(%q)=%v, want %v", in, got, want)
			}
		}
	})

	// A NANP-disabled allowlist blocks bare US numbers (only the listed intl ones).
	t.Run("NANP off blocks bare US numbers", func(t *testing.T) {
		t.Setenv("SMS_COUNTRIES", "44")
		if SmsReachable("5551234567") {
			t.Error("bare US number should be blocked when '1' is not in the allowlist")
		}
		if !SmsReachable("+44 7911 123456") {
			t.Error("UK number should be reachable when '44' is allowlisted")
		}
	})
}
