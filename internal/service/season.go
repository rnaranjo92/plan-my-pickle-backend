package service

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// roundCreatedAt pulls the embedded round.created_at off a match row selected
// with `round:rounds!round_id(created_at)`. Empty when absent.
func roundCreatedAt(r map[string]any) string {
	if rd := asMap(r, "round"); rd != nil {
		return strings.TrimSpace(asStr(rd, "created_at"))
	}
	return ""
}

// seasonStartFor returns the ISO timestamp a perpetual league's CURRENT season
// began, or "" when the event isn't a season-scoped perpetual league (so the
// standings RPC path is used unchanged — every tournament and legacy league).
// Gated on the column existing, so it's zero-cost until the migration runs.
// eventIsLeaguePlay reports whether this event belongs to a league (including a
// perpetual one), which decides the tiebreak order: leagues rank on the point
// differential the table displays, tournaments keep the USAP head-to-head.
//
// A standalone tournament has no league_id, so the default when the lookup fails
// is FALSE — a failed query must not quietly move a sanctioned tournament off
// the USAP criteria.
func (s *Service) eventIsLeaguePlay(eventID string) bool {
	if strings.TrimSpace(eventID) == "" {
		return false
	}
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=league_id")
	if err != nil || row == nil {
		return false
	}
	return strings.TrimSpace(asStr(row, "league_id")) != ""
}

func (s *Service) seasonStartFor(eventID string) string {
	if !s.columnReady("events", "season_started_at") {
		return ""
	}
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=season_started_at,perpetual")
	if err != nil || row == nil || !asBool(row, "perpetual") {
		return ""
	}
	return strings.TrimSpace(asStr(row, "season_started_at"))
}

// standingRowsSince returns the box-score rows for an event. With since=="" it
// uses the pmp_standings RPC (all-time, unchanged). With a season start it
// aggregates completed pool matches in Go, scoped to rounds created on/after the
// season start — so a new season reads as zero without deleting any history.
func (s *Service) standingRowsSince(eventID, bracketID, since string) ([]model.Standing, error) {
	if since == "" {
		payload := map[string]any{"p_event_id": eventID}
		if bracketID != "" {
			payload["p_bracket_id"] = bracketID
		}
		body, err := s.sb.RPC("pmp_standings", payload)
		if err != nil {
			return nil, err
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, err
		}
		out := make([]model.Standing, 0, len(rows))
		for _, r := range rows {
			out = append(out, mapStanding(r))
		}
		return out, nil
	}
	return s.seasonStandingRows(eventID, bracketID, since)
}

// seasonStandingRows aggregates GP/W/L/PF/PA per player from completed pool
// matches whose round was created on/after the season start. Mirrors what the
// pmp_standings RPC produces, minus the SQL — the Go ranking in Standings then
// orders these exactly as it orders the RPC rows.
func (s *Service) seasonStandingRows(eventID, bracketID, since string) ([]model.Standing, error) {
	q := "event_id=eq." + store.Q(eventID) +
		"&stage=eq.pool&status=eq.completed" +
		"&select=team1_score,team2_score,winning_team,counts_for_diff," +
		"round:rounds!round_id(created_at)," +
		"participants:match_participants(team,player:players!player_id(id,full_name))"
	if bracketID != "" {
		q += "&bracket_id=eq." + store.Q(bracketID)
	}
	rows, err := s.sb.SelectAll("matches", q)
	if err != nil {
		return nil, err
	}
	byPlayer := map[string]*model.Standing{}
	for _, r := range rows {
		if since != "" && roundCreatedAt(r) < since {
			continue
		}
		// Mirror the app-wide box-score convention (see the lifetime profile
		// aggregation and gamesPlayedForUser): a bye is a completed match with
		// NULL scores — not a game; a forfeit/walkover fabricates a
		// points_to_win–0 score with counts_for_diff=false — it decides the
		// match but must not inflate games played, points, or differential.
		if asIntPtr(r, "team1_score") == nil || asIntPtr(r, "team2_score") == nil {
			continue
		}
		if cd := r["counts_for_diff"]; cd != nil && cd == false {
			continue
		}
		wt := asInt(r, "winning_team")
		t1, t2 := asInt(r, "team1_score"), asInt(r, "team2_score")
		parts, _ := r["participants"].([]any)
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			team := asInt(pm, "team")
			pl := asMap(pm, "player")
			pid := asStr(pl, "id")
			if pid == "" {
				continue
			}
			st := byPlayer[pid]
			if st == nil {
				st = &model.Standing{PlayerID: pid, FullName: asStr(pl, "full_name")}
				byPlayer[pid] = st
			}
			st.GamesPlayed++
			pf, pa := t2, t1
			if team == 1 {
				pf, pa = t1, t2
			}
			st.PointsFor += pf
			st.PointsAgainst += pa
			if wt == team {
				st.Wins++
			} else if wt == 1 || wt == 2 {
				st.Losses++
			}
		}
	}
	out := make([]model.Standing, 0, len(byPlayer))
	for _, st := range byPlayer {
		st.PointDiff = st.PointsFor - st.PointsAgainst
		out = append(out, *st)
	}
	return out, nil
}

