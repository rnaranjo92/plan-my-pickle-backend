package api

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"San Diego":      "san-diego",
		"  New York  ":   "new-york",
		"St. Petersburg": "st-petersburg",
		"Coeur d'Alene":  "coeur-d-alene",
		"CALIFORNIA":     "california",
		"Winston-Salem":  "winston-salem",
		"":               "",
		"!!!":            "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCentsToDollars(t *testing.T) {
	cases := map[int]string{0: "0", 100: "1", 2500: "25", 999: "9.99", 1050: "10.50", 5: "0.05"}
	for in, want := range cases {
		if got := centsToDollars(in); got != want {
			t.Errorf("centsToDollars(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSeoIsDemoName(t *testing.T) {
	for _, n := range []string{"Summer Slam Test", "DEMO Event", "authcheck run", "dbg 3"} {
		if !seoIsDemoName(n) {
			t.Errorf("%q should be flagged demo", n)
		}
	}
	for _, n := range []string{"SoCal Contest", "Tested Champions", "Austin Summer Slam"} {
		if seoIsDemoName(n) {
			t.Errorf("%q should NOT be flagged demo (word-boundary)", n)
		}
	}
}

func TestSeoDateHelpers(t *testing.T) {
	if got := isoDate("2026-07-04T15:30:00Z"); got != "2026-07-04" {
		t.Errorf("isoDate = %q", got)
	}
	if got := fmtEventDate("2026-07-04T15:30:00Z"); got != "Sat, Jul 4, 2026" {
		t.Errorf("fmtEventDate = %q", got)
	}
	if isoDate("") != "" || fmtEventDate("garbage") != "" {
		t.Error("bad/empty dates must return empty")
	}
}

func TestPlural(t *testing.T) {
	if plural(1, "event", "events") != "1 event" {
		t.Error("singular")
	}
	if plural(3, "event", "events") != "3 events" {
		t.Error("plural")
	}
}
