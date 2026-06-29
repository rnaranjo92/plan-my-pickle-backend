package service

import (
	"errors"
	"fmt"
	"sort"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// ----------------------------------------------------------------------------
// MLP-style team events. A team-format event (events.team_size > 0) registers
// TEAMS, each with a roster of players (with a gender). Each team-vs-team
// matchup is a TeamTie whose lines REUSE the matches table (matches.tie_id +
// line_type): women's doubles (wd), men's doubles (md), two mixed (mx1, mx2),
// plus a lazily-created decider (dec) on a 2-2 split.
//
// Lines are scored through the NORMAL match score path (RecordSeries); the
// rollup is hooked into applySeries so finishing a line re-evaluates the tie.
// ----------------------------------------------------------------------------

// regulation tie lines, in play order. The decider (dec) is created lazily.
var tieLineOrder = []string{"wd", "md", "mx1", "mx2"}

func mapEventTeam(m map[string]any) model.EventTeam {
	t := model.EventTeam{
		ID:      asStr(m, "id"),
		EventID: asStr(m, "event_id"),
		Name:    asStr(m, "name"),
	}
	if b := asStr(m, "bracket_id"); b != "" {
		t.BracketID = &b
	}
	if s, ok := m["seed"]; ok && s != nil {
		v := asInt(m, "seed")
		t.Seed = &v
	}
	return t
}

func mapTeamMember(m map[string]any) model.TeamMember {
	tm := model.TeamMember{
		ID:       asStr(m, "id"),
		TeamID:   asStr(m, "team_id"),
		FullName:  asStr(m, "full_name"),
		Gender:    asStr(m, "gender"),
		CheckedIn: asBool(m, "checked_in"),
	}
	if p := asStr(m, "player_id"); p != "" {
		tm.PlayerID = &p
	}
	if pl, ok := m["player"].(map[string]any); ok {
		tm.Phone = asStr(pl, "phone")
		tm.DuprID = asStr(pl, "dupr_id")
		tm.DuprRating = asFloatPtr(pl, "dupr_rating")
	}
	return tm
}

// CreateTeam registers a team on a team-format event (owner-gated by the route).
func (s *Service) CreateTeam(eventID string, req model.CreateTeamRequest) (model.EventTeam, error) {
	if req.Name == "" {
		return model.EventTeam{}, errors.New("team name is required")
	}
	row := map[string]any{"event_id": eventID, "name": req.Name}
	if req.BracketID != nil && *req.BracketID != "" {
		row["bracket_id"] = *req.BracketID
	}
	out, err := s.sb.Insert("event_teams", []map[string]any{row})
	if err != nil {
		return model.EventTeam{}, err
	}
	if len(out) == 0 {
		return model.EventTeam{}, errors.New("team insert returned no row")
	}
	return mapEventTeam(out[0]), nil
}

// RenameEventTeam updates a team's display name.
func (s *Service) RenameEventTeam(teamID, name string) error {
	if name == "" {
		return errors.New("team name is required")
	}
	_, err := s.sb.Update("event_teams", "id=eq."+store.Q(teamID),
		map[string]any{"name": name})
	return err
}

// AddTeamMember adds a roster member. Every member gets a players row (created if
// not linked) so tie lines can reuse match_participants; gender is required and
// drives line eligibility.
func (s *Service) AddTeamMember(teamID string, req model.AddTeamMemberRequest) (model.TeamMember, error) {
	if req.FullName == "" {
		return model.TeamMember{}, errors.New("member name is required")
	}
	if req.Gender != "M" && req.Gender != "F" {
		return model.TeamMember{}, errors.New("gender must be 'M' or 'F'")
	}
	playerID := ""
	if req.PlayerID != nil {
		playerID = *req.PlayerID
	}
	if playerID == "" {
		// Mint a players row so lines can reference the member; carry optional
		// contact + DUPR fields so they show on the card and profile.
		prow := map[string]any{"full_name": req.FullName}
		if req.Phone != "" {
			prow["phone"] = req.Phone
		}
		if req.DuprID != "" {
			prow["dupr_id"] = req.DuprID
		}
		if req.DuprRating != nil {
			prow["dupr_rating"] = *req.DuprRating
		}
		pl, err := s.sb.Insert("players", []map[string]any{prow})
		if err != nil {
			return model.TeamMember{}, err
		}
		if len(pl) == 0 {
			return model.TeamMember{}, errors.New("player insert returned no row")
		}
		playerID = asStr(pl[0], "id")
	}
	row := map[string]any{
		"team_id":   teamID,
		"player_id": playerID,
		"full_name": req.FullName,
		"gender":    req.Gender,
	}
	out, err := s.sb.Insert("event_team_members", []map[string]any{row})
	if err != nil {
		return model.TeamMember{}, err
	}
	if len(out) == 0 {
		return model.TeamMember{}, errors.New("member insert returned no row")
	}
	return mapTeamMember(out[0]), nil
}

// RemoveEventTeam deletes a team (members + its ties/lines cascade in PG).
func (s *Service) RemoveEventTeam(teamID string) error {
	return s.sb.Delete("event_teams", "id=eq."+store.Q(teamID))
}

// RemoveTeamMember drops one roster member.
func (s *Service) RemoveTeamMember(memberID string) error {
	return s.sb.Delete("event_team_members", "id=eq."+store.Q(memberID))
}

// SetMemberCheckedIn toggles a roster member's check-in.
func (s *Service) SetMemberCheckedIn(memberID string, checkedIn bool) error {
	_, err := s.sb.Update("event_team_members", "id=eq."+store.Q(memberID),
		map[string]any{"checked_in": checkedIn})
	return err
}

// ListTeams returns an event's teams with their rosters attached.
func (s *Service) ListTeams(eventID string) ([]model.EventTeam, error) {
	rows, err := s.sb.Select("event_teams",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=name")
	if err != nil {
		return nil, err
	}
	teams := make([]model.EventTeam, 0, len(rows))
	ids := make([]string, 0, len(rows))
	idx := map[string]int{}
	for _, r := range rows {
		t := mapEventTeam(r)
		idx[t.ID] = len(teams)
		teams = append(teams, t)
		ids = append(ids, t.ID)
	}
	if len(ids) == 0 {
		return teams, nil
	}
	mrows, err := s.sb.Select("event_team_members",
		"team_id=in.("+joinIDs(ids)+")&select=*,player:players!player_id(phone,dupr_id,dupr_rating)&order=gender,full_name")
	if err != nil {
		return nil, err
	}
	for _, mr := range mrows {
		mem := mapTeamMember(mr)
		if i, ok := idx[mem.TeamID]; ok {
			teams[i].Members = append(teams[i].Members, mem)
		}
	}
	return teams, nil
}

// teamLineup splits a team's roster into its first two men + two women — the
// fixed 4-player MLP lineup. Errors if the roster can't field a line.
func teamLineup(members []model.TeamMember) (men, women []string, err error) {
	for _, m := range members {
		pid := ""
		if m.PlayerID != nil {
			pid = *m.PlayerID
		}
		if pid == "" {
			continue
		}
		if m.Gender == "M" {
			men = append(men, pid)
		} else if m.Gender == "F" {
			women = append(women, pid)
		}
	}
	if len(men) < 2 || len(women) < 2 {
		return nil, nil, fmt.Errorf("a team needs at least 2 men and 2 women (has %d men, %d women)", len(men), len(women))
	}
	return men[:2], women[:2], nil
}

// GenerateTeamTies builds a single round-robin of TIES among the event's teams
// (grouped by bracket), each tie's 4 lines pre-filled with the standard 4-player
// lineup. It refuses to clobber once any tie has a result.
func (s *Service) GenerateTeamTies(eventID string) (int, error) {
	teams, err := s.ListTeams(eventID)
	if err != nil {
		return 0, err
	}
	if len(teams) < 2 {
		return 0, errors.New("need at least 2 teams to generate a schedule")
	}
	// Pre-validate every roster can field a lineup so we never half-build.
	for _, t := range teams {
		if _, _, err := teamLineup(t.Members); err != nil {
			return 0, fmt.Errorf("%s: %w", t.Name, err)
		}
	}
	// Refuse if any tie already has a winner (results exist).
	existing, err := s.sb.Select("team_ties",
		"event_id=eq."+store.Q(eventID)+"&winner_team_id=not.is.null&select=id&limit=1")
	if err != nil {
		return 0, err
	}
	if len(existing) > 0 {
		return 0, fmt.Errorf("%w: this event already has team results", ErrScheduleHasResults)
	}
	// Clear any prior (unplayed) ties + their lines, then rebuild.
	old, err := s.sb.Select("team_ties", "event_id=eq."+store.Q(eventID)+"&select=id")
	if err != nil {
		return 0, err
	}
	for _, o := range old {
		if id := asStr(o, "id"); id != "" {
			_ = s.sb.Delete("team_ties", "id=eq."+store.Q(id)) // lines cascade
		}
	}

	// Put the lines in the event's division so the division-filtered Game tab
	// shows them (the create/edit form always makes at least an "Open" division;
	// a team event uses a single one). Bracket-less otherwise.
	bracketID := ""
	if bks, berr := s.GetBrackets(eventID); berr == nil && len(bks) > 0 {
		bracketID = bks[0].ID
	}

	// Courts for conflict-free line placement (assigned directly at creation —
	// the registration-based spreadCourts doesn't handle these lines).
	courtByNum, err := s.courtIDsByNumber(eventID)
	if err != nil {
		return 0, err
	}
	courtNums := make([]int, 0, len(courtByNum))
	for n := range courtByNum {
		courtNums = append(courtNums, n)
	}
	sort.Ints(courtNums)

	// A single round-robin over all teams: each round pairs every team once, so a
	// team never plays two ties at the same time.
	count := 0
	for r, round := range roundRobinRounds(len(teams)) {
		for ti, pair := range round {
			if err := s.createTie(eventID, bracketID, teams[pair[0]], teams[pair[1]], r, ti, courtNums, courtByNum); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// roundRobinRounds returns the rounds of a single round-robin over n teams (the
// circle method): each round pairs every team once; an odd n sits one out via a
// phantom bye. Returns rounds of [i,j] team-index pairs.
func roundRobinRounds(n int) [][][2]int {
	if n < 2 {
		return nil
	}
	m, bye := n, -1
	if n%2 == 1 {
		m, bye = n+1, n
	}
	idx := make([]int, m)
	for i := range idx {
		idx[i] = i
	}
	rounds := make([][][2]int, 0, m-1)
	for r := 0; r < m-1; r++ {
		var round [][2]int
		for i := 0; i < m/2; i++ {
			a, b := idx[i], idx[m-1-i]
			if a != bye && b != bye {
				round = append(round, [2]int{a, b})
			}
		}
		rounds = append(rounds, round)
		// Rotate: fix idx[0], shift the rest clockwise by one.
		last := idx[m-1]
		copy(idx[2:], idx[1:m-1])
		idx[1] = last
	}
	return rounds
}

// createTie writes one tie + its 4 lines (team A = match team 1). Lines are
// placed CONFLICT-FREE at creation: wd+md run together (disjoint players), then
// mx1+mx2. Round r occupies time-slots r*2 (wd,md) and r*2+1 (mx1,mx2); tie ti in
// the round uses courts [ti*2] and [ti*2+1].
func (s *Service) createTie(eventID, bracketID string, a, b model.EventTeam, r, ti int, courtNums []int, courtByNum map[int]string) error {
	aMen, aWomen, err := teamLineup(a.Members)
	if err != nil {
		return fmt.Errorf("%s: %w", a.Name, err)
	}
	bMen, bWomen, err := teamLineup(b.Members)
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name, err)
	}
	courtA, courtB := "", ""
	if nc := len(courtNums); nc > 0 {
		courtA = courtByNum[courtNums[(ti*2)%nc]]
		courtB = courtByNum[courtNums[(ti*2+1)%nc]]
	}
	slotA, slotB := r*2, r*2+1

	tieRow := map[string]any{
		"event_id":   eventID,
		"stage":      "pool",
		"team_a_id":  a.ID,
		"team_b_id":  b.ID,
		"status":     "scheduled",
		"play_order": slotA,
	}
	if bracketID != "" {
		tieRow["bracket_id"] = bracketID
	}
	tieOut, err := s.sb.Insert("team_ties", []map[string]any{tieRow})
	if err != nil {
		return err
	}
	if len(tieOut) == 0 {
		return errors.New("tie insert returned no row")
	}
	tieID := asStr(tieOut[0], "id")

	// {line, side-A lineup, side-B lineup, court, time-slot}.
	type lineSpec struct {
		lt     string
		t1, t2 []string
		court  string
		slot   int
	}
	for _, sp := range []lineSpec{
		{"wd", aWomen, bWomen, courtA, slotA},
		{"md", aMen, bMen, courtB, slotA},
		{"mx1", []string{aMen[0], aWomen[0]}, []string{bMen[0], bWomen[0]}, courtA, slotB},
		{"mx2", []string{aMen[1], aWomen[1]}, []string{bMen[1], bWomen[1]}, courtB, slotB},
	} {
		if err := s.createTieLine(eventID, bracketID, tieID, sp.lt, sp.slot, sp.court, sp.t1, sp.t2); err != nil {
			return err
		}
	}
	return nil
}

// createTieLinesFor writes a tie's 4 regulation lines (team A = match side 1):
// women's + men's share slotA; the two mixed share slotB. Used by the playoff,
// where courts/slots are assigned differently than the pool round-robin.
func (s *Service) createTieLinesFor(eventID, bracketID, tieID string, a, b model.EventTeam, courtA, courtB string, slotA, slotB int) error {
	aMen, aWomen, err := teamLineup(a.Members)
	if err != nil {
		return fmt.Errorf("%s: %w", a.Name, err)
	}
	bMen, bWomen, err := teamLineup(b.Members)
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name, err)
	}
	type lineSpec struct {
		lt     string
		t1, t2 []string
		court  string
		slot   int
	}
	for _, sp := range []lineSpec{
		{"wd", aWomen, bWomen, courtA, slotA},
		{"md", aMen, bMen, courtB, slotA},
		{"mx1", []string{aMen[0], aWomen[0]}, []string{bMen[0], bWomen[0]}, courtA, slotB},
		{"mx2", []string{aMen[1], aWomen[1]}, []string{bMen[1], bWomen[1]}, courtB, slotB},
	} {
		if err := s.createTieLine(eventID, bracketID, tieID, sp.lt, sp.slot, sp.court, sp.t1, sp.t2); err != nil {
			return err
		}
	}
	return nil
}

func sortedCourtNums(courtByNum map[int]string) []int {
	nums := make([]int, 0, len(courtByNum))
	for n := range courtByNum {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums
}

// seedPairs returns the round-1 seed matchups (0-based seed indices) for a
// single-elim bracket of the top k seeds.
func seedPairs(k int) [][2]int {
	switch k {
	case 8:
		return [][2]int{{0, 7}, {3, 4}, {1, 6}, {2, 5}}
	case 4:
		return [][2]int{{0, 3}, {1, 2}}
	default:
		return [][2]int{{0, 1}}
	}
}

// ErrPoolIncomplete is returned when a playoff is requested before every pool tie
// has a winner.
var ErrPoolIncomplete = errors.New("finish every pool match before generating the playoff")

// GeneratePlayoff seeds a single-elimination playoff from the pool standings: the
// top 4 teams (top 2 if fewer than 4) play a seeded first round; later rounds are
// created automatically as winners advance (maybeAdvancePlayoffRound). No new
// columns — playoff ties are stage="playoff" with play_order = 1000 + round*100 + slot.
func (s *Service) GeneratePlayoff(eventID string) (int, error) {
	ties, err := s.ListTies(eventID)
	if err != nil {
		return 0, err
	}
	var pool, playoff []model.TeamTie
	for _, t := range ties {
		if t.Stage == "playoff" {
			playoff = append(playoff, t)
		} else {
			pool = append(pool, t)
		}
	}
	if len(pool) == 0 {
		return 0, errors.New("generate the pool schedule first")
	}
	for _, t := range pool {
		if t.WinnerTeamID == nil {
			return 0, ErrPoolIncomplete
		}
	}
	for _, t := range playoff {
		if t.WinnerTeamID != nil {
			return 0, errors.New("the playoff already has results")
		}
	}
	for _, t := range playoff {
		_ = s.sb.Delete("team_ties", "id=eq."+store.Q(t.ID)) // lines cascade
	}

	seeds, err := s.TeamEventStandings(eventID)
	if err != nil {
		return 0, err
	}
	n := len(seeds)
	if n < 2 {
		return 0, errors.New("need at least 2 teams for a playoff")
	}
	k := 4
	if n < 4 {
		k = 2
	}

	bracketID := ""
	if bks, berr := s.GetBrackets(eventID); berr == nil && len(bks) > 0 {
		bracketID = bks[0].ID
	}
	teams, err := s.ListTeams(eventID)
	if err != nil {
		return 0, err
	}
	byID := map[string]model.EventTeam{}
	for _, t := range teams {
		byID[t.ID] = t
	}
	courtByNum, err := s.courtIDsByNumber(eventID)
	if err != nil {
		return 0, err
	}
	courtNums := sortedCourtNums(courtByNum)

	count := 0
	for slot, pr := range seedPairs(k) {
		if pr[0] >= n || pr[1] >= n {
			continue
		}
		a := byID[seeds[pr[0]].TeamID]
		b := byID[seeds[pr[1]].TeamID]
		playOrder := 1000 + 100 + slot // round 1
		if err := s.createPlayoffTie(eventID, bracketID, a, b, playOrder, slot, courtNums, courtByNum); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// createPlayoffTie writes one playoff tie (stage="playoff") + its 4 lines.
func (s *Service) createPlayoffTie(eventID, bracketID string, a, b model.EventTeam, playOrder, slot int, courtNums []int, courtByNum map[int]string) error {
	tieRow := map[string]any{
		"event_id":   eventID,
		"stage":      "playoff",
		"team_a_id":  a.ID,
		"team_b_id":  b.ID,
		"status":     "scheduled",
		"play_order": playOrder,
	}
	if bracketID != "" {
		tieRow["bracket_id"] = bracketID
	}
	out, err := s.sb.Insert("team_ties", []map[string]any{tieRow})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return errors.New("playoff tie insert returned no row")
	}
	tieID := asStr(out[0], "id")
	courtA, courtB := "", ""
	if nc := len(courtNums); nc > 0 {
		courtA = courtByNum[courtNums[(slot*2)%nc]]
		courtB = courtByNum[courtNums[(slot*2+1)%nc]]
	}
	return s.createTieLinesFor(eventID, bracketID, tieID, a, b, courtA, courtB, playOrder*2, playOrder*2+1)
}

// maybeAdvancePlayoffRound runs when a playoff tie finishes: once every tie in the
// current (highest) round has a winner, it pairs the winners in seed order to
// create the next round. A single-tie round is the final — nothing to do.
func (s *Service) maybeAdvancePlayoffRound(eventID string) error {
	rows, err := s.sb.Select("team_ties",
		"event_id=eq."+store.Q(eventID)+"&stage=eq.playoff&select=id,winner_team_id,play_order,bracket_id&order=play_order")
	if err != nil || len(rows) == 0 {
		return err
	}
	byRound := map[int][]map[string]any{}
	maxRound := 0
	for _, r := range rows {
		rd := (asInt(r, "play_order") - 1000) / 100
		byRound[rd] = append(byRound[rd], r)
		if rd > maxRound {
			maxRound = rd
		}
	}
	cur := byRound[maxRound]
	if len(cur) <= 1 {
		return nil // the final — champion decided
	}
	winners := make([]string, 0, len(cur))
	for _, t := range cur {
		w := asStr(t, "winner_team_id")
		if w == "" {
			return nil // round not complete yet
		}
		winners = append(winners, w)
	}
	bracketID := asStr(cur[0], "bracket_id")
	teams, err := s.ListTeams(eventID)
	if err != nil {
		return err
	}
	byID := map[string]model.EventTeam{}
	for _, t := range teams {
		byID[t.ID] = t
	}
	courtByNum, err := s.courtIDsByNumber(eventID)
	if err != nil {
		return err
	}
	courtNums := sortedCourtNums(courtByNum)
	for i := 0; i+1 < len(winners); i += 2 {
		slot := i / 2
		playOrder := 1000 + (maxRound+1)*100 + slot
		if err := s.createPlayoffTie(eventID, bracketID, byID[winners[i]], byID[winners[i+1]], playOrder, slot, courtNums, courtByNum); err != nil {
			return err
		}
	}
	return nil
}

// createTieLine inserts one line as a matches row + its 2-v-2 participants,
// placed on courtID at playOrder (courtID "" = unplaced).
func (s *Service) createTieLine(eventID, bracketID, tieID, lineType string, playOrder int, courtID string, team1, team2 []string) error {
	row := map[string]any{
		"event_id":   eventID,
		"stage":      "pool",
		"status":     "scheduled",
		"tie_id":     tieID,
		"line_type":  lineType,
		"play_order": playOrder,
	}
	if bracketID != "" {
		row["bracket_id"] = bracketID
	}
	if courtID != "" {
		row["court_id"] = courtID
	}
	out, err := s.sb.Insert("matches", []map[string]any{row})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return errors.New("line insert returned no row")
	}
	matchID := asStr(out[0], "id")
	parts := make([]map[string]any, 0, len(team1)+len(team2))
	for _, pid := range team1 {
		if pid != "" {
			parts = append(parts, map[string]any{"match_id": matchID, "player_id": pid, "team": 1})
		}
	}
	for _, pid := range team2 {
		if pid != "" {
			parts = append(parts, map[string]any{"match_id": matchID, "player_id": pid, "team": 2})
		}
	}
	if len(parts) > 0 {
		if _, err := s.sb.Upsert("match_participants", "match_id,player_id", parts); err != nil {
			return err
		}
	}
	return nil
}

// rollupTie re-evaluates a tie after one of its lines is scored: it counts
// regulation lines won by each side, lazily spawns a decider on a 2-2 split, and
// sets the tie winner + status once decided. winning_team 1 == team A, 2 == B.
func (s *Service) rollupTie(tieID string) error {
	tie, err := s.sb.SelectOne("team_ties",
		"id=eq."+store.Q(tieID)+"&select=id,event_id,bracket_id,team_a_id,team_b_id,stage,play_order")
	if err != nil {
		return err
	}
	if tie == nil {
		return nil
	}
	lines, err := s.sb.Select("matches",
		"tie_id=eq."+store.Q(tieID)+"&select=id,line_type,status,winning_team")
	if err != nil {
		return err
	}
	var aWins, bWins int
	var decider map[string]any
	for _, ln := range lines {
		lt := asStr(ln, "line_type")
		if lt == "dec" {
			decider = ln
			continue
		}
		if asStr(ln, "status") == "completed" {
			switch asInt(ln, "winning_team") {
			case 1:
				aWins++
			case 2:
				bWins++
			}
		}
	}

	// Still playing the four regulation lines. Count only ATTRIBUTED wins so an
	// unattributed completed line can never finish a tie early.
	if aWins+bWins < len(tieLineOrder) {
		return s.setTieState(tieID, aWins+bWins > 0, "")
	}

	// All four in. A clean majority decides it.
	if aWins != bWins {
		winnerTeam := 1
		if bWins > aWins {
			winnerTeam = 2
		}
		return s.finishTie(tie, winnerTeam)
	}

	// 2-2 → decider. Spawn one (mixed) if absent; otherwise wait for / read it.
	if decider == nil {
		if err := s.spawnDecider(tie); err != nil {
			return err
		}
		return s.setTieState(tieID, true, "")
	}
	if asStr(decider, "status") != "completed" {
		return s.setTieState(tieID, true, "")
	}
	return s.finishTie(tie, asInt(decider, "winning_team"))
}

// spawnDecider creates the lazy DreamBreaker line on a 2-2 split: the MLP singles
// rotation — 4 players (rotating every 4 points) to 21, win by 2. Per the rules
// the DreamBreaker is ALWAYS 4 players, even for Premier (the captain picks 4 of
// any gender from the 6-player roster); we default to 2M+2W. Recorded as the
// line's participants; placed on the first court.
func (s *Service) spawnDecider(tie map[string]any) error {
	eventID := asStr(tie, "event_id")
	bracketID := asStr(tie, "bracket_id")
	tieID := asStr(tie, "id")
	a, err := s.ListTeams(eventID)
	if err != nil {
		return err
	}
	byID := map[string][]model.TeamMember{}
	for _, t := range a {
		byID[t.ID] = t.Members
	}
	// The DreamBreaker fields 4 players regardless of roster size (2M+2W default).
	aMen, aWomen, err := teamLineup(byID[asStr(tie, "team_a_id")])
	if err != nil {
		return err
	}
	bMen, bWomen, err := teamLineup(byID[asStr(tie, "team_b_id")])
	if err != nil {
		return err
	}
	// Place the DreamBreaker on the first court, just after this tie's regulation
	// slots, so it appears on the schedule the moment a tie reaches 2-2.
	court := ""
	if cb, cerr := s.courtIDsByNumber(eventID); cerr == nil && len(cb) > 0 {
		nums := make([]int, 0, len(cb))
		for n := range cb {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		court = cb[nums[0]]
	}
	slot := asInt(tie, "play_order") + 100
	// The DreamBreaker is a SINGLES rotation: all four players of each team take
	// turns (swapping every 4 points on court). Record them all as the line's
	// participants; the running score is kept like any game (to 21, win by 2).
	return s.createTieLine(eventID, bracketID, tieID, "dec", slot, court,
		append(append([]string{}, aMen...), aWomen...),
		append(append([]string{}, bMen...), bWomen...))
}

func (s *Service) setTieState(tieID string, started bool, _ string) error {
	status := "scheduled"
	if started {
		status = "in_progress"
	}
	_, err := s.sb.Update("team_ties", "id=eq."+store.Q(tieID),
		map[string]any{"status": status, "winner_team_id": nil})
	return err
}

func (s *Service) finishTie(tie map[string]any, winnerTeam int) error {
	winnerID := asStr(tie, "team_a_id")
	if winnerTeam == 2 {
		winnerID = asStr(tie, "team_b_id")
	}
	if _, err := s.sb.Update("team_ties", "id=eq."+store.Q(asStr(tie, "id")),
		map[string]any{"status": "completed", "winner_team_id": winnerID}); err != nil {
		return err
	}
	// Playoff ties grow the bracket: once a round completes, winners pair up.
	if asStr(tie, "stage") == "playoff" {
		return s.maybeAdvancePlayoffRound(asStr(tie, "event_id"))
	}
	return nil
}

// ListTies returns an event's ties with their lines' results attached.
func (s *Service) ListTies(eventID string) ([]model.TeamTie, error) {
	rows, err := s.sb.Select("team_ties",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=play_order")
	if err != nil {
		return nil, err
	}
	ties := make([]model.TeamTie, 0, len(rows))
	idx := map[string]int{}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		t := model.TeamTie{
			ID:      asStr(r, "id"),
			EventID: asStr(r, "event_id"),
			Stage:   asStr(r, "stage"),
			TeamAID: asStr(r, "team_a_id"),
			TeamBID: asStr(r, "team_b_id"),
			Status:  asStr(r, "status"),
		}
		if t.Stage == "playoff" {
			t.Round = (asInt(r, "play_order") - 1000) / 100
		}
		if b := asStr(r, "bracket_id"); b != "" {
			t.BracketID = &b
		}
		if w := asStr(r, "winner_team_id"); w != "" {
			t.WinnerTeamID = &w
		}
		idx[t.ID] = len(ties)
		ties = append(ties, t)
		ids = append(ids, t.ID)
	}
	if len(ids) == 0 {
		return ties, nil
	}
	lrows, err := s.sb.Select("matches",
		"tie_id=in.("+joinIDs(ids)+")&select=id,tie_id,line_type,status,team1_score,team2_score,winning_team,participants:match_participants(team,player_id)&order=play_order")
	if err != nil {
		return nil, err
	}
	for _, lr := range lrows {
		ln := model.TieLine{
			MatchID:      asStr(lr, "id"),
			LineType:     asStr(lr, "line_type"),
			Status:       asStr(lr, "status"),
			WinningTeam:  asInt(lr, "winning_team"),
			Team1Players: []string{},
			Team2Players: []string{},
		}
		if v, ok := lr["team1_score"]; ok && v != nil {
			t1 := asInt(lr, "team1_score")
			ln.Team1Score = &t1
		}
		if v, ok := lr["team2_score"]; ok && v != nil {
			t2 := asInt(lr, "team2_score")
			ln.Team2Score = &t2
		}
		if ps, ok := lr["participants"].([]any); ok {
			for _, p := range ps {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				pid := asStr(pm, "player_id")
				if pid == "" {
					continue
				}
				if asInt(pm, "team") == 1 {
					ln.Team1Players = append(ln.Team1Players, pid)
				} else {
					ln.Team2Players = append(ln.Team2Players, pid)
				}
			}
		}
		if i, ok := idx[asStr(lr, "tie_id")]; ok {
			ties[i].Lines = append(ties[i].Lines, ln)
		}
	}
	return ties, nil
}

// lineGenderReq is how many men + women a line needs per side.
func lineGenderReq(lineType string) (men, women int, ok bool) {
	switch lineType {
	case "wd":
		return 0, 2, true
	case "md":
		return 2, 0, true
	case "mx1", "mx2":
		return 1, 1, true
	}
	return 0, 0, false
}

// teamGenderMap returns a team roster's player_id -> gender.
func (s *Service) teamGenderMap(teamID string) (map[string]string, error) {
	rows, err := s.sb.Select("event_team_members",
		"team_id=eq."+store.Q(teamID)+"&select=player_id,gender")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		if pid := asStr(r, "player_id"); pid != "" {
			out[pid] = asStr(r, "gender")
		}
	}
	return out, nil
}

// validateLineup checks one side's players against a line's gender + count rule.
func validateLineup(lineType string, ids []string, roster map[string]string) error {
	men, women, ok := lineGenderReq(lineType)
	if !ok {
		return fmt.Errorf("can't set a lineup for a %q line", lineType)
	}
	if len(ids) != men+women {
		return fmt.Errorf("this line needs %d player(s) per team", men+women)
	}
	gotM, gotF := 0, 0
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return errors.New("a player can't be listed twice on a line")
		}
		seen[id] = true
		g, in := roster[id]
		if !in {
			return errors.New("a selected player isn't on that team")
		}
		switch g {
		case "M":
			gotM++
		case "F":
			gotF++
		}
	}
	if gotM != men || gotF != women {
		return fmt.Errorf("this line needs %dM + %dW (got %dM + %dW)", men, women, gotM, gotF)
	}
	return nil
}

