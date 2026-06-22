package service

import (
	"errors"
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// ----------------------------------------------------------------------------
// Team League (engine #4 of 4 — the LAST). The SIMPLE "single fixture result"
// model (NOT lineup-of-lines).
//
// Per league division (league_brackets row) the organizer registers a set of
// TEAMS — each just a name + an OPTIONAL roster link — and records FIXTURES:
// Team A vs Team B, which team WON, and an OPTIONAL free-text score. There is NO
// per-line / lineup detail.
//
// Standings == each team's fixtures won / lost (+ win %), ordered by wins then
// win %. They are NOT stored — computeTeamStandings() derives them in Go from
// the recorded fixtures (no leapfrog, unlike the ladder). Organizer-driven only;
// the backend gates every write behind league ownership.
//
// Modeled CLOSELY on ladder.go (per-division entities + recorded results + a
// standings list). The pure computeTeamStandings() below is unit-tested.
// ----------------------------------------------------------------------------

// TeamOwner returns the owner (auth-user id) of the league behind a division
// (league_bracket), so the HTTP layer can gate team writes on league ownership.
// Returns ErrNotFound if the division or its league is missing.
func (s *Service) TeamOwner(leagueBracketID string) (string, error) {
	row, err := s.sb.SelectOne("league_brackets",
		"id=eq."+store.Q(leagueBracketID)+"&select=league_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return s.OwnerOf("league", asStr(row, "league_id"))
}

// TeamOwnerOfTeam resolves the owning league's owner from a team id (a team →
// its division → its league → owner). Used to gate per-team writes.
func (s *Service) TeamOwnerOfTeam(teamID string) (string, error) {
	div, err := s.divisionOfTeam(teamID)
	if err != nil {
		return "", err
	}
	return s.TeamOwner(div)
}

// divisionOfTeam returns the league_bracket_id a team belongs to.
func (s *Service) divisionOfTeam(teamID string) (string, error) {
	row, err := s.sb.SelectOne("teams",
		"id=eq."+store.Q(teamID)+"&select=league_bracket_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return asStr(row, "league_bracket_id"), nil
}

// listTeams returns a division's teams ordered by name. Internal helper shared
// by ListTeamStandings (which also needs the team set) and name lookups.
func (s *Service) listTeams(leagueBracketID string) ([]model.Team, error) {
	rows, err := s.sb.Select("teams",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+"&select=*&order=name.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Team, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapTeam(r))
	}
	return out, nil
}

// ListTeamStandings returns a division's teams with their computed W-L record
// and win %, ordered by wins (then win %). This is the standings view — there is
// no stored ranking; it is derived from the fixtures every read.
func (s *Service) ListTeamStandings(leagueBracketID string) ([]model.TeamStanding, error) {
	teams, err := s.listTeams(leagueBracketID)
	if err != nil {
		return nil, err
	}
	fixtures, err := s.TeamFixtures(leagueBracketID)
	if err != nil {
		return nil, err
	}
	return computeTeamStandings(teams, fixtures), nil
}

// AddTeam registers a team on a division. Name is required; PlayerID optional
// (roster link). The division must exist (the caller already proved ownership).
func (s *Service) AddTeam(leagueBracketID string, req model.AddTeamRequest) (model.Team, error) {
	if strings.TrimSpace(req.Name) == "" {
		return model.Team{}, errors.New("name is required")
	}
	rows, err := s.sb.Insert("teams", map[string]any{
		"league_bracket_id": leagueBracketID,
		"name":              strings.TrimSpace(req.Name),
		"player_id":         orNull(strOr(req.PlayerID)),
	})
	if err != nil {
		return model.Team{}, err
	}
	if len(rows) == 0 {
		return model.Team{}, errors.New("team insert returned no row")
	}
	return mapTeam(rows[0]), nil
}

// RemoveTeam deletes a team; its fixture history cascade-deletes (FK on delete
// cascade). Standings recompute on the next read, so no compaction is needed
// (unlike the ladder's contiguous-position invariant).
func (s *Service) RemoveTeam(teamID string) error {
	// Resolve the division first so a missing team is reported as ErrNotFound.
	if _, err := s.divisionOfTeam(teamID); err != nil {
		return err
	}
	return s.sb.Delete("teams", "id=eq."+store.Q(teamID))
}

// RecordFixture records a single fixture result between two teams. WinnerTeamID
// must be one of A/B. Returns the recorded fixture. No reorder (standings are
// computed on read).
func (s *Service) RecordFixture(leagueBracketID string, req model.RecordFixtureRequest) (model.TeamFixture, error) {
	a := strings.TrimSpace(req.TeamAID)
	b := strings.TrimSpace(req.TeamBID)
	w := strings.TrimSpace(req.WinnerTeamID)
	if a == "" || b == "" || w == "" {
		return model.TeamFixture{}, errors.New("teamAId, teamBId and winnerTeamId are required")
	}
	if a == b {
		return model.TeamFixture{}, errors.New("the two teams must be different")
	}
	if w != a && w != b {
		return model.TeamFixture{}, errors.New("winnerTeamId must be teamAId or teamBId")
	}

	payload := map[string]any{
		"league_bracket_id": leagueBracketID,
		"team_a_id":         a,
		"team_b_id":         b,
		"winner_team_id":    w,
		"score":             orNull(strings.TrimSpace(req.Score)),
	}
	if pa := strings.TrimSpace(req.PlayedAt); pa != "" {
		payload["played_at"] = pa
	}
	rows, err := s.sb.Insert("team_fixtures", payload)
	if err != nil {
		return model.TeamFixture{}, err
	}
	if len(rows) == 0 {
		return model.TeamFixture{}, errors.New("fixture insert returned no row")
	}
	return mapTeamFixture(rows[0]), nil
}

// TeamFixtures returns a division's recorded fixtures, newest first (history).
func (s *Service) TeamFixtures(leagueBracketID string) ([]model.TeamFixture, error) {
	rows, err := s.sb.Select("team_fixtures",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+"&select=*&order=played_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.TeamFixture, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapTeamFixture(r))
	}
	return out, nil
}

// computeTeamStandings is the PURE standings computation for the Team League:
// from a set of teams and the recorded fixtures, it tallies each team's wins /
// losses, computes win % (wins / played, 0 when a team has no fixtures), and
// returns the teams ordered by wins DESC, then win % DESC, then name ASC (a
// stable tiebreak). A team with no fixtures still appears (0-0, 0% win).
//
// Fixtures referencing a team not in the set are ignored (defensive). The
// winner takes a win; the OTHER side of the fixture takes a loss.
func computeTeamStandings(teams []model.Team, fixtures []model.TeamFixture) []model.TeamStanding {
	idx := make(map[string]*model.TeamStanding, len(teams))
	out := make([]model.TeamStanding, len(teams))
	for i, t := range teams {
		out[i] = model.TeamStanding{TeamID: t.ID, Name: t.Name}
		idx[t.ID] = &out[i]
	}

	for _, f := range fixtures {
		winnerID := f.WinnerTeamID
		// The loser is whichever recorded side is NOT the winner.
		loserID := f.TeamAID
		if winnerID == f.TeamAID {
			loserID = f.TeamBID
		}
		if ws, ok := idx[winnerID]; ok {
			ws.Wins++
			ws.Played++
		}
		if ls, ok := idx[loserID]; ok {
			ls.Losses++
			ls.Played++
		}
	}

	for i := range out {
		if out[i].Played > 0 {
			out[i].WinPct = float64(out[i].Wins) / float64(out[i].Played)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Wins != out[j].Wins {
			return out[i].Wins > out[j].Wins
		}
		if out[i].WinPct != out[j].WinPct {
			return out[i].WinPct > out[j].WinPct
		}
		return out[i].Name < out[j].Name
	})
	return out
}
