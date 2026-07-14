package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Supabase Auth (GoTrue) issues access tokens signed either with the project's
// shared secret (HS256, legacy) or an asymmetric signing key (ES256, current —
// verified against the project's published JWKS). We verify both inline with
// the standard library — no external dependency — then enforce ownership in the
// service layer.

type ctxKey int

const (
	userIDKey ctxKey = iota
	userEmailKey
	userNameKey
	userPhoneKey
)

var (
	// supabaseJWTSecret verifies legacy HS256 tokens (dashboard → Settings →
	// API → JWT Secret). Optional once the project uses asymmetric keys.
	supabaseJWTSecret = os.Getenv("SUPABASE_JWT_SECRET")
	// supabaseURL is the project URL; its JWKS endpoint provides the public
	// keys for ES256 token verification.
	supabaseURL = strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
)

var errInvalidToken = errors.New("invalid token")

// tokenClaims is the subset of the Supabase JWT we use. Aud is decoded raw
// because the JWT spec allows it to be either a string or an array of strings.
type tokenClaims struct {
	Sub          string          `json:"sub"` // the auth user's uuid
	Email        string          `json:"email"`
	Exp          int64           `json:"exp"`
	Iss          string          `json:"iss"`
	Aud          json.RawMessage `json:"aud"`
	UserMetadata map[string]any  `json:"user_metadata"` // Supabase profile metadata (e.g. full_name from signup)
}

// claimName pulls the signup display name out of user_metadata.full_name.
func claimName(c tokenClaims) string {
	if c.UserMetadata == nil {
		return ""
	}
	if n, ok := c.UserMetadata["full_name"].(string); ok {
		return strings.TrimSpace(n)
	}
	return ""
}

// claimPhone pulls the signup phone out of user_metadata.phone. Used to link
// guest registrations (registered by phone) to the account even before the user
// has saved a profile — pmp_profiles may not carry the phone yet.
func claimPhone(c tokenClaims) string {
	if c.UserMetadata == nil {
		return ""
	}
	if p, ok := c.UserMetadata["phone"].(string); ok {
		return strings.TrimSpace(p)
	}
	return ""
}

// expectedIssuer is the issuer Supabase stamps on its access tokens (the
// project's GoTrue URL). Empty when SUPABASE_URL is unset (dev), in which case
// the issuer check is skipped.
var expectedIssuer = func() string {
	if supabaseURL == "" {
		return ""
	}
	return supabaseURL + "/auth/v1"
}()

// hasAudience reports whether the token's `aud` claim contains want. It accepts
// both the string ("authenticated") and array (["authenticated", ...]) forms.
func hasAudience(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// jwks caches the project's ES256 public keys (by kid), refreshed lazily on a
// miss so key rotation is picked up without a restart.
var jwks = &jwksCache{keys: map[string]*ecdsa.PublicKey{}}

var jwksHTTP = &http.Client{Timeout: 5 * time.Second}

type jwksCache struct {
	mu   sync.RWMutex
	keys map[string]*ecdsa.PublicKey
}

func (j *jwksCache) keyFor(kid string) *ecdsa.PublicKey {
	j.mu.RLock()
	k := j.keys[kid]
	j.mu.RUnlock()
	if k != nil {
		return k
	}
	j.refresh()
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.keys[kid]
}

func (j *jwksCache) refresh() {
	if supabaseURL == "" {
		return
	}
	resp, err := jwksHTTP.Get(supabaseURL + "/auth/v1/.well-known/jwks.json")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return
	}
	next := make(map[string]*ecdsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "EC" || k.Crv != "P-256" {
			continue
		}
		xb, err1 := base64.RawURLEncoding.DecodeString(k.X)
		yb, err2 := base64.RawURLEncoding.DecodeString(k.Y)
		if err1 != nil || err2 != nil {
			continue
		}
		next[k.Kid] = &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(xb),
			Y:     new(big.Int).SetBytes(yb),
		}
	}
	j.mu.Lock()
	j.keys = next
	j.mu.Unlock()
}

