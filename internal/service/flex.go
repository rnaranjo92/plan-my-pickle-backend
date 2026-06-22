package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// ----------------------------------------------------------------------------
// Flex League — the self-scheduled, season-long round-robin of fixed-partner
// TEAMS (the fastest-growing modern league format).
//
// Per league division (league_brackets row) the organizer registers a set of
// fixed-partner TEAMS — REUSING the Team-league `teams` table (the name captures
// the pair for the MVP) — and GENERATES a round-robin SCHEDULE: every team pair
// becomes a "matchup" with status='pending'. Matchups are SELF-SCHEDULED (no
// fixed date): teams play whenever, then the organizer records a result (score +
// winner) and the matchup flips to 'completed' (played_at set).
//
// Standings (each team's wins / losses + win %) are NOT stored — they are
// computed in Go from the COMPLETED matchups, REUSING the Team-league
// computeTeamStandings() (a completed matchup is, for standings purposes, exactly
// a team fixture). The DIFFERENCE from the Team league: the Team league records
// AD-HOC fixtures; Flex PRE-GENERATES the full round-robin so everyone knows who
// they still owe a game.
//
// Modeled CLOSELY on team.go (per-division entities + recorded results + a
// standings list). REUSES team.go's divisionOfTeam / listTeams / AddTeam /
// RemoveTeam / computeTeamStandings. The HTTP layer gates every write behind
// league ownership (the FlexOwner* guards below mirror the team guards).
// ----------------------------------------------------------------------------

// FlexOwner returns the owner (auth-user id) of the league behind a division
// (league_bracket), so the HTTP layer can gate Flex writes on league ownership.
// Identical chain to TeamOwner; named separately so the route wiring reads clean.
func (s *Service) FlexOwner(leagueBracketID string) (string, error) {
	return s.TeamOwner(leagueBracketID)
}

// FlexOwnerOfMatchup resolves the owning league's owner from a matchup id (a
// matchup → its division → its league → owner). Used to gate per-matchup writes
// (recording a result) without trusting the body's division.
func (s *Service) FlexOwnerOfMatchup(matchupID string) (string, error) {
	div, err := s.divisionOfMatchup(matchupID)
	if err != nil {
		return "", err
	}
	return s.FlexOwner(div)
}

