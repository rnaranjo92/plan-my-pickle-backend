// Package api exposes the PlanMyPickle service over a small JSON REST API.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

type Server struct{ svc *service.Service }

// NewServer wires the routes and returns the HTTP handler.
func NewServer(svc *service.Service) http.Handler {
	s := &Server{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /events", s.listEvents)
	mux.HandleFunc("POST /events", s.createEvent)
	mux.HandleFunc("GET /events/{id}", s.getEvent)
	mux.HandleFunc("DELETE /events/{id}", s.deleteEvent)
	mux.HandleFunc("POST /dev/seed", s.seedDemo)
	mux.HandleFunc("POST /dev/seed-playoff", s.seedPlayoffDemo)
	mux.HandleFunc("GET /events/{id}/brackets", s.getBrackets)
	mux.HandleFunc("POST /events/{id}/register", s.register)
	mux.HandleFunc("GET /events/{id}/registrations", s.registrations)
	mux.HandleFunc("GET /events/{id}/finance", s.financeEntries)
	mux.HandleFunc("POST /events/{id}/finance", s.addFinanceEntry)
	mux.HandleFunc("DELETE /finance/{id}", s.deleteFinanceEntry)
	mux.HandleFunc("GET /events/{id}/checklist", s.checklist)
	mux.HandleFunc("POST /events/{id}/checklist", s.addChecklistItem)
	mux.HandleFunc("POST /checklist/{id}/check", s.setChecklistChecked)
	mux.HandleFunc("DELETE /checklist/{id}", s.deleteChecklistItem)
	mux.HandleFunc("POST /events/{id}/schedule", s.schedule)
	mux.HandleFunc("GET /events/{id}/standings", s.standings)
	mux.HandleFunc("POST /matches/{id}/score", s.recordScore)
	mux.HandleFunc("POST /matches/{id}/start", s.startMatch)
	mux.HandleFunc("POST /matches/{id}/swap", s.swapMatchPlayer)
	mux.HandleFunc("POST /brackets/{id}/playoff", s.playoff)
	mux.HandleFunc("GET /brackets/{id}/matches", s.bracketMatches)
	mux.HandleFunc("GET /events/{id}/rounds", s.rounds)
	mux.HandleFunc("GET /events/{id}/busy-courts", s.busyCourts)
	mux.HandleFunc("GET /events/{id}/matches", s.eventMatches)
	mux.HandleFunc("GET /rounds/{id}/matches", s.roundMatches)
	mux.HandleFunc("GET /courts/nearby", s.nearbyCourts)
	mux.HandleFunc("GET /geocode", s.geocode)
	// payments / check-in / SMS / DUPR (server-side integrations)
	mux.HandleFunc("POST /registrations/{id}/pay", s.pay)
	mux.HandleFunc("POST /registrations/{id}/checkin", s.checkin)
	mux.HandleFunc("POST /registrations/{id}/shirt", s.saveShirt)
	mux.HandleFunc("POST /events/{id}/checkin", s.checkinByToken)
	mux.HandleFunc("POST /events/{id}/checkin-by-phone", s.checkinByPhone)
	mux.HandleFunc("POST /rounds/{id}/start", s.startRound)
	mux.HandleFunc("POST /events/{id}/dupr/import", s.duprImport)
	mux.HandleFunc("POST /events/{id}/verify-admin", s.verifyAdmin)
	return withCORS(mux)
}

func (s *Server) listEvents(w http.ResponseWriter, _ *http.Request) {
	events, err := s.svc.ListEvents()
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
	id, err := s.svc.CreateEvent(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
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

func (s *Server) seedDemo(w http.ResponseWriter, _ *http.Request) {
	id, err := s.svc.SeedDemo()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"eventId": id})
}

func (s *Server) seedPlayoffDemo(w http.ResponseWriter, _ *http.Request) {
	id, err := s.svc.SeedPlayoffDemo()
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
	reg, err := s.svc.RegisterPlayer(r.PathValue("id"), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
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
	n, err := s.svc.GenerateSchedule(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"matches": n})
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
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
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
	writeErr(w, http.StatusBadRequest, err)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
