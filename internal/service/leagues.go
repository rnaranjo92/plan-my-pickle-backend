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
func (s *Service) CreateLeague(ownerID string, req model.CreateLeagueRequest) (string, error) {
	if strings.TrimSpace(ownerID) == "" {
		return "", errors.New("an owner is required")
	}
	if strings.TrimSpace(req.Name) == "" {
		return "", errors.New("name is required")
	}
	// Reject obviously-bad numbers so they can't poison standings / UI math.
	if req.CashPrizeAmount != nil && *req.CashPrizeAmount < 0 {
		return "", errors.New("cashPrizeAmount cannot be negative")
	}
	for _, d := range req.Divisions {
		if d.MinRating != nil && d.MaxRating != nil && *d.MinRating > *d.MaxRating {
			return "", errors.New("a division's minRating cannot exceed its maxRating")
		}
		if d.MinAge != nil && d.MaxAge != nil && *d.MinAge > *d.MaxAge {
			return "", errors.New("a division's minAge cannot exceed its maxAge")
		}
		if d.DuprMin != nil && d.DuprMax != nil && *d.DuprMin > *d.DuprMax {
			return "", errors.New("a division's duprMin cannot exceed its duprMax")
		}
	}
	leagueType := req.LeagueType
	if leagueType == "" {
		leagueType = "round_robin"
	}
	dayType := req.DayType
	if dayType == "" {
		dayType = "multi"
	}
	rows, err := s.sb.Insert("leagues", map[string]any{
		"owner_id":          ownerID,
		"name":              req.Name,
		"description":       orNull(req.Description),
		"league_type":       leagueType,
		"day_type":          dayType,
		"sanctioned":        req.Sanctioned,
		"cash_prize":        req.CashPrize,
		"cash_prize_amount": fOrNull(req.CashPrizeAmount),
	})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("league insert returned no row")
	}
	id := asStr(rows[0], "id")

	// Batch-insert the league's divisions (mirrors event→brackets in
	// CreateEvent): default to a single "Open" division when none supplied, and
	// default an empty division_type to "open".
	divs := req.Divisions
	if len(divs) == 0 {
		divs = []model.LeagueBracketInput{{Name: "Open"}}
	}
	brackets := make([]map[string]any, 0, len(divs))
	for i, d := range divs {
		dt := d.DivisionType
		if dt == "" {
			dt = "open"
		}
		name := d.Name
		if strings.TrimSpace(name) == "" {
			name = "Open"
		}
		brackets = append(brackets, map[string]any{
			"league_id":     id,
			"name":          name,
			"division_type": dt,
			"min_rating":    fOrNull(d.MinRating),
			"max_rating":    fOrNull(d.MaxRating),
			"min_age":       iOrNull(d.MinAge),
			"max_age":       iOrNull(d.MaxAge),
			"dupr_min":      fOrNull(d.DuprMin),
			"dupr_max":      fOrNull(d.DuprMax),
			"sort_order":    i,
		})
	}
	if _, err := s.sb.Insert("league_brackets", brackets); err != nil {
		return "", err
	}
	return id, nil
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

