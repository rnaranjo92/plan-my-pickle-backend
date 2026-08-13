package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
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
	if mode, _ := s.rotationLoserMode(sessionID); mode == engine.LosersStay {
		session.LoserMode = "stay"
	} else {
		session.LoserMode = "down"
	}

	players, byID, err := s.rotationPlayers(sessionID)
	if err != nil {
		return model.RotationBoard{}, err
	}

	courts, err := s.rotationCourtsForRound(sessionID, session.CurrentRound, byID)
	if err != nil {
		return model.RotationBoard{}, err
	}

	// Scorecard (name × round grid): the organizer enters scores here and the
	// totals drive the standings. Inert until the migration runs.
	card, totals := s.rotationScorecard(sessionID, session.CurrentRound)
	for i := range players {
		players[i].Points = totals[players[i].ID]
	}

	standings := append([]model.RotationPlayer(nil), players...)
	sort.SliceStable(standings, func(i, j int) bool {
		a, b := standings[i], standings[j]
		// SCORES are the ladder's scoreboard here, by design: the organizer only
		// tracks points, and which court someone happened to be on is not part of
		// the standings. (Courts still drive PAIRING inside the engine — winners
		// move up, losers down — they're just not ranked or shown.)
		if card.Enabled && a.Points != b.Points {
			return a.Points > b.Points
		}
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		return a.Games < b.Games
	})

	// Players sitting out the current round (the bench), resolved to roster order.
	byes := make([]model.RotationPlayer, 0)
	for _, id := range asStrSlice(srow, "bench") {
		if p, ok := byID[id]; ok {
			byes = append(byes, p)
		}
	}

	subs, _ := s.RotationSubstitutions(sessionID)
	return model.RotationBoard{
		Session:       session,
		Players:       players,
		Courts:        courts,
		Standings:     standings,
		Byes:          byes,
		Scorecard:     card,
		Substitutions: subs,
	}, nil
}

// teamPoints sums a doubles team's entered scores for a round.
// The bool reports whether EVERY seat on the team has a score — a partially
// entered team must NOT be compared against a fully entered one, or half a
// team's points lose to the opponent's full total and the winner inverts.
func teamPoints(scoreOf map[string]int, team [2]string) (int, bool) {
	total, complete := 0, true
	for _, pid := range team {
		if pid == "" {
			continue // empty seat (odd roster) — not a missing score
		}
		v, ok := scoreOf[pid]
		if !ok {
			complete = false
			continue
		}
		total += v
	}
	return total, complete
}

