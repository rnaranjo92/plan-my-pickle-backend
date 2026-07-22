package api

import (
	"errors"
	"net/http"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

// Rotation session ("up and down the river" / king-of-the-court) HTTP layer. A
// session runs UNDER a ladder division: management (create/start/advance/roster)
// is owner-gated; the live board is readable by any division participant; and
// reporting a court / triggering the auto-advance is allowed for a linked
// participant OR the owner (so the "app is the cowbell" auto-advance can fire
// from any player's device, guarded idempotently by the advance RPC).

// --- middleware -------------------------------------------------------------

// rotationSessionOwner gates a handler keyed on a session path id: valid token +
// the caller owns the division behind the session.
func (s *Server) rotationSessionOwner(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.OwnerOfRotationSession(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// rotationPlayerOwner gates a handler keyed on a roster-player path id (resolves
// player → session → division → owner).
func (s *Server) rotationPlayerOwner(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.OwnerOfRotationPlayer(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// rotationSessionViewer gates the live board: valid token + the caller owns OR
// participates in the league behind the session.
func (s *Server) rotationSessionViewer(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		div, err := s.svc.DivisionOfRotationSession(r.PathValue(idParam))
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		leagueID, err := s.svc.LeagueIDOfDivision(div)
		if err != nil {
			status(w, err)
			return
		}
		if !s.allowLeagueRead(w, r, leagueID) {
			return
		}
		next(w, r)
	})
}

// rotationSessionActor gates the report + advance handlers: valid token + the
// caller is EITHER the division owner OR a linked participant in the session.
func (s *Server) rotationSessionActor(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue(idParam)
		owner, err := s.svc.OwnerOfRotationSession(sessionID)
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if owner != userID(r) && !s.svc.IsRotationParticipant(sessionID, userID(r)) {
			writeErr(w, http.StatusForbidden, errForbidden)
			return
		}
		next(w, r)
	})
}

// --- handlers ---------------------------------------------------------------

// listRotationSessions returns a division's sessions (owner-gated management view).
func (s *Server) listRotationSessions(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.ListRotationSessions(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createRotationSession opens a new session under a division.
func (s *Server) createRotationSession(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRotationSessionRequest
	if !decode(w, r, &req) {
		return
	}
	sess, err := s.svc.CreateRotationSession(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

// deleteRotationSession removes a session and its roster/rounds (owner-gated).
func (s *Server) deleteRotationSession(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteRotationSession(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// rotationBoard returns the full live view (session + roster + current courts +
// standings) — the screen both the organizer board and each player render from.
func (s *Server) rotationBoard(w http.ResponseWriter, r *http.Request) {
	board, err := s.svc.GetRotationBoard(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, board)
}

// addRotationPlayer adds one roster player (walk-up or linked entrant).
func (s *Server) addRotationPlayer(w http.ResponseWriter, r *http.Request) {
	var req model.AddRotationPlayerRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := s.svc.AddRotationPlayer(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// importRotationEntrants snapshots the division's ladder entrants into the roster.
func (s *Server) importRotationEntrants(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.ImportLadderEntrantsToSession(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": n})
}

// removeRotationPlayer deletes a roster player (pre-start cleanup).
func (s *Server) removeRotationPlayer(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RemoveRotationPlayer(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// setRotationPlayerActive benches / brings back a roster player (to hit a 4:1
// ratio without deleting anyone).
func (s *Server) setRotationPlayerActive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Active bool `json:"active"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationPlayerActive(r.PathValue("id"), req.Active); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"active": req.Active})
}

// startRotation seeds round 1 and flips the session live (owner-gated).
func (s *Server) startRotation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.StartRotationSession(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

// reportRotationCourt records which team won a court in the current round.
func (s *Server) reportRotationCourt(w http.ResponseWriter, r *http.Request) {
	var req model.ReportRotationCourtRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.ReportRotationCourt(r.PathValue("id"), req); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reported"})
}

// advanceRotation closes the current round and opens the next. Idempotent — the
// advance RPC no-ops if the round already moved, so any client's auto-advance is
// safe to fire when the timer expires.
func (s *Server) advanceRotation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.AdvanceRotationSession(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "advanced"})
}

// endRotation marks a session done (owner-gated).
func (s *Server) endRotation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EndRotationSession(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}
