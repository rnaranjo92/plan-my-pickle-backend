package service

import (
	"errors"
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// CreateLeague creates an owner-scoped league (season/recurring play). Returns
// the new league's id.
func (s *Service) CreateLeague(ownerID, name, description string) (string, error) {
	if strings.TrimSpace(ownerID) == "" {
		return "", errors.New("an owner is required")
	}
	if strings.TrimSpace(name) == "" {
		return "", errors.New("name is required")
	}
	rows, err := s.sb.Insert("leagues", map[string]any{
		"owner_id":    ownerID,
		"name":        name,
		"description": orNull(description),
	})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("league insert returned no row")
	}
	return asStr(rows[0], "id"), nil
}

// ListLeagues returns the leagues OWNED by ownerID, newest first. An empty
// ownerID (anonymous caller) returns nothing.
func (s *Service) ListLeagues(ownerID string) ([]model.League, error) {
	if ownerID == "" {
		return []model.League{}, nil
	}
	rows, err := s.sb.Select("leagues",
		"owner_id=eq."+store.Q(ownerID)+"&select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.League, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapLeague(r))
	}
	return out, nil
}

// GetLeague returns a league plus its sessions (events), ordered by start date
// (events without a start date sort last, then by creation).
func (s *Service) GetLeague(id string) (model.LeagueDetail, error) {
	row, err := s.sb.SelectOne("leagues", "id=eq."+store.Q(id)+"&select=*")
	if err != nil {
		return model.LeagueDetail{}, err
	}
	if row == nil {
		return model.LeagueDetail{}, ErrNotFound
	}
	detail := model.LeagueDetail{League: mapLeague(row)}

	// nullsfirst=false keeps date-less sessions at the bottom; created_at breaks
	// ties so the order is stable.
	evRows, err := s.sb.Select("events",
		"league_id=eq."+store.Q(id)+"&select=*&order=starts_at.asc.nullslast,created_at.asc")
	if err != nil {
		return model.LeagueDetail{}, err
	}
	events := make([]model.Event, 0, len(evRows))
	for _, r := range evRows {
		events = append(events, mapEvent(r))
	}
	// Best-effort registered counts for the session cards (mirrors ListEvents).
	if len(events) > 0 {
		ids := make([]string, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		if regs, rerr := s.sb.Select("registrations",
			"event_id=in.("+strings.Join(ids, ",")+")&select=event_id"); rerr == nil {
			counts := make(map[string]int, len(events))
			for _, r := range regs {
				counts[asStr(r, "event_id")]++
			}
			for i := range events {
				events[i].RegisteredCount = counts[events[i].ID]
			}
		}
	}
	detail.Events = events
	return detail, nil
}

// AddEventToLeague links an existing event into a league. The caller must own
// BOTH the league and the event (verified by the HTTP layer). Returns
// ErrNotFound if the event is missing.
func (s *Service) AddEventToLeague(leagueID, eventID string) error {
	rows, err := s.sb.Update("events",
		"id=eq."+store.Q(eventID),
		map[string]any{"league_id": leagueID})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveEventFromLeague unlinks an event from a league. It only clears the link
// when the event currently belongs to THIS league, so a stale request can't
// detach an event that was meanwhile moved elsewhere.
func (s *Service) RemoveEventFromLeague(leagueID, eventID string) error {
	rows, err := s.sb.Update("events",
		"id=eq."+store.Q(eventID)+"&league_id=eq."+store.Q(leagueID),
		map[string]any{"league_id": nil})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrNotFound
	}
	return nil
}

// LeagueStandings aggregates each player's GP/W/L/points across ALL of the
// league's events' COMPLETED matches: it reuses the per-event standings
// computation (event-wide, all divisions) and sums every stat per player,
// keyed by player id. The result is sorted by the same USAP-style record order
// (wins, then losses, then point differential, then points allowed/scored).
func (s *Service) LeagueStandings(leagueID string) ([]model.Standing, error) {
	evRows, err := s.sb.Select("events",
		"league_id=eq."+store.Q(leagueID)+"&select=id")
	if err != nil {
		return nil, err
	}

	agg := map[string]*model.Standing{}
	order := []string{} // first-seen order, for a stable sort base
	for _, ev := range evRows {
		eid := asStr(ev, "id")
		if eid == "" {
			continue
		}
		// Event-wide standings (bracketID empty) by wins — the same per-event
		// computation the dashboard uses. Best-effort per event so one bad event
		// doesn't blank the whole league.
		st, serr := s.Standings(eid, "", true)
		if serr != nil {
			return nil, serr
		}
		for _, row := range st {
			cur, ok := agg[row.PlayerID]
			if !ok {
				cur = &model.Standing{PlayerID: row.PlayerID, FullName: row.FullName}
				agg[row.PlayerID] = cur
				order = append(order, row.PlayerID)
			}
			// A later event may carry a fresher display name; prefer a non-empty one.
			if row.FullName != "" {
				cur.FullName = row.FullName
			}
			cur.GamesPlayed += row.GamesPlayed
			cur.Wins += row.Wins
			cur.Losses += row.Losses
			cur.PointsFor += row.PointsFor
			cur.PointsAgainst += row.PointsAgainst
		}
	}

	out := make([]model.Standing, 0, len(order))
	for _, pid := range order {
		s := agg[pid]
		s.PointDiff = s.PointsFor - s.PointsAgainst
		out = append(out, *s)
	}
	// USAP-style record order (no head-to-head across events — that's per-event).
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		if a.Losses != b.Losses {
			return a.Losses < b.Losses
		}
		if a.PointDiff != b.PointDiff {
			return a.PointDiff > b.PointDiff
		}
		if a.PointsAgainst != b.PointsAgainst {
			return a.PointsAgainst < b.PointsAgainst
		}
		return a.PointsFor > b.PointsFor
	})
	return out, nil
}
