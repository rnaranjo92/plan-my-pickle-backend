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
	payload := map[string]any{
		"owner_id":          ownerID,
		"name":              req.Name,
		"description":       orNull(req.Description),
		"league_type":       leagueType,
		"day_type":          dayType,
		"sanctioned":        req.Sanctioned,
		"cash_prize":        req.CashPrize,
		"cash_prize_amount": fOrNull(req.CashPrizeAmount),
	}
	// `listed` ships in add_league_listed.sql — only written (when opting in) once
	// the probe confirms the column exists, so create never breaks pre-migration.
	if req.Listed && s.columnReady("leagues", "listed") {
		payload["listed"] = true
	}
	// Ladder rule config (0068 columns) — only for ladder leagues, and only when
	// the columns exist so create stays safe pre-migration.
	if leagueType == "ladder" && req.Ladder != nil && s.columnReady("leagues", "ladder_reorder_model") {
		for k, v := range ladderConfigColumns(*req.Ladder) {
			payload[k] = v
		}
	}
	rows, err := s.sb.Insert("leagues", payload)
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

// SetLeagueListed opts a league into (or out of) public discovery. Owner-only.
// No-op-safe pre-migration: returns a clear error until the `listed` column exists.
func (s *Service) SetLeagueListed(leagueID, ownerID string, listed bool) error {
	if ownerID == "" {
		return ErrForbidden
	}
	row, err := s.sb.SelectOne("leagues", "id=eq."+store.Q(leagueID)+"&select=owner_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "owner_id") != ownerID {
		return ErrForbidden
	}
	if !s.columnReady("leagues", "listed") {
		return errors.New("public league listing isn't available yet")
	}
	_, err = s.sb.Update("leagues", "id=eq."+store.Q(leagueID),
		map[string]any{"listed": listed})
	return err
}

