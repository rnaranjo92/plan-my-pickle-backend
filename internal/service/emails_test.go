package service

import (
	"strings"
	"testing"
)

func TestRegistrationEmailBody(t *testing.T) {
	html, text := registrationEmailBody(
		"Kim Naranjo", "GREENS vs RETRO", "Saturday, July 4, 2026 · 8:35 AM",
		"The HUB — Chula Vista, CA", "Intermediate 2",
		"https://app.planmypickle.com/?event=abc", false, "")

	for _, want := range []string{
		"GREENS vs RETRO", "Hi Kim", "Intermediate 2",
		"The HUB — Chula Vista, CA", "https://app.planmypickle.com/?event=abc",
		"Powered by", // free tier carries the house mark
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing %q", want)
		}
	}
	for _, want := range []string{"GREENS vs RETRO", "Intermediate 2", "Powered by PlanMyPickle"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q", want)
		}
	}

	// Premium organizer → unbranded email (same rule as the app views).
	htmlP, textP := registrationEmailBody(
		"Kim", "Slam", "TBA", "", "", "https://x", true, "")
	if strings.Contains(htmlP, "Powered by") || strings.Contains(textP, "Powered by") {
		t.Fatal("premium email must not carry the house mark")
	}
	// Empty detail rows disappear rather than rendering blank labels.
	if strings.Contains(htmlP, "Division") {
		t.Fatal("empty division must not render a row")
	}

	// Event names with HTML get escaped, not injected.
	htmlX, _ := registrationEmailBody(
		"A", "<script>alert(1)</script>", "", "", "", "https://x", true, "")
	if strings.Contains(htmlX, "<script>") {
		t.Fatal("event name must be HTML-escaped")
	}

	// Custom organizer note renders in both bodies, preserves line breaks, and is
	// HTML-escaped (no injection).
	htmlN, textN := registrationEmailBody(
		"Kim", "Slam", "TBA", "", "", "https://x", true,
		"Bring water!\n<b>Gate opens 8am</b>")
	if !strings.Contains(htmlN, "Bring water!<br>") {
		t.Fatal("custom note must render with newlines as <br>")
	}
	if strings.Contains(htmlN, "<b>Gate") {
		t.Fatal("custom note must be HTML-escaped")
	}
	if !strings.Contains(textN, "Bring water!") {
		t.Fatal("custom note must appear in the plain-text body")
	}
}
