package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// makeToken forges a JWT the same way Supabase signs them (HS256 over the
// base64url header.payload), so we can exercise verifyToken without a live
// Supabase project.
func makeToken(secret, alg string, claims map[string]any) string {
	hb, _ := json.Marshal(map[string]any{"alg": alg, "typ": "JWT"})
	cb, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(hb)
	c := base64.RawURLEncoding.EncodeToString(cb)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h + "." + c))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return h + "." + c + "." + sig
}

func TestVerifyToken(t *testing.T) {
	supabaseJWTSecret = "test-secret"
	future := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()

	t.Run("valid token returns subject", func(t *testing.T) {
		tok := makeToken("test-secret", "HS256",
			map[string]any{"sub": "user-1", "email": "a@b.c", "exp": future})
		c, err := verifyToken(tok)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Sub != "user-1" {
			t.Fatalf("sub = %q, want user-1", c.Sub)
		}
	})

	t.Run("expired token rejected", func(t *testing.T) {
		tok := makeToken("test-secret", "HS256", map[string]any{"sub": "u", "exp": past})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected expiry rejection")
		}
	})

	t.Run("wrong secret rejected", func(t *testing.T) {
		tok := makeToken("other-secret", "HS256", map[string]any{"sub": "u", "exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected signature rejection")
		}
	})

	t.Run("alg none rejected", func(t *testing.T) {
		tok := makeToken("test-secret", "none", map[string]any{"sub": "u", "exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected alg rejection (alg confusion)")
		}
	})

	t.Run("missing sub rejected", func(t *testing.T) {
		tok := makeToken("test-secret", "HS256", map[string]any{"exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected missing-subject rejection")
		}
	})

	t.Run("malformed rejected", func(t *testing.T) {
		if _, err := verifyToken("not.a.real.jwt"); err == nil {
			t.Fatal("expected malformed rejection")
		}
	})

	t.Run("empty secret fails closed", func(t *testing.T) {
		saved := supabaseJWTSecret
		supabaseJWTSecret = ""
		defer func() { supabaseJWTSecret = saved }()
		tok := makeToken("anything", "HS256", map[string]any{"sub": "u", "exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected fail-closed when no secret configured")
		}
	})
}
