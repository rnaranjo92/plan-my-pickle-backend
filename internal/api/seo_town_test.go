package api

import (
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
)

// The city page's job is to be substantial: the county pages it replaces in
// search were ~265 words of listing chrome, which is not a page Google has a
// reason to rank. Guard the body so a future edit can't quietly gut it.
func TestTownPageIsSubstantial(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).seoRender(w, seoTownTmpl, seoTownData{
		Title: "T", Canonical: "C", Description: "D",
		H1:    "Pickleball Tournaments in Chula Vista, California",
		Intro: "2 upcoming pickleball tournaments in Chula Vista, California.",
		City:  "Chula Vista", Place: "Chula Vista, California",
		County:    "San Diego County",
		CountyURL: "/pickleball-tournaments/california/san-diego-county",
		Cards:     []seoHubCard{{Name: "Battle of the Courts II", URL: "/e/1"}},
		JSONLD:    template.HTML(`{}`),
	})
	body := w.Body.String()

	// The city name must appear in the prose, not only the heading — that's the
	// difference between a page about a city and a template with a city in it.
	if n := strings.Count(body, "Chula Vista"); n < 4 {
		t.Fatalf("city named only %d times; page should read as being about it", n)
	}
	for _, want := range []string{
		"DUPR", "Round robin", "Double elimination", "Pools to playoff",
		"Entry fees", "Picking the right bracket",
		"/pickleball-tournaments/california/san-diego-county", // links up to county
		"/coaches", // and across to coaching
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}

	text := strings.Join(strings.Fields(stripTags(body)), " ")
	if n := len(strings.Fields(text)); n < 450 {
		t.Fatalf("page is %d words; the thin-content problem is not fixed", n)
	}
}

func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>':
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
