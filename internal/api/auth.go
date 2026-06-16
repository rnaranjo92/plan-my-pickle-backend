package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

// Supabase Auth (GoTrue) issues HS256 access tokens signed with the project's
// JWT secret (dashboard → Settings → API → JWT Secret). We verify them inline
// with the standard library — no external dependency, matching this backend's
// minimalism — then enforce ownership in the service layer.

type ctxKey int

const userIDKey ctxKey = iota

// supabaseJWTSecret signs every Supabase access token. When empty, requireAuth
// fails closed (rejects everything) so we never accidentally run unprotected.
var supabaseJWTSecret = os.Getenv("SUPABASE_JWT_SECRET")

var errInvalidToken = errors.New("invalid token")

// tokenClaims is the subset of the Supabase JWT we use.
type tokenClaims struct {
	Sub   string `json:"sub"` // the auth user's uuid
	Email string `json:"email"`
	Exp   int64  `json:"exp"`
}

// verifyToken validates a bearer token's HS256 signature and expiry and returns
// its claims. It rejects "none"/asymmetric algs and expired tokens.
func verifyToken(raw string) (tokenClaims, error) {
	var c tokenClaims
	if supabaseJWTSecret == "" {
		return c, errInvalidToken
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return c, errInvalidToken
	}

	// Header: must be HS256 (block alg confusion / "none").
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return c, errInvalidToken
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(hdrJSON, &hdr) != nil || hdr.Alg != "HS256" {
		return c, errInvalidToken
	}

	// Signature: HMAC-SHA256 over "header.payload", constant-time compared.
	mac := hmac.New(sha256.New, []byte(supabaseJWTSecret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(want, got) {
		return c, errInvalidToken
	}

	// Claims.
	clJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, errInvalidToken
	}
	if json.Unmarshal(clJSON, &c) != nil || c.Sub == "" {
		return c, errInvalidToken
	}
	if c.Exp > 0 && time.Now().Unix() >= c.Exp {
		return c, errInvalidToken
	}
	return c, nil
}

// bearer pulls the token out of an Authorization header ("Bearer <jwt>").
func bearer(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(authz, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// userID returns the authenticated user's uuid, or "" if the request was not
// (successfully) authenticated.
func userID(r *http.Request) string {
	id, _ := r.Context().Value(userIDKey).(string)
	return id
}

// requireAuth rejects requests without a valid Supabase token (401) and stashes
// the user id in context for the handler.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := verifyToken(bearer(r))
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, c.Sub)))
	}
}

// optionalAuth attaches the user id when a valid token is present but never
// rejects — for public endpoints that personalize when signed in.
func optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := verifyToken(bearer(r)); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), userIDKey, c.Sub))
		}
		next(w, r)
	}
}
