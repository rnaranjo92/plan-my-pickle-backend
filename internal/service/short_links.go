package service

import (
	"crypto/rand"
	"log"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Short links for SMS: the report/confirm token URLs (~85 chars) forced every
// text into 2 SMS segments — half the per-tournament Twilio bill. Texted links
// go out as {api}/r/<7-char code> (~30 chars) and 302-redirect to the full
// token URL, keeping the whole message inside one 160-char segment.

const shortLinkBase = "https://api.planmypickle.com/r/"

const shortCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789" // no 0/O/1/l/I

// shortCode returns a random 7-char code (58^7 ≈ 2e12 — collisions are
// practically impossible; the caller retries once anyway).
func shortCode() string {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		return "" // caller falls back to the long URL
	}
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(shortCodeAlphabet[int(c)%len(shortCodeAlphabet)])
	}
	return sb.String()
}

// ShortLink stores target under a fresh code and returns the short URL.
// STRICTLY best-effort: any failure (table missing pre-migration, collision
// twice, rand failure) returns the ORIGINAL url — a text with a long link
// costs an extra segment; a text with a broken link costs a player's score.
func (s *Service) ShortLink(target string) string {
	for attempt := 0; attempt < 2; attempt++ {
		code := shortCode()
		if code == "" {
			break
		}
		if _, err := s.sb.Insert("short_links", map[string]any{
			"code": code, "target": target,
		}); err != nil {
			continue // collision or missing table — retry once, then fall back
		}
		return shortLinkBase + code
	}
	log.Printf("short-link: falling back to long URL (insert failed)")
	return target
}

// ResolveShortLink returns the stored target for a code ("" = unknown).
func (s *Service) ResolveShortLink(code string) (string, error) {
	row, err := s.sb.SelectOne("short_links",
		"code=eq."+store.Q(code)+"&select=target")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", nil
	}
	return asStr(row, "target"), nil
}