// verifyToken validates a bearer token's signature (HS256 or ES256) and expiry
// and returns its claims. It rejects "none", unknown algs and expired tokens.
func verifyToken(raw string) (tokenClaims, error) {
	var c tokenClaims
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return c, errInvalidToken
	}

	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return c, errInvalidToken
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hdrJSON, &hdr) != nil {
		return c, errInvalidToken
	}

	signing := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, errInvalidToken
	}

	switch hdr.Alg {
	case "HS256":
		if supabaseJWTSecret == "" {
			return c, errInvalidToken
		}
		mac := hmac.New(sha256.New, []byte(supabaseJWTSecret))
		mac.Write([]byte(signing))
		if !hmac.Equal(mac.Sum(nil), sig) {
			return c, errInvalidToken
		}
	case "ES256":
		pub := jwks.keyFor(hdr.Kid)
		if pub == nil || len(sig) != 64 {
			return c, errInvalidToken
		}
		h := sha256.Sum256([]byte(signing))
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub, h[:], r, s) {
			return c, errInvalidToken
		}
	default:
		return c, errInvalidToken
	}

	clJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, errInvalidToken
	}
	if json.Unmarshal(clJSON, &c) != nil || c.Sub == "" {
		return c, errInvalidToken
	}
	// Require an expiry and enforce it — reject forged no-exp tokens.
	if c.Exp == 0 || time.Now().Unix() >= c.Exp {
		return c, errInvalidToken
	}
	// Audience must be Supabase's signed-in audience — rejects anon/service
	// keys and tokens minted for a different audience even if the signature and
	// expiry check out.
	if !hasAudience(c.Aud, "authenticated") {
		return c, errInvalidToken
	}
	// Issuer check (defense-in-depth; the signature already binds the token to
	// this project's keys). Only reject a token that *claims* a foreign issuer:
	// accept this project's GoTrue URL and the legacy "supabase" issuer, and
	// don't block when the claim is absent — so it can never lock out a valid
	// token whose issuer format differs across GoTrue versions.
	if c.Iss != "" && c.Iss != "supabase" && expectedIssuer != "" && c.Iss != expectedIssuer {
		return c, errInvalidToken
	}
	return c, nil
}

// bearer pulls the token out of an Authorization header ("Bearer <jwt>").
func bearer(r *http.Request) string {
	if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
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

// userEmail returns the authenticated user's verified email claim, or "".
func userEmail(r *http.Request) string {
	e, _ := r.Context().Value(userEmailKey).(string)
	return e
}

// userName returns the signup display name from the token's user_metadata, or
// "" — a fallback for the profile when pmp_profiles/players have no name yet.
func userName(r *http.Request) string {
	n, _ := r.Context().Value(userNameKey).(string)
	return n
}

// userPhone returns the signup phone from the token's user_metadata, or "".
func userPhone(r *http.Request) string {
	p, _ := r.Context().Value(userPhoneKey).(string)
	return p
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
		ctx := context.WithValue(r.Context(), userIDKey, c.Sub)
		ctx = context.WithValue(ctx, userEmailKey, c.Email)
		ctx = context.WithValue(ctx, userNameKey, claimName(c))
		ctx = context.WithValue(ctx, userPhoneKey, claimPhone(c))
		next(w, r.WithContext(ctx))
	}
}

// optionalAuth attaches the user id when a valid token is present but never
// rejects — for public endpoints that personalize when signed in.
func optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := verifyToken(bearer(r)); err == nil {
			ctx := context.WithValue(r.Context(), userIDKey, c.Sub)
			ctx = context.WithValue(ctx, userEmailKey, c.Email)
			ctx = context.WithValue(ctx, userNameKey, claimName(c))
			ctx = context.WithValue(ctx, userPhoneKey, claimPhone(c))
			r = r.WithContext(ctx)
		}
		next(w, r)
	}
}