// SetLineLineup replaces the players on one tie line (each side from its own
// team's roster; gender + count enforced per the line type). Blocked once the
// line is scored.
func (s *Service) SetLineLineup(matchID string, team1, team2 []string) error {
	m, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=line_type,tie_id,status")
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	if asStr(m, "status") == "completed" {
		return fmt.Errorf("%w: this line is already scored", ErrScheduleHasResults)
	}
	lt := asStr(m, "line_type")
	tieID := asStr(m, "tie_id")
	if tieID == "" {
		return errors.New("not a team tie line")
	}
	if lt == "dec" {
		return errors.New("the DreamBreaker uses the whole roster — no lineup to set")
	}
	tie, err := s.sb.SelectOne("team_ties",
		"id=eq."+store.Q(tieID)+"&select=team_a_id,team_b_id")
	if err != nil {
		return err
	}
	if tie == nil {
		return ErrNotFound
	}
	rosterA, err := s.teamGenderMap(asStr(tie, "team_a_id"))
	if err != nil {
		return err
	}
	rosterB, err := s.teamGenderMap(asStr(tie, "team_b_id"))
	if err != nil {
		return err
	}
	if err := validateLineup(lt, team1, rosterA); err != nil {
		return err
	}
	if err := validateLineup(lt, team2, rosterB); err != nil {
		return err
	}
	if err := s.sb.Delete("match_participants", "match_id=eq."+store.Q(matchID)); err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(team1)+len(team2))
	for _, pid := range team1 {
		rows = append(rows, map[string]any{"match_id": matchID, "player_id": pid, "team": 1})
	}
	for _, pid := range team2 {
		rows = append(rows, map[string]any{"match_id": matchID, "player_id": pid, "team": 2})
	}
	if len(rows) > 0 {
		_, err = s.sb.Insert("match_participants", rows)
	}
	return err
}

