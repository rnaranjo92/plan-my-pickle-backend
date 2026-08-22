package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The club page tints the top rule with the club's brand; every other hub page
// leaves Accent empty and must render byte-for-byte as before. The value only
// ever reaches the template already normalized to "#rrggbb", and html/template
// filters CSS values besides — but the template wiring itself is only checked
// at Execute time, so render both branches once.
func TestHubTemplateClubAccent(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).seoRender(w, seoHubTmpl, seoHubData{
		Title: "T", Canonical: "C", Description: "D", H1: "The Locals",
		Intro: "12 members.", Accent: "#0f4299",
	})
	body := w.Body.String()
	if !strings.Contains(body, "body::before{background:#0f4299}") {
		t.Fatalf("club accent missing from the page\n%s", body)
	}

	w = httptest.NewRecorder()
	(&Server{}).seoRender(w, seoHubTmpl, seoHubData{
		Title: "T", Canonical: "C", Description: "D", H1: "Tournaments",
		Intro: "intro",
	})
	if strings.Contains(w.Body.String(), "body::before{background:") {
		t.Fatal("pages without a brand must not emit the override at all")
	}
}
