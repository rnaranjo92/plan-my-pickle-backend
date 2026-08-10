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
	if loc := strings.TrimSpace(req.Location); loc != "" && s.columnReady("leagues", "location") {
		payload["location"] = loc
	}
	// Ladder rule config (0068 columns) — only for ladder leagues, and only when
	// the columns exist so create stays safe pre-migration.
	if leagueType == "ladder" && req.Ladder != nil && s.columnReady("leagues", "ladder_reorder_model") {
		for k, v := range ladderConfigColumns(*req.Ladder) {
			payload[k] = v
		}
	}
	// Ladder format (0072) — challenge (default) vs rotation. Only for ladder
	// leagues, and only when the column exists (safe pre-migration).
	if leagueType == "ladder" && s.columnReady("leagues", "ladder_format") {
		format := req.LadderFormat
		if format != "rotation" {
			format = "challenge"
		}
		payload["ladder_format"] = format
	}
	// Coach-led league: auto-enroll every registrant as the owner's coaching
	// student. Turning it on makes the OWNER the coach — enroll them as an
	// instructor (idempotent, best-effort) so it works even if they weren't one.
	if req.CoachLed && s.columnReady("leagues", "coach_led") {
		if em := strings.TrimSpace(s.emailOf(ownerID)); em != "" {
			_, _ = s.AddInstructor(strings.ToLower(em), "")
		}
		payload["coach_led"] = true
		payload["coach_id"] = ownerID
	}
	if req.CourtCount != nil && *req.CourtCount > 0 &&
		s.columnReady("leagues", "court_count") {
		payload["court_count"] = *req.CourtCount
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
		"league_id="+store.In(ids)+"&select=league_id,starts_at,ends_at,poster_url,perpetual")
	if err != nil {
		return err
	}
	// posterURL captures a banner to fall back to when the league has none of its
	// own: the "Edit league" form edits the underlying (perpetual) EVENT and saves
	// events.poster_url, while the league card reads leagues.poster_url — so a
	// perpetual league's edited banner would never show without this fallback.
	// Prefer the perpetual (ongoing) event's poster; otherwise any session's.
	type span struct {
		first, last string
		posterURL   string
		posterFixed bool // true once a perpetual event's poster locked it in
	}
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
		if p := asStr(r, "poster_url"); p != "" {
			// A perpetual event's poster wins and is sticky; otherwise take the
			// first non-empty session poster seen.
			if asBool(r, "perpetual") {
				sp.posterURL = p
				sp.posterFixed = true
			} else if !sp.posterFixed && sp.posterURL == "" {
				sp.posterURL = p
			}
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
		// Fallback banner: only when the league has no poster of its own.
		if sp.posterURL != "" &&
			(leagues[i].PosterURL == nil || *leagues[i].PosterURL == "") {
			p := sp.posterURL
			leagues[i].PosterURL = &p
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
	// For ladder leagues (challenge or rotation), the "roster" is the ladder
	// entrants, not event registrations — count them per division so the header
	// shows the real player count.
	if detail.League.LeagueType == "ladder" && len(brackets) > 0 {
		ids := make([]string, len(brackets))
		for i, b := range brackets {
			ids[i] = b.ID
		}
		if ents, eerr := s.sb.Select("ladder_entrants",
			"league_bracket_id="+store.In(ids)+"&select=league_bracket_id"); eerr == nil {
			counts := make(map[string]int, len(brackets))
			for _, r := range ents {
				counts[asStr(r, "league_bracket_id")]++
			}
			for i := range brackets {
				brackets[i].EntrantCount = counts[brackets[i].ID]
			}
		}
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
	// A recurring/"forever" league runs as ONE ongoing event (the normal
	// tournament interface). Adopt it (mark perpetual + stop cloning) and hand the
	// client the event id so it opens the tournament directly instead of a session
	// list. Idempotent once adopted.
	if detail.League.Recurs {
		if eid := s.ensurePerpetualLeagueEvent(detail.League, brackets, events); eid != nil {
			detail.League.OngoingEventID = eid
		}
	}
	return detail, nil
}

// DeleteLeague removes a league and everything under it (owner only): its
// linked events — each cascade-deletes its own matches/rounds/registrations/
// brackets — then the league's divisions, members, videos, and finally the
// league row. Best-effort on the optional sub-tables so a missing one (older
// DB / non-ladder league) doesn't block the delete.
func (s *Service) DeleteLeague(leagueID, ownerID string) error {
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=owner_id")
	if err != nil {
		return err
	}
	if lg == nil {
		return ErrNotFound
	}
	if asStr(lg, "owner_id") != ownerID {
		return ErrForbidden
	}
	// Coach-led: un-enroll the league's students from the coach roster BEFORE we
	// delete the members/events/league (so leagueCoach still resolves). Covers
	// BOTH league_members AND event REGISTRATIONS — a perpetual league's players
	// exist only as registrations on its ongoing event, not as league_members, so
	// members-only cleanup would orphan them on the coach's roster. Best-effort.
	if s.leagueCoach(leagueID) != "" {
		// Collect UNIQUE contacts from members + registrations first, so a player
		// in several of the league's sessions/divisions is unenrolled once (not
		// once per registration — which was O(regs) sequential lookups).
		seenContact := map[string]bool{}
		unenroll := func(email, phone string) {
			key := strings.ToLower(strings.TrimSpace(email)) + "|" + normPhone(phone)
			if key == "|" || seenContact[key] {
				return
			}
			seenContact[key] = true
			s.unenrollLeagueCoachStudent(leagueID, email, phone, true)
		}
		if ms, err := s.sb.Select("league_members",
			"league_id=eq."+store.Q(leagueID)+"&select=email,phone"); err == nil {
			for _, m := range ms {
				unenroll(asStr(m, "email"), asStr(m, "phone"))
			}
		}
		// Registration-based students (perpetual + self-registered): resolve each
		// registrant's contact via the players FK and unenroll.
		if evs, err := s.sb.Select("events",
			"league_id=eq."+store.Q(leagueID)+"&select=id"); err == nil {
			eids := idList(evs, "id")
			if len(eids) > 0 {
				if regs, err := s.sb.Select("registrations",
					"event_id="+store.In(eids)+
						"&select=player:players!player_id(email,phone)"); err == nil {
					for _, r := range regs {
						if p := asMap(r, "player"); p != nil {
							unenroll(asStr(p, "email"), asStr(p, "phone"))
						}
					}
				}
			}
		}
	}
	// The league's events (cascade removes their matches/rounds/registrations).
	// Route through the DUPR-reversal helper so a sanctioned league's already-
	// submitted results don't stay live on official ratings after the delete.
	if rows, err := s.sb.Select("events",
		"league_id=eq."+store.Q(leagueID)+"&select=id"); err == nil {
		for _, r := range rows {
			_ = s.deleteEventWithDuprReversal(asStr(r, "id"))
		}
	}
	// Ladder leagues: clear entrants/challenges under this league's divisions
	// before the divisions themselves (they FK to the bracket).
	if bks, err := s.sb.Select("league_brackets",
		"league_id=eq."+store.Q(leagueID)+"&select=id"); err == nil && len(bks) > 0 {
		ids := make([]string, 0, len(bks))
		for _, b := range bks {
			ids = append(ids, asStr(b, "id"))
		}
		_ = s.sb.Delete("ladder_challenges", "league_bracket_id="+store.In(ids))
		_ = s.sb.Delete("ladder_entrants", "league_bracket_id="+store.In(ids))
	}
	_ = s.sb.Delete("league_brackets", "league_id=eq."+store.Q(leagueID))
	if s.leagueMembersReady() {
		_ = s.sb.Delete("league_members", "league_id=eq."+store.Q(leagueID))
	}
	_ = s.sb.Delete("league_videos", "league_id=eq."+store.Q(leagueID))
	return s.sb.Delete("leagues", "id=eq."+store.Q(leagueID))
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
	// Coach-led league: enroll the session's existing registrants as the coach's
	// students (new registrants get enrolled at RegisterPlayer time). Best-effort.
	if coachID := s.leagueCoach(leagueID); coachID != "" {
		go s.enrollLeaguePlayersForEvent(coachID, eventID)
	}
	// Membership league: seed the session's court count + auto-roster members.
	go s.applyLeagueSessionDefaults(leagueID, eventID)
	return nil
}

func (s *Service) leagueRecurrenceReady() bool {
	return s.columnReady("leagues", "recurs")
}

// SetLeagueSchedule puts a league on a recurring weekly schedule: it creates (or
// re-times) ONE recurring Round-Robin session anchored at startsAt (RFC3339 UTC),
// repeating every 7 days, open-ended ("forever"). The recurrence materializer
// spawns each week's session and auto-rosters the league's members. Owner only.
func (s *Service) SetLeagueSchedule(leagueID, ownerID, startsAt string, courtCount int) (model.Event, error) {
	if !s.leagueRecurrenceReady() {
		return model.Event{}, errors.New("league scheduling isn't available yet")
	}
	startsAt = strings.TrimSpace(startsAt)
	if startsAt == "" {
		return model.Event{}, errors.New("pick a day and time")
	}
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=owner_id,name,recur_event_id,court_count,league_type")
	if err != nil {
		return model.Event{}, err
	}
	if lg == nil {
		return model.Event{}, ErrNotFound
	}
	if asStr(lg, "owner_id") != ownerID {
		return model.Event{}, ErrForbidden
	}
	// The recurring "one ongoing round-robin session" model applies ONLY to a
	// standard league. Ladder / team / flex leagues have their own structures.
	if lt := asStr(lg, "league_type"); lt == "ladder" || lt == "team" || lt == "flex" {
		return model.Event{}, errors.New("a recurring weekly session is only available for round-robin leagues")
	}
	if courtCount <= 0 {
		courtCount = asInt(lg, "court_count")
	}
	if courtCount <= 0 {
		courtCount = 1
	}
	// Re-time the existing recurring session if it's still around.
	if existing := asStr(lg, "recur_event_id"); existing != "" {
		if ev, _ := s.sb.SelectOne("events",
			"id=eq."+store.Q(existing)+"&select=id,perpetual"); ev != nil {
			upd := map[string]any{
				"starts_at":  startsAt,
				"num_courts": courtCount,
			}
			// A perpetual (adopted) league runs as ONE ongoing event — never
			// re-arm weekly cloning (recur_interval_days>0 would spawn orphan
			// session events alongside it). Only a not-yet-adopted event clones.
			if !asBool(ev, "perpetual") {
				upd["recur_interval_days"] = 7
			}
			if _, err := s.sb.Update("events", "id=eq."+store.Q(existing),
				upd); err != nil {
				return model.Event{}, err
			}
			_, _ = s.sb.Update("leagues", "id=eq."+store.Q(leagueID),
				map[string]any{"recurs": true, "recur_start_at": startsAt})
			e, _ := s.GetEvent(existing)
			return e, nil
		}
	}
	// Otherwise create the recurring RR session + link it to the league.
	name := strings.TrimSpace(asStr(lg, "name"))
	if name == "" {
		name = "League"
	}
	eventID, err := s.CreateEvent(model.CreateEventRequest{
		Name:              name + " — weekly session",
		Format:            "doubles",
		TournamentFormat:  "round_robin",
		NumCourts:         courtCount,
		PointsToWin:       11,
		WinBy:             2,
		StartsAt:          startsAt,
		RecurIntervalDays: 7,
		Brackets:          []model.BracketInput{{Name: "Open", DivisionType: "open"}},
	}, ownerID)
	if err != nil {
		return model.Event{}, err
	}
	if err := s.AddEventToLeague(leagueID, eventID); err != nil {
		return model.Event{}, err
	}
	_, _ = s.sb.Update("leagues", "id=eq."+store.Q(leagueID), map[string]any{
		"recurs":         true,
		"recur_event_id": eventID,
		"recur_start_at": startsAt,
	})
	e, _ := s.GetEvent(eventID)
	return e, nil
}

// ClearLeagueSchedule stops a league's recurring schedule — future weekly
// sessions stop spawning; past sessions (and their results) are kept. Owner only.
func (s *Service) ClearLeagueSchedule(leagueID, ownerID string) error {
	if !s.leagueRecurrenceReady() {
		return nil
	}
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=owner_id,recur_event_id")
	if err != nil {
		return err
	}
	if lg == nil {
		return ErrNotFound
	}
	if asStr(lg, "owner_id") != ownerID {
		return ErrForbidden
	}
	if eid := asStr(lg, "recur_event_id"); eid != "" {
		// Stop future materialization; keep the sessions already created.
		_, _ = s.sb.Update("events", "id=eq."+store.Q(eid),
			map[string]any{"recur_interval_days": 0})
	}
	_, err = s.sb.Update("leagues", "id=eq."+store.Q(leagueID), map[string]any{
		"recurs":         false,
		"recur_event_id": nil,
		"recur_start_at": nil,
	})
	return err
}

func (s *Service) leagueVideosReady() bool {
	return s.columnReady("league_videos", "id")
}

// AddLeagueVideo posts a clip to a league's video feed. The caller must be the
// league owner or a participant (a player registered in one of its sessions).
func (s *Service) AddLeagueVideo(leagueID, userID, email, videoURL, title string) (model.LeagueVideo, error) {
	if !s.leagueVideosReady() {
		return model.LeagueVideo{}, errors.New("league videos aren't available yet")
	}
	videoURL = strings.TrimSpace(videoURL)
	if videoURL == "" {
		return model.LeagueVideo{}, errors.New("upload a video first")
	}
	lg, err := s.sb.SelectOne("leagues", "id=eq."+store.Q(leagueID)+"&select=owner_id")
	if err != nil {
		return model.LeagueVideo{}, err
	}
	if lg == nil {
		return model.LeagueVideo{}, ErrNotFound
	}
	if asStr(lg, "owner_id") != userID {
		part, _ := s.IsLeagueParticipant(leagueID, userID, email)
		// A league member can post even before their first session (a participant
		// is derived from having played) — covers the join-once membership model.
		if !part && !s.isActiveLeagueMember(leagueID, userID, email) {
			return model.LeagueVideo{}, ErrForbidden
		}
	}
	name := s.coachingName(userID)
	ins, err := s.sb.Insert("league_videos", map[string]any{
		"league_id":     leagueID,
		"uploaded_by":   userID,
		"uploader_name": orNull(name),
		"video_url":     videoURL,
		"title":         orNull(strings.TrimSpace(title)),
	})
	if err != nil {
		return model.LeagueVideo{}, err
	}
	if len(ins) == 0 {
		return model.LeagueVideo{}, errors.New("could not save the video")
	}
	return mapLeagueVideo(ins[0]), nil
}

// ListLeagueVideos returns a league's video feed, newest first.
func (s *Service) ListLeagueVideos(leagueID string) ([]model.LeagueVideo, error) {
	if !s.leagueVideosReady() {
		return []model.LeagueVideo{}, nil
	}
	rows, err := s.sb.Select("league_videos",
		"league_id=eq."+store.Q(leagueID)+"&order=created_at.desc&limit=500")
	if err != nil {
		return nil, err
	}
	out := make([]model.LeagueVideo, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapLeagueVideo(r))
	}
	return out, nil
}

func mapLeagueVideo(m map[string]any) model.LeagueVideo {
	return model.LeagueVideo{
		ID:           asStr(m, "id"),
		LeagueID:     asStr(m, "league_id"),
		UploadedBy:   asStr(m, "uploaded_by"),
		UploaderName: asStr(m, "uploader_name"),
		VideoURL:     asStr(m, "video_url"),
		Title:        asStr(m, "title"),
		CreatedAt:    asStr(m, "created_at"),
	}
}

// leagueCoach returns the coach id of a coach-led league, or "" if the league
// isn't coach-led (or the column doesn't exist yet).
func (s *Service) leagueCoach(leagueID string) string {
	if leagueID == "" || !s.columnReady("leagues", "coach_led") {
		return ""
	}
	lg, _ := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=coach_led,coach_id")
	if lg == nil || !asBool(lg, "coach_led") {
		return ""
	}
	return asStr(lg, "coach_id")
}

// maybeEnrollLeagueCoachStudent enrolls a single new registrant as a coaching
// student IF their event belongs to a coach-led league. Called off the register
// path — best-effort, so a guest with no email/phone simply isn't enrollable.
func (s *Service) maybeEnrollLeagueCoachStudent(eventID, email, phone, name string) {
	if !s.coachingReady() || eventID == "" {
		return
	}
	ev, _ := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=league_id,name")
	if ev == nil {
		return
	}
	coachID := s.leagueCoach(asStr(ev, "league_id"))
	if coachID == "" {
		return
	}
	student, err := s.AddCoachStudent(coachID, email, phone, name, "", true)
	if err != nil {
		return // already on the roster / no contact info — nothing new to announce
	}
	// Tell the coach a new student joined from the league, so they don't have to
	// discover it by pull-to-refreshing their roster.
	who := strings.TrimSpace(name)
	if who == "" {
		who = strings.TrimSpace(student.StudentName)
	}
	if who == "" {
		who = "A new player"
	}
	body := who + " joined your coaching"
	if evName := strings.TrimSpace(asStr(ev, "name")); evName != "" {
		body += " from " + evName
	}
	s.notifyUser(coachID, "coaching", "", "", body, "coachstudents")
}

// unenrollLeagueCoachStudent removes a player from the league coach's roster when
// they leave the league (or the coach-led league is deleted). No-op unless the
// league is coach-led and the player matches a coach_students row. Best-effort —
// reuses RemoveCoachStudent so sessions/threads tear down cleanly.
// unenrollLeagueCoachStudent drops a player from a coach-led league's coach
// roster. [wholeLeague] distinguishes the caller: true when the ENTIRE league is
// being deleted (its registrations still exist, so the retention check must
// EXCLUDE this league); false when a single registration/member was removed (it
// is already gone, so the check looks across ALL the coach's events — including
// this league's other sessions). Either way, keep the student if they remain in
// any of the coach's other coach-led events.
func (s *Service) unenrollLeagueCoachStudent(leagueID, email, phone string, wholeLeague bool) {
	if !s.coachingReady() {
		return
	}
	coachID := s.leagueCoach(leagueID)
	if coachID == "" {
		return
	}
	e := strings.ToLower(strings.TrimSpace(email))
	np := normPhone(phone)
	var filter string
	if e != "" {
		filter = "coach_id=eq." + store.Q(coachID) + "&student_email=eq." + store.Q(e)
	} else if np != "" {
		filter = "coach_id=eq." + store.Q(coachID) + "&student_phone=eq." + store.Q(np)
	} else {
		return
	}
	sel := "id"
	fromLeagueReady := s.columnReady("coach_students", "from_league")
	if fromLeagueReady {
		sel = "id,from_league"
	}
	row, _ := s.sb.SelectOne("coach_students", filter+"&select="+sel)
	if row == nil {
		return
	}
	// ONLY tear down a row that a league auto-enroll created. A manually-added
	// (or manually-claimed) student shares the same (coach,contact) row; removing
	// it would cascade-delete the coach's own clip/feedback history. When the
	// from_league column isn't migrated yet we can't tell them apart, so we skip
	// cleanup entirely (fail safe — never destroy data).
	if !fromLeagueReady || !asBool(row, "from_league") {
		return
	}
	excludeLeague := ""
	if wholeLeague {
		excludeLeague = leagueID
	}
	if s.playerHasRegUnderCoach(coachID, email, phone, excludeLeague) {
		return
	}
	_ = s.RemoveCoachStudent(coachID, asStr(row, "id"))
}

// idList pulls a non-empty string column out of a set of rows.
func idList(rows []map[string]any, key string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if v := asStr(r, key); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// playerHasRegUnderCoach reports whether the player (matched by email or phone)
// is still registered in ANY coach-led league run by [coachID], optionally
// excluding [excludeLeagueID]. SUBSTITUTE registrations don't count (a one-night
// sub never made them a real student). Best-effort: on any lookup error it
// returns false so the caller falls back to unenrolling (the prior behavior).
func (s *Service) playerHasRegUnderCoach(coachID, email, phone, excludeLeagueID string) bool {
	if !s.columnReady("leagues", "coach_led") {
		return false
	}
	e := strings.ToLower(strings.TrimSpace(email))
	np := normPhone(phone)
	if e == "" && np == "" {
		return false
	}
	// Fail CLOSED: a swallowed query error here used to read as "not registered"
	// → the caller un-enrolls and (for a league-created row) cascade-deletes the
	// player's clips. Since this guards irreversible data loss, any real error
	// must instead KEEP the student (return true). Only a genuine empty result
	// (no rows, no error) means "not registered under this coach".
	//
	// Contacts are stored RAW on players (the app sends "(619) 555-0100" and
	// mixed-case emails), while coach_students holds them normalized — so an
	// exact eq. match missed nearly every phone-only player and any odd-case
	// email, silently taking the delete path. Match case-insensitively on email,
	// and for phone compare NORMALIZED values in Go over a bounded candidate set
	// (last-7-digit prefix filter), mirroring CheckInByPhone.
	pidSet := map[string]bool{}
	if e != "" {
		if prows, perr := s.sb.Select("players",
			"email=ilike."+store.Q(escapeLike(e))+"&select=id"); perr != nil {
			return true
		} else {
			for _, id := range idList(prows, "id") {
				pidSet[id] = true
			}
		}
	}
	if np != "" {
		tail := np
		if len(tail) > 7 {
			tail = tail[len(tail)-7:]
		}
		if prows, perr := s.sb.Select("players",
			"phone=ilike."+store.Q("%"+escapeLike(tail)+"%")+"&select=id,phone"); perr != nil {
			return true
		} else {
			for _, r := range prows {
				if normPhone(asStr(r, "phone")) == np {
					pidSet[asStr(r, "id")] = true
				}
			}
		}
	}
	pids := make([]string, 0, len(pidSet))
	for id := range pidSet {
		pids = append(pids, id)
	}
	if len(pids) == 0 {
		// We could not resolve this contact to ANY player row — but they were
		// just registered, so this is a lookup/normalization miss, not proof
		// they're unregistered. Keep the student (irreversible-delete guard).
		return true
	}
	lq := "coach_id=eq." + store.Q(coachID) + "&coach_led=is.true"
	if excludeLeagueID != "" {
		lq += "&id=neq." + store.Q(excludeLeagueID)
	}
	lrows, lerr := s.sb.Select("leagues", lq+"&select=id")
	if lerr != nil {
		return true
	}
	lids := idList(lrows, "id")
	if len(lids) == 0 {
		return false
	}
	erows, eerr := s.sb.Select("events", "league_id="+store.In(lids)+"&select=id")
	if eerr != nil {
		return true
	}
	eids := idList(erows, "id")
	if len(eids) == 0 {
		return false
	}
	rq := "event_id=" + store.In(eids) + "&player_id=" + store.In(pids)
	if s.columnReady("registrations", "is_substitute") {
		rq += "&is_substitute=is.false"
	}
	reg, rerr := s.sb.SelectOne("registrations", rq+"&select=id")
	if rerr != nil {
		return true
	}
	return reg != nil
}

// enrollLeaguePlayersForEvent enrolls every current registrant of an event as
// the coach's student (used when a session with players is linked to a coach-led
// league). Best-effort; AddCoachStudent dedupes and reactivates.
func (s *Service) enrollLeaguePlayersForEvent(coachID, eventID string) {
	if !s.coachingReady() || coachID == "" || eventID == "" {
		return
	}
	// Never back-enroll one-night substitutes as coaching students (they carry
	// SkipCoachEnroll at register time; the backfill must honor the same rule).
	filter := "event_id=eq." + store.Q(eventID) + "&select=player_id"
	if s.columnReady("registrations", "is_substitute") {
		filter = "event_id=eq." + store.Q(eventID) +
			"&is_substitute=is.false&select=player_id"
	}
	regs, err := s.sb.Select("registrations", filter)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(regs))
	for _, r := range regs {
		if pid := asStr(r, "player_id"); pid != "" {
			ids = append(ids, pid)
		}
	}
	if len(ids) == 0 {
		return
	}
	players, err := s.sb.Select("players",
		"id="+store.In(ids)+"&select=full_name,email,phone")
	if err != nil {
		return
	}
	for _, p := range players {
		_, _ = s.AddCoachStudent(coachID,
			asStr(p, "email"), asStr(p, "phone"), asStr(p, "full_name"), "", true)
	}
}

// SetEventCoachLed toggles coach-led on the event's LEAGUE (owner enforced at the
// route). Enabling requires the owner to be an instructor and back-enrolls every
// current player of the league's events as the coach's students. Disabling stops
// future auto-enroll (existing students are kept — the coach can prune them).
// Returns the refreshed event so the client sees the new coachLed state.
func (s *Service) SetEventCoachLed(eventID, ownerID string, enabled bool) (model.Event, error) {
	if !s.columnReady("leagues", "coach_led") {
		return model.Event{}, errors.New("coaching isn't available yet")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=league_id")
	if err != nil {
		return model.Event{}, err
	}
	if ev == nil {
		return model.Event{}, ErrNotFound
	}
	leagueID := asStr(ev, "league_id")
	if leagueID == "" {
		return model.Event{}, errors.New("this event isn't part of a league")
	}
	lg, _ := s.sb.SelectOne("leagues", "id=eq."+store.Q(leagueID)+"&select=owner_id")
	if lg == nil {
		return model.Event{}, ErrNotFound
	}
	if asStr(lg, "owner_id") != ownerID {
		return model.Event{}, ErrForbidden
	}
	if enabled {
		// Turning coach-led ON makes the OWNER the coach — enroll them as an
		// instructor (idempotent, best-effort) so it never fails on "not an
		// instructor" and the Coach tab / roster become available to them.
		if em := strings.TrimSpace(s.emailOf(ownerID)); em != "" {
			_, _ = s.AddInstructor(strings.ToLower(em), "")
		}
		if _, err := s.sb.Update("leagues", "id=eq."+store.Q(leagueID),
			map[string]any{"coach_led": true, "coach_id": ownerID}); err != nil {
			return model.Event{}, err
		}
		// Back-enroll everyone already registered across the league's events.
		if evs, err := s.sb.Select("events",
			"league_id=eq."+store.Q(leagueID)+"&select=id"); err == nil {
			for _, e := range evs {
				go s.enrollLeaguePlayersForEvent(ownerID, asStr(e, "id"))
			}
		}
	} else if _, err := s.sb.Update("leagues", "id=eq."+store.Q(leagueID),
		map[string]any{"coach_led": false, "coach_id": nil}); err != nil {
		return model.Event{}, err
	}
	return s.GetEvent(eventID)
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
