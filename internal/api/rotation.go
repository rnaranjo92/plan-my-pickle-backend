package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

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
		// Super users run other organizers' nights for support. Every other
		// rotation gate goes through ladderOwnerOK, which grants this; without it
		// here a supported night reached 0:00 and stalled — the board rendered
		// every control and the server refused the one that matters.
		if owner != userID(r) &&
			!(isSuperUser(userEmail(r)) && superUserAllowed(r)) &&
			!s.svc.IsRotationParticipant(sessionID, userID(r)) {
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

// setRotationCourts sets the venue court count on a session (extras beyond
// courts×4 become byes). Owner-gated.
func (s *Server) setRotationCourts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CourtCount int `json:"courtCount"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationSessionCourts(r.PathValue("id"), req.CourtCount); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"courtCount": req.CourtCount})
}

// rotationTestPush sends a sample rotation-round push to the organizer's own
// device so they can verify delivery before a real session. Owner-gated.
func (s *Server) rotationTestPush(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.SendRotationTestPush(userID(r)); err != nil {
		// NOT 502 — Railway/Cloudflare's edge intercepts gateway (502/503/504)
		// statuses and serves its own error page (no CORS), which the browser
		// reports as "Failed to fetch". 400 passes through with CORS intact.
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// setRotationAutoAdvance toggles auto-rotate vs organizer-taps-Next. Owner-gated.
func (s *Server) setRotationAutoAdvance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoAdvance bool `json:"autoAdvance"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationSessionAutoAdvance(r.PathValue("id"), req.AutoAdvance); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"autoAdvance": req.AutoAdvance})
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

// setRotationPlayerRating sets a roster player's self-rating (pre-start) so the
// organizer can rate imported ladder players before seeding. Owner-gated.
func (s *Server) setRotationPlayerRating(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SelfRating float64 `json:"selfRating"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationPlayerRating(r.PathValue("id"), req.SelfRating); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]float64{"selfRating": req.SelfRating})
}

// setRotationPlayerStartCourt places one player on a starting court by hand.
// A null/absent court un-places them (back to the rating-seeded tail).
func (s *Server) setRotationPlayerStartCourt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartCourt *int `json:"startCourt"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationPlayerStartCourt(r.PathValue("id"), req.StartCourt); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"startCourt": req.StartCourt})
}

// shuffleRotationStartCourts randomly redistributes the roster across the
// starting courts (owner-gated, pre-start only).
func (s *Server) shuffleRotationStartCourts(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.ShuffleRotationStartCourts(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"placed": n})
}

// setRotationPlayerName fixes a roster player's name (owner-gated, any time).
func (s *Server) setRotationPlayerName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string `json:"displayName"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationPlayerName(r.PathValue("id"), req.DisplayName); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"displayName": req.DisplayName})
}

// substituteRotationPlayer hands a player's seat to a new roster row, splitting
// the record at the current round (owner-gated, live only).
func (s *Server) substituteRotationPlayer(w http.ResponseWriter, r *http.Request) {
	var req model.SubstituteRotationPlayerRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := s.svc.SubstituteRotationPlayer(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
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
	sessionID := r.PathValue("id")
	owner, oerr := s.svc.OwnerOfRotationSession(sessionID)
	// A read failure is NOT "you're not the owner". Folding oerr into isOwner
	// silently demoted the real organizer on a transient blip, so their taps
	// started coming back "you can only report the result for your own court"
	// mid-night with nothing to explain it.
	if oerr != nil && !errors.Is(oerr, service.ErrNotFound) {
		status(w, fmt.Errorf("%w: couldn't check who owns this session",
			service.ErrUpstream))
		return
	}
	// Super users too. rotationSessionActor already admits them, but this
	// recomputed ownership without the grant — so the support account saw the
	// who-won buttons on every court (the client keys them on owner OR super
	// user) and every tap 400'd. On a tied court in scorecard mode that is a
	// deadlock: /report is the only tie-break control, and advance refuses a tie.
	isOwner := (owner != "" && owner == userID(r)) ||
		(isSuperUser(userEmail(r)) && superUserAllowed(r))
	if err := s.svc.ReportRotationCourt(sessionID, userID(r), isOwner, req); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reported"})
}

// setRotationScore upserts one cell of the ladder scorecard: a player's score
// for a round. A null score clears the cell. Owner-gated at the route — the
// organizer is the only one entering scores.
func (s *Server) setRotationScore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Round    int    `json:"round"`
		PlayerID string `json:"playerId"`
		Score    *int   `json:"score"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationScore(
		r.PathValue("id"), req.Round, req.PlayerID, req.Score); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// addScorecardRound appends an empty column to the ladder scorecard.
func (s *Server) addScorecardRound(w http.ResponseWriter, r *http.Request) {
	round, err := s.svc.AddScorecardRound(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"round": round})
}

// deleteScorecardRound removes an added column and its scores. Rounds the
// engine has already played are refused by the service.
func (s *Server) deleteScorecardRound(w http.ResponseWriter, r *http.Request) {
	round, err := strconv.Atoi(r.PathValue("round"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid round"))
		return
	}
	if err := s.svc.DeleteScorecardRound(r.PathValue("id"), round); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// advanceRotation closes the current round and opens the next. The optional
// `round` in the body is the round the caller believes is current; the service
// no-ops if it no longer matches (so a "Ring now" racing the auto-advance can't
// skip a round). Idempotent regardless.
func (s *Server) advanceRotation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Round int `json:"round"`
	}
	_ = decode(w, r, &req) // body is optional; round 0 = advance whatever's current
	if err := s.svc.AdvanceRotationSession(r.PathValue("id"), req.Round); err != nil {
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

// pauseRotation / resumeRotation stop and restart the round clock without
// ending the night. Owner-gated like end: pausing everyone's session is an
// organizer action, not something a player on the roster can do.
func (s *Server) pauseRotation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.PauseRotationSession(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *Server) resumeRotation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ResumeRotationSession(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

// leagueMedia returns every photo and video posted anywhere in a league,
// newest first, for the league's Media tab. Read-gated like the rest of a
// league's content — the same people who can see the feed can see its pictures.
func (s *Server) leagueMedia(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.LeagueMedia(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
