package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
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
			map[string]any{"sub": "user-1", "email": "a@b.c", "aud": "authenticated", "exp": future})
		c, err := verifyToken(tok)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Sub != "user-1" {
			t.Fatalf("sub = %q, want user-1", c.Sub)
		}
	})

	t.Run("wrong audience rejected", func(t *testing.T) {
		tok := makeToken("test-secret", "HS256",
			map[string]any{"sub": "u", "aud": "anon", "exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected audience rejection")
		}
	})

	t.Run("missing audience rejected", func(t *testing.T) {
		tok := makeToken("test-secret", "HS256", map[string]any{"sub": "u", "exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected rejection when aud is absent")
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

// makeES256Token signs a JWT the way modern Supabase does (ES256 over the
// base64url header.payload, signature = r||s, 32 bytes each).
func makeES256Token(t *testing.T, priv *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(map[string]any{"alg": "ES256", "kid": kid, "typ": "JWT"})
	cb, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(hb)
	c := base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(h + "." + c))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return h + "." + c + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyTokenES256(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "test-es256-kid"
	jwks.mu.Lock()
	jwks.keys[kid] = &priv.PublicKey
	jwks.mu.Unlock()
	future := time.Now().Add(time.Hour).Unix()

	t.Run("valid ES256 accepted", func(t *testing.T) {
		tok := makeES256Token(t, priv, kid,
			map[string]any{"sub": "u-es", "aud": "authenticated", "exp": future})
		c, err := verifyToken(tok)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Sub != "u-es" {
			t.Fatalf("sub = %q, want u-es", c.Sub)
		}
	})

	t.Run("array-form audience accepted", func(t *testing.T) {
		tok := makeES256Token(t, priv, kid,
			map[string]any{"sub": "u-es", "aud": []string{"authenticated", "other"}, "exp": future})
		if _, err := verifyToken(tok); err != nil {
			t.Fatalf("unexpected error for array aud: %v", err)
		}
	})

	t.Run("wrong key rejected", func(t *testing.T) {
		other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tok := makeES256Token(t, other, kid, map[string]any{"sub": "u", "exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected signature rejection")
		}
	})

	t.Run("unknown kid rejected", func(t *testing.T) {
		tok := makeES256Token(t, priv, "no-such-kid", map[string]any{"sub": "u", "exp": future})
		if _, err := verifyToken(tok); err == nil {
			t.Fatal("expected unknown-kid rejection")
		}
	})

	t.Run("tampered claims rejected", func(t *testing.T) {
		tok := makeES256Token(t, priv, kid, map[string]any{"sub": "u-es", "exp": future})
		parts := strings.Split(tok, ".")
		// Swap in an "admin" payload but keep the original signature.
		forged := base64.RawURLEncoding.EncodeToString(
			[]byte(`{"sub":"admin","exp":` + strconv.FormatInt(future, 10) + `}`))
		bad := parts[0] + "." + forged + "." + parts[2]
		if _, err := verifyToken(bad); err == nil {
			t.Fatal("expected rejection of payload tampering")
		}
	})
}
