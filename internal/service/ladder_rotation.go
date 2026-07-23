package service

import (
	"encoding/json"
	"fmt"
	"os"
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
	body := map[string]any{
		"league_bracket_id": divisionID,
		"name":              name,
		"court_count":       courts,
		"round_minutes":     mins,
	}
	if req.AutoAdvance != nil && s.columnReady("rotation_sessions", "auto_advance") {
		body["auto_advance"] = *req.AutoAdvance
	}
	rows, err := s.sb.Insert("rotation_sessions", body)
	if err != nil {
		return model.RotationSession{}, err
	}
	if len(rows) == 0 {
		return model.RotationSession{}, fmt.Errorf("rotation session insert returned no row")
	}
	session := rotationSessionFromRow(rows[0])
	// Pre-fill the roster from the division's ladder so the players are already
	// there in setup (the organizer just prunes no-shows + adds walk-ups). Best
	// effort — a failure here shouldn't fail session creation.
	_, _ = s.ImportLadderEntrantsToSession(session.ID)
	return session, nil
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

	// Players sitting out the current round (the bench), resolved to roster order.
	byes := make([]model.RotationPlayer, 0)
	for _, id := range asStrSlice(srow, "bench") {
		if p, ok := byID[id]; ok {
			byes = append(byes, p)
		}
	}

	return model.RotationBoard{
		Session:   session,
		Players:   players,
		Courts:    courts,
		Standings: standings,
		Byes:      byes,
	}, nil
}

// autoAdvanceOf reads a session row's auto_advance flag, defaulting to true when
// the column is absent (pre-migration) or unset — so existing sessions keep the
// original fully-automatic behavior.
func autoAdvanceOf(r map[string]any) bool {
	if v, ok := r["auto_advance"]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return true
}

// SetRotationSessionCourts sets the venue court count on a session (a positive
// number = cap; the extras become byes). Only meaningful before the session
// starts; owner-gated at the route.
func (s *Service) SetRotationSessionCourts(sessionID string, courtCount int) error {
	if courtCount < 1 {
		courtCount = 1
	}
	_, err := s.sb.Update("rotation_sessions", "id=eq."+store.Q(sessionID),
		map[string]any{"court_count": courtCount})
	return err
}

// SetRotationSessionAutoAdvance toggles whether the app auto-rotates at the
// buzzer (true) or waits for the organizer to tap "Next round" (false).
func (s *Service) SetRotationSessionAutoAdvance(sessionID string, auto bool) error {
	if !s.columnReady("rotation_sessions", "auto_advance") {
		return fmt.Errorf("auto-advance toggle isn't available yet")
	}
	_, err := s.sb.Update("rotation_sessions", "id=eq."+store.Q(sessionID),
		map[string]any{"auto_advance": auto})
	return err
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

// joinBench appends newly-added players to the session's FIFO bench when it's
// LIVE, so a mid-session arrival rotates in on the next round (best-effort +
// no-op in setup, where players seed at Start). Atomic via the RPC's row lock.
func (s *Service) joinBench(sessionID string, playerIDs []string) {
	if len(playerIDs) == 0 {
		return
	}
	_, _ = s.sb.RPC("rotation_join_bench", map[string]any{
		"p_session": sessionID,
		"p_players": playerIDs,
	})
}

// AddRotationPlayer adds one competitor to a session's roster (a walk-up, or a
// linked ladder entrant). Works before AND during a session — a live add joins
// the bench and rotates in next round (rulebook: "anyone present may join").
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
	p := rotationPlayerFromRow(rows[0])
	s.joinBench(sessionID, []string{p.ID}) // live → rotate in next round; setup → no-op
	return p, nil
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
	var newIDs []string
	for _, e := range entrants {
		id := asStr(e, "id")
		if id == "" || have[id] {
			continue
		}
		rows, err := s.sb.Insert("rotation_players", map[string]any{
			"session_id":   sessionID,
			"entrant_id":   id,
			"display_name": asStr(e, "display_name"),
			"self_rating":  3.0,
		})
		if err != nil {
			return added, err
		}
		if len(rows) > 0 {
			newIDs = append(newIDs, asStr(rows[0], "id"))
		}
		added++
	}
	// A live "Sync from ladder" pulls late joiners onto the bench (no-op in setup).
	s.joinBench(sessionID, newIDs)
	return added, nil
}

