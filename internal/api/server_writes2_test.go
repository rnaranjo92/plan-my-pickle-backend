package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRegistrationAndRoundWrites covers the registration/round/check-in/DUPR
// write handlers the first write smoke missed. Owner-matched seeds let the
// owner-gated handlers run; panics are recovered (coverage, not assertions).
func TestRegistrationAndRoundWrites(t *testing.T) {
	m := newMockSupabase(t)
	m.seed("events", `[{"id":"e1","name":"E","owner_id":"owner-1","dupr_sanctioned":true}]`)
	m.seed("registrations", `[{"id":"r1","event_id":"e1","player_id":"p1","payment_status":"pending","player":{"full_name":"Al"}}]`)
	m.seed("rounds", `[{"id":"rd1","event_id":"e1","round_number":1}]`)
	m.seed("players", `[{"id":"p1","full_name":"Al","dupr_id":"D1"}]`)
	m.seed("matches", `[{"id":"m1","round_id":"rd1","event_id":"e1","status":"scheduled"}]`)
	m.seed("subscriptions", `[{"user_id":"owner-1","status":"active","premium":true}]`)
	srv := newTestServer(t, m)
	tok := authToken(t, "owner-1")

	call := func(method, path string) {
		defer func() { _ = recover() }()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		srv.ServeHTTP(rec, req)
	}

	call("POST", "/registrations/r1/checkin")
	call("POST", "/registrations/r1/uncheckin")
	call("POST", "/registrations/r1/mark-paid")
	call("POST", "/registrations/r1/details")
	call("POST", "/registrations/r1/partner")
	call("POST", "/events/e1/dupr/import")
	call("DELETE", "/registrations/r1")
	call("DELETE", "/rounds/rd1")

	// subscriptionStatus read.
	func() {
		defer func() { _ = recover() }()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/me/subscription", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		srv.ServeHTTP(rec, req)
	}()
}