// persistScorecardWinners derives each court's winner for a round from the
// scorecard and writes it to rotation_round_courts. Both the ADVANCE and the END
// paths call this: the tally RPCs only credit a win when winner is 'a'/'b', so
// without it the final round of a scorecard session recorded zero wins (points
// counted, wins didn't — and wins is the tiebreak). Returns the derived winners
// by court so the caller can reuse them. Best-effort.
func (s *Service) persistScorecardWinners(sessionID string, round int) map[int]string {
	out := map[int]string{}
	if !s.rotationScoresReady() {
		return out
	}
	srows, serr := s.sb.SelectAll("rotation_round_scores",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(round)+
			"&select=rotation_player_id,score")
	if serr != nil || len(srows) == 0 {
		return out
	}
	scoreOf := map[string]int{}
	for _, r := range srows {
		if r["score"] == nil {
			continue
		}
		scoreOf[asStr(r, "rotation_player_id")] = asInt(r, "score")
	}
	courts, cerr := s.sb.Select("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(round))
	if cerr != nil {
		return out
	}
	for _, r := range courts {
		teamA := [2]string{asStr(r, "team_a_p1"), asStr(r, "team_a_p2")}
		teamB := [2]string{asStr(r, "team_b_p1"), asStr(r, "team_b_p2")}
		aPts, aFull := teamPoints(scoreOf, teamA)
		bPts, bFull := teamPoints(scoreOf, teamB)
		if !aFull || !bFull || (aPts == 0 && bPts == 0) || aPts == bPts {
			continue // incomplete or tied → leave undecided
		}
		w := "a"
		if bPts > aPts {
			w = "b"
		}
		court := asInt(r, "court")
		out[court] = w
		_, _ = s.sb.Update("rotation_round_courts",
			"session_id=eq."+store.Q(sessionID)+
				"&round=eq."+fmt.Sprint(round)+
				"&court=eq."+fmt.Sprint(court),
			map[string]any{"winner": w, "reported_at": nowRFC3339()})
	}
	return out
}

// maxScorecardRounds bounds the scorecard's column count. The grid renders one
// column per round from 1..max, so an unbounded round would brick the board.
const maxScorecardRounds = 60

// rotationScoresReady reports whether add_rotation_round_scores.sql has run.
func (s *Service) rotationScoresReady() bool {
	return s.columnReady("rotation_round_scores", "id")
}

// rotationScorecard loads the whole session's grid plus each player's total.
// Best-effort: a read failure yields an empty (disabled) card rather than
// failing the board, so the session stays usable.
func (s *Service) rotationScorecard(sessionID string, currentRound int) (model.RotationScorecard, map[string]int) {
	totals := map[string]int{}
	card := model.RotationScorecard{
		Rounds: []int{},
		Scores: map[string]map[int]int{},
	}
	if !s.rotationScoresReady() {
		return card, totals
	}
	card.Available = true
	// SelectAll (Range-paginated): PostgREST truncates a plain Select at ~1000
	// rows, which a long session (players × rounds) can exceed — a truncated read
	// would silently under-count totals.
	rows, err := s.sb.SelectAll("rotation_round_scores",
		"session_id=eq."+store.Q(sessionID)+"&select=round,rotation_player_id,score")
	if err != nil {
		// Leave Enabled=false on a read failure: standings then fall back to wins
		// rather than presenting an all-zero leaderboard as if it were real.
		return card, totals
	}
	// PER-SESSION opt-in. Enabled must NOT mean merely "the table exists" — that
	// flipped every existing rotation session into scorecard mode the moment the
	// migration ran, stripping their who-won taps and zeroing their standings. A
	// session is in scorecard mode only once it actually has scorecard rows (the
	// organizer tapped "Add round" or entered a score).
	if len(rows) == 0 {
		return card, totals
	}
	card.Enabled = true
	// Columns run to whichever is further along: the round the ENGINE is on, or
	// the furthest round the organizer has created on the scorecard. A row with a
	// NULL score is exactly that marker — "this column exists but is blank" —
	// which is how "Add round" persists an empty column without its own table.
	maxRound := currentRound
	for _, r := range rows {
		if rd := asInt(r, "round"); rd > maxRound {
			maxRound = rd
		}
	}
	for r := 1; r <= maxRound; r++ {
		card.Rounds = append(card.Rounds, r)
	}
	for _, r := range rows {
		pid := asStr(r, "rotation_player_id")
		if pid == "" || r["score"] == nil {
			continue // blank cell
		}
		round := asInt(r, "round")
		score := asInt(r, "score")
		if card.Scores[pid] == nil {
			card.Scores[pid] = map[int]int{}
		}
		card.Scores[pid][round] = score
		totals[pid] += score
	}
	return card, totals
}

// AddScorecardRound appends an empty column to the scorecard and returns the new
// round number. Persisted as one NULL-score row per player, which the loader
// reads as "this column exists but is blank". Owner-gated at the route.
func (s *Service) AddScorecardRound(sessionID string) (int, error) {
	if !s.rotationScoresReady() {
		return 0, ErrCoachingUnavailable
	}
	srow, err := s.sb.SelectOne("rotation_sessions", "id=eq."+store.Q(sessionID))
	if err != nil {
		return 0, err
	}
	if srow == nil {
		return 0, ErrNotFound
	}
	next := asInt(srow, "current_round")
	rows, err := s.sb.SelectAll("rotation_round_scores",
		"session_id=eq."+store.Q(sessionID)+"&select=round")
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if rd := asInt(r, "round"); rd > next {
			next = rd
		}
	}
	next++
	if next > maxScorecardRounds {
		return 0, fmt.Errorf("a scorecard can hold at most %d rounds", maxScorecardRounds)
	}
	// Never upsert onto an existing round: the merge would overwrite real scores
	// with NULLs. next is max+1 so this should be impossible — it guards a race
	// between two organizers tapping "Add round" at once.
	if dup, derr := s.sb.SelectOne("rotation_round_scores",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(next)+
			"&select=id"); derr != nil {
		return 0, derr
	} else if dup != nil {
		return 0, errors.New("that round already exists — refresh and try again")
	}
	players, _, err := s.rotationPlayers(sessionID)
	if err != nil {
		return 0, err
	}
	if len(players) == 0 {
		return 0, errors.New("add players before adding a round")
	}
	batch := make([]map[string]any, 0, len(players))
	for _, p := range players {
		batch = append(batch, map[string]any{
			"session_id":         sessionID,
			"round":              next,
			"rotation_player_id": p.ID,
			"score":              nil,
			"updated_at":         nowRFC3339(),
		})
	}
	if _, err := s.sb.Upsert("rotation_round_scores",
		"session_id,round,rotation_player_id", batch); err != nil {
		return 0, err
	}
	return next, nil
}

// DeleteScorecardRound removes a column and every score in it. Refuses to touch
// a round the ENGINE is still on or has already played (<= current_round): those
// rounds have real games and standings behind them, so only an extra column the
// organizer added can be deleted. Owner-gated at the route.
func (s *Service) DeleteScorecardRound(sessionID string, round int) error {
	if !s.rotationScoresReady() {
		return ErrCoachingUnavailable
	}
	if round < 1 {
		return errors.New("invalid round")
	}
	srow, err := s.sb.SelectOne("rotation_sessions", "id=eq."+store.Q(sessionID))
	if err != nil {
		return err
	}
	if srow == nil {
		return ErrNotFound
	}
	if cur := asInt(srow, "current_round"); round <= cur {
		return fmt.Errorf(
			"round %d has been played — clear its scores instead of deleting it", round)
	}
	return s.sb.Delete("rotation_round_scores",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(round))
}

