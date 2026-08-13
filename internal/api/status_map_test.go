package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

// The move-division screen branches on the STATUS, not the message, and the two
// refusals it can get need opposite remedies:
//
//	409 ErrDrawExists       -> "the draw is built; clear it and move anyway"
//	422 ErrAlreadyInDivision -> "they're in both; remove one entry"
//
// Collapsing these onto one code sends an organizer down the destructive path
// (clearing a bracket) to fix a duplicate registration, which would not have
// helped and would have thrown away the draw. So the mapping is the contract.
func TestStatus_RefusalsAreDistinguishable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"already in division", service.ErrAlreadyInDivision, http.StatusUnprocessableEntity},
		{"draw exists", service.ErrDrawExists, http.StatusConflict},
		{"draw has scores", service.ErrDrawHasScores, http.StatusConflict},
		{"not found", service.ErrNotFound, http.StatusNotFound},
		{"forbidden", service.ErrForbidden, http.StatusForbidden},
		// Anything unclassified stays 400 — the historical default.
		{"unclassified", errors.New("something else"), http.StatusBadRequest},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		status(w, c.err)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// The mapping has to survive wrapping — a caller that adds context with %w must
// still land on the same status.
func TestStatus_SurvivesWrapping(t *testing.T) {
	w := httptest.NewRecorder()
	status(w, fmt.Errorf("moving Leslie: %w", service.ErrAlreadyInDivision))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrapped ErrAlreadyInDivision mapped to %d, want 422", w.Code)
	}
}