// PublicLeagues returns every publicly-listed, non-demo league with its city/state
// DERIVED from its events (sessions). Best-effort: a missing `listed` column
// (pre-migration) or any error yields nil so the SEO hubs just show nothing.
// Leagues with no geocoded events are skipped (they can't be placed on a hub).
func (s *Service) PublicLeagues() ([]model.PublicLeague, error) {
	rows, err := s.sb.SelectAll("leagues",
		"listed=eq.true&select=id,name,league_type,sanctioned&limit=2000")
	if err != nil {
		return nil, nil // pre-migration / error → treat as none
	}
	type meta struct {
		name, ltype string
		sanctioned  bool
	}
	m := map[string]meta{}
	var ids []string
	for _, r := range rows {
		id, name := asStr(r, "id"), asStr(r, "name")
		if id == "" || publicFeedTestName.MatchString(name) {
			continue
		}
		m[id] = meta{name: name, ltype: asStr(r, "league_type"), sanctioned: asBool(r, "sanctioned")}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	evs, err := s.sb.SelectAll("events",
		"league_id="+store.In(ids)+"&select=league_id,county,state,starts_at&limit=5000")
	if err != nil {
		return nil, nil
	}
	type geo struct {
		county, state, next string
		count               int
	}
	g := map[string]*geo{}
	for _, e := range evs {
		lid := asStr(e, "league_id")
		if lid == "" {
			continue
		}
		gg := g[lid]
		if gg == nil {
			gg = &geo{}
			g[lid] = gg
		}
		gg.count++
		if gg.county == "" {
			if c := asStr(e, "county"); c != "" {
				gg.county, gg.state = c, asStr(e, "state")
			}
		}
		if sa := asStr(e, "starts_at"); sa != "" && (gg.next == "" || sa < gg.next) {
			gg.next = sa
		}
	}
	out := make([]model.PublicLeague, 0, len(ids))
	for _, id := range ids {
		gg := g[id]
		if gg == nil || gg.county == "" {
			continue
		}
		md := m[id]
		out = append(out, model.PublicLeague{
			ID: id, Name: md.name, LeagueType: md.ltype, Sanctioned: md.sanctioned,
			County: gg.county, State: gg.state, SessionCount: gg.count, NextDate: gg.next,
		})
	}
	return out, nil
}

// PublicLeagueByID returns a single listed, non-demo league (with derived geo)
// plus its non-demo events (sessions) for the per-league SEO page. ErrNotFound
// if the league isn't public.
func (s *Service) PublicLeagueByID(id string) (model.PublicLeague, []model.Event, error) {
	row, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(id)+"&listed=eq.true&select=id,name,league_type,sanctioned,description")
	if err != nil {
		return model.PublicLeague{}, nil, err
	}
	if row == nil || publicFeedTestName.MatchString(asStr(row, "name")) {
		return model.PublicLeague{}, nil, ErrNotFound
	}
	lg := model.PublicLeague{
		ID: id, Name: asStr(row, "name"), LeagueType: asStr(row, "league_type"),
		Sanctioned: asBool(row, "sanctioned"), Description: asStr(row, "description"),
	}
	rows, err := s.sb.SelectAll("events",
		"league_id=eq."+store.Q(id)+"&select=*&order=starts_at.asc.nullslast&limit=500")
	sessions := make([]model.Event, 0)
	if err == nil {
		for _, r := range rows {
			e := mapEvent(r)
			if publicFeedTestName.MatchString(e.Name) {
				continue
			}
			if lg.County == "" && e.County != "" {
				lg.County, lg.State = e.County, e.State
			}
			sessions = append(sessions, e)
		}
	}
	lg.SessionCount = len(sessions)
	return lg, sessions, nil
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
	pids := store.In(pidList)

	// (a) Registered for an event that belongs to a league. Two steps (mirrors
	// MyEvents): the caller's registrations → their event ids → the events that
	// have a non-null league_id → those leagues.
	regs, err := s.sb.Select("registrations",
		"player_id="+pids+"&select=event_id")
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
			"id="+store.In(ids)+"&league_id=not.is.null&select=league_id")
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
			"player_id="+pids+"&select=league_bracket_id")
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
			"id="+store.In(bids)+"&select=league_id")
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
			"id="+store.In(missing)+"&select=*")
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
	if err := s.attachLeagueSessionDates(out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachLeagueSessionDates fills FirstSessionAt / LastSessionAt on each league
// from its sessions (events), in ONE batched query, so the home screen can
// group leagues by lifecycle without a per-league read. Best-effort shape:
// first = earliest starts_at; last = latest ends_at (falling back to starts_at).
func (s *Service) attachLeagueSessionDates(leagues []model.League) error {
	if len(leagues) == 0 {
		return nil
	}
	ids := make([]string, len(leagues))
	for i, l := range leagues {
		ids[i] = l.ID
	}
	rows, err := s.sb.Select("events",
		"league_id="+store.In(ids)+"&select=league_id,starts_at,ends_at")
	if err != nil {
		return err
	}
	type span struct{ first, last string }
	byLeague := map[string]*span{}
	for _, r := range rows {
		lid := asStr(r, "league_id")
		if lid == "" {
			continue
		}
		start := asStr(r, "starts_at")
		end := asStr(r, "ends_at")
		if end == "" {
			end = start // single-day session: end == start
		}
		sp := byLeague[lid]
		if sp == nil {
			sp = &span{first: start, last: end}
			byLeague[lid] = sp
		}
		// RFC3339 UTC strings compare lexically in time order.
		if start != "" && (sp.first == "" || start < sp.first) {
			sp.first = start
		}
		if end != "" && (sp.last == "" || end > sp.last) {
			sp.last = end
		}
	}
	for i := range leagues {
		sp := byLeague[leagues[i].ID]
		if sp == nil {
			continue
		}
		if sp.first != "" {
			f := sp.first
			leagues[i].FirstSessionAt = &f
		}
		if sp.last != "" {
			l := sp.last
			leagues[i].LastSessionAt = &l
		}
	}
	return nil
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
			"event_id="+store.In(ids)+"&select=event_id"); rerr == nil {
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
		// doesn't blank the whole league (skip it, keep aggregating the rest).
		st, serr := s.Standings(eid, "", true)
		if serr != nil {
			continue
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

// CopyRoster registers every player from a previous session into a target
// event — the league "season roster" move (same crew, new week, one tap).
// The route enforces ownership of the TARGET; the source is verified here so
// a caller can't siphon another organizer's roster. Players already in the
// target are skipped; divisions carry over by (case-insensitive) name match.
func (s *Service) CopyRoster(targetEventID, fromEventID, callerID string) (added, skipped int, err error) {
	if targetEventID == fromEventID {
		return 0, 0, errors.New("source and target are the same event")
	}
	srcOwner, err := s.OwnerOf("event", fromEventID)
	if err != nil {
		return 0, 0, err
	}
	if srcOwner == "" || srcOwner != callerID {
		return 0, 0, ErrForbidden
	}
	srcRegs, err := s.sb.SelectAll("registrations",
		"event_id=eq."+store.Q(fromEventID)+"&select=player_id,bracket_id,partner_id,partner_name")
	if err != nil {
		return 0, 0, err
	}
	// Fail loudly if we can't read the target's current roster — silently
	// treating it as empty would defeat the duplicate-skip guard and re-register
	// everyone.
	existing := map[string]bool{}
	exRows, err := s.sb.SelectAll("registrations",
		"event_id=eq."+store.Q(targetEventID)+"&select=player_id")
	if err != nil {
		return 0, 0, err
	}
	for _, r := range exRows {
		existing[asStr(r, "player_id")] = true
	}
	// Division mapping by name: source bracket_id -> name -> target bracket id.
	srcName := map[string]string{}
	if bks, err := s.GetBrackets(fromEventID); err == nil {
		for _, b := range bks {
			srcName[b.ID] = strings.ToLower(strings.TrimSpace(b.Name))
		}
	}
	tgtByName := map[string]string{}
	if bks, err := s.GetBrackets(targetEventID); err == nil {
		for _, b := range bks {
			tgtByName[strings.ToLower(strings.TrimSpace(b.Name))] = b.ID
		}
	}
	rows := []map[string]any{}
	for _, r := range srcRegs {
		pid := asStr(r, "player_id")
		if pid == "" || existing[pid] {
			skipped++
			continue
		}
		existing[pid] = true // a doubles pair shares players across rows
		row := map[string]any{
			"event_id":       targetEventID,
			"player_id":      pid,
			"check_in_token": newID(),
		}
		if bid := tgtByName[srcName[asStr(r, "bracket_id")]]; bid != "" {
			row["bracket_id"] = bid
		}
		// Carry doubles pairing across sessions: partner_id (a player id, valid
		// once that partner is also copied — both sides are in this loop) and the
		// free-text partner_name for unregistered partners.
		if p := asStr(r, "partner_id"); p != "" {
			row["partner_id"] = p
		}
		if pn := asStr(r, "partner_name"); pn != "" {
			row["partner_name"] = pn
		}
		rows = append(rows, row)
		added++
	}
	if len(rows) > 0 {
		if _, err := s.sb.Insert("registrations", rows); err != nil {
			return 0, 0, err
		}
	}
	return added, skipped, nil
}