// rosterEditable guards roster mutations to before the session starts — editing
// the roster mid-session would null court seats (on delete set null) and corrupt
// the board. Returns an error once the session is live/done.
func (s *Service) rosterEditable(playerID string) error {
	row, err := s.sb.SelectOne("rotation_players", "id=eq."+store.Q(playerID)+"&select=session_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	srow, err := s.sb.SelectOne("rotation_sessions",
		"id=eq."+store.Q(asStr(row, "session_id"))+"&select=status")
	if err != nil {
		return err
	}
	if srow != nil && asStr(srow, "status") != "setup" {
		return fmt.Errorf("the roster can't be changed once the session has started")
	}
	return nil
}

// RemoveRotationPlayer deletes a roster player (pre-start cleanup only).
func (s *Service) RemoveRotationPlayer(playerID string) error {
	if err := s.rosterEditable(playerID); err != nil {
		return err
	}
	return s.sb.Delete("rotation_players", "id=eq."+store.Q(playerID))
}

// SetRotationPlayerActive benches (active=false) or brings back a roster player
// (pre-start only). The way to trim the roster without deleting anyone.
func (s *Service) SetRotationPlayerActive(playerID string, active bool) error {
	if err := s.rosterEditable(playerID); err != nil {
		return err
	}
	_, err := s.sb.Update("rotation_players", "id=eq."+store.Q(playerID),
		map[string]any{"active": active})
	return err
}

