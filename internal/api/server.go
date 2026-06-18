// Package api exposes the PlanMyPickle service over a small JSON REST API.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

type Server struct {
	svc *service.Service
	// phoneCheckin throttles the public phone check-in endpoint so it can't be
	// brute-forced to enumerate registrants.
	phoneCheckin *rateLimiter
	// regLimiter throttles the public self-registration endpoint per event so a
	// bot can't flood an event with fake players.
	regLimiter *rateLimiter
	// captcha verifies a Turnstile token on PUBLIC (anonymous) self-registration.
	// Active only when TURNSTILE_SECRET is set; otherwise it skips (fail-open).
	captcha *gateway.Captcha
}

// NewServer wires the routes and returns the HTTP handler.
func NewServer(svc *service.Service) http.Handler {
	s := &Server{
		svc:          svc,
		phoneCheckin: newRateLimiter(60, 60),
		regLimiter:   newRateLimiter(40, 60), // 40 self-registrations/min per event
		captcha:      gateway.NewTurnstile(os.Getenv("TURNSTILE_SECRET")),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// --- Public: spectator/shareable reads + the on-site self-service flows
	// reached from QR codes (register, pay, shirt order, self check-in). These
	// intentionally need no account so players and spectators have zero
	// friction. optionalAuth attaches the user when a token happens to be sent.
	// GET /events is the organizer DASHBOARD list — it returns only the caller's
	// own events (optionalAuth attaches the user; anonymous callers get nothing).
	// Spectator/registration flows use GET /events/{id} (single), not this list.
	mux.HandleFunc("GET /events", optionalAuth(s.listEvents))
	// Events the signed-in user is registered to PLAY in (the "Playing" home tab).
	mux.HandleFunc("GET /me/events", requireAuth(s.myEvents))
	mux.HandleFunc("GET /events/{id}", s.getEvent)
	mux.HandleFunc("GET /events/{id}/brackets", s.getBrackets)
	mux.HandleFunc("GET /events/{id}/standings", s.standings)
	mux.HandleFunc("GET /events/{id}/rounds", s.rounds)
	mux.HandleFunc("GET /events/{id}/matches", s.eventMatches)
	mux.HandleFunc("GET /events/{id}/busy-courts", s.busyCourts)
	mux.HandleFunc("GET /events/{id}/feed", optionalAuth(s.feedList))
	mux.HandleFunc("GET /feed/{id}/comments", optionalAuth(s.commentList))
	mux.HandleFunc("GET /brackets/{id}/matches", s.bracketMatches)
	mux.HandleFunc("GET /rounds/{id}/matches", s.roundMatches)
	mux.HandleFunc("GET /courts/nearby", s.nearbyCourts)
	mux.HandleFunc("GET /geocode", s.geocode)
	mux.HandleFunc("POST /events/{id}/register", optionalAuth(s.register))
	mux.HandleFunc("POST /registrations/{id}/pay", s.pay)
	mux.HandleFunc("POST /registrations/{id}/shirt", s.saveShirt)
	mux.HandleFunc("POST /events/{id}/checkin", s.checkinByToken)
	mux.HandleFunc("POST /events/{id}/checkin-by-phone", s.checkinByPhone)
	mux.HandleFunc("POST /events/{id}/verify-admin", s.verifyAdmin)

	// --- Authenticated: creating an event stamps the caller as its owner.
	mux.HandleFunc("POST /events", requireAuth(s.createEvent))

	// --- Owner-only: management actions require a valid token AND that the
	// caller owns the event behind the resource (see service.OwnerOf).
	mux.HandleFunc("POST /events/{id}", s.ownerOnly("event", "id", s.updateEvent))
	mux.HandleFunc("DELETE /events/{id}", s.ownerOnly("event", "id", s.deleteEvent))
	mux.HandleFunc("GET /events/{id}/registrations", s.ownerOnly("event", "id", s.registrations))
	mux.HandleFunc("GET /events/{id}/finance", s.ownerOnly("event", "id", s.financeEntries))
	mux.HandleFunc("POST /events/{id}/finance", s.ownerOnly("event", "id", s.addFinanceEntry))
	mux.HandleFunc("DELETE /finance/{id}", s.ownerOnly("finance", "id", s.deleteFinanceEntry))
	mux.HandleFunc("GET /events/{id}/checklist", s.ownerOnly("event", "id", s.checklist))
	mux.HandleFunc("POST /events/{id}/checklist", s.ownerOnly("event", "id", s.addChecklistItem))
	mux.HandleFunc("POST /checklist/{id}/check", s.ownerOnly("checklist", "id", s.setChecklistChecked))
	mux.HandleFunc("DELETE /checklist/{id}", s.ownerOnly("checklist", "id", s.deleteChecklistItem))
	mux.HandleFunc("POST /events/{id}/schedule", s.ownerOnly("event", "id", s.schedule))
	mux.HandleFunc("POST /events/{id}/auto-schedule", s.ownerOnly("event", "id", s.autoSchedule))
	mux.HandleFunc("POST /events/{id}/game-duration", s.ownerOnly("event", "id", s.setGameDuration))
	mux.HandleFunc("POST /events/{id}/start-time", s.ownerOnly("event", "id", s.setStartTime))
	mux.HandleFunc("POST /events/{id}/fill-demo-players", s.ownerOnly("event", "id", s.fillRandomPlayers))
	mux.HandleFunc("POST /events/{id}/feed", s.ownerOnly("event", "id", s.feedPost))
	mux.HandleFunc("DELETE /feed/{id}", s.ownerOnly("feed_item", "id", s.feedDelete))
	// Feed social — any signed-in user may react/comment (not just the owner).
	mux.HandleFunc("POST /feed/{id}/react", requireAuth(s.feedReact))
	mux.HandleFunc("POST /feed/{id}/comments", requireAuth(s.commentAdd))
	mux.HandleFunc("DELETE /comments/{id}", requireAuth(s.commentDelete))
	mux.HandleFunc("POST /events/{id}/dupr/import", s.ownerOnly("event", "id", s.duprImport))
	mux.HandleFunc("POST /matches/{id}/score", s.ownerOnly("match", "id", s.recordScore))
	mux.HandleFunc("POST /matches/{id}/forfeit", s.ownerOnly("match", "id", s.forfeitMatch))
	mux.HandleFunc("POST /matches/{id}/start", s.ownerOnly("match", "id", s.startMatch))
	mux.HandleFunc("POST /matches/{id}/unstart", s.ownerOnly("match", "id", s.unstartMatch))
	mux.HandleFunc("POST /matches/{id}/swap", s.ownerOnly("match", "id", s.swapMatchPlayer))
	mux.HandleFunc("POST /matches/{id}/court", s.ownerOnly("match", "id", s.setMatchCourt))
	mux.HandleFunc("POST /matches/{id}/duration", s.ownerOnly("match", "id", s.setMatchDuration))
	mux.HandleFunc("POST /brackets/{id}/playoff", s.ownerOnly("bracket", "id", s.playoff))
	mux.HandleFunc("POST /rounds/{id}/start", s.ownerOnly("round", "id", s.startRound))
	mux.HandleFunc("POST /registrations/{id}/checkin", s.ownerOnly("registration", "id", s.checkin))
	mux.HandleFunc("POST /registrations/{id}/uncheckin", s.ownerOnly("registration", "id", s.uncheckin))
	mux.HandleFunc("POST /registrations/{id}/mark-paid", s.ownerOnly("registration", "id", s.markPaid))
	mux.HandleFunc("POST /registrations/{id}/details", s.ownerOnly("registration", "id", s.updateRegistrationDetails))
	mux.HandleFunc("DELETE /registrations/{id}", s.ownerOnly("registration", "id", s.deleteRegistration))

	// --- Demo seeding: load a sample tournament owned by the signed-in user, so
	// the "Load demo" buttons produce events the caller can actually manage.
	// requireAuth keeps it from being an anonymous data-injection endpoint.
	mux.HandleFunc("POST /dev/seed", requireAuth(s.seedDemo))
	mux.HandleFunc("POST /dev/seed-playoff", requireAuth(s.seedPlayoffDemo))

	return withCORS(mux)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.svc.ListEvents(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// myEvents returns the events the signed-in user is registered to play in.
func (s *Server) myEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.svc.MyEvents(userID(r), userEmail(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	var req model.CreateEventRequest
	if !decode(w, r, &req) {
		return
	}
	id, err := s.svc.CreateEvent(req, userID(r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// updateEvent edits an existing event's metadata (owner-only). Structural
// format / brackets are intentionally not editable here.
func (s *Server) updateEvent(w http.ResponseWriter, r *http.Request) {
	var req model.CreateEventRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.UpdateEvent(r.PathValue("id"), req); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.GetEvent(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteEvent(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) seedDemo(w http.ResponseWriter, r *http.Request) {
	id, err := s.svc.SeedDemo(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"eventId": id})
}

func (s *Server) seedPlayoffDemo(w http.ResponseWriter, r *http.Request) {
	id, err := s.svc.SeedPlayoffDemo(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"eventId": id})
}

func (s *Server) getBrackets(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.GetBrackets(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req model.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	// Per-event throttle so the public self-registration link can't be flooded
	// with fake players. Generous enough for a real registration rush.
	if !s.regLimiter.allow(r.PathValue("id")) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many registrations right now, try again shortly"))
		return
	}
	// Bot check on the PUBLIC form only: anonymous (no token) self-registration
	// must pass Turnstile. An authenticated organizer adding a player is trusted
	// and skips it. No-op unless TURNSTILE_SECRET is configured.
	if userID(r) == "" && !s.captcha.Verify(req.CaptchaToken, "") {
		writeErr(w, http.StatusForbidden, errors.New("please complete the human check and try again"))
		return
	}
	// Link the player to the caller's account ONLY when a logged-in user is
	// registering themselves (req.Self). An organizer adding others does not.
	linkUserID := ""
	if req.Self {
		linkUserID = userID(r)
	}
	reg, err := s.svc.RegisterPlayer(r.PathValue("id"), req, linkUserID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if name := strings.TrimSpace(req.FullName); name != "" {
		s.svc.AddFeedItem(r.PathValue("id"), "registered", name+" registered", reg.ID)
	}
	writeJSON(w, http.StatusCreated, reg)
}

func (s *Server) financeEntries(w http.ResponseWriter, r *http.Request) {
	entries, err := s.svc.FinanceEntries(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) addFinanceEntry(w http.ResponseWriter, r *http.Request) {
	var req model.FinanceEntryRequest
	if !decode(w, r, &req) {
		return
	}
	entry, err := s.svc.AddFinanceEntry(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) deleteFinanceEntry(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteFinanceEntry(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) checklist(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Checklist(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) addChecklistItem(w http.ResponseWriter, r *http.Request) {
	var req model.ChecklistItemRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.svc.AddChecklistItem(r.PathValue("id"), req.Label)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) setChecklistChecked(w http.ResponseWriter, r *http.Request) {
	var req model.ChecklistItemRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetChecklistChecked(r.PathValue("id"), req.Checked); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) deleteChecklistItem(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteChecklistItem(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) registrations(w http.ResponseWriter, r *http.Request) {
	regs, err := s.svc.Registrations(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, regs)
}

// busyCourts returns the court numbers that currently have an in-progress
// match, so the schedule UI can gray out other scheduled matches on those
// courts.
func (s *Server) busyCourts(w http.ResponseWriter, r *http.Request) {
	nums, err := s.svc.BusyCourts(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, nums)
}

func (s *Server) eventMatches(w http.ResponseWriter, r *http.Request) {
	matches, err := s.svc.EventPoolMatches(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, matches)
}

func (s *Server) schedule(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"
	n, err := s.svc.GenerateSchedule(r.PathValue("id"), force)
	if errors.Is(err, service.ErrScheduleHasResults) {
		// 409 — the app should confirm with the user, then retry with ?force=true.
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		status(w, err)
		return
	}
	if n > 0 {
		s.svc.AddFeedItem(r.PathValue("id"), "schedule_posted",
			fmt.Sprintf("Schedule posted — %d matches", n), "")
	}
	writeJSON(w, http.StatusOK, map[string]int{"matches": n})
}

// autoSchedule lays the pool games onto courts + time-slots ordered by division
// rating band (lowest first). Owner-only; powers "Build schedule by rating".
func (s *Server) autoSchedule(w http.ResponseWriter, r *http.Request) {
	interleave := r.URL.Query().Get("interleave") == "true"
	n, err := s.svc.AutoScheduleByRating(r.PathValue("id"), interleave)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"scheduled": n})
}

// setGameDuration updates the per-game slot length (minutes). Owner-only.
func (s *Server) setGameDuration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	m, err := s.svc.SetGameDuration(r.PathValue("id"), req.Minutes)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"minutes": m})
}

// setStartTime sets (or clears) the tournament start (RFC3339 UTC). Owner-only.
func (s *Server) setStartTime(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartsAt string `json:"startsAt"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetStartTime(r.PathValue("id"), req.StartsAt); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "set"})
}

func (s *Server) standings(w http.ResponseWriter, r *http.Request) {
	byWins := r.URL.Query().Get("by") != "points"
	bracketID := r.URL.Query().Get("bracketId")
	st, err := s.svc.Standings(r.PathValue("id"), bracketID, byWins)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) recordScore(w http.ResponseWriter, r *http.Request) {
	var req model.ScoreRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.RecordScore(r.PathValue("id"), req.Team1Score, req.Team2Score); err != nil {
		status(w, err)
		return
	}
	if eid, txt := s.svc.MatchFeedText(r.PathValue("id"), true); txt != "" {
		s.svc.AddFeedItem(eid, "match_final", txt, r.PathValue("id"))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

func (s *Server) forfeitMatch(w http.ResponseWriter, r *http.Request) {
	var req model.ForfeitRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.ForfeitMatch(r.PathValue("id"), req.WinningTeam, req.Kind, req.Team1Score, req.Team2Score); err != nil {
		status(w, err)
		return
	}
	if eid, txt := s.svc.MatchFeedText(r.PathValue("id"), true); txt != "" {
		s.svc.AddFeedItem(eid, "match_final", txt, r.PathValue("id"))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

func (s *Server) playoff(w http.ResponseWriter, r *http.Request) {
	topN, _ := strconv.Atoi(r.URL.Query().Get("topN"))
	n, err := s.svc.GeneratePlayoffBracket(r.PathValue("id"), topN)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"matches": n})
}

func (s *Server) bracketMatches(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.BracketMatches(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) geocode(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("q query param is required"))
		return
	}
	res, err := s.svc.Geocode(q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if res == nil {
		writeErr(w, http.StatusNotFound, errors.New("location not found"))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) nearbyCourts(w http.ResponseWriter, r *http.Request) {
	lat, err1 := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lng, err2 := strconv.ParseFloat(r.URL.Query().Get("lng"), 64)
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, errors.New("lat and lng query params are required"))
		return
	}
	radius, _ := strconv.ParseFloat(r.URL.Query().Get("radiusKm"), 64)
	found, err := s.svc.NearbyCourts(lat, lng, radius)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (s *Server) rounds(w http.ResponseWriter, r *http.Request) {
	rs, err := s.svc.Rounds(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (s *Server) roundMatches(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.MatchesForRound(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) pay(w http.ResponseWriter, r *http.Request) {
	var req model.PayRequest
	if !decode(w, r, &req) {
		return
	}
	ok, err := s.svc.CollectPayment(r.PathValue("id"), req.Provider)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paid": ok})
}

// markPaid is the organizer confirming, owner-only, that a fee-bearing
// registration was paid out of band (cash/e-transfer). The public /pay endpoint
// cannot do this for fee-bearing events (no real gateway is wired up).
func (s *Server) markPaid(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.CollectPaymentManually(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paid": true})
}

// updateRegistrationDetails edits a registered player's name/rating (owner-only).
func (s *Server) updateRegistrationDetails(w http.ResponseWriter, r *http.Request) {
	var req model.RegistrationDetailsRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.UpdateRegistrationDetails(r.PathValue("id"), req.FullName, req.DuprRating); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// deleteRegistration removes a player's registration from an event (owner-only).
func (s *Server) deleteRegistration(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteRegistration(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) checkin(w http.ResponseWriter, r *http.Request) {
	var req model.CheckinRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.CheckIn(r.PathValue("id"), req.Method); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "checked_in"})
}

func (s *Server) uncheckin(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.UncheckIn(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveShirt(w http.ResponseWriter, r *http.Request) {
	var req model.ShirtRequest
	if !decode(w, r, &req) {
		return
	}
	order, err := s.svc.SaveShirtOrder(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) checkinByToken(w http.ResponseWriter, r *http.Request) {
	var req model.TokenRequest
	if !decode(w, r, &req) {
		return
	}
	regID, err := s.svc.CheckInByToken(r.PathValue("id"), req.Token)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"registrationId": regID})
}

func (s *Server) checkinByPhone(w http.ResponseWriter, r *http.Request) {
	var req model.PhoneCheckinRequest
	if !decode(w, r, &req) {
		return
	}
	// Throttle this lookup PER EVENT (not per IP): behind Railway's proxy chain a
	// client IP isn't reliably attributable — the leftmost X-Forwarded-For hop is
	// client-spoofable and the rightmost can be a rotating internal hop — so an IP
	// key is either bypassable or explodes. A per-event cap bounds total attempts
	// regardless and, with the exact full-phone match this endpoint requires (the
	// real anti-enumeration control), is enough to stop name harvesting. QR and
	// organizer check-in are separate endpoints and unaffected.
	if !s.phoneCheckin.allow(r.PathValue("id")) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many attempts, try again shortly"))
		return
	}
	regID, name, err := s.svc.CheckInByPhone(r.PathValue("id"), req.Phone)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"registrationId": regID, "name": name})
}

func (s *Server) swapMatchPlayer(w http.ResponseWriter, r *http.Request) {
	var req model.SwapRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SwapMatchPlayer(r.PathValue("id"), req.OutPlayerID, req.InPlayerID); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "swapped"})
}

// setMatchCourt reassigns a match to a different court (courtNumber <= 0 = clear
// the assignment). Owner-only; powers the drag-to-reassign schedule board.
func (s *Server) setMatchCourt(w http.ResponseWriter, r *http.Request) {
	var req model.SetCourtRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetMatchCourt(r.PathValue("id"), req.CourtNumber, req.PlayOrder); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "court-set"})
}

// setMatchDuration overrides one game's length (minutes); 0 clears it. Owner-only.
func (s *Server) setMatchDuration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	m, err := s.svc.SetMatchDuration(r.PathValue("id"), req.Minutes)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"minutes": m})
}

func (s *Server) startRound(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.StartRound(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}

func (s *Server) startMatch(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.StartMatch(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	if eid, txt := s.svc.MatchFeedText(r.PathValue("id"), false); txt != "" {
		s.svc.AddFeedItem(eid, "match_live", txt, r.PathValue("id"))
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}

// feedList returns an event's activity feed (public — like the scoreboard).
// optionalAuth so a signed-in caller's own reactions come back flagged.
func (s *Server) feedList(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.ListFeed(r.PathValue("id"), userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// feedReact toggles the signed-in user's reaction on a feed item.
func (s *Server) feedReact(w http.ResponseWriter, r *http.Request) {
	var req model.ReactionRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.svc.ToggleReaction(r.PathValue("id"), userID(r), req.Type)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// commentList returns a feed item's comments (public read; canDelete/mine
// reflect the optional caller).
func (s *Server) commentList(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.ListComments(r.PathValue("id"), userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// commentAdd posts a comment as the signed-in user.
func (s *Server) commentAdd(w http.ResponseWriter, r *http.Request) {
	var req model.CommentRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.svc.AddComment(r.PathValue("id"), userID(r), userEmail(r), req.Text)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// commentDelete removes a comment (author or event owner).
func (s *Server) commentDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteComment(r.PathValue("id"), userID(r)); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// feedPost adds an organizer announcement to the feed (owner-only).
func (s *Server) feedPost(w http.ResponseWriter, r *http.Request) {
	var req model.FeedPostRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.svc.PostAnnouncement(r.PathValue("id"), req.Text, "Organizer")
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

// feedDelete removes a feed item (owner-only).
func (s *Server) feedDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteFeedItem(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fillRandomPlayers seeds the event with a day's worth of demo players spread
// across its divisions (owner-only). Temporary organizer convenience.
func (s *Server) fillRandomPlayers(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.FillRandomPlayers(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": n})
}

func (s *Server) unstartMatch(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.UnstartMatch(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) duprImport(w http.ResponseWriter, r *http.Request) {
	sum, err := s.svc.SubmitPendingToDupr(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) verifyAdmin(w http.ResponseWriter, r *http.Request) {
	var req model.PasscodeRequest
	if !decode(w, r, &req) {
		return
	}
	ok, err := s.svc.VerifyAdminPasscode(r.PathValue("id"), req.Code)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": ok})
}

// ---- helpers ----

var errForbidden = errors.New("you don't have access to this resource")

// ownerOnly guards a handler so only the authenticated owner of the event
// behind a resource may call it. kind/idParam identify the resource (see
// service.OwnerOf); idParam is the path wildcard holding its id. Unowned
// (legacy) resources are treated as forbidden — nobody may mutate them.
func (s *Server) ownerOnly(kind, idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.OwnerOf(kind, r.PathValue(idParam))
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if owner == "" || owner != userID(r) {
			writeErr(w, http.StatusForbidden, errForbidden)
			return
		}
		next(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// Tolerate an empty body so optional-field endpoints (pay, checkin) work
	// with no JSON; fields fall back to their service-side defaults.
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func status(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, service.ErrForbidden) {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	writeErr(w, http.StatusBadRequest, err)
}

// rateLimiter is a small in-memory sliding-window limiter (best-effort; per
// process, fine for a single backend instance). It throttles the public phone
// check-in endpoint against name-enumeration brute force.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]int64
	limit  int
	window int64 // seconds
}

func newRateLimiter(limit int, windowSec int64) *rateLimiter {
	rl := &rateLimiter{hits: map[string][]int64{}, limit: limit, window: windowSec}
	go rl.sweep()
	return rl
}

// sweep periodically drops keys whose newest attempt is older than the window,
// so the map can't grow without bound (one entry per IP+event would otherwise
// live for the whole process — and an attacker varying the key could pile them
// up). Runs for the process lifetime.
func (rl *rateLimiter) sweep() {
	for range time.Tick(time.Duration(rl.window) * time.Second) {
		cutoff := time.Now().Unix() - rl.window
		rl.mu.Lock()
		for k, ts := range rl.hits {
			newest := int64(0)
			for _, t := range ts {
				if t > newest {
					newest = t
				}
			}
			if newest <= cutoff {
				delete(rl.hits, k)
			}
		}
		rl.mu.Unlock()
	}
}

// allow records an attempt for key and reports whether it's within the limit.
func (rl *rateLimiter) allow(key string) bool {
	nowTs := time.Now().Unix()
	cutoff := nowTs - rl.window
	rl.mu.Lock()
	defer rl.mu.Unlock()
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, nowTs)
	return true
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		// DELETE is used by events/finance/checklist; without it a browser's
		// preflight blocks those calls. Origin "*" is safe — the API carries no
		// cookies/credentials (the Supabase service key is server-side only).
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		// Authorization carries the user's bearer token; without it the browser
		// preflight blocks every authenticated request.
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
