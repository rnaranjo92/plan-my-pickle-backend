package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/engine"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Rotation session ("up and down the river" / king-of-the-court) — a LIVE, timed
// session run UNDER a ladder division. The MOVEMENT math is the pure, unit-tested
// engine (internal/engine/rotation.go); this file orchestrates it against the DB:
// seed round 1, report court winners, and advance (tally the finished round in
// one atomic RPC, then write the next round the engine computed). See migration
// 0071_ladder_rotation.sql for the schema + the start/advance RPCs.

// --- ownership / scoping ----------------------------------------------------

// OwnerOfRotationSession resolves a session → its division → the owning user id
// (for the owner-gated management + advance routes).
func (s *Service) OwnerOfRotationSession(sessionID string) (string, error) {
	div, err := s.DivisionOfRotationSession(sessionID)
	if err != nil {
		return "", err
	}
	return s.LadderOwner(div)
}

// DivisionOfRotationSession returns the league_bracket (division) id a session
// runs under. ErrNotFound if the session is missing.
func (s *Service) DivisionOfRotationSession(sessionID string) (string, error) {
	row, err := s.sb.SelectOne("rotation_sessions",
		"id=eq."+store.Q(sessionID)+"&select=league_bracket_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return asStr(row, "league_bracket_id"), nil
}

// IsRotationParticipant reports whether the authenticated caller is a LINKED
// player in the session (their account's entrant appears in the roster). Used to
// let a participant report their court + trigger the auto-advance.
func (s *Service) IsRotationParticipant(sessionID, userID string) bool {
	if userID == "" {
		return false
	}
	div, err := s.DivisionOfRotationSession(sessionID)
	if err != nil {
		return false
	}
	entrant := s.callerEntrantID(userID, div)
	if entrant == "" {
		return false
	}
	row, err := s.sb.SelectOne("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&entrant_id=eq."+store.Q(entrant)+"&select=id&limit=1")
	return err == nil && row != nil
}

// --- session CRUD -----------------------------------------------------------

// CreateRotationSession opens a new session under a ladder division.
func (s *Service) CreateRotationSession(divisionID string, req model.CreateRotationSessionRequest) (model.RotationSession, error) {
	name := req.Name
	if name == "" {
		name = "Session"
	}
	courts := req.CourtCount
	if courts < 1 {
		courts = 1
	}
	mins := req.RoundMinutes
	if mins < 1 {
		mins = 12
	}
	rows, err := s.sb.Insert("rotation_sessions", map[string]any{
		"league_bracket_id": divisionID,
		"name":              name,
		"court_count":       courts,
		"round_minutes":     mins,
	})
	if err != nil {
		return model.RotationSession{}, err
	}
	if len(rows) == 0 {
		return model.RotationSession{}, fmt.Errorf("rotation session insert returned no row")
	}
	return rotationSessionFromRow(rows[0]), nil
}

// ListRotationSessions returns a division's sessions, newest first.
func (s *Service) ListRotationSessions(divisionID string) ([]model.RotationSession, error) {
	rows, err := s.sb.Select("rotation_sessions",
		"league_bracket_id=eq."+store.Q(divisionID)+"&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.RotationSession, 0, len(rows))
	for _, r := range rows {
		out = append(out, rotationSessionFromRow(r))
	}
	return out, nil
}

// DeleteRotationSession removes a session and (via ON DELETE CASCADE) its roster
// + round-court rows. Owner-gated at the route.
func (s *Service) DeleteRotationSession(sessionID string) error {
	return s.sb.Delete("rotation_sessions", "id=eq."+store.Q(sessionID))
}

// GetRotationBoard returns the full live view: session + roster + current round's
// courts (with player display names resolved) + standings (by wins).
func (s *Service) GetRotationBoard(sessionID string) (model.RotationBoard, error) {
	srow, err := s.sb.SelectOne("rotation_sessions", "id=eq."+store.Q(sessionID))
	if err != nil {
		return model.RotationBoard{}, err
	}
	if srow == nil {
		return model.RotationBoard{}, ErrNotFound
	}
	session := rotationSessionFromRow(srow)

	players, byID, err := s.rotationPlayers(sessionID)
	if err != nil {
		return model.RotationBoard{}, err
	}

	courts, err := s.rotationCourtsForRound(sessionID, session.CurrentRound, byID)
	if err != nil {
		return model.RotationBoard{}, err
	}

	standings := append([]model.RotationPlayer(nil), players...)
	sort.SliceStable(standings, func(i, j int) bool {
		if standings[i].Wins != standings[j].Wins {
			return standings[i].Wins > standings[j].Wins
		}
		return standings[i].Games < standings[j].Games // fewer games = better win rate at equal wins
	})

	return model.RotationBoard{
		Session:   session,
		Players:   players,
		Courts:    courts,
		Standings: standings,
	}, nil
}

// rotationPlayers loads a session's roster and returns both the slice (roster
// order: rating desc) and an id→player map (for resolving court seat names).
func (s *Service) rotationPlayers(sessionID string) ([]model.RotationPlayer, map[string]model.RotationPlayer, error) {
	rows, err := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&order=self_rating.desc,created_at.asc")
	if err != nil {
		return nil, nil, err
	}
	players := make([]model.RotationPlayer, 0, len(rows))
	byID := make(map[string]model.RotationPlayer, len(rows))
	for _, r := range rows {
		p := rotationPlayerFromRow(r)
		players = append(players, p)
		byID[p.ID] = p
	}
	return players, byID, nil
}

// rotationCourtsForRound loads the court layout for one round, resolving each
// seat's display name from the roster map. Returns an empty slice for round 0.
func (s *Service) rotationCourtsForRound(sessionID string, round int, byID map[string]model.RotationPlayer) ([]model.RotationCourt, error) {
	if round < 1 {
		return []model.RotationCourt{}, nil
	}
	rows, err := s.sb.Select("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(round)+"&order=court.asc")
	if err != nil {
		return nil, err
	}
	seat := func(id string) model.RotationCourtSeat {
		if id == "" {
			return model.RotationCourtSeat{}
		}
		return model.RotationCourtSeat{PlayerID: id, DisplayName: byID[id].DisplayName}
	}
	pair := func(a, b string) []model.RotationCourtSeat {
		out := []model.RotationCourtSeat{}
		if a != "" {
			out = append(out, seat(a))
		}
		if b != "" {
			out = append(out, seat(b))
		}
		return out
	}
	out := make([]model.RotationCourt, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.RotationCourt{
			Court:  asInt(r, "court"),
			Round:  asInt(r, "round"),
			TeamA:  pair(asStr(r, "team_a_p1"), asStr(r, "team_a_p2")),
			TeamB:  pair(asStr(r, "team_b_p1"), asStr(r, "team_b_p2")),
			Winner: asStr(r, "winner"),
		})
	}
	return out, nil
}

// --- roster -----------------------------------------------------------------

// AddRotationPlayer adds one competitor to a session's roster (a walk-up, or a
// linked ladder entrant). Only allowed before the session goes live.
func (s *Service) AddRotationPlayer(sessionID string, req model.AddRotationPlayerRequest) (model.RotationPlayer, error) {
	rating := req.SelfRating
	if rating < 1.0 || rating > 7.0 {
		rating = 3.0
	}
	body := map[string]any{
		"session_id":   sessionID,
		"display_name": req.DisplayName,
		"self_rating":  rating,
	}
	if req.EntrantID != nil && *req.EntrantID != "" {
		body["entrant_id"] = *req.EntrantID
	}
	rows, err := s.sb.Insert("rotation_players", body)
	if err != nil {
		return model.RotationPlayer{}, err
	}
	if len(rows) == 0 {
		return model.RotationPlayer{}, fmt.Errorf("rotation player insert returned no row")
	}
	return rotationPlayerFromRow(rows[0]), nil
}

// ImportLadderEntrantsToSession snapshots every entrant on the division's ladder
// into the session roster (idempotent per entrant via the unique index), seeding
// each at self_rating 3.0 by default. Returns the number newly added.
func (s *Service) ImportLadderEntrantsToSession(sessionID string) (int, error) {
	div, err := s.DivisionOfRotationSession(sessionID)
	if err != nil {
		return 0, err
	}
	entrants, err := s.sb.Select("ladder_entrants",
		"league_bracket_id=eq."+store.Q(div)+"&select=id,display_name&order=position.asc")
	if err != nil {
		return 0, err
	}
	// Which entrants are already in the session?
	existing, err := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&select=entrant_id")
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, r := range existing {
		if id := asStr(r, "entrant_id"); id != "" {
			have[id] = true
		}
	}
	added := 0
	for _, e := range entrants {
		id := asStr(e, "id")
		if id == "" || have[id] {
			continue
		}
		if _, err := s.sb.Insert("rotation_players", map[string]any{
			"session_id":   sessionID,
			"entrant_id":   id,
			"display_name": asStr(e, "display_name"),
			"self_rating":  3.0,
		}); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

// RemoveRotationPlayer deletes a roster player (pre-start cleanup).
func (s *Service) RemoveRotationPlayer(playerID string) error {
	return s.sb.Delete("rotation_players", "id=eq."+store.Q(playerID))
}

// SetRotationPlayerActive benches (active=false) or brings back a roster player.
// Benched players aren't seeded — the way to hit a perfect 4:1 without deleting
// anyone (e.g. sit 2 out of 22 to seat 20 → 5 courts).
func (s *Service) SetRotationPlayerActive(playerID string, active bool) error {
	_, err := s.sb.Update("rotation_players", "id=eq."+store.Q(playerID),
		map[string]any{"active": active})
	return err
}

// OwnerOfRotationPlayer resolves a roster player → session → division → owner.
func (s *Service) OwnerOfRotationPlayer(playerID string) (string, error) {
	row, err := s.sb.SelectOne("rotation_players", "id=eq."+store.Q(playerID)+"&select=session_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return s.OwnerOfRotationSession(asStr(row, "session_id"))
}

// --- lifecycle: start / report / advance ------------------------------------

// StartRotationSession seeds round 1: order the active roster by self-rating,
// SeedCourts them via the engine, and call the atomic start RPC (which flips the
// session live + stamps the round timer). Idempotent — a second call is a no-op.
func (s *Service) StartRotationSession(sessionID string) error {
	srow, err := s.sb.SelectOne("rotation_sessions", "id=eq."+store.Q(sessionID))
	if err != nil {
		return err
	}
	if srow == nil {
		return ErrNotFound
	}
	if asStr(srow, "status") != "setup" {
		return fmt.Errorf("session already started")
	}
	mins := asInt(srow, "round_minutes")

	// Active players, strongest self-rating first (stable by created_at).
	rows, err := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&active=eq.true&order=self_rating.desc,created_at.asc&select=id")
	if err != nil {
		return err
	}
	// Up-and-down-the-river needs a perfect 4:1 player:court ratio, so the active
	// roster must be a positive multiple of 4 (extras "sit out" — active=false).
	if len(rows) < 4 || len(rows)%4 != 0 {
		short := (4 - len(rows)%4) % 4
		return fmt.Errorf(
			"rotation needs a multiple of 4 players (4 per court) — you have %d active; sit %d out or add %d",
			len(rows), len(rows)%4, short)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, asStr(r, "id"))
	}

	// maxCourts 0 = no cap (bench unused) — the court-cap/bye wiring lands with 0073.
	courts, _ := engine.SeedCourts(ids, 0)
	payload := map[string]any{
		"p_session": sessionID,
		"p_courts":  rotationCourtsJSON(courts),
		"p_ends_at": roundEndsAt(mins),
	}
	body, err := s.sb.RPC("start_rotation_session", payload)
	if err != nil {
		return err
	}
	var res struct {
		Started bool   `json:"started"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	if !res.Started && res.Reason != "already_started" {
		return fmt.Errorf("could not start session: %s", res.Reason)
	}
	return nil
}

// ReportRotationCourt records which team won a court in the CURRENT round. Guards
// that the reported round is the live one (a stale report for a past round is
// rejected). Winner must be "a" or "b".
func (s *Service) ReportRotationCourt(sessionID string, req model.ReportRotationCourtRequest) error {
	if req.Winner != "a" && req.Winner != "b" {
		return fmt.Errorf("winner must be 'a' or 'b'")
	}
	srow, err := s.sb.SelectOne("rotation_sessions", "id=eq."+store.Q(sessionID)+"&select=current_round,status")
	if err != nil {
		return err
	}
	if srow == nil {
		return ErrNotFound
	}
	if asInt(srow, "current_round") != req.Round {
		return fmt.Errorf("round %d is no longer live", req.Round)
	}
	_, err = s.sb.Update("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(req.Round)+"&court=eq."+fmt.Sprint(req.Court),
		map[string]any{"winner": req.Winner, "reported_at": nowRFC3339()})
	return err
}

// AdvanceRotationSession closes the current round and opens the next. It reads
// the finished round's courts + winners, asks the engine for the next round's
// layout, and calls the atomic advance RPC (which tallies wins/games for the
// finished round and writes the next). Concurrency-safe: the RPC no-ops if the
// round was already advanced (so any client's auto-advance is idempotent).
func (s *Service) AdvanceRotationSession(sessionID string) error {
	srow, err := s.sb.SelectOne("rotation_sessions", "id=eq."+store.Q(sessionID))
	if err != nil {
		return err
	}
	if srow == nil {
		return ErrNotFound
	}
	status := asStr(srow, "status")
	if status != "live" && status != "paused" {
		return fmt.Errorf("session is not live")
	}
	round := asInt(srow, "current_round")
	mins := asInt(srow, "round_minutes")

	rows, err := s.sb.Select("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(round)+"&order=court.asc")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no courts for round %d", round)
	}
	cur := make([]engine.RotCourt, 0, len(rows))
	results := make([]engine.RotResult, 0, len(rows))
	for _, r := range rows {
		court := asInt(r, "court")
		cur = append(cur, engine.RotCourt{
			Court: court,
			TeamA: [2]string{asStr(r, "team_a_p1"), asStr(r, "team_a_p2")},
			TeamB: [2]string{asStr(r, "team_b_p1"), asStr(r, "team_b_p2")},
		})
		w := asStr(r, "winner")
		if w == "" {
			w = "a" // unreported court defaults to team A (matches the RPC tally)
		}
		results = append(results, engine.RotResult{Court: court, Winner: w})
	}

	nextCourts, _ := engine.NextRound(cur, results, nil)
	payload := map[string]any{
		"p_session": sessionID,
		"p_round":   round,
		"p_courts":  rotationCourtsJSON(nextCourts),
		"p_ends_at": roundEndsAt(mins),
	}
	body, err := s.sb.RPC("advance_rotation_session", payload)
	if err != nil {
		return err
	}
	var res struct {
		Advanced bool   `json:"advanced"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	if !res.Advanced && res.Reason != "stale" {
		return fmt.Errorf("could not advance session: %s", res.Reason)
	}
	return nil
}

// EndRotationSession tallies the CURRENT round's reported courts (which a normal
// advance never got to, since the organizer ends instead of ringing again) and
// marks the session done. Only reported courts count — a round abandoned mid-play
// doesn't award phantom wins.
func (s *Service) EndRotationSession(sessionID string) error {
	srow, err := s.sb.SelectOne("rotation_sessions",
		"id=eq."+store.Q(sessionID)+"&select=current_round,status")
	if err != nil {
		return err
	}
	if srow == nil {
		return ErrNotFound
	}
	// Tally the final round's reported courts (idempotency: only when still live,
	// so a double-tap End can't double-count).
	if status := asStr(srow, "status"); status == "live" || status == "paused" {
		round := asInt(srow, "current_round")
		if round >= 1 {
			if err := s.tallyRotationRound(sessionID, round); err != nil {
				return err
			}
		}
	}
	_, err = s.sb.Update("rotation_sessions", "id=eq."+store.Q(sessionID),
		map[string]any{"status": "done", "round_ends_at": nil})
	return err
}

// tallyRotationRound credits wins/games for one round's REPORTED courts (used by
// End; advance does its own tally atomically in the RPC). Unreported courts are
// skipped. Non-atomic, but End is a single-owner terminal action.
func (s *Service) tallyRotationRound(sessionID string, round int) error {
	rows, err := s.sb.Select("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(round))
	if err != nil {
		return err
	}
	inc := func(ids []string, wins bool) {
		for _, id := range ids {
			if id == "" {
				continue
			}
			cur, _ := s.sb.SelectOne("rotation_players",
				"id=eq."+store.Q(id)+"&select=wins,games")
			body := map[string]any{"games": asInt(cur, "games") + 1}
			if wins {
				body["wins"] = asInt(cur, "wins") + 1
			}
			s.sb.Update("rotation_players", "id=eq."+store.Q(id), body)
		}
	}
	for _, r := range rows {
		w := asStr(r, "winner")
		if w != "a" && w != "b" {
			continue // unreported → don't award anything
		}
		a := []string{asStr(r, "team_a_p1"), asStr(r, "team_a_p2")}
		b := []string{asStr(r, "team_b_p1"), asStr(r, "team_b_p2")}
		if w == "a" {
			inc(a, true)
			inc(b, false)
		} else {
			inc(b, true)
			inc(a, false)
		}
	}
	return nil
}

// --- mapping helpers --------------------------------------------------------

func rotationSessionFromRow(r map[string]any) model.RotationSession {
	return model.RotationSession{
		ID:              asStr(r, "id"),
		LeagueBracketID: asStr(r, "league_bracket_id"),
		Name:            asStr(r, "name"),
		Status:          asStr(r, "status"),
		CourtCount:      asInt(r, "court_count"),
		RoundMinutes:    asInt(r, "round_minutes"),
		CurrentRound:    asInt(r, "current_round"),
		RoundStartedAt:  asStr(r, "round_started_at"),
		RoundEndsAt:     asStr(r, "round_ends_at"),
		CreatedAt:       asStr(r, "created_at"),
	}
}

func rotationPlayerFromRow(r map[string]any) model.RotationPlayer {
	return model.RotationPlayer{
		ID:          asStr(r, "id"),
		SessionID:   asStr(r, "session_id"),
		EntrantID:   asStrPtr(r, "entrant_id"),
		DisplayName: asStr(r, "display_name"),
		SelfRating:  asFloatOr(r, "self_rating", 3.0),
		Wins:        asInt(r, "wins"),
		Games:       asInt(r, "games"),
		Active:      asBool(r, "active"),
	}
}

// rotationCourtsJSON converts the engine's court layout into the jsonb shape the
// start/advance RPCs consume: [{court, a:[p1,p2], b:[p1,p2]}, ...].
func rotationCourtsJSON(courts []engine.RotCourt) []map[string]any {
	out := make([]map[string]any, 0, len(courts))
	for _, c := range courts {
		out = append(out, map[string]any{
			"court": c.Court,
			"a":     []string{c.TeamA[0], c.TeamA[1]},
			"b":     []string{c.TeamB[0], c.TeamB[1]},
		})
	}
	return out
}

// roundEndsAt returns the RFC3339 buzzer time `mins` minutes from now (UTC).
func roundEndsAt(mins int) string {
	return time.Now().Add(time.Duration(mins) * time.Minute).UTC().Format(time.RFC3339)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