// RollSeason archives the current season's standings to league_season_snapshots
// and starts a fresh season (bumps season_number, resets season_started_at to
// now). Roster, match history, and DUPR submissions are all KEPT — only the live
// leaderboard's scope moves forward. Returns the season number just archived.
func (s *Service) RollSeason(eventID string) (int, error) {
	if !s.columnReady("events", "season_started_at") ||
		!s.columnReady("league_season_snapshots", "season_number") {
		return 0, errors.New("season support isn't enabled yet — run the migration")
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	if !ev.Perpetual {
		return 0, errors.New("only a recurring league can start a new season")
	}
	seasonNum := ev.SeasonNumber
	if seasonNum < 1 {
		seasonNum = 1
	}
	// Freeze the current standings (event-wide, USAP wins order) as the archive.
	st, err := s.Standings(eventID, "", true)
	if err != nil {
		return 0, err
	}
	ended := now()
	snap := map[string]any{
		"event_id":      eventID,
		"season_number": seasonNum,
		"ended_at":      ended,
		"standings":     st, // jsonb — the store marshals the slice to a JSON array
	}
	if strings.TrimSpace(ev.SeasonStartedAt) != "" {
		snap["started_at"] = ev.SeasonStartedAt
	}
	// Upsert (not Insert) on (event_id, season_number): the snapshot write and the
	// events season bump below are two non-transactional HTTP calls. If the bump
	// fails after the snapshot lands, season_number stays at N and a retry re-reads
	// N — a plain Insert would then collide with the unique(event_id, season_number)
	// row and wedge the league forever. Merge-duplicates makes the retry overwrite
	// the same-season snapshot instead, so RollSeason is safely repeatable.
	if _, err := s.sb.Upsert("league_season_snapshots", "event_id,season_number", []map[string]any{snap}); err != nil {
		return 0, err
	}
	if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID), map[string]any{
		"season_number":     seasonNum + 1,
		"season_started_at": ended,
	}); err != nil {
		return 0, err
	}
	return seasonNum, nil
}

// ListSeasonSnapshots returns the archived seasons for an event, newest first
// (metadata only — no standings payload). Empty before the migration runs.
func (s *Service) ListSeasonSnapshots(eventID string) ([]model.SeasonSnapshot, error) {
	if !s.columnReady("league_season_snapshots", "season_number") {
		return []model.SeasonSnapshot{}, nil
	}
	rows, err := s.sb.Select("league_season_snapshots",
		"event_id=eq."+store.Q(eventID)+
			"&select=season_number,started_at,ended_at&order=season_number.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.SeasonSnapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.SeasonSnapshot{
			SeasonNumber: asInt(r, "season_number"),
			StartedAt:    asStr(r, "started_at"),
			EndedAt:      asStr(r, "ended_at"),
		})
	}
	return out, nil
}

// GetSeasonSnapshot returns one archived season's frozen standings.
func (s *Service) GetSeasonSnapshot(eventID string, seasonNumber int) (model.SeasonSnapshot, error) {
	var out model.SeasonSnapshot
	if !s.columnReady("league_season_snapshots", "season_number") {
		return out, ErrNotFound
	}
	row, err := s.sb.SelectOne("league_season_snapshots",
		"event_id=eq."+store.Q(eventID)+
			"&season_number=eq."+store.Q(strconv.Itoa(seasonNumber))+
			"&select=season_number,started_at,ended_at,standings&limit=1")
	if err != nil {
		return out, err
	}
	if row == nil {
		return out, ErrNotFound
	}
	out.SeasonNumber = asInt(row, "season_number")
	out.StartedAt = asStr(row, "started_at")
	out.EndedAt = asStr(row, "ended_at")
	// standings is stored as a jsonb array; re-marshal→unmarshal into typed rows.
	if raw, ok := row["standings"]; ok && raw != nil {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &out.Standings)
		}
	}
	return out, nil
}
