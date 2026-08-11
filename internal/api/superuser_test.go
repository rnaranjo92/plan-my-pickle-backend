package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A support super user may EDIT anything but must never destroy a whole event,
// league, session, round or match. Routine support deletes stay allowed.
func TestSuperUserDeleteBoundary(t *testing.T) {
	blocked := []string{
		"/events/abc", "/leagues/abc", "/rotation-sessions/abc",
		"/rounds/abc", "/matches/abc", "/teams/abc", "/flex-teams/abc",
		"/events/abc/session", "/leagues/abc/schedule", "/leagues/abc/events/xyz",
	}
	for _, p := range blocked {
		r := httptest.NewRequest(http.MethodDelete, p, nil)
		if superUserAllowed(r) {
			t.Errorf("DELETE %s should be BLOCKED for a super user", p)
		}
	}
	allowed := []string{
		"/registrations/abc", "/leagues/abc/members/xyz", "/events/abc/waitlist/x",
		"/rotation-players/abc", "/ladder-entrants/abc", "/feed/abc",
		"/finance/abc", "/checklist/abc", "/events/abc/teams/xyz",
		"/rotation-sessions/abc/scorecard/rounds/3",
	}
	for _, p := range allowed {
		r := httptest.NewRequest(http.MethodDelete, p, nil)
		if !superUserAllowed(r) {
			t.Errorf("DELETE %s should be ALLOWED (routine support)", p)
		}
	}
	// Non-DELETE verbs are always fine — editing is the whole point.
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodGet} {
		r := httptest.NewRequest(m, "/events/abc", nil)
		if !superUserAllowed(r) {
			t.Errorf("%s /events/abc should be allowed", m)
		}
	}
}
