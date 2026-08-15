package service

import "testing"

func TestSlugForIndexMatchesSEOSlugs(t *testing.T) {
	// Must produce byte-identical slugs to the api package's slugify, or the
	// pinged hub URLs 404 and every submission is wasted.
	cases := map[string]string{
		"San Diego County": "san-diego-county",
		"Chula Vista":      "chula-vista",
		"California":       "california",
		"  O'Fallon  ":     "o-fallon",
		"St. Petersburg":   "st-petersburg",
		"":                 "",
		"---":              "",
	}
	for in, want := range cases {
		if got := slugForIndex(in); got != want {
			t.Errorf("slugForIndex(%q) = %q, want %q", in, got, want)
		}
	}
}

// With no key configured this must be completely inert — it ships dark.
func TestIndexNowInertWithoutKey(t *testing.T) {
	if indexNowKey != "" {
		t.Skip("a key is configured in this environment")
	}
	SubmitURLs("https://planmypickle.com/e/abc")
	SubmitEventURLs("abc", "Chula Vista", "San Diego County", "California")
	if len(indexNowRecent) != 0 {
		t.Fatal("nothing should be recorded when no key is set")
	}
}