// SetRotationPlayerRating sets a roster player's self-rating (pre-start only) —
// so the organizer can rate imported ladder players before seeding the courts.
func (s *Service) SetRotationPlayerRating(playerID string, rating float64) error {
	if err := s.rosterEditable(playerID); err != nil {
		return err
	}
	if rating < 1.0 {
		rating = 1.0
	} else if rating > 7.0 {
		rating = 7.0
	}
	_, err := s.sb.Update("rotation_players", "id=eq."+store.Q(playerID),
		map[string]any{"self_rating": rating})
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
	maxCourts := asInt(srow, "court_count") // 0/absent = no cap (auto from roster)

	// Active players, strongest self-rating first (stable by created_at).
	loadActive := func() ([]map[string]any, error) {
		return s.sb.Select("rotation_players",
			"session_id=eq."+store.Q(sessionID)+"&active=eq.true&order=self_rating.desc,created_at.asc&select=id")
	}
	rows, err := loadActive()
	if err != nil {
		return err
	}
	// Safety net: an empty roster at Start → pull the division's ladder in first
	// (covers players who joined the ladder after the session was created).
	if len(rows) == 0 {
		if _, ierr := s.ImportLadderEntrantsToSession(sessionID); ierr == nil {
			if rows, err = loadActive(); err != nil {
				return err
			}
		}
	}
	// Need at least one full court. Any remainder (or players beyond the court
	// cap) becomes the bench and rotates in — no perfect 4:1 required.
	if len(rows) < 4 {
		return fmt.Errorf("need at least 4 players to start a rotation (have %d)", len(rows))
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, asStr(r, "id"))
	}

	courts, bench := engine.SeedCourts(ids, maxCourts)
	payload := map[string]any{
		"p_session": sessionID,
		"p_courts":  rotationCourtsJSON(courts),
		"p_bench":   bench,
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
	if res.Started {
		// Push each player their opening court (round 1). Fire-and-forget.
		go s.notifyRotationRound(sessionID, courts, bench, 1)
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
// finished round and writes the next). expectedRound is the round the caller
// believes is current; if it no longer matches (someone already advanced), this
// is a no-op — so two racing advances (e.g. "Ring now" + auto-advance) can't
// skip a round. Pass 0 to advance whatever's current (unguarded).
func (s *Service) AdvanceRotationSession(sessionID string, expectedRound int) error {
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
	// Someone already advanced past the round the caller saw → no-op (idempotent).
	if expectedRound > 0 && expectedRound != round {
		return nil
	}
	mins := asInt(srow, "round_minutes")
	bench := asStrSlice(srow, "bench") // players sitting out the current round

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

	nextCourts, nextBench := engine.NextRound(cur, results, bench)
	payload := map[string]any{
		"p_session": sessionID,
		"p_round":   round,
		"p_courts":  rotationCourtsJSON(nextCourts),
		"p_bench":   nextBench,
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
	if res.Advanced {
		// Only the winning advance (not the stale/no-op racer) pushes the new
		// round, so nobody gets a duplicate. Fire-and-forget.
		go s.notifyRotationRound(sessionID, nextCourts, nextBench, round+1)
	}
	return nil
}

// EndRotationSession tallies the current round's reported courts AND marks the
// session done in ONE transaction (the end_rotation_session RPC), so it can't
// race a participant-fired auto-advance and double-count / drop the final round.
// Idempotent — a second End is a no-op (RPC returns already_done).
//
// Pre-0074 (the RPC + auto_advance column ship together), fall back to a plain
// status flip so End never hard-fails during the deploy window — the RPC path
// takes over the moment the migration is applied.
func (s *Service) EndRotationSession(sessionID string) error {
	if !s.columnReady("rotation_sessions", "auto_advance") {
		_, err := s.sb.Update("rotation_sessions",
			"id=eq."+store.Q(sessionID)+"&status=in.(live,paused)",
			map[string]any{"status": "done", "round_ends_at": nil})
		return err
	}
	body, err := s.sb.RPC("end_rotation_session", map[string]any{"p_session": sessionID})
	if err != nil {
		return err
	}
	var res struct {
		Ended  bool   `json:"ended"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	if !res.Ended && res.Reason == "not_found" {
		return ErrNotFound
	}
	return nil
}

// notifyRotationRound pushes each LINKED player their new court (and the byes
// their rest), plus a summary to the organizer, when a round starts. Fire-and-
// forget + no-op when push isn't configured; call in a goroutine so it never
// blocks start/advance. `courts`/`bench` are the round that just began (`round`).
// (Native delivery requires FCM/APNs configured in OneSignal; web delivers now.)
func (s *Service) notifyRotationRound(sessionID string, courts []engine.RotCourt, bench []string, round int) {
	if os.Getenv("ONESIGNAL_REST_API_KEY") == "" {
		return // push not configured → skip the recipient lookups entirely
	}
	rows, err := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&select=id,entrant_id")
	if err != nil {
		return
	}
	entrantOf := make(map[string]string, len(rows))
	for _, r := range rows {
		entrantOf[asStr(r, "id")] = asStr(r, "entrant_id")
	}
	// player id → the linked account's push external id (auth user id), or "".
	uidOf := func(playerID string) string {
		ent := entrantOf[playerID]
		if ent == "" {
			return ""
		}
		return s.entrantUserID(ent)
	}

	// Per court: "you're on Court N" to the four (linked) players there.
	for _, c := range courts {
		var uids []string
		for _, pid := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			if u := uidOf(pid); u != "" {
				uids = append(uids, u)
			}
		}
		if len(uids) > 0 {
			_ = s.sendPush(uids, "PlanMyPickle 🎾",
				fmt.Sprintf("Round %d — head to Court %d", round, c.Court), "")
		}
	}
	// Byes: resting this round.
	var benchUids []string
	for _, pid := range bench {
		if u := uidOf(pid); u != "" {
			benchUids = append(benchUids, u)
		}
	}
	if len(benchUids) > 0 {
		_ = s.sendPush(benchUids, "PlanMyPickle 🎾",
			fmt.Sprintf("Round %d — you're resting this round. You rotate back in next.", round), "")
	}
	// Organizer summary (the session owner).
	if owner, _ := s.OwnerOfRotationSession(sessionID); owner != "" {
		word := "courts"
		if len(courts) == 1 {
			word = "court"
		}
		msg := fmt.Sprintf("Round %d started — %d %s playing", round, len(courts), word)
		if len(bench) > 0 {
			msg += fmt.Sprintf(", %d resting", len(bench))
		}
		_ = s.sendPush([]string{owner}, "Rotation session", msg, "")
	}
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
		AutoAdvance:     autoAdvanceOf(r),
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
