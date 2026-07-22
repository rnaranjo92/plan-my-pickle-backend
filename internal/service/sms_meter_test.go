package service

import "testing"

// smsMonthlyAllowance parses SMS_MONTHLY_ALLOWANCE; unset / invalid / negative
// all mean "metering off" (0), which the gate treats as unlimited.
func TestSmsMonthlyAllowance(t *testing.T) {
	cases := []struct {
		env  string
		set  bool
		want int
	}{
		{"", false, 0},   // unset → off
		{"", true, 0},    // empty → off
		{"0", true, 0},   // explicit zero → off
		{"500", true, 500},
		{"-5", true, 0},  // negative is nonsense → off (fail open)
		{"abc", true, 0}, // garbage → off
		{" 250 ", true, 250},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv("SMS_MONTHLY_ALLOWANCE", c.env)
		} else {
			// Ensure a clean environment for the unset case.
			t.Setenv("SMS_MONTHLY_ALLOWANCE", "")
		}
		if got := smsMonthlyAllowance(); got != c.want {
			t.Errorf("smsMonthlyAllowance() with %q = %d, want %d", c.env, got, c.want)
		}
	}
}

// smsPeriod is a UTC YYYY-MM stamp — 7 chars, dash in the middle.
func TestSmsPeriodShape(t *testing.T) {
	p := smsPeriod()
	if len(p) != 7 || p[4] != '-' {
		t.Errorf("smsPeriod() = %q, want YYYY-MM", p)
	}
}
