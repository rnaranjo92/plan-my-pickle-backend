package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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

// setRotationRoundMinutes changes the round length before the session starts.
func (s *Server) setRotationRoundMinutes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoundMinutes int `json:"roundMinutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationSessionRoundMinutes(
		r.PathValue("id"), req.RoundMinutes); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"roundMinutes": req.RoundMinutes})
}

// setRotationLimits sets when the night stops: after N rounds, at a wall-clock
// time, or neither. Owner-gated; allowed while live so an organizer who gets
// another half hour of court time can move it.
func (s *Server) setRotationLimits(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlannedRounds int    `json:"plannedRounds"`
		StopAt        string `json:"stopAt"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationSessionLimits(
		r.PathValue("id"), req.PlannedRounds, req.StopAt); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plannedRounds": req.PlannedRounds,
		"stopAt":        req.StopAt,
	})
}

// tvLink returns a SHORT, stable URL for a live TV board — a rotation session's
// or an event's.
//
// The long form is app.planmypickle.com/?ladder_tv=<uuid>: about sixty
// characters, most of them a UUID nobody can read off a whiteboard or type into
// a smart-TV remote. This hands back the /r/<code> short link instead (~38
// chars), reusing the redirect that already exists for SMS.
//
// Stable by design: asking twice returns the same URL, so a link written down
// last week still works.
func (s *Server) tvLink(w http.ResponseWriter, r *http.Request) {
	kind := "ladder_tv"
	id := r.PathValue("id")
	if strings.HasPrefix(r.URL.Path, "/events/") {
		kind = "scoreboard"
		// A rotation-ladder event's live play is in a rotation SESSION, and the
		// tournament board it would otherwise link to stays permanently empty —
		// the organizer put "Live scoreboard (TV)" on the venue screen and got
		// "No games in progress" all night. Point this at the session's board
		// instead, from the same control, so there is one TV button that is
		// always right rather than two the organizer has to choose between.
		//
		// Best effort: no session (or a lookup failure) falls through to the
		// event board, which is the correct destination for every other format
		// and an honest empty state for this one.
		if sid, err := s.svc.EventRotationSessionID(id); err == nil && sid != "" {
			kind, id = "ladder_tv", sid
		}
	}
	target := "https://app.planmypickle.com/?" + kind + "=" + id
	writeJSON(w, http.StatusOK, map[string]string{
		"url":  s.svc.StableShortLink(target),
		"full": target,
		// What this link actually points at. The in-app "Open" button pushes a
		// board directly rather than following the URL, so without this it would
		// keep opening the event scoreboard while the copied link, the QR and
		// the printed flyer all went somewhere else.
		"kind":      kind,
		"sessionId": map[bool]string{true: id, false: ""}[kind == "ladder_tv"],
	})
}

// publicEventRotationSession tells a caller holding an EVENT id which rotation
// session that event's TV board should show, or "" when it has none.
//
// It exists so an old ?scoreboard=<eventId> link can correct itself. Those
// links are permanent by design — the short-link code is stable per target, so
// every one already written on a whiteboard, printed on a flyer or saved as a
// TV bookmark points at the tournament board forever. Since the link can't be
// changed after the fact, the destination has to be able to redirect itself.
//
// Never an error for "no session": a normal tournament legitimately has none,
// and the caller's job is then to render the event board it was already going
// to render.
func (s *Server) publicEventRotationSession(w http.ResponseWriter, r *http.Request) {
	sid, err := s.svc.EventRotationSessionID(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"sessionId": sid})
}

// publicRotationBoard serves the courtside TV board with NO authentication — a
// television can't sign in, and the tournament scoreboard has always worked
// this way. The payload is the trimmed one (see PublicRotationBoard): names,
// courts, the clock, the bench and the running order, and nothing else.
//
// Unlisted rather than advertised: reaching it needs the session's UUID, which
// the organizer shares deliberately when they open the board.
func (s *Server) publicRotationBoard(w http.ResponseWriter, r *http.Request) {
	board, err := s.svc.PublicRotationBoard(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, board)
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
	// Through the batch, deliberately: SetRotationScores is what re-derives a
	// past round's winner and retallies wins afterwards. Calling the single
	// writer directly skipped that — and a one-cell edit in the grid is the most
	// likely way anyone ever fixes a wrong score, so it was exactly the path
	// that left the win with the loser.
	if err := s.svc.SetRotationScores(r.PathValue("id"), req.Round,
		[]service.RotationScoreEntry{{PlayerID: req.PlayerID, Score: req.Score}},
	); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// setRotationScores writes a whole court's scores in one request — see
// SetRotationScores. The per-player endpoint stays for the single-cell edit in
// the grid.
func (s *Server) setRotationScores(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Round   int                          `json:"round"`
		Entries []service.RotationScoreEntry `json:"entries"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationScores(r.PathValue("id"), req.Round, req.Entries); err != nil {
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

// reopenRotation puts a finished session back on the clock — the answer to a
// mis-tapped End (or a venue that extended the booking) that isn't "start over
// with zeroed standings". Owner-gated, like End itself.
func (s *Server) reopenRotation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ReopenRotationSession(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

// setRotationCourtWinner corrects a court's result AFTER its round is over.
//
// Owner-only and separate from /report on purpose: reporting is a participant
// action pinned to the live round, this is the organizer fixing the record of
// the night. An empty winner clears the result back to undecided.
func (s *Server) setRotationCourtWinner(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Round  int    `json:"round"`
		Court  int    `json:"court"`
		Winner string `json:"winner"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetRotationCourtWinner(
		r.PathValue("id"), req.Round, req.Court, req.Winner); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "corrected"})
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