// SetRotationScore upserts ONE cell of the scorecard (a player's score for a
// round). A nil score clears the cell. Owner-gated at the route.
func (s *Service) SetRotationScore(sessionID string, round int, playerID string, score *int) error {
	// A player who was substituted out doesn't own the rounds after they left.
	// The grid still shows their row (it carries every round they DID play), so
	// typing into it is the natural mistake — and it orphans the score from the
	// court, which makes that court unresolvable and hands the round to team A.
	if subs, _ := s.rotationSubstitutionsStrict(sessionID); subs != nil {
		for _, sub := range subs {
			if sub.OutPlayerID == playerID && round >= sub.Round {
				return fmt.Errorf(
					"they had already been substituted out by round %d — enter "+
						"that score on the player who took over", sub.Round)
			}
			if sub.InPlayerID == playerID && round < sub.Round {
				return fmt.Errorf(
					"they didn't come on until round %d", sub.Round)
			}
		}
	}

	if !s.rotationScoresReady() {
		return ErrCoachingUnavailable // migration not run yet
	}
	// Bound the round HARD. The board renders a column for every round from 1 to
	// the max stored, so an unbounded value (round=100000) would build a column
	// list that size on every board read — bricking the session and chewing
	// memory. No real ladder night runs anywhere near this many rounds.
	if round < 1 || round > maxScorecardRounds || strings.TrimSpace(playerID) == "" {
		return errors.New("round and player are required")
	}
	// The player must belong to THIS session — otherwise an organizer could write
	// a score onto someone else's session by passing a foreign player id.
	own, err := s.sb.SelectOne("rotation_players",
		"id=eq."+store.Q(playerID)+"&session_id=eq."+store.Q(sessionID)+"&select=id")
	if err != nil {
		return err
	}
	if own == nil {
		return ErrNotFound
	}
	row := map[string]any{
		"session_id":         sessionID,
		"round":              round,
		"rotation_player_id": playerID,
		"updated_at":         nowRFC3339(),
	}
	if score != nil {
		row["score"] = *score
	} else {
		row["score"] = nil
	}
	_, err = s.sb.Upsert("rotation_round_scores",
		"session_id,round,rotation_player_id", row)
	return err
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
// LIVE, so a mid-session arrival shows as "resting" immediately and rotates in
// next round (best-effort + no-op in setup). The RPC's row lock only serializes
// concurrent joinBench calls; the join-vs-advance race is handled separately by
// AdvanceRotationSession's reconciliation (active-but-unseated players are
// re-added), so a clobbered append here can never permanently strand a player.
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

// SetRotationPlayerActive sits a player out (active=false) or brings them back.
//
// Allowed LIVE as well as in setup, because the alternative is worse: a player
// rolls an ankle at round 4 with nobody to replace them, and without this the
// organizer must invent a fake substitute or leave a ghost holding a seat and
// being scored all night. Sitting out mid-session retires them the same way a
// substitution does — the advance reconciliation drops inactive players from the
// queue, and their scores stay theirs.
//
// Bringing someone BACK mid-session is the ordinary rejoin path: they're active
// and unseated, so that same reconciliation puts them at the back of the queue.
func (s *Service) SetRotationPlayerActive(playerID string, active bool) error {
	row, err := s.sb.SelectOne("rotation_players",
		"id=eq."+store.Q(playerID)+"&select=id,session_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	sessionID := asStr(row, "session_id")
	srow, serr := s.sb.SelectOne("rotation_sessions",
		"id=eq."+store.Q(sessionID)+"&select=status,bench")
	live := serr == nil && srow != nil &&
		(asStr(srow, "status") == "live" || asStr(srow, "status") == "paused")

	if live && !active {
		// A court must keep four. If this player is on one and nobody is waiting,
		// there is no honest way to free the seat — say so and point at the
		// operation that CAN do it, instead of leaving a ghost on court.
		seated, cerr := s.isSeatedNow(sessionID, playerID)
		if cerr == nil && seated {
			free := 0
			for _, id := range asStrSlice(srow, "bench") {
				if id != playerID {
					free++
				}
			}
			if free == 0 {
				return fmt.Errorf("nobody is waiting to take that spot — use " +
					"“Substitute a player” so their court still has four")
			}
		}
	}
	if live && active {
		// Bringing back someone who was SUBSTITUTED out would seat their
		// replacement twice: the advance remap resolves the returning player
		// straight onto the id that took over from them.
		if subs, _ := s.rotationSubstitutionsStrict(sessionID); subs != nil {
			for _, sub := range subs {
				if sub.OutPlayerID == playerID {
					return fmt.Errorf("someone already took over for them — add " +
						"them back as a new player instead")
				}
			}
		}
	}
	_, err = s.sb.Update("rotation_players", "id=eq."+store.Q(playerID),
		map[string]any{"active": active})
	return err
}

// isSeatedNow reports whether a player holds a seat in the live round.
func (s *Service) isSeatedNow(sessionID, playerID string) (bool, error) {
	srow, err := s.sb.SelectOne("rotation_sessions",
		"id=eq."+store.Q(sessionID)+"&select=current_round")
	if err != nil || srow == nil {
		return false, err
	}
	rows, err := s.sb.Select("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+
			"&round=eq."+fmt.Sprint(asInt(srow, "current_round")))
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		for _, col := range []string{"team_a_p1", "team_a_p2", "team_b_p1", "team_b_p2"} {
			if asStr(r, col) == playerID {
				return true, nil
			}
		}
	}
	return false, nil
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

// SetRotationPlayerStartCourt places a player on a starting court by hand
// (pre-start only). Passing nil un-places them, sending them back to the
// rating-ordered tail.
//
// The court number is NOT validated against the session's court count: the
// count is itself derived from a roster the organizer is still editing, so
// "court 4" typed while only 12 players are in the room has to survive the
// 4 more that arrive a minute later. Anything past the last real court simply
// seeds onto the bench, which is where those players would have started anyway.
func (s *Service) SetRotationPlayerStartCourt(playerID string, court *int) error {
	if err := s.rosterEditable(playerID); err != nil {
		return err
	}
	var val any
	if court != nil {
		if *court < 1 {
			return fmt.Errorf("court number starts at 1")
		}
		val = *court
	}
	_, err := s.sb.Update("rotation_players", "id=eq."+store.Q(playerID),
		map[string]any{"start_court": val})
	return err
}

// ShuffleRotationStartCourts randomly distributes the active roster across the
// starting courts (pre-start only) — the "nobody has a rating, just mix us up"
// button. Returns how many players were placed.
//
// Overflow players are numbered past the last court on purpose rather than left
// NULL: it keeps the shuffle a total order, so who waits first is part of what
// got randomised instead of falling back to a rating nobody set.
func (s *Service) ShuffleRotationStartCourts(sessionID string) (int, error) {
	srow, err := s.sb.SelectOne("rotation_sessions", "id=eq."+store.Q(sessionID)+"&select=status")
	if err != nil {
		return 0, err
	}
	if srow == nil {
		return 0, ErrNotFound
	}
	if asStr(srow, "status") != "setup" {
		return 0, fmt.Errorf("the courts can't be redrawn once the session has started")
	}
	rows, err := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&select=id,active")
	if err != nil {
		return 0, err
	}
	var ids, inactive []string
	for _, r := range rows {
		id := asStr(r, "id")
		if id == "" {
			continue
		}
		if asBool(r, "active") {
			ids = append(ids, id)
		} else {
			inactive = append(inactive, id)
		}
	}
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })

	// ONE write, not one per player. Sequential PATCHes had no transaction and
	// returned on the first failure, so a 40-player night could end up with half
	// the room on the new draw and half on the old one — a placement that is
	// neither, and that the organizer can only make worse by tapping again.
	batch := make([]map[string]any, 0, len(rows))
	for i, id := range ids {
		batch = append(batch, map[string]any{"id": id, "start_court": i/4 + 1})
	}
	// Clear anyone sitting out. A stale placement on a benched player silently
	// un-randomises the shuffle the moment they're brought back — they'd sort
	// ahead of the players actually drawn onto that court.
	for _, id := range inactive {
		batch = append(batch, map[string]any{"id": id, "start_court": nil})
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if _, err := s.sb.Upsert("rotation_players", "id", batch); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// SetRotationPlayerName fixes a roster player's name — a typo, a nickname, a
// surname that was missing. Allowed at any point in the session, INCLUDING live,
// because it changes only how a person is labelled.
//
// Deliberately separate from SubstituteRotationPlayer. Typing a different
// person's name over this one would be the tempting way to swap a player, and
// it would silently hand the whole night's record -- every score already on the
// board -- to someone who never played those rounds.
func (s *Service) SetRotationPlayerName(playerID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("a name is required")
	}
	row, err := s.sb.SelectOne("rotation_players", "id=eq."+store.Q(playerID)+"&select=id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	_, err = s.sb.Update("rotation_players", "id=eq."+store.Q(playerID),
		map[string]any{"display_name": name})
	return err
}

// SubstituteRotationPlayer hands one player's seat to another mid-session.
//
// The outgoing player keeps every point they have already scored and stops
// there; the incoming player starts a fresh record from the current round. That
// split is the whole feature, and it's why this creates a roster row instead of
// renaming: scores are keyed to the roster row, so a rename would move the
// night's work to someone who didn't do it.
//
// A chain (A -> B -> C) needs no special handling. The substitute is an ordinary
// roster player, so subbing THEM out later is this same call again.
func (s *Service) SubstituteRotationPlayer(sessionID string,
	req model.SubstituteRotationPlayerRequest) (model.RotationPlayer, error) {
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		return model.RotationPlayer{}, fmt.Errorf("who is coming in? a name is required")
	}
	if len(name) > 80 {
		name = name[:80]
	}

	// Refuse a name that is ALREADY playing. Otherwise one human ends up with two
	// roster rows, seated on two courts at once and splitting their own points —
	// and the likeliest way to reach it is the sensible-sounding move of covering
	// a walk-off with someone who is currently resting.
	// The active check is done HERE rather than left to the query filter: a name
	// belonging to someone who already left must stay reusable, which is what
	// makes a player returning later work.
	if dup, derr := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&select=id,display_name,active"); derr == nil {
		for _, r := range dup {
			if asBool(r, "active") &&
				strings.EqualFold(strings.TrimSpace(asStr(r, "display_name")), name) &&
				asStr(r, "id") != req.OutPlayerID {
				return model.RotationPlayer{}, fmt.Errorf(
					"%s is already playing in this session — pick a different name, "+
						"or substitute them out first", asStr(r, "display_name"))
			}
		}
	}

	rating := req.SelfRating
	if rating < 1.0 || rating > 7.0 {
		// Inherit the outgoing player's rating rather than defaulting to 3.0: the
		// substitute is stepping into that player's court, and a stranger rating
		// would misplace them the moment the courts reseed.
		rating = 3.0
		if out, oerr := s.sb.SelectOne("rotation_players",
			"id=eq."+store.Q(req.OutPlayerID)+"&select=self_rating"); oerr == nil && out != nil {
			rating = asFloatOr(out, "self_rating", 3.0)
		}
	}

	// One RPC, under the session's row lock. Done as separate calls from Go, the
	// gaps between them are ways to end the night with a broken room: a failure
	// midway leaves an ACTIVE, unseated substitute whom the next advance sweeps
	// onto a court (unremovable, since the roster locks once live); a double-tap
	// passes the "still active?" check twice and builds two substitutes; and the
	// queue gets rewritten from a snapshot read several round-trips earlier,
	// clobbering a concurrent advance.
	payload := map[string]any{
		"p_session": sessionID,
		"p_out":     req.OutPlayerID,
		"p_name":    name,
		"p_rating":  rating,
		"p_entrant": nil,
	}
	if req.EntrantID != nil && *req.EntrantID != "" {
		payload["p_entrant"] = *req.EntrantID
	}
	body, err := s.sb.RPC("rotation_substitute", payload)
	if err != nil {
		// Only blame the migration when it is genuinely absent. The RPC does real
		// writes that fail for ordinary reasons (a duplicate entrant, a rejected
		// rating), and sending the organizer off to run SQL about those — mid
		// session, with people waiting — is worse than saying nothing.
		if !s.columnReady("rotation_substitutions", "id") {
			return model.RotationPlayer{}, fmt.Errorf(
				"substitutions aren't available yet — the rotation_substitute "+
					"migration needs to run: %w", err)
		}
		return model.RotationPlayer{}, fmt.Errorf("could not substitute: %w", err)
	}
	var res struct {
		OK     bool           `json:"ok"`
		Reason string         `json:"reason"`
		Round  int            `json:"round"`
		Player map[string]any `json:"player"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return model.RotationPlayer{}, err
	}
	if !res.OK {
		switch res.Reason {
		case "no_session":
			return model.RotationPlayer{}, ErrNotFound
		case "not_started":
			return model.RotationPlayer{}, fmt.Errorf(
				"the session hasn't started — remove that player and add the new one instead")
		case "finished":
			return model.RotationPlayer{}, fmt.Errorf("this session has finished")
		case "out_not_active":
			return model.RotationPlayer{}, fmt.Errorf(
				"that player is already out of this session")
		case "already_substituted":
			return model.RotationPlayer{}, fmt.Errorf(
				"someone has already taken over for that player")
		default:
			return model.RotationPlayer{}, fmt.Errorf("could not substitute: %s", res.Reason)
		}
	}
	return rotationPlayerFromRow(res.Player), nil
}

// RotationSubstitutions lists a session's swaps in the order they happened, so
// each player's card can say who they came in for and who took over from them.
func (s *Service) RotationSubstitutions(sessionID string) ([]model.RotationSubstitution, error) {
	out, err := s.rotationSubstitutionsStrict(sessionID)
	if err != nil {
		// Display only: a session must still open if the history can't be read.
		return []model.RotationSubstitution{}, nil
	}
	return out, nil
}

// rotationSubstitutionsStrict is RotationSubstitutions for callers where a
// missing answer changes what happens on court, so an error must not be
// flattened into "nobody was substituted".
func (s *Service) rotationSubstitutionsStrict(sessionID string) ([]model.RotationSubstitution, error) {
	if !s.columnReady("rotation_substitutions", "id") {
		return nil, nil // table not migrated yet: genuinely no substitutions
	}
	rows, err := s.sb.Select("rotation_substitutions",
		"session_id=eq."+store.Q(sessionID)+"&order=round.asc,created_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.RotationSubstitution, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.RotationSubstitution{
			Round:       asInt(r, "round"),
			OutPlayerID: asStr(r, "out_player"),
			InPlayerID:  asStr(r, "in_player"),
		})
	}
	return out, nil
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

	// Active players, strongest self-rating first (stable by created_at). The
	// placements ride along as a COLUMN, not as sort order — see below.
	placed := s.columnReady("rotation_players", "start_court")
	loadActive := func() ([]map[string]any, error) {
		sel := "&select=id"
		if placed {
			sel = "&select=id,start_court"
		}
		return s.sb.Select("rotation_players",
			"session_id=eq."+store.Q(sessionID)+
				"&active=eq.true&order=self_rating.desc,created_at.asc"+sel)
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
	// A court number the organizer typed is a DESTINATION, not a ranking. Sorting
	// the roster by start_court and chunking it into fours — the obvious reading —
	// silently does the opposite: place two players on court 3, leave the rest
	// unplaced, and those two sort to the front and open the TOP court.
	seats := make([]engine.Seat, 0, len(rows))
	for _, r := range rows {
		seat := engine.Seat{ID: asStr(r, "id")}
		if placed {
			if c := asIntPtr(r, "start_court"); c != nil {
				seat.Court = *c
			}
		}
		seats = append(seats, seat)
	}

	courts, bench := engine.SeedPlacedCourts(seats, maxCourts)
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
func (s *Service) ReportRotationCourt(sessionID, callerUserID string,
	isOwner bool, req model.ReportRotationCourtRequest) error {
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
	// A player may report THEIR court, not any court. The route only checks that
	// the caller is somewhere in this session, so without this anyone on court 4
	// could overwrite court 1's result and send the wrong pair up — and the
	// update is a blind overwrite, so the real result is simply gone.
	if !isOwner && !s.sitsOnCourt(sessionID, callerUserID, req.Round, req.Court) {
		return fmt.Errorf("you can only report the result for your own court")
	}
	_, err = s.sb.Update("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(req.Round)+"&court=eq."+fmt.Sprint(req.Court),
		map[string]any{"winner": req.Winner, "reported_at": nowRFC3339()})
	return err
}

// inactiveIDs is the set of roster rows no longer in the session.
func (s *Service) inactiveIDs(sessionID string) (map[string]bool, error) {
	rows, err := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&select=id,active")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if id := asStr(r, "id"); id != "" && !asBool(r, "active") {
			out[id] = true
		}
	}
	return out, nil
}

// sitsOnCourt reports whether the caller occupies one of the four seats on a
// given court this round.
func (s *Service) sitsOnCourt(sessionID, userID string, round, court int) bool {
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
	me, err := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&entrant_id=eq."+store.Q(entrant)+"&select=id")
	if err != nil || len(me) == 0 {
		return false
	}
	mine := make(map[string]bool, len(me))
	for _, r := range me {
		mine[asStr(r, "id")] = true
	}
	row, err := s.sb.SelectOne("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+
			"&round=eq."+fmt.Sprint(round)+"&court=eq."+fmt.Sprint(court))
	if err != nil || row == nil {
		return false
	}
	for _, col := range []string{"team_a_p1", "team_a_p2", "team_b_p1", "team_b_p2"} {
		if mine[asStr(row, col)] {
			return true
		}
	}
	return false
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
	// Scorecard-driven results: the organizer now records a SCORE per player per
	// round, so a court's winner is DERIVED by comparing the two teams' totals
	// rather than read from a who-won tap. Falls back to the stored winner when
	// no scores were entered (or the scorecard migration hasn't run).
	scoreOf := map[string]int{} // rotation_player_id -> this round's score
	if s.rotationScoresReady() {
		if srows, serr := s.sb.Select("rotation_round_scores",
			"session_id=eq."+store.Q(sessionID)+"&round=eq."+fmt.Sprint(round)+
				"&select=rotation_player_id,score"); serr == nil {
			for _, r := range srows {
				if r["score"] == nil {
					continue
				}
				scoreOf[asStr(r, "rotation_player_id")] = asInt(r, "score")
			}
		}
	}
	cur := make([]engine.RotCourt, 0, len(rows))
	results := make([]engine.RotResult, 0, len(rows))
	// Courts whose two teams entered EQUAL totals — enforced after the loop.
	var tied []int
	for _, r := range rows {
		court := asInt(r, "court")
		teamA := [2]string{asStr(r, "team_a_p1"), asStr(r, "team_a_p2")}
		teamB := [2]string{asStr(r, "team_b_p1"), asStr(r, "team_b_p2")}
		cur = append(cur, engine.RotCourt{Court: court, TeamA: teamA, TeamB: teamB})

		w := ""
		// Decide from the scorecard ONLY when BOTH teams are fully entered — a
		// half-typed court would otherwise compare one player's points against two
		// and hand the win to the wrong side. A tie stays undecided and falls
		// through to the default below.
		aPts, aFull := teamPoints(scoreOf, teamA)
		bPts, bFull := teamPoints(scoreOf, teamB)
		if aFull && bFull && (aPts > 0 || bPts > 0) {
			if aPts > bPts {
				w = "a"
			} else if bPts > aPts {
				w = "b"
			} else if rep := asStr(r, "winner"); rep == "a" || rep == "b" {
				// Equal on the scorecard, but the sudden-death point was already
				// tapped. That IS the resolution — checking this before declaring
				// a tie is what makes the refusal escapable without the organizer
				// having to edit a score cell.
				w = rep
			} else {
				// A REAL tie: both teams fully entered, equal, and nobody has
				// tapped a winner. The house rule is a sudden-death point, so the
				// round isn't over — refuse rather than fall through to the
				// unreported default below, which moves team A up and would
				// promote the pair that didn't win.
				tied = append(tied, court)
			}
			// PERSIST it: the tally RPC only credits a win when
			// rotation_round_courts.winner is 'a'/'b'. Without this the scorecard
			// drove movement but every player finished the session with 0 wins, so
			// the documented tiebreak was dead and the UI showed a false "0W".
			if w != "" {
				_, _ = s.sb.Update("rotation_round_courts",
					"session_id=eq."+store.Q(sessionID)+
						"&round=eq."+fmt.Sprint(round)+
						"&court=eq."+fmt.Sprint(court),
					map[string]any{"winner": w, "reported_at": nowRFC3339()})
			}
		}
		if w == "" {
			w = asStr(r, "winner") // legacy who-won tap, if it was used
		}
		if w == "" {
			// Nothing recorded → default team A UP for MOVEMENT only. (The RPC
			// tally credits games to all four but a win only to a reported team,
			// so an unreported court awards no phantom win.)
			w = "a"
		}
		results = append(results, engine.RotResult{Court: court, Winner: w})
	}

	if len(tied) > 0 {
		what := "Court " + fmt.Sprint(tied[0]) + " is"
		if len(tied) > 1 {
			what = "Courts "
			for i, c := range tied {
				if i > 0 {
					what += ", "
				}
				what += fmt.Sprint(c)
			}
			what += " are"
		}
		return fmt.Errorf("%s tied — play a sudden-death point and update the "+
			"score before moving on", what)
	}

	// The court rows were read above, so a substitution completing in between
	// leaves `cur` naming a player who has since gone home — and the bench prune
	// below only cleans the QUEUE, not the seats. Remap through the swap history
	// so the substitute inherits the seat rather than the next round being
	// written with someone who left. Chains resolve by walking the map.
	subs, serr := s.rotationSubstitutionsStrict(sessionID)
	if serr != nil {
		// Skipping the remap on a failed read is indistinguishable from "nobody
		// was substituted", and the consequence is a round written with someone
		// who went home. Refuse instead — the organizer can tap again.
		return fmt.Errorf("couldn't read this session's substitutions — not "+
			"advancing rather than risk seating someone who has left: %w", serr)
	}
	if len(subs) > 0 {
		takeover := make(map[string]string, len(subs))
		for _, sub := range subs {
			takeover[sub.OutPlayerID] = sub.InPlayerID
		}
		resolve := func(id string) string {
			for hop := 0; hop <= len(takeover); hop++ {
				next, ok := takeover[id]
				if !ok {
					break
				}
				id = next
			}
			return id
		}
		for i := range cur {
			cur[i].TeamA[0] = resolve(cur[i].TeamA[0])
			cur[i].TeamA[1] = resolve(cur[i].TeamA[1])
			cur[i].TeamB[0] = resolve(cur[i].TeamB[0])
			cur[i].TeamB[1] = resolve(cur[i].TeamB[1])
		}
		// The QUEUE needs the same treatment, and for the same reason. It also
		// keeps the substitute in the departing player's PLACE — the reconcile
		// below would otherwise drop the inactive id and append the substitute at
		// the back, undoing the position the RPC deliberately preserved.
		for i := range bench {
			bench[i] = resolve(bench[i])
		}
	}

	// Anyone still SEATED who is no longer in the session takes their seat with
	// them unless we refill it. Sitting a player out only flipped a flag, so
	// without this they were re-seated by the engine every round for the rest of
	// the night — collecting games, blocking the scorecard from ever completing
	// (which silently stopped auto-advance), and making their court unresolvable.
	// Exactly the ghost the feature was written to remove.
	if left, lerr := s.inactiveIDs(sessionID); lerr == nil && len(left) > 0 {
		waiting := make([]string, 0, len(bench))
		for _, id := range bench {
			if !left[id] {
				waiting = append(waiting, id)
			}
		}
		take := 0
		for i := range cur {
			for _, seat := range []*string{
				&cur[i].TeamA[0], &cur[i].TeamA[1],
				&cur[i].TeamB[0], &cur[i].TeamB[1],
			} {
				if !left[*seat] {
					continue
				}
				if take < len(waiting) {
					*seat = waiting[take] // longest-waiting steps on
					take++
				}
			}
		}
		bench = waiting[take:]
	}

	// Reconcile the bench against the live roster. Two directions:
	//
	//  ADD — any ACTIVE player who isn't seated this round and isn't already on
	//  the bench snapshot joined since we read the bench (self-heals the
	//  join-vs-advance race). Appended at the back, so the newest waits longest.
	//
	//  DROP — any player who is no longer active. Substitution is the only thing
	//  that deactivates someone mid-session, and a substituted-out player left on
	//  the bench would rotate back onto a court later in the night, after their
	//  replacement had already taken over their spot.
	seated := map[string]bool{}
	for _, c := range cur {
		for _, id := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			if id != "" {
				seated[id] = true
			}
		}
	}
	benchSet := map[string]bool{}
	for _, id := range bench {
		benchSet[id] = true
	}
	// active is read from the ROW, not inferred from the query filter. Whether
	// someone is still in the session decides who gets seated for the rest of the
	// night, so it shouldn't rest on a filter staying attached to this URL.
	if activeRows, aerr := s.sb.Select("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&active=eq.true&order=created_at.asc&select=id,active"); aerr == nil {
		active := make(map[string]bool, len(activeRows))
		for _, r := range activeRows {
			if id := asStr(r, "id"); id != "" && asBool(r, "active") {
				active[id] = true
			}
		}
		kept := bench[:0]
		for _, id := range bench {
			if active[id] {
				kept = append(kept, id)
			}
		}
		bench = kept
		for _, r := range activeRows {
			id := asStr(r, "id")
			if id != "" && active[id] && !seated[id] && !benchSet[id] {
				bench = append(bench, id)
			}
		}
	}

	mode, merr := s.rotationLoserMode(sessionID)
	if merr != nil {
		return fmt.Errorf("couldn't read this ladder's loser rule — not advancing "+
			"rather than risk running the round under the wrong rules: %w", merr)
	}
	nextCourts, nextBench := engine.NextRound(cur, results, bench, mode)
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
	// Derive + persist the FINAL round's winners before the tally RPC runs —
	// otherwise the last round of every scorecard session credits zero wins.
	if srow, err := s.sb.SelectOne("rotation_sessions",
		"id=eq."+store.Q(sessionID)+"&select=current_round"); err == nil && srow != nil {
		s.persistScorecardWinners(sessionID, asInt(srow, "current_round"))
	}
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
		StartCourt:  asIntPtr(r, "start_court"),
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

// rotationLoserMode resolves the league's loser rule for a session: 'stay' means
// losers hold their court (and the top court's losers fall to the bottom),
// anything else is the classic river where losers drop one.
//
// Fails safe to LosersDown — the behaviour every existing ladder already has —
// so a missing column, an unreadable league or a session whose division has gone
// keeps running the way it always did rather than silently changing the rules
// mid-night.
func (s *Service) rotationLoserMode(sessionID string) (engine.LoserMode, error) {
	s.loserModeMu.Lock()
	if s.loserModeCache == nil {
		s.loserModeCache = map[string]engine.LoserMode{}
	}
	if m, ok := s.loserModeCache[sessionID]; ok {
		s.loserModeMu.Unlock()
		return m, nil
	}
	s.loserModeMu.Unlock()

	// Not yet chosen for this ladder → the classic river, permanently and
	// correctly. This is the ONLY case that may silently default: it's a fact
	// about the schema, not a failed read.
	if !s.columnReady("leagues", "ladder_loser_mode") {
		return engine.LosersDown, nil
	}
	// A MISSING division or league is a definite answer, not a failed read — the
	// session simply has no league to carry a rule, so the classic river stands.
	// Propagating it would block every advance on that session forever.
	div, err := s.DivisionOfRotationSession(sessionID)
	if errors.Is(err, ErrNotFound) || div == "" {
		return engine.LosersDown, nil
	}
	if err != nil {
		return engine.LosersDown, err
	}
	leagueID, err := s.LeagueIDOfDivision(div)
	if errors.Is(err, ErrNotFound) || leagueID == "" {
		return engine.LosersDown, nil
	}
	if err != nil {
		return engine.LosersDown, err
	}
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=ladder_loser_mode")
	if err != nil {
		// A TRANSIENT read failure must not quietly run the round under the other
		// set of rules. Returning down-plus-error lets the advance refuse; the
		// board, where being wrong is only cosmetic, ignores it.
		return engine.LosersDown, err
	}
	mode := engine.LosersDown
	if lg != nil && asStr(lg, "ladder_loser_mode") == "stay" {
		mode = engine.LosersStay
	}
	// Fixed at league creation with no update path, so it is safe to keep.
	s.loserModeMu.Lock()
	s.loserModeCache[sessionID] = mode
	s.loserModeMu.Unlock()
	return mode, nil
}
