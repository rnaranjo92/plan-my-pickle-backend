package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetEndpointsSmoke drives the full server's GET routes through the real
// router + auth middleware + service, against the seeded mock Supabase. The
// caller owns the seeded resources so owner-gated reads pass. We assert no route
// 5xxes (a server error). External-network routes (geocode, city-autocomplete,
// courts, stripe) are exercised only via their no-param validation branch.
func TestGetEndpointsSmoke(t *testing.T) {
	m := newMockSupabase(t)
	m.seed("events", `[{"id":"e1","name":"E","owner_id":"owner-1","status":"live","listed":true,"format":"doubles","num_courts":2}]`)
	m.seed("leagues", `[{"id":"l1","owner_id":"owner-1","name":"L","league_type":"round_robin","day_type":"multi"}]`)
	m.seed("clubs", `[{"id":"cl1","owner_id":"owner-1","name":"C","city":"Austin"}]`)
	m.seed("brackets", `[{"id":"b1","event_id":"e1","name":"Open","division_type":"open"}]`)
	m.seed("league_brackets", `[{"id":"lb1","league_id":"l1","name":"Open","division_type":"open"}]`)
	m.seed("profiles", `[{"id":"owner-1","full_name":"Al","email":"a@b.com"}]`)
	m.seed("players", `[{"id":"p1","full_name":"Al"}]`)
	srv := newTestServer(t, m)
	tok := authToken(t, "owner-1")

	get := func(path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	// DB-backed reads (no external calls). Assert none 5xx.
	paths := []string{
		"/healthz",
		"/events", "/me/events", "/me/profile", "/me/feed",
		"/me/dupr/sso-url", "/me/dupr/connection",
		"/events/e1", "/events/e1/brackets", "/events/e1/standings",
		"/events/e1/rounds", "/events/e1/matches", "/events/e1/my-next-match",
		"/events/public", "/events/e1/busy-courts", "/events/e1/feed",
		"/events/e1/roster", "/events/e1/registrations", "/events/e1/finance",
		"/events/e1/checklist", "/events/e1/dupr-status",
		"/events/e1/results.csv", "/events/e1/roster.csv",
		"/players/p1/profile", "/feed/fi1/comments",
		"/brackets/b1/matches", "/brackets/b1/playoff-seed",
		"/rounds/rd1/matches",
		"/me/clubs", "/clubs/cl1", "/clubs/cl1/members", "/clubs/cl1/events",
		"/me/following", "/me/followers",
		"/leagues", "/my-leagues", "/leagues/l1", "/leagues/l1/standings",
		"/league-brackets/lb1/ladder", "/league-brackets/lb1/ladder/history",
		"/league-brackets/lb1/teams", "/league-brackets/lb1/teams/fixtures",
		"/league-brackets/lb1/flex/teams", "/league-brackets/lb1/flex/matchups",
		"/events/nearby?lat=30.2&lng=-97.7",
		"/users/search?q=al",
	}
	for _, p := range paths {
		if code := get(p); code >= 500 {
			t.Errorf("GET %s -> %d (server error)", p, code)
		}
	}

	// No-param validation branches (must reject BEFORE any external call).
	for _, p := range []string{"/geocode", "/city-autocomplete", "/courts/nearby"} {
		if code := get(p); code >= 500 {
			t.Errorf("GET %s -> %d (server error)", p, code)
		}
	}
}

// TestGetEndpointsAnon hits the public/optional-auth GET routes with no token,
// confirming they don't 5xx (and authed ones reject cleanly with 401).
func TestGetEndpointsAnon(t *testing.T) {
	m := newMockSupabase(t)
	m.seed("events", `[{"id":"e1","name":"E","owner_id":"o","listed":true}]`)
	srv := newTestServer(t, m)

	get := func(path string) int {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	for _, p := range []string{"/healthz", "/events", "/events/public", "/events/e1", "/events/e1/roster"} {
		if code := get(p); code >= 500 {
			t.Errorf("anon GET %s -> %d", p, code)
		}
	}
	// An authed-only route without a token must be 401, not 5xx.
	if code := get("/me/profile"); code != http.StatusUnauthorized {
		t.Errorf("anon /me/profile -> %d, want 401", code)
	}
}
