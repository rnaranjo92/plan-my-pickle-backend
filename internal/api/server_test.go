package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

// ---------------------------------------------------------------------------
// Test scaffolding
// ---------------------------------------------------------------------------

// authToken forges a valid HS256 Supabase token for the given subject so
// requireAuth/optionalAuth accept it. Callers must set supabaseJWTSecret first
// (newTestServer does). Reuses makeToken from auth_test.go.
func authToken(t *testing.T, sub string) string {
	t.Helper()
	return makeToken("test-secret", "HS256", map[string]any{
		"sub": sub,
		"aud": "authenticated",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
}

// mockSupabase is a fake PostgREST + RPC server. It routes by table/function
// name (the path segment after /rest/v1/) and returns whatever JSON the test
// seeded for that route. Unseeded GET routes return an empty array so
// best-effort count/activity queries don't fail the read under test.
type mockSupabase struct {
	srv    *httptest.Server
	tables map[string]string // table or rpc/<fn> -> JSON body
}

func newMockSupabase(t *testing.T) *mockSupabase {
	t.Helper()
	m := &mockSupabase{tables: map[string]string{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// path looks like /rest/v1/<table> or /rest/v1/rpc/<fn>
		rest := strings.TrimPrefix(r.URL.Path, "/rest/v1/")
		key := rest
		if strings.HasPrefix(rest, "rpc/") {
			key = rest // keep the rpc/ prefix as the lookup key
		}
		if body, ok := m.tables[key]; ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		// Default: empty result set. Keeps best-effort secondary queries quiet.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// seed registers a canned JSON body for a table (e.g. "events") or rpc
// ("rpc/standings").
func (m *mockSupabase) seed(table, jsonBody string) { m.tables[table] = jsonBody }

// newTestServer wires a real api.NewServer(service.New()) against the mock
// Supabase, with supabaseJWTSecret set so forged tokens verify.
func newTestServer(t *testing.T, m *mockSupabase) http.Handler {
	t.Helper()
	supabaseJWTSecret = "test-secret"
	t.Setenv("SUPABASE_URL", m.srv.URL)
	t.Setenv("SUPABASE_SERVICE_KEY", "k")
	return NewServer(service.New())
}

// ---------------------------------------------------------------------------
// Middleware: requireAuth / optionalAuth
// ---------------------------------------------------------------------------

func TestRequireAuth(t *testing.T) {
	supabaseJWTSecret = "test-secret"

	var sawUser string
	h := requireAuth(func(w http.ResponseWriter, r *http.Request) {
		sawUser = userID(r)
		w.WriteHeader(http.StatusOK)
	})

	t.Run("missing token is 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("garbage token is 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer not.a.jwt")
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("expired token is 401", func(t *testing.T) {
		tok := makeToken("test-secret", "HS256", map[string]any{
			"sub": "u", "aud": "authenticated",
			"exp": time.Now().Add(-time.Hour).Unix(),
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("valid token passes through with user id", func(t *testing.T) {
		tok := makeToken("test-secret", "HS256", map[string]any{
			"sub": "user-42", "aud": "authenticated",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if sawUser != "user-42" {
			t.Fatalf("userID = %q, want user-42", sawUser)
		}
	})
}

func TestOptionalAuth(t *testing.T) {
	supabaseJWTSecret = "test-secret"

	t.Run("no token still calls handler with empty user", func(t *testing.T) {
		called := false
		var uid string
		h := optionalAuth(func(w http.ResponseWriter, r *http.Request) {
			called = true
			uid = userID(r)
		})
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if !called {
			t.Fatal("handler not called")
		}
		if uid != "" {
			t.Fatalf("userID = %q, want empty", uid)
		}
	})

	t.Run("invalid token still calls handler (never rejects)", func(t *testing.T) {
		called := false
		h := optionalAuth(func(w http.ResponseWriter, r *http.Request) { called = true })
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer garbage")
		h(rec, req)
		if !called {
			t.Fatal("handler not called for invalid token")
		}
	})

	t.Run("valid token attaches user + email", func(t *testing.T) {
		var uid, email string
		h := optionalAuth(func(w http.ResponseWriter, r *http.Request) {
			uid = userID(r)
			email = userEmail(r)
		})
		tok := makeToken("test-secret", "HS256", map[string]any{
			"sub": "u-opt", "email": "x@y.z", "aud": "authenticated",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		h(rec, req)
		if uid != "u-opt" || email != "x@y.z" {
			t.Fatalf("uid=%q email=%q, want u-opt/x@y.z", uid, email)
		}
	})
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestBearer(t *testing.T) {
	cases := []struct {
		hdr  string
		want string
	}{
		{"Bearer abc", "abc"},
		{"Bearer  spaced  ", "spaced"},
		{"abc", ""},        // no prefix
		{"", ""},           // empty
		{"bearer abc", ""}, // case-sensitive prefix
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.hdr != "" {
			req.Header.Set("Authorization", c.hdr)
		}
		if got := bearer(req); got != c.want {
			t.Errorf("bearer(%q) = %q, want %q", c.hdr, got, c.want)
		}
	}
}

func TestUserIDAndEmailEmptyContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if userID(req) != "" {
		t.Error("userID with no context value should be empty")
	}
	if userEmail(req) != "" {
		t.Error("userEmail with no context value should be empty")
	}
	// And with values present.
	ctx := context.WithValue(req.Context(), userIDKey, "abc")
	ctx = context.WithValue(ctx, userEmailKey, "e@x.y")
	req = req.WithContext(ctx)
	if userID(req) != "abc" {
		t.Error("userID should read context value")
	}
	if userEmail(req) != "e@x.y" {
		t.Error("userEmail should read context value")
	}
}

func TestHasAudience(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`"authenticated"`, true},
		{`"anon"`, false},
		{`["authenticated","x"]`, true},
		{`["x","y"]`, false},
		{``, false},
		{`123`, false},
	}
	for _, c := range cases {
		if got := hasAudience(json.RawMessage(c.raw), "authenticated"); got != c.want {
			t.Errorf("hasAudience(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]string{"a": "b"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["a"] != "b" {
		t.Fatalf("body = %v", got)
	}
}

func TestWriteErr(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusBadRequest, errors.New("boom"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["error"] != "boom" {
		t.Fatalf("error body = %v", got)
	}
}

func TestStatusMapsServiceErrors(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{service.ErrNotFound, http.StatusNotFound},
		{service.ErrForbidden, http.StatusForbidden},
		{service.ErrPremiumRequired, http.StatusPaymentRequired},
		{errors.New("anything else"), http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		status(rec, c.err)
		if rec.Code != c.want {
			t.Errorf("status(%v) = %d, want %d", c.err, rec.Code, c.want)
		}
	}
}

func TestDecode(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"x"}`))
		var v struct {
			Name string `json:"name"`
		}
		if !decode(rec, req, &v) {
			t.Fatal("decode returned false for valid JSON")
		}
		if v.Name != "x" {
			t.Fatalf("name = %q", v.Name)
		}
	})

	t.Run("empty body tolerated", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
		var v struct{ Name string }
		if !decode(rec, req, &v) {
			t.Fatal("decode returned false for empty body (should tolerate EOF)")
		}
	})

	t.Run("malformed JSON is 400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
		var v struct{ Name string }
		if decode(rec, req, &v) {
			t.Fatal("decode returned true for malformed JSON")
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3, 60)
	key := "1.2.3.4"
	for i := 0; i < 3; i++ {
		if !rl.allow(key) {
			t.Fatalf("attempt %d should be allowed (limit 3)", i+1)
		}
	}
	if rl.allow(key) {
		t.Fatal("4th attempt should be blocked")
	}
	// A different key is independent.
	if !rl.allow("other") {
		t.Fatal("different key should be allowed")
	}
}

func TestWithCORS(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := withCORS(inner)

	acao := func(origin, method string) (int, string) {
		req := httptest.NewRequest(method, "/", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("Access-Control-Allow-Origin")
	}

	t.Run("OPTIONS preflight from an allow-listed origin → 204 + reflected origin", func(t *testing.T) {
		code, got := acao("https://app.planmypickle.com", http.MethodOptions)
		if code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", code)
		}
		if got != "https://app.planmypickle.com" {
			t.Fatalf("ACAO = %q, want the reflected origin (never \"*\")", got)
		}
	})

	t.Run("allow-listed + preview origins are reflected; others get no ACAO", func(t *testing.T) {
		for _, o := range []string{
			"https://app.planmypickle.com",
			"https://planmypickle.com",
			"https://app-git-branch.vercel.app",
		} {
			if _, got := acao(o, http.MethodGet); got != o {
				t.Errorf("origin %q: ACAO = %q, want it reflected", o, got)
			}
		}
		if _, got := acao("https://evil.example.com", http.MethodGet); got != "" {
			t.Errorf("third-party origin should get no ACAO, got %q", got)
		}
	})

	t.Run("non-OPTIONS passes through with method headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Fatalf("inner not reached: status = %d", rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Methods") == "" {
			t.Fatal("missing CORS methods header")
		}
	})
}

// ---------------------------------------------------------------------------
// Validation / error paths that fail before touching the DB
// ---------------------------------------------------------------------------

func TestGeocodeMissingQuery(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/geocode", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing q", rec.Code)
	}
}

func TestNearbyEventsMissingLatLng(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/nearby", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing lat/lng", rec.Code)
	}
}

func TestProtectedRoutesRejectAnon(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	// A sampling of requireAuth-guarded routes — all must 401 without a token.
	routes := []struct {
		method, path string
	}{
		{http.MethodGet, "/me/events"},
		{http.MethodGet, "/me/profile"},
		{http.MethodGet, "/me/feed"},
		{http.MethodPost, "/events"},
		{http.MethodPost, "/clubs"},
		{http.MethodGet, "/leagues"},
		{http.MethodGet, "/users/search"},
		{http.MethodDelete, "/me"},
	}
	for _, rt := range routes {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", rt.method, rt.path, rec.Code)
		}
	}
}

func TestCreateEventMalformedBody(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{bad`))
	req.Header.Set("Authorization", "Bearer "+authToken(t, "owner-1"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed body", rec.Code)
	}
}

// A non-Premium organizer creating a DUPR-SANCTIONED event must be blocked with
// 402 — sanctioning is the Premium gate (basic event creation itself is free).
// The premium check runs before any DB work, so this is deterministic under the
// mock. (Previously this test asserted that *all* event creation was Premium,
// which no longer matches the model: the tournament engine is free.)
func TestCreateEventSanctionedNonPremiumIs402(t *testing.T) {
	m := newMockSupabase(t)
	// IsPremium queries Supabase; with empty/default rows the user is not premium.
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events",
		strings.NewReader(`{"name":"Test Open","format":"singles","duprSanctioned":true}`))
	req.Header.Set("Authorization", "Bearer "+authToken(t, "owner-1"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (premium required) for a non-premium creator of a DUPR-sanctioned event", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Read handlers, end-to-end through the mux + mock Supabase
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "ok" {
		t.Fatalf("body = %v", got)
	}
}

func TestListEventsAnonReturnsEmpty(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	// optionalAuth with no token => userID == "" => ListEvents returns [].
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("anon list body = %q, want []", rec.Body.String())
	}
}

func TestListEventsAuthed(t *testing.T) {
	m := newMockSupabase(t)
	m.seed("events", `[{"id":"e1","name":"Spring Open","owner_id":"owner-1"}]`)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, "owner-1"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not a JSON array: %v (%s)", err, rec.Body.String())
	}
	if len(got) != 1 || got[0]["name"] != "Spring Open" {
		t.Fatalf("events = %v", got)
	}
}

func TestGetEventFound(t *testing.T) {
	m := newMockSupabase(t)
	m.seed("events", `[{"id":"e1","name":"Spring Open","owner_id":"owner-1"}]`)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/e1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON object: %v", err)
	}
	if got["name"] != "Spring Open" {
		t.Fatalf("event = %v", got)
	}
}

func TestGetEventNotFound(t *testing.T) {
	m := newMockSupabase(t)
	// events table returns empty => SelectOne nil => ErrNotFound => 404.
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPublicEvents(t *testing.T) {
	m := newMockSupabase(t)
	// QA/test-named events stay out of the marketing feed even when listed —
	// but only on a whole-word "test" match ("Contest"/"Tested" still show).
	m.seed("events", `[
		{"id":"e1","name":"Listed One","listed":true},
		{"id":"e2","name":"Test","listed":true},
		{"id":"e3","name":"Bday Smash Test 2","listed":true},
		{"id":"e4","name":"TEST · Doubles 3.0-4.0 · 150","listed":true},
		{"id":"e5","name":"SoCal Contest","listed":true},
		{"id":"e6","name":"Demo Open Slam","listed":true},
		{"id":"e7","name":"dbg","listed":true},
		{"id":"e8","name":"authcheck","listed":true}
	]`)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/public", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not a JSON array: %v", err)
	}
	names := make([]string, len(got))
	for i, e := range got {
		names[i] = e["name"].(string)
	}
	if len(got) != 2 {
		t.Fatalf("public events = %v, want only [Listed One, SoCal Contest]", names)
	}
	for _, n := range names {
		if n != "Listed One" && n != "SoCal Contest" {
			t.Fatalf("test-named event leaked into the public feed: %v", names)
		}
	}
}

func TestRosterEmpty(t *testing.T) {
	m := newMockSupabase(t)
	// registrations returns [] (default) => empty roster, 200.
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/e1/roster", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("roster body = %q, want []", rec.Body.String())
	}
}

func TestSanctionCSV(t *testing.T) {
	m := newMockSupabase(t)
	m.seed("events", `[{"id":"e1","name":"Sanctioned Slam","owner_id":"owner-1","dupr_sanctioned":true}]`)
	h := newTestServer(t, m)

	// Anonymous → 401 (owner-only export).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/e1/sanction.csv", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon status = %d, want 401", rec.Code)
	}

	// Owner → 200 CSV with the sanction header row.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events/e1/sanction.csv", nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, "owner-1"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("content-type = %q, want text/csv", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sanction-Ready Export") ||
		!strings.Contains(body, "DUPR sanctioned,yes") {
		t.Fatalf("csv missing expected headers: %q", body)
	}
}

func TestVendorApplicationFlow(t *testing.T) {
	// The mock store returns canned bodies (no real state), so each assertion
	// exercises one read/write against a fixed vendors table: a pending
	// application must be OWNER-visible (with contact info) but hidden from
	// the public list; approved rows show publicly with contact stripped.
	m := newMockSupabase(t)
	m.seed("events", `[{"id":"e1","name":"Slam","owner_id":"owner-1"}]`)
	m.seed("vendors", `[
		{"id":"v1","event_id":"e1","name":"Squeeze Lemonade","status":"pending","contact_email":"v@x.com","pitch":"fresh"},
		{"id":"v2","event_id":"e1","name":"Paddle Demos","status":"approved","contact_email":"p@x.com"}
	]`)
	h := newTestServer(t, m)

	// Public application → 201 (insert echoes the canned table's first row).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events/e1/vendor-apply",
		strings.NewReader(`{"name":"Squeeze Lemonade","contactEmail":"v@x.com","pitch":"fresh"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("apply status = %d (%s)", rec.Code, rec.Body.String())
	}

	// Application without any contact info → 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/events/e1/vendor-apply",
		strings.NewReader(`{"name":"No Contact"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("contactless apply = %d, want 400", rec.Code)
	}

	// Anonymous list → pending hidden, approved shown, contact stripped.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/e1/vendors", nil))
	body := rec.Body.String()
	if strings.Contains(body, "Squeeze") {
		t.Fatalf("pending application leaked to public list: %s", body)
	}
	if !strings.Contains(body, "Paddle Demos") {
		t.Fatalf("approved vendor missing from public list: %s", body)
	}
	if strings.Contains(body, "@x.com") {
		t.Fatalf("contact info leaked publicly: %s", body)
	}

	// Owner list → pending row visible WITH contact email.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/events/e1/vendors", nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, "owner-1"))
	h.ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, "Squeeze") ||
		!strings.Contains(body, "v@x.com") {
		t.Fatalf("owner list missing pending application/contact: %s", body)
	}

	// Approve: owner → 200; non-owner → 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/vendors/v1/status",
		strings.NewReader(`{"status":"approved"}`))
	req.Header.Set("Authorization", "Bearer "+authToken(t, "owner-1"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner approve = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/vendors/v1/status",
		strings.NewReader(`{"status":"rejected"}`))
	req.Header.Set("Authorization", "Bearer "+authToken(t, "not-owner"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner approve = %d, want 403", rec.Code)
	}
}

func TestOwnerOnlyForbidsNonOwner(t *testing.T) {
	m := newMockSupabase(t)
	// The event is owned by someone else; a different authed caller => 403.
	m.seed("events", `[{"id":"e1","owner_id":"someone-else"}]`)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/events/e1", nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, "not-the-owner"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for non-owner", rec.Code)
	}
}

func TestOwnerOnlyNotFound(t *testing.T) {
	m := newMockSupabase(t)
	// events empty => OwnerOf returns ErrNotFound => 404.
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/events/missing", nil)
	req.Header.Set("Authorization", "Bearer "+authToken(t, "owner-1"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for missing resource", rec.Code)
	}
}

func TestOwnerOnlyRejectsAnon(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	// No token: ownerOnly wraps requireAuth => 401 before any DB lookup.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/events/e1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestCORSPreflightThroughServer exercises withCORS at the top of the real mux.
func TestCORSPreflightThroughServer(t *testing.T) {
	m := newMockSupabase(t)
	h := newTestServer(t, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/events", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
}

// Guard against a typo in corsAllowedOrigins drifting from a valid URL list.
func TestCorsAllowedOriginsParse(t *testing.T) {
	for _, o := range corsAllowedOrigins {
		if _, err := url.Parse(o); err != nil {
			t.Errorf("corsAllowedOrigins entry %q does not parse: %v", o, err)
		}
	}
}

func TestShortLinkRedirect(t *testing.T) {
	m := newMockSupabase(t)
	m.seed("short_links", `[{"code":"Ab3x9Cd","target":"https://app.planmypickle.com/?report=m1&t=tok1"}]`)
	h := newTestServer(t, m)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/Ab3x9Cd", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://app.planmypickle.com/?report=m1&t=tok1" {
		t.Fatalf("location = %q", loc)
	}
}

func TestSmsBodiesFitOneSegment(t *testing.T) {
	// GSM-7 single segment = 160 chars. Short link = 38 chars
	// (api.planmypickle.com/r/ + 7-char code + https://).
	link := "https://api.planmypickle.com/r/Ab3x9Cd"
	start := "PlanMyPickle: You're up! Court 12, round 10. Report score: " + link + " Reply STOP to opt out."
	confirm := "PlanMyPickle: opponents reported 11-9. Confirm or dispute (60m auto-confirm): " + link + " Reply STOP to opt out."
	for name, body := range map[string]string{"start": start, "confirm": confirm} {
		if len(body) > 160 {
			t.Fatalf("%s SMS is %d chars (>160 = 2 segments): %q", name, len(body), body)
		}
	}
}
