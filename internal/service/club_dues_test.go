package service

import "testing"

// The reference is the ONLY thing connecting a Stripe payment back to a member
// and a period — dues have no registration row to hang off. A malformed one
// must never be guessed at, because the guess would credit the wrong person's
// membership.
func TestDuesRefRoundTrip(t *testing.T) {
	period, user := "11111111-2222-3333-4444-555555555555", "user-abc"
	gotPeriod, gotUser := parseDuesRef(duesRef(period, user))
	if gotPeriod != period || gotUser != user {
		t.Errorf("round trip lost data: %q / %q", gotPeriod, gotUser)
	}
}

func TestParseDuesRefRefusesGarbage(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "no-colon", ":", "period:", ":user", " : ",
	} {
		p, u := parseDuesRef(bad)
		if p != "" || u != "" {
			t.Errorf("parseDuesRef(%q) = %q / %q — a malformed reference must "+
				"credit nobody", bad, p, u)
		}
	}
}

// A user id containing a colon must not silently truncate: SplitN with a limit
// of 2 keeps everything after the first separator on the user side.
func TestParseDuesRefKeepsTheWholeUserID(t *testing.T) {
	p, u := parseDuesRef("period-1:weird:user:id")
	if p != "period-1" || u != "weird:user:id" {
		t.Errorf("got %q / %q", p, u)
	}
}
