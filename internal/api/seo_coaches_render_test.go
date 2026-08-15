package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The templates are template.Must at package init and the data structs are only
// bound at Execute time, so a typo'd field is invisible to the compiler. Render
// both pages once with real-shaped data.
func TestCoachTemplatesRender(t *testing.T) {
	rate := 7500
	yrs := 6
	w := httptest.NewRecorder()
	(&Server{}).seoRender(w, seoCoachHubTmpl, seoCoachHubData{
		Title: "T", Canonical: "C", Description: "D", H1: "Pickleball Coaches",
		Intro: "intro",
		Cards: []seoCoachCard{{Name: "Ann Lee", City: "Chula Vista",
			Skills: "Dinking", Rate: coachRate(&rate), URL: "/coach/x", Verified: true}},
	})
	body := w.Body.String()
	for _, want := range []string{"Ann Lee", "Chula Vista", "$75/hr", "/coach/x",
		"Verified", "/coaches/apply"} {
		if !strings.Contains(body, want) {
			t.Fatalf("hub missing %q\n%s", want, body)
		}
	}

	w = httptest.NewRecorder()
	(&Server{}).seoRender(w, seoCoachTmpl, seoCoachPageData{
		Title: "T", Canonical: "C", Description: "D", H1: "Ann Lee",
		CityLine: "Chula Vista", RateLine: coachRate(&rate),
		ExpLine: plural(yrs, "year", "years") + " coaching",
		Bio:     "Bio here", Verified: true, BookURL: "https://app/x",
	})
	body = w.Body.String()
	for _, want := range []string{"Ann Lee", "6 years coaching", "$75/hr",
		"Bio here", "https://app/x", "Verified Coach"} {
		if !strings.Contains(body, want) {
			t.Fatalf("profile missing %q\n%s", want, body)
		}
	}
}