// leagueIDsForUser returns the set of league ids the caller PARTICIPATES in
// (not owns) — the shared "what leagues am I connected to as a player" rule,
// used by both MyLeagues and IsLeagueParticipant so the definition lives in one
// place. A participant is:
//
//   - registered for an event whose league_id is set (reuse the MyEvents
//     registration-matching: caller's player rows → registrations → events with
//     a non-null league_id → their league), and/or
//   - an entrant in a league bracket: a ladder_entrants / teams row whose
//     player_id matches one of the caller's player rows → league_bracket →
//     league.
//
// Returns a deduped set keyed by league id. An empty caller (no player rows)
// yields an empty set.
func (s *Service) leagueIDsForUser(userID, email string) (map[string]bool, error) {
	out := map[string]bool{}
	pidList, err := s.playerIDsForUser(userID, email)
	if err != nil {
		return nil, err
	}
	if len(pidList) == 0 {
		return out, nil
	}
	pids := strings.Join(pidList, ",")

	// (a) Registered for an event that belongs to a league. Two steps (mirrors
	// MyEvents): the caller's registrations → their event ids → the events that
	// have a non-null league_id → those leagues.
	regs, err := s.sb.Select("registrations",
		"player_id=in.("+pids+")&select=event_id")
	if err != nil {
		return nil, err
	}
	evIDs := map[string]bool{}
	for _, r := range regs {
		if eid := asStr(r, "event_id"); eid != "" {
			evIDs[eid] = true
		}
	}
	if len(evIDs) > 0 {
		ids := make([]string, 0, len(evIDs))
		for id := range evIDs {
			ids = append(ids, id)
		}
		evs, err := s.sb.Select("events",
			"id=in.("+strings.Join(ids, ",")+")&league_id=not.is.null&select=league_id")
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if lid := asStr(e, "league_id"); lid != "" {
				out[lid] = true
			}
		}
	}

	// (b) An entrant in a league bracket — a ladder_entrants or teams row whose
	// player_id matches the caller. Resolve each row's league_bracket_id → the
	// owning league via league_brackets.
	bracketIDs := map[string]bool{}
	for _, table := range []string{"ladder_entrants", "teams"} {
		rows, err := s.sb.Select(table,
			"player_id=in.("+pids+")&select=league_bracket_id")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if bid := asStr(r, "league_bracket_id"); bid != "" {
				bracketIDs[bid] = true
			}
		}
	}
	if len(bracketIDs) > 0 {
		bids := make([]string, 0, len(bracketIDs))
		for id := range bracketIDs {
			bids = append(bids, id)
		}
		bks, err := s.sb.Select("league_brackets",
			"id=in.("+strings.Join(bids, ",")+")&select=league_id")
		if err != nil {
			return nil, err
		}
		for _, b := range bks {
			if lid := asStr(b, "league_id"); lid != "" {
				out[lid] = true
			}
		}
	}

	return out, nil
}

// IsLeagueParticipant reports whether the caller participates in a league —
// the gate (alongside ownership) for league READ access. Same participant
// definition as MyLeagues (leagueIDsForUser): registered for one of the
// league's events, or an entrant in one of its brackets.
func (s *Service) IsLeagueParticipant(leagueID, userID, email string) (bool, error) {
	ids, err := s.leagueIDsForUser(userID, email)
	if err != nil {
		return false, err
	}
	return ids[leagueID], nil
}

// MyLeagues returns the DISTINCT leagues the caller is connected to: the ones
// they OWN (owner_id) UNION the ones they PARTICIPATE in (leagueIDsForUser —
// registered for a league's event, or an entrant in a league's bracket). The
// result is the deduped union, newest first.
func (s *Service) MyLeagues(userID, email string) ([]model.League, error) {
	byID := map[string]model.League{}

	// OWNED — reuse the owner-scoped list.
	owned, err := s.ListLeagues(userID)
	if err != nil {
		return nil, err
	}
	for _, l := range owned {
		byID[l.ID] = l
	}

	// PARTICIPANT — the league ids the caller plays in, fetched and merged.
	partIDs, err := s.leagueIDsForUser(userID, email)
	if err != nil {
		return nil, err
	}
	missing := make([]string, 0, len(partIDs))
	for id := range partIDs {
		if _, ok := byID[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		rows, err := s.sb.Select("leagues",
			"id=in.("+strings.Join(missing, ",")+")&select=*")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			l := mapLeague(r)
			byID[l.ID] = l
		}
	}

	out := make([]model.League, 0, len(byID))
	for _, l := range byID {
		out = append(out, l)
	}
	// Newest first (created_at desc), matching ListLeagues' order.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
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

	// Attach the league's divisions (brackets), ordered by sort_order so the
	// detail payload carries them for LeagueDto to read.
	bkRows, err := s.sb.Select("league_brackets",
		"league_id=eq."+store.Q(id)+"&select=*&order=sort_order")
	if err != nil {
		return model.LeagueDetail{}, err
	}
	brackets := make([]model.LeagueBracket, 0, len(bkRows))
	for _, r := range bkRows {
		brackets = append(brackets, mapLeagueBracket(r))
	}
	detail.Brackets = brackets

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