// TeamEventStandings tallies each team's ties/lines/points from completed ties,
// ordered by ties won, then lines won, then point differential.
func (s *Service) TeamEventStandings(eventID string) ([]model.TeamEventStanding, error) {
	teams, err := s.ListTeams(eventID)
	if err != nil {
		return nil, err
	}
	ties, err := s.ListTies(eventID)
	if err != nil {
		return nil, err
	}
	st := map[string]*model.TeamEventStanding{}
	for _, t := range teams {
		st[t.ID] = &model.TeamEventStanding{TeamID: t.ID, Name: t.Name}
	}
	for _, tie := range ties {
		if tie.Stage != "pool" {
			continue // pool standings only; the playoff is a separate bracket
		}
		a, b := st[tie.TeamAID], st[tie.TeamBID]
		if a == nil || b == nil {
			continue
		}
		for _, ln := range tie.Lines {
			// Only the 4 regulation lines feed lines-won + point diff; the decider
			// only decides the tie winner (via winner_team_id below).
			if ln.Status != "completed" || ln.LineType == "dec" {
				continue
			}
			if ln.Team1Score != nil && ln.Team2Score != nil {
				a.PointsFor += *ln.Team1Score
				a.PointsAgainst += *ln.Team2Score
				b.PointsFor += *ln.Team2Score
				b.PointsAgainst += *ln.Team1Score
			}
			switch ln.WinningTeam {
			case 1:
				a.LinesWon++
				b.LinesLost++
			case 2:
				b.LinesWon++
				a.LinesLost++
			}
		}
		if tie.WinnerTeamID != nil {
			if *tie.WinnerTeamID == tie.TeamAID {
				a.TiesWon++
				b.TiesLost++
			} else if *tie.WinnerTeamID == tie.TeamBID {
				b.TiesWon++
				a.TiesLost++
			}
		}
	}
	out := make([]model.TeamEventStanding, 0, len(st))
	for _, v := range st {
		out = append(out, *v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TiesWon != out[j].TiesWon {
			return out[i].TiesWon > out[j].TiesWon
		}
		if out[i].LinesWon != out[j].LinesWon {
			return out[i].LinesWon > out[j].LinesWon
		}
		di := out[i].PointsFor - out[i].PointsAgainst
		dj := out[j].PointsFor - out[j].PointsAgainst
		return di > dj
	})
	return out, nil
}

// joinIDs builds a PostgREST in-list body from raw ids.
func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}
