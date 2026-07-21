package service

import (
	"strings"
	"testing"
)

func fp(v float64) *float64 { return &v }

func TestRatingOutsideBand(t *testing.T) {
	cases := []struct {
		name           string
		rating, mn, mx *float64
		wantOutside    bool
		wantContains   string
	}{
		{"in band", fp(3.2), fp(3.0), fp(3.5), false, ""},
		{"above ceiling (sandbagger)", fp(4.2), fp(3.0), fp(3.5), true, "above"},
		{"below floor", fp(2.4), fp(3.0), fp(3.5), true, "below"},
		{"no rating", nil, fp(3.0), fp(3.5), false, ""},
		{"open division (no bounds)", fp(5.0), nil, nil, false, ""},
		{"ceiling only, ok", fp(3.4), nil, fp(3.5), false, ""},
		{"exactly at ceiling ok", fp(3.5), fp(3.0), fp(3.5), false, ""},
	}
	for _, c := range cases {
		out, reason := ratingOutsideBand(c.rating, c.mn, c.mx)
		if out != c.wantOutside {
			t.Errorf("%s: outside=%v want %v", c.name, out, c.wantOutside)
		}
		if c.wantContains != "" && !strings.Contains(reason, c.wantContains) {
			t.Errorf("%s: reason %q missing %q", c.name, reason, c.wantContains)
		}
	}
}

func TestNormalizeRatingEnforcement(t *testing.T) {
	for in, want := range map[string]string{
		"off": "off", "warn": "warn", "block": "block", "BLOCK": "block",
		"  Warn ": "warn", "": "off", "garbage": "off",
	} {
		if got := normalizeRatingEnforcement(in); got != want {
			t.Errorf("normalize(%q)=%q want %q", in, got, want)
		}
	}
}
