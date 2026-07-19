package service

import (
	"strings"
	"testing"
)

func TestSanitizeEmailField(t *testing.T) {
	// Subject: single-line — CRLF collapsed to a space, trimmed.
	if got := sanitizeEmailField("  Hi\r\nthere  ", 120, true); got != "Hi there" {
		t.Fatalf("subject sanitize = %q, want %q", got, "Hi there")
	}
	// Whitespace-only collapses to empty (so it isn't stored as junk).
	if got := sanitizeEmailField("   \n  ", 1000, false); got != "" {
		t.Fatalf("whitespace-only should be empty, got %q", got)
	}
	// Message: multiline preserved with \r\n normalized to \n.
	if got := sanitizeEmailField("a\r\nb", 1000, false); got != "a\nb" {
		t.Fatalf("multiline normalize = %q, want %q", got, "a\nb")
	}
	// Clamp measures runes, not bytes (unicode-safe).
	if got := sanitizeEmailField(strings.Repeat("é", 1500), 1000, false); len([]rune(got)) != 1000 {
		t.Fatalf("clamp = %d runes, want 1000", len([]rune(got)))
	}
}

func TestRegistrationEmailBody(t *testing.T) {
	html, text := registrationEmailBody(emailBrand{},
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
	htmlP, textP := registrationEmailBody(emailBrand{},
		"Kim", "Slam", "TBA", "", "", "https://x", true, "")
	if strings.Contains(htmlP, "Powered by") || strings.Contains(textP, "Powered by") {
		t.Fatal("premium email must not carry the house mark")
	}
	// Empty detail rows disappear rather than rendering blank labels.
	if strings.Contains(htmlP, "Division") {
		t.Fatal("empty division must not render a row")
	}

	// Event names with HTML get escaped, not injected.
	htmlX, _ := registrationEmailBody(emailBrand{},
		"A", "<script>alert(1)</script>", "", "", "", "https://x", true, "")
	if strings.Contains(htmlX, "<script>") {
		t.Fatal("event name must be HTML-escaped")
	}

	// Custom organizer note renders in both bodies, preserves line breaks, and is
	// HTML-escaped (no injection).
	htmlN, textN := registrationEmailBody(emailBrand{},
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

func TestSanitizeHexColor(t *testing.T) {
	cases := map[string]string{
		"#4f8b3b": "#4f8b3b", "#ABC": "#aabbcc", "#FFFFFF": "#ffffff",
		"4f8b3b": "", "#12": "", "#12345g": "", "": "", "  #0F4299 ": "#0f4299",
		"red": "", "#1234567": "",
	}
	for in, want := range cases {
		if got := sanitizeHexColor(in); got != want {
			t.Errorf("sanitizeHexColor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeHTTPURL(t *testing.T) {
	cases := map[string]string{
		"https://cdn.example.com/logo.png": "https://cdn.example.com/logo.png",
		"http://x.io/a.jpg":                "http://x.io/a.jpg",
		"javascript:alert(1)":              "",
		"data:image/png;base64,AAA":        "",
		"/relative/logo.png":               "",
		"ftp://x.io/a.png":                 "",
		"":                                 "",
		"not a url":                        "",
	}
	for in, want := range cases {
		if got := sanitizeHTTPURL(in, 400); got != want {
			t.Errorf("sanitizeHTTPURL(%q) = %q, want %q", in, got, want)
		}
	}
	// Over-length URL is rejected.
	if got := sanitizeHTTPURL("https://x.io/"+strings.Repeat("a", 500), 400); got != "" {
		t.Errorf("over-length URL should be rejected, got %q", got)
	}
}

func TestReadableText(t *testing.T) {
	// Dark accent → white text; light accent → dark text.
	if got := readableText("#16245c"); got != "#ffffff" {
		t.Errorf("navy should get white text, got %q", got)
	}
	if got := readableText("#f5c518"); got != "#16203a" {
		t.Errorf("yellow should get dark text, got %q", got)
	}
	if got := readableText("bad"); got != "#ffffff" {
		t.Errorf("malformed hex should fall back to white, got %q", got)
	}
}

func TestBrandedEmailApplies(t *testing.T) {
	b := emailBrand{
		logoURL:   "https://cdn.example.com/logo.png",
		color:     "#0f4299",
		signature: "— The Chula Vista Pickleball Club",
	}
	html, text := customEmailBody(b, "Kim Naranjo", "Summer Slam",
		"Courts moved to the west side", "Please use the west entrance.",
		"https://app.planmypickle.com/?event=abc", true)
	for _, want := range []string{
		"Courts moved to the west side",    // subject → title
		"https://cdn.example.com/logo.png", // logo in header
		"#0f4299",                          // accent on header/button
		"Please use the west entrance.",    // body
		"Chula Vista Pickleball Club",      // signature
		"Summer Slam",                      // event name as eyebrow
	} {
		if !strings.Contains(html, want) {
			t.Errorf("branded html missing %q", want)
		}
	}
	// Premium → no house mark; signature present in text.
	if strings.Contains(html, "Powered by") {
		t.Error("premium branded email must not carry the house mark")
	}
	if !strings.Contains(text, "Chula Vista Pickleball Club") {
		t.Error("signature must appear in the plain-text body")
	}

	// Free-tier custom email keeps the house mark and ignores stored branding
	// (brandFor gates it), so a default emailBrand still renders powered-by.
	htmlFree, _ := customEmailBody(emailBrand{}, "Kim", "Slam", "Hi", "msg",
		"https://x", false)
	if !strings.Contains(htmlFree, "Powered by") {
		t.Error("free-tier custom email must carry the house mark")
	}
	// Message HTML is escaped, not injected.
	htmlInj, _ := customEmailBody(emailBrand{}, "Kim", "Slam", "S",
		"<script>alert(1)</script>", "https://x", true)
	if strings.Contains(htmlInj, "<script>") {
		t.Error("custom message must be HTML-escaped")
	}
}