// divisionOfMatchup returns the league_bracket_id a flex matchup belongs to.
func (s *Service) divisionOfMatchup(matchupID string) (string, error) {
	row, err := s.sb.SelectOne("flex_matchups",
		"id=eq."+store.Q(matchupID)+"&select=league_bracket_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return asStr(row, "league_bracket_id"), nil
}

// ListFlexStandings returns a division's teams with their computed W-L record and
// win %, ordered by wins (then win %) — the standings view, computed on read from
// the COMPLETED matchups (not stored). Reuses listTeams + computeTeamStandings.
func (s *Service) ListFlexStandings(leagueBracketID string) ([]model.TeamStanding, error) {
	teams, err := s.listTeams(leagueBracketID)
	if err != nil {
		return nil, err
	}
	completed, err := s.completedFlexFixtures(leagueBracketID)
	if err != nil {
		return nil, err
	}
	return computeTeamStandings(teams, completed), nil
}

// AddFlexTeam registers a team on a Flex division. Thin wrapper over the shared
// AddTeam (Flex reuses the `teams` table) so the Flex service surface is complete
// and the route wiring doesn't have to reach across to the Team service.
func (s *Service) AddFlexTeam(leagueBracketID string, req model.AddTeamRequest) (model.Team, error) {
	return s.AddTeam(leagueBracketID, req)
}

// RemoveFlexTeam deletes a team; its matchups cascade-delete (FK on delete
// cascade). Standings recompute on the next read. Thin wrapper over RemoveTeam.
func (s *Service) RemoveFlexTeam(teamID string) error {
	return s.RemoveTeam(teamID)
}

// ListFlexMatchups returns a division's full generated schedule (every matchup,
// pending and completed), pending first then completed, each group ordered by
// creation so the round-robin reads in a stable order.
func (s *Service) ListFlexMatchups(leagueBracketID string) ([]model.FlexMatchup, error) {
	rows, err := s.sb.Select("flex_matchups",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+
			"&select=*&order=status.asc,created_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.FlexMatchup, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapFlexMatchup(r))
	}
	return out, nil
}

// completedFlexFixtures loads a division's COMPLETED matchups as TeamFixtures, so
// computeTeamStandings (which is written against fixtures) can tally them. Only
// completed rows have a winner, so only those count toward standings.
func (s *Service) completedFlexFixtures(leagueBracketID string) ([]model.TeamFixture, error) {
	rows, err := s.sb.Select("flex_matchups",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+
			"&status=eq.completed&select=*")
	if err != nil {
		return nil, err
	}
	out := make([]model.TeamFixture, 0, len(rows))
	for _, r := range rows {
		m := mapFlexMatchup(r)
		out = append(out, model.TeamFixture{
			ID:              m.ID,
			LeagueBracketID: m.LeagueBracketID,
			TeamAID:         m.TeamAID,
			TeamBID:         m.TeamBID,
			WinnerTeamID:    m.WinnerTeamID,
			Score:           m.Score,
			PlayedAt:        m.PlayedAt,
		})
	}
	return out, nil
}

// GenerateFlexSchedule creates the full round-robin schedule for a division:
// every UNORDERED team pair C(n,2) becomes a pending matchup. It is IDEMPOTENT —
// pairs that already have a matchup (in either A/B order) are skipped, so
// re-running after adding new teams only fills in the missing pairings without
// duplicating existing ones. Returns the number of matchups created.
func (s *Service) GenerateFlexSchedule(leagueBracketID string) (int, error) {
	teams, err := s.listTeams(leagueBracketID)
	if err != nil {
		return 0, err
	}
	if len(teams) < 2 {
		// Nothing to pair — not an error; an empty/one-team division just yields 0.
		return 0, nil
	}

	existing, err := s.ListFlexMatchups(leagueBracketID)
	if err != nil {
		return 0, err
	}

	// The new pairings (every C(n,2) pair not already scheduled) come from a pure
	// helper so the round-robin logic is unit-testable without a DB.
	pairs := flexPairingsToCreate(teams, existing)
	if len(pairs) == 0 {
		return 0, nil
	}
	rows := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		rows = append(rows, map[string]any{
			"league_bracket_id": leagueBracketID,
			"team_a_id":         p[0],
			"team_b_id":         p[1],
			"status":            "pending",
		})
	}
	if _, err := s.sb.Insert("flex_matchups", rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// flexPairingsToCreate is the PURE round-robin generator: given the division's
// teams and the matchups that already exist, it returns the [teamA, teamB] pairs
// that still need a matchup — every UNORDERED pair C(n,2) not already scheduled
// (in either A/B order). With no existing matchups it returns all n*(n-1)/2
// pairs; re-running with the full schedule returns none (idempotent). Order is
// the stable nested-loop order over the input team slice.
func flexPairingsToCreate(teams []model.Team, existing []model.FlexMatchup) [][2]string {
	have := make(map[string]bool, len(existing))
	for _, m := range existing {
		have[pairKey(m.TeamAID, m.TeamBID)] = true
	}
	out := make([][2]string, 0)
	for i := 0; i < len(teams); i++ {
		for j := i + 1; j < len(teams); j++ {
			if have[pairKey(teams[i].ID, teams[j].ID)] {
				continue
			}
			out = append(out, [2]string{teams[i].ID, teams[j].ID})
		}
	}
	return out
}

// pairKey returns an order-independent key for a team pair so A-vs-B and B-vs-A
// collapse to the same entry (used to dedupe the round-robin generation).
func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// RecordFlexResult records the result of a pending matchup, flipping it to
// completed. WinnerTeamID must be one of the matchup's two teams. The matchup's
// own league_bracket_id is verified against the authorized (path) division — the
// same scope-binding the team/ladder fixes use — so a caller can't record a
// result onto a matchup outside the division they proved ownership of.
func (s *Service) RecordFlexResult(leagueBracketID, matchupID string, req model.RecordFlexResultRequest) (model.FlexMatchup, error) {
	w := strings.TrimSpace(req.WinnerTeamID)
	if w == "" {
		return model.FlexMatchup{}, errors.New("winnerTeamId is required")
	}

	row, err := s.sb.SelectOne("flex_matchups",
		"id=eq."+store.Q(matchupID)+"&select=*")
	if err != nil {
		return model.FlexMatchup{}, err
	}
	if row == nil {
		return model.FlexMatchup{}, ErrNotFound
	}
	m := mapFlexMatchup(row)
	// Bind the matchup to the authorized division (path), not the body — guard
	// against recording onto another organizer's matchup (cross-division ref).
	if m.LeagueBracketID != leagueBracketID {
		return model.FlexMatchup{}, errors.New("matchup does not belong to this division")
	}
	if w != m.TeamAID && w != m.TeamBID {
		return model.FlexMatchup{}, errors.New("winnerTeamId must be one of the matchup's teams")
	}

	fields := map[string]any{
		"winner_team_id": w,
		"score":          orNull(strings.TrimSpace(req.Score)),
		"status":         "completed",
	}
	// flex_matchups.played_at has no DB default (it's NULL while pending), so set
	// it explicitly on completion — the organizer's backdate, or now().
	if pa := strings.TrimSpace(req.PlayedAt); pa != "" {
		fields["played_at"] = pa
	} else {
		fields["played_at"] = now()
	}

	rows, err := s.sb.Update("flex_matchups", "id=eq."+store.Q(matchupID), fields)
	if err != nil {
		return model.FlexMatchup{}, err
	}
	if len(rows) == 0 {
		return model.FlexMatchup{}, errors.New("matchup update returned no row")
	}
	return mapFlexMatchup(rows[0]), nil
}
