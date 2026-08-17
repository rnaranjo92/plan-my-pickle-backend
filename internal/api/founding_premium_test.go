package api

import (
	"os"
	"testing"
)

// Austen is the first coach on the platform and was comped for the COACH plan in
// payments.go — but nothing else. Everything gated by mayUsePremium reads
// premiumAllowed, so without a premium comp he would keep his coaching and lose
// ladder creation the moment SUBSCRIPTIONS_ENABLED flips on.
//
// Two separate comps for one founding user is a trap. These tests are the thing
// that notices if someone edits the list or the env handling and re-opens it.
func TestFoundingPremiumSurvivesEveryEnv(t *testing.T) {
	const austen = "asveom@lt.life"

	cases := []struct {
		name      string
		allowlist string
		setAllow  bool
	}{
		{name: "no PREMIUM_ALLOWLIST set"},
		{name: "allowlist set to someone else", allowlist: "other@example.com", setAllow: true},
		{name: "allowlist explicitly empty", allowlist: "", setAllow: true},
		{name: "allowlist wide open", allowlist: "*", setAllow: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setAllow {
				t.Setenv("PREMIUM_ALLOWLIST", c.allowlist)
			} else {
				_ = os.Unsetenv("PREMIUM_ALLOWLIST")
			}
			if !premiumAllowed(austen) {
				t.Errorf("%s lost premium with PREMIUM_ALLOWLIST=%q — a founding "+
					"comp must not be revocable by config", austen, c.allowlist)
			}
		})
	}
}

// Case and stray whitespace come from however the address was typed at sign-up,
// and an email that only matches when typed exactly is a comp that fails quietly.
func TestFoundingPremiumIgnoresCaseAndSpace(t *testing.T) {
	t.Setenv("PREMIUM_ALLOWLIST", "nobody@example.com")
	for _, form := range []string{
		"asveom@lt.life", "ASVEOM@LT.LIFE", "Asveom@Lt.Life", "  asveom@lt.life  ",
	} {
		if !premiumAllowed(form) {
			t.Errorf("premiumAllowed(%q) = false, want true", form)
		}
	}
}

// The comp must not accidentally hand premium to everyone — a bug here would be
// invisible until the revenue didn't arrive.
func TestFoundingPremiumDoesNotLeak(t *testing.T) {
	t.Setenv("PREMIUM_ALLOWLIST", "nobody@example.com")
	for _, other := range []string{
		"", "someone@else.com", "asveom@lt.life.evil.com", "notasveom@lt.life",
	} {
		if premiumAllowed(other) {
			t.Errorf("premiumAllowed(%q) = true — the founding comp is leaking", other)
		}
	}
}
