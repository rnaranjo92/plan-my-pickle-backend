package service

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// perpetualSeedNames — 25 players for the perpetual-league test seed.
var perpetualSeedNames = []string{
	"Ana Rivera", "Ben Carter", "Cara Lopez", "Dan Patel", "Evan Brooks",
	"Fae Nguyen", "Gus Holt", "Hana Park", "Iris Cole", "Jay Mercer",
	"Kira Bose", "Liam Frost", "Mara Quinn", "Nora Vale", "Omar Reed",
	"Pia Shah", "Quinn Ames", "Ravi Shah", "Sky Tran", "Tom Yorke",
	"Uma Diaz", "Vic Lane", "Wes Kim", "Xena Ford", "Yara Cruz",
}

// lastWeekThursdayAt returns last week's Thursday at hour:min (UTC) — a clearly-
// past session date for the seed. Uses the most recent past Thursday, backing up
// a week if it's only a day or two ago so it reads as "last week".
func lastWeekThursdayAt(hour, min int) time.Time {
	now := time.Now().UTC()
	d := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, time.UTC)
	for i := 0; i < 7 && d.Weekday() != time.Thursday; i++ {
		d = d.AddDate(0, 0, -1)
	}
	if !d.Before(now) || now.Sub(d) < 3*24*time.Hour {
		d = d.AddDate(0, 0, -7)
	}
	return d
}

// ---- League session RSVP ("coming Thursday?") ----

// SessionRsvp is the RSVP poll for a league's upcoming session.
type SessionRsvp struct {
	Enabled     bool   `json:"enabled"`     // false until the migration is run
	SessionDate string `json:"sessionDate"` // YYYY-MM-DD of the next session; "" if none
	Weekday     string `json:"weekday"`     // e.g. "Thursday" for the UI label
	MyStatus    string `json:"myStatus"`    // going | maybe | out | ""
	Going       int    `json:"going"`
	Maybe       int    `json:"maybe"`
	Out         int    `json:"out"`
}

var rsvpStatuses = map[string]bool{"going": true, "maybe": true, "out": true}

// nextSessionDate derives the date (YYYY-MM-DD) of a perpetual league's upcoming
// session from the recurring cadence on starts_at (its weekday), taking the next
// occurrence >= today and honoring skip-until/pause. ok=false when there's no
// upcoming session.
func nextSessionDate(startsAt, skipUntil string, paused bool) (string, time.Weekday, bool) {
	if paused || strings.TrimSpace(startsAt) == "" {
		return "", 0, false
	}
	st, err := time.Parse(time.RFC3339, startsAt)
	if err != nil {
		st, err = time.Parse("2006-01-02T15:04:05", strings.TrimSuffix(startsAt, "Z"))
		if err != nil {
			return "", 0, false
		}
	}
	// Compute the next occurrence in the VENUE's local zone (from starts_at's
	// offset). Using UTC would roll a west-of-UTC league to next week on game
	// night once local evening crosses UTC midnight.
	loc := st.Location()
	wd := st.Weekday()
	now := time.Now().In(loc)
	d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < 7 && d.Weekday() != wd; i++ {
		d = d.AddDate(0, 0, 1)
	}
	if len(skipUntil) >= 10 {
		if su, e := time.ParseInLocation("2006-01-02", skipUntil[:10], loc); e == nil {
			for d.Before(su) {
				d = d.AddDate(0, 0, 7)
			}
		}
	}
	return d.Format("2006-01-02"), wd, true
}

func (s *Service) nextSessionForEvent(eventID string) (string, time.Weekday, bool) {
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+
			"&select=starts_at,recur_skip_until,recur_paused,perpetual")
	if err != nil || row == nil || !asBool(row, "perpetual") {
		return "", 0, false
	}
	return nextSessionDate(asStr(row, "starts_at"),
		asStr(row, "recur_skip_until"), asBool(row, "recur_paused"))
}

// eventRsvpEnabled reports whether the organizer turned the RSVP strip on for
// this event. Opt-in: a missing column (pre-migration) or unset flag = off.
func (s *Service) eventRsvpEnabled(eventID string) bool {
	if !s.columnReady("events", "rsvp_enabled") {
		return false
	}
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=rsvp_enabled")
	if err != nil || row == nil {
		return false
	}
	return asBool(row, "rsvp_enabled")
}

// GetSessionRsvp returns the RSVP poll for the event's upcoming session (counts +
// the caller's own status). Disabled (Enabled=false) until the migration runs
// AND the organizer opts in via the edit form.
func (s *Service) GetSessionRsvp(eventID, userID string) SessionRsvp {
	if !s.columnReady("session_rsvps", "status") || !s.eventRsvpEnabled(eventID) {
		return SessionRsvp{}
	}
	date, wd, ok := s.nextSessionForEvent(eventID)
	out := SessionRsvp{Enabled: true, SessionDate: date, Weekday: wd.String()}
	if !ok {
		return out
	}
	rows, err := s.sb.Select("session_rsvps",
		"event_id=eq."+store.Q(eventID)+"&session_date=eq."+store.Q(date)+
			"&select=user_id,status")
	if err != nil {
		return out
	}
	for _, r := range rows {
		switch asStr(r, "status") {
		case "going":
			out.Going++
		case "maybe":
			out.Maybe++
		case "out":
			out.Out++
		}
		if userID != "" && asStr(r, "user_id") == userID {
			out.MyStatus = asStr(r, "status")
		}
	}
	return out
}

// SetSessionRsvp records the caller's RSVP for the upcoming session and returns
// the refreshed poll. Owner or a registered player only.
func (s *Service) SetSessionRsvp(eventID, userID, status string) (SessionRsvp, error) {
	if !s.columnReady("session_rsvps", "status") || !s.eventRsvpEnabled(eventID) {
		return SessionRsvp{}, errors.New("RSVP isn't enabled for this league")
	}
	if userID == "" {
		return SessionRsvp{}, ErrForbidden
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if !rsvpStatuses[status] {
		return SessionRsvp{}, errors.New("invalid RSVP status")
	}
	// Owner or registered player.
	if userID != s.eventOwnerID(eventID) && !s.IsRegisteredInEvent(eventID, userID) {
		return SessionRsvp{}, ErrForbidden
	}
	date, _, ok := s.nextSessionForEvent(eventID)
	if !ok {
		return SessionRsvp{}, errors.New("no upcoming session to RSVP for")
	}
	if _, err := s.sb.Upsert("session_rsvps", "event_id,user_id,session_date",
		map[string]any{
			"event_id":     eventID,
			"user_id":      userID,
			"session_date": date,
			"status":       status,
			"updated_at":   now(),
		}); err != nil {
		return SessionRsvp{}, err
	}
	return s.GetSessionRsvp(eventID, userID), nil
}

// SeedPerpetualLeagueDemo (QA-only) stands up a perpetual-league test scenario:
// a recurring league running as one ongoing event, 25 players, a COMPLETED
// session dated last Thursday (fully scored), and a fresh ONGOING session today.
// Returns the ongoing event id. Lets Kim exercise the session history, Game
// Standings, and Leaderboard in one tap.
func (s *Service) SeedPerpetualLeagueDemo(ownerID string) (string, error) {
	lastThu := lastWeekThursdayAt(18, 0)

	leagueID, err := s.CreateLeague(ownerID, model.CreateLeagueRequest{
		Name:       "TEST · Never-Ending League",
		LeagueType: "round_robin",
		Location:   "Test Courts",
	})
	if err != nil {
		return "", err
	}
	eid, err := s.CreateEvent(model.CreateEventRequest{
		Name:             "TEST · Never-Ending League",
		Format:           "doubles",
		PartnerMode:      "rotating",
		TournamentFormat: "round_robin",
		ScoringMode:      "wins",
		NumCourts:        4,
		Location:         "Test Courts",
		StartsAt:         lastThu.Format(time.RFC3339),
		Brackets:         []model.BracketInput{{Name: "Open", DivisionType: "open"}},
	}, ownerID)
	if err != nil {
		return "", err
	}
	// Link + flag the event as the league's single ongoing perpetual event.
	if _, err := s.sb.Update("events", "id=eq."+store.Q(eid), map[string]any{
		"league_id": leagueID, "perpetual": true, "recur_interval_days": 0,
	}); err != nil {
		return "", err
	}
	// Make the league recurring so it opens as a perpetual league, anchored on the
	// Thursday cadence.
	lupd := map[string]any{}
	if s.columnReady("leagues", "recurs") {
		lupd["recurs"] = true
		lupd["recur_start_at"] = lastThu.Format(time.RFC3339)
	}
	if s.columnReady("leagues", "recur_event_id") {
		lupd["recur_event_id"] = eid
	}
	if len(lupd) > 0 {
		_, _ = s.sb.Update("leagues", "id=eq."+store.Q(leagueID), lupd)
	}

	if err := s.seedPerpetualScenario(eid, lastThu); err != nil {
		return "", err
	}
	return eid, nil
}

// SeedPerpetualEventSessions seeds the test scenario (25 players, a scored
// last-Thursday session + an ongoing today session) INTO an existing perpetual
// event — the league-page "Seed test data" button. Owner-only; QA-gated at the
// route. Returns the event id.
func (s *Service) SeedPerpetualEventSessions(eventID, ownerID string) (string, error) {
	// Ownership is enforced by the ownerOnly route wrapper.
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return "", err
	}
	if !ev.Perpetual {
		return "", errors.New("this isn't a perpetual (recurring-league) event")
	}
	if err := s.seedPerpetualScenario(eventID, lastWeekThursdayAt(18, 0)); err != nil {
		return "", err
	}
	return eventID, nil
}

// seedPerpetualScenario registers 25 players into a perpetual event, checks them
// in, then lays down a scored session dated lastThu and a fresh ongoing session
// today. Shared by the from-scratch seeder and the league-page button.
func (s *Service) seedPerpetualScenario(eid string, lastThu time.Time) error {
	bks, err := s.GetBrackets(eid)
	if err != nil || len(bks) == 0 {
		return errors.New("seed: no division to register into")
	}
	bid := bks[0].ID
	for i, nm := range perpetualSeedNames {
		_, _ = s.RegisterPlayer(eid, model.RegisterRequest{
			TrustedAdd:      true,
			SkipCoachEnroll: true,
			FullName:        nm,
			Phone:           fmt.Sprintf("+1555%07d", 2000000+i),
			BracketID:       bid,
			SkillLevel:      ratingPtr(3.0 + float64(i%10)*0.05),
		}, "")
	}
	// Check everyone in — a perpetual session builds from checked-in players.
	nowStr := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.sb.Update("registrations", "event_id=eq."+store.Q(eid),
		map[string]any{"checked_in": true, "checked_in_at": nowStr})

	// SESSION 1 (last Thursday): generate, backdate the rounds, score them all.
	if _, err := s.GenerateSchedule(eid, true, true); err != nil {
		return err
	}
	_, _ = s.sb.Update("rounds", "event_id=eq."+store.Q(eid),
		map[string]any{"created_at": lastThu.Format(time.RFC3339)})
	poolIDs, _ := s.listPoolMatchIDs(eid)
	for i, mid := range poolIDs {
		lo := 5 + (i*3)%6 // 5..10, deterministic
		if i%2 == 0 {
			_ = s.applyScore(mid, 11, lo)
		} else {
			_ = s.applyScore(mid, lo, 11)
		}
	}
	_ = s.reconcileRoundStatuses(eid)

	// SESSION 2 (today): append a fresh session for the same checked-in players,
	// left UNSCORED so it reads as ongoing. New rounds default created_at = now.
	if _, err := s.GenerateSchedule(eid, true, true); err != nil {
		return err
	}
	return nil
}

// AutoScoreCurrentSession (QA test flow) fills a deterministic result into every
// UNSCORED game in the event — which, for a perpetual league, is exactly the
// current/ongoing session (past sessions are already scored). Byes (completed,
// no score) are left alone. Returns the number of games scored. Lets Kim watch
// Game Standings + the Leaderboard update without hand-entering 25 scores.
func (s *Service) AutoScoreCurrentSession(eventID string) (int, error) {
	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&stage=eq.pool&select=id,status&order=created_at,id")
	if err != nil {
		return 0, err
	}
	n := 0
	for i, r := range rows {
		if asStr(r, "status") == "completed" { // already scored, or a bye
			continue
		}
		lo := 5 + (i*3)%6 // 5..10, deterministic
		var e error
		if i%2 == 0 {
			e = s.applyScore(asStr(r, "id"), 11, lo)
		} else {
			e = s.applyScore(asStr(r, "id"), lo, 11)
		}
		if e == nil {
			n++
		}
	}
	_ = s.reconcileRoundStatuses(eventID)
	return n, nil
}

// ResetCheckins (QA test flow) clears check-in for every player in the event so
// the coach can re-take attendance — exercises the "check players in before you
// build a session" gate that gates each new session. Returns how many players
// were reset.
func (s *Service) ResetCheckins(eventID string) (int, error) {
	rows, err := s.sb.Update("registrations",
		"event_id=eq."+store.Q(eventID)+"&checked_in=eq.true",
		map[string]any{"checked_in": false, "checked_in_at": nil})
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// RemoveRandomTestPlayer (QA test flow) drops one player from the league to
// simulate a mid-season dropout. Only the registration is removed — the player's
// already-played games stay in History and keep counting on the Leaderboard, so
// this exercises "does a dropout keep their earned results?". Picks the most
// recently added player (so repeated taps peel players off one at a time).
// Returns the removed player's name.
func (s *Service) RemoveRandomTestPlayer(eventID string) (string, error) {
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&order=created_at.desc&limit=1&select=id,full_name")
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("no players left to remove")
	}
	name := asStr(rows[0], "full_name")
	if err := s.sb.Delete("registrations", "id=eq."+store.Q(asStr(rows[0], "id"))); err != nil {
		return "", err
	}
	return name, nil
}

// RollToNextSession (QA test flow) advances a perpetual league by one session
// without waiting a real day: it scores any leftover games in the current
// session, backdates that session into History on a free past day, re-checks-in
// every registered player, and builds a fresh ongoing session for today. Tapping
// it repeatedly walks the league through its full lifecycle. Returns the event id.
func (s *Service) RollToNextSession(eventID string) (string, error) {
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return "", err
	}
	if !ev.Perpetual {
		return "", errors.New("this isn't a perpetual (recurring-league) event")
	}
	// 1. Complete the current session so it reads clean in History.
	if _, err := s.AutoScoreCurrentSession(eventID); err != nil {
		return "", err
	}
	// 2. Push the current (newest) session's rounds back to a free past day.
	if err := s.backdateCurrentSession(eventID); err != nil {
		return "", err
	}
	// 3. Re-check-in everyone — a session builds from checked-in players, and
	//    real sessions reset check-ins each day.
	nowStr := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.sb.Update("registrations", "event_id=eq."+store.Q(eventID),
		map[string]any{"checked_in": true, "checked_in_at": nowStr})
	// 4. Append a fresh session dated today.
	if _, err := s.GenerateSchedule(eventID, true, true); err != nil {
		return "", err
	}
	return eventID, nil
}

// backdateCurrentSession moves the newest session's rounds (the current one) to
// the most recent day BEFORE it that isn't already a session date, so it drops
// into History as a distinct past session. Day buckets are computed in UTC to
// match the seeder's backdating.
func (s *Service) backdateCurrentSession(eventID string) error {
	rows, err := s.sb.Select("rounds",
		"event_id=eq."+store.Q(eventID)+"&select=id,created_at")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	day := func(t time.Time) time.Time {
		u := t.UTC()
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	}
	parse := func(ts string) (time.Time, bool) {
		if t, e := time.Parse(time.RFC3339, ts); e == nil {
			return t, true
		}
		if t, e := time.Parse("2006-01-02T15:04:05", ts); e == nil {
			return t, true
		}
		return time.Time{}, false
	}
	used := map[string]bool{}
	type rd struct {
		id  string
		day time.Time
	}
	var parsed []rd
	var maxDay time.Time
	for _, r := range rows {
		t, ok := parse(asStr(r, "created_at"))
		if !ok {
			continue
		}
		d := day(t)
		used[d.Format("2006-01-02")] = true
		parsed = append(parsed, rd{asStr(r, "id"), d})
		if d.After(maxDay) {
			maxDay = d
		}
	}
	cur := make([]string, 0)
	for _, p := range parsed {
		if p.day.Equal(maxDay) {
			cur = append(cur, p.id)
		}
	}
	if len(cur) == 0 {
		return nil
	}
	target := maxDay.AddDate(0, 0, -1)
	for used[target.Format("2006-01-02")] {
		target = target.AddDate(0, 0, -1)
	}
	newTs := time.Date(target.Year(), target.Month(), target.Day(), 18, 0, 0, 0, time.UTC).
		Format(time.RFC3339)
	_, err = s.sb.Update("rounds", "id="+store.In(cur),
		map[string]any{"created_at": newTs})
	return err
}

// SetRecurringControls updates a perpetual league's schedule controls (owner
// enforced at the route). startsAt reschedules the weekday/time; paused pauses/
// resumes; skipUntil skips sessions up to that date ("" clears it). A nil pointer
// leaves that field unchanged. Returns the refreshed event.
func (s *Service) SetRecurringControls(eventID string, startsAt *string, paused *bool, skipUntil *string) (model.Event, error) {
	upd := map[string]any{}
	if startsAt != nil {
		if st := strings.TrimSpace(*startsAt); st != "" {
			if _, err := time.Parse(time.RFC3339, st); err != nil {
				return model.Event{}, errors.New("start time must be a valid RFC3339 timestamp")
			}
			upd["starts_at"] = st
		}
	}
	if paused != nil {
		upd["recur_paused"] = *paused
	}
	if skipUntil != nil {
		if su := strings.TrimSpace(*skipUntil); su == "" {
			upd["recur_skip_until"] = nil
		} else {
			upd["recur_skip_until"] = su
		}
	}
	if len(upd) == 0 {
		return s.GetEvent(eventID)
	}
	if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID), upd); err != nil {
		return model.Event{}, err
	}
	return s.GetEvent(eventID)
}

// perpetualProvisionMu serializes on-demand creation of a recurring league's
// ongoing event, so the league-detail poll (which calls GetLeague repeatedly)
// can't create duplicate events by racing itself.
var perpetualProvisionMu sync.Mutex

// Perpetual leagues — a recurring/"forever" league runs as ONE ongoing event
// (the normal tournament interface: Feed/Players/Game/Standings…) instead of
// spawning a fresh session event every week. Standings + games accumulate
// season-long; only CHECK-INS reset each day so the organizer re-takes
// attendance each session. This file holds the adoption (mark the league's one
// event perpetual + stop cloning) and the daily check-in reset.

// perpetualCheckinWindow is how long a check-in stays "current". A perpetual
// league plays one session per day, so a check-in older than this is from a
// previous session and is cleared — the roster shows everyone un-checked-in for
// the new day. Comfortably longer than any single session, shorter than a day.
const perpetualCheckinWindow = 12 * time.Hour

// ensurePerpetualLeagueEvent makes a recurring league run as ONE ongoing event
// (the normal tournament interface). It:
//   - returns the existing perpetual event if already adopted;
//   - else ADOPTS the league's current session in place (marks it perpetual,
//     stops cloning, retitles it to the league name);
//   - else (the league has NO event) CREATES the ongoing event from league
//     config + members, so opening the league always lands in the tournament UI
//     instead of an empty session shell.
//
// Returns the ongoing event id (nil only if creation genuinely fails). The
// perpetual flag is set in place on the passed events.
func (s *Service) ensurePerpetualLeagueEvent(league model.League, brackets []model.LeagueBracket, events []model.Event) *string {
	// The perpetual (one ongoing round-robin event) model applies ONLY to a
	// standard league. Ladder / team / flex leagues keep their own structures, so
	// never adopt or provision a perpetual event for them (even if flagged
	// recurring by legacy data).
	if lt := league.LeagueType; lt == "ladder" || lt == "team" || lt == "flex" {
		return nil
	}
	// Already adopted → return the perpetual event.
	for i := range events {
		if events[i].Perpetual {
			id := events[i].ID
			return &id
		}
	}
	// Adopt the "current session": the series head (the event the recurrence was
	// set on), else the earliest event.
	var head *model.Event
	for i := range events {
		e := &events[i]
		if e.RecurIntervalDays > 0 || (e.SeriesID != nil && *e.SeriesID == e.ID) {
			head = e
			break
		}
	}
	if head == nil && len(events) > 0 {
		head = &events[0]
	}
	if head == nil {
		// No event at all → provision the ongoing one so the league IS a
		// tournament from the first open.
		return s.provisionPerpetualLeagueEvent(league, brackets)
	}
	// Mark perpetual and stop the series so no more weekly clones spawn. Clearing
	// the cursor/until is belt-and-suspenders (recur_interval_days=0 already halts
	// the materializer). Also retitle the one ongoing event to the league's name
	// (drop the "— weekly session" clone label) since it IS the league now.
	upd := map[string]any{
		"perpetual":           true,
		"recur_interval_days": 0,
		"recur_until":         nil,
		"series_cursor":       nil,
	}
	if n := strings.TrimSpace(league.Name); n != "" {
		upd["name"] = n
	}
	if _, err := s.sb.Update("events", "id=eq."+store.Q(head.ID), upd); err != nil {
		// Adoption failed — report no ongoing id so the client falls back to the
		// session list rather than pointing at an unconverted event.
		return nil
	}
	head.Perpetual = true
	if n := strings.TrimSpace(league.Name); n != "" {
		head.Name = n
	}
	id := head.ID
	return &id
}

// provisionPerpetualLeagueEvent creates the single ongoing event for a recurring
// league that has none — from the league's divisions + config + members — so the
// league opens straight into the tournament interface. Serialized + re-checked
// under a mutex so the league-detail poll can't create duplicates.
func (s *Service) provisionPerpetualLeagueEvent(league model.League, brackets []model.LeagueBracket) *string {
	perpetualProvisionMu.Lock()
	defer perpetualProvisionMu.Unlock()
	// Re-check under the lock: a concurrent poll may have just created it.
	if rows, err := s.sb.Select("events",
		"league_id=eq."+store.Q(league.ID)+"&select=id,perpetual&limit=1"); err == nil && len(rows) > 0 {
		id := asStr(rows[0], "id")
		if !asBool(rows[0], "perpetual") {
			_, _ = s.sb.Update("events", "id=eq."+store.Q(id),
				map[string]any{"perpetual": true, "recur_interval_days": 0})
		}
		return &id
	}
	// Divisions from the league; CreateEvent defaults to a single "Open" division
	// when none are given.
	binputs := make([]model.BracketInput, 0, len(brackets))
	for _, b := range brackets {
		binputs = append(binputs, model.BracketInput{
			Name:         b.Name,
			DivisionType: b.DivisionType,
			MinRating:    b.MinRating,
			MaxRating:    b.MaxRating,
			MinAge:       b.MinAge,
			MaxAge:       b.MaxAge,
			DuprMin:      b.DuprMin,
			DuprMax:      b.DuprMax,
		})
	}
	startsAt := ""
	if league.RecurStartAt != nil {
		startsAt = *league.RecurStartAt
	}
	courts := 1
	if league.CourtCount != nil && *league.CourtCount > 0 {
		courts = *league.CourtCount
	}
	loc := ""
	if league.Location != nil {
		loc = *league.Location
	}
	// A social recurring league is a rotating-partner doubles round-robin by
	// default (the same shape the Never Ending League used).
	req := model.CreateEventRequest{
		Name:             strings.TrimSpace(league.Name),
		Format:           "doubles",
		PartnerMode:      "rotating",
		TournamentFormat: "round_robin",
		ScoringMode:      "wins",
		NumCourts:        courts,
		StartsAt:         startsAt,
		Location:         loc,
		Brackets:         binputs,
	}
	eventID, err := s.CreateEvent(req, league.OwnerID)
	if err != nil || eventID == "" {
		return nil
	}
	// Link to the league + mark perpetual (so it never clones and rolls check-ins).
	if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID), map[string]any{
		"league_id": league.ID,
		"perpetual": true,
	}); err != nil {
		return nil
	}
	// Court count + auto-roster the league's active members into it.
	s.applyLeagueSessionDefaults(league.ID, eventID)
	// Point the league at its ongoing event for anyone reading recur_event_id.
	if s.columnReady("leagues", "recur_event_id") {
		_, _ = s.sb.Update("leagues", "id=eq."+store.Q(league.ID),
			map[string]any{"recur_event_id": eventID})
	}
	return &eventID
}

// resetStaleCheckins clears check-ins on a perpetual event that are older than
// the session window, so each new day/session starts with everyone un-checked-in
// (standings + games are untouched). Best-effort; a no-op when nothing's stale.
func (s *Service) resetStaleCheckins(eventID string) {
	if eventID == "" {
		return
	}
	// Anchor "stale" to the current session's START boundary — the most recent
	// occurrence of the event's configured session time-of-day — rather than a
	// flat age. This handles sessions less than the window apart (e.g. a Friday
	// evening + Saturday morning block), where a fixed 12h age would leave the
	// prior night's roster checked in. Falls back to the fixed window when the
	// event has no usable start time. The boundary is clamped to be at least
	// [perpetualCheckinWindow/6 ≈ 2h] in the past so it never clears check-ins
	// from a session that only just started.
	cut := time.Now().UTC().Add(-perpetualCheckinWindow)
	if ev, _ := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=starts_at"); ev != nil {
		if st := strings.TrimSpace(asStr(ev, "starts_at")); st != "" {
			if t, err := time.Parse(time.RFC3339, st); err == nil {
				loc := t.Location()
				now := time.Now().In(loc)
				b := time.Date(now.Year(), now.Month(), now.Day(),
					t.Hour(), t.Minute(), 0, 0, loc)
				if b.After(now) {
					b = b.AddDate(0, 0, -1) // today's session hasn't started yet
				}
				floor := time.Now().Add(-perpetualCheckinWindow / 6)
				if b.After(floor) {
					b = floor // don't clear a session that only just began
				}
				cut = b.UTC()
			}
		}
	}
	cutoff := cut.Format(time.RFC3339)
	_, _ = s.sb.Update("registrations",
		"event_id=eq."+store.Q(eventID)+"&checked_in=eq.true&checked_in_at=lt."+store.Q(cutoff),
		map[string]any{"checked_in": false, "checked_in_at": nil})
}

// ResetPerpetualCheckins runs on the recurring ticker: for every perpetual event
// it clears check-ins from a prior session so the flag flips over without anyone
// having to open the event. Best-effort per event.
func (s *Service) ResetPerpetualCheckins() error {
	rows, err := s.sb.SelectAll("events", "perpetual=eq.true&select=id")
	if err != nil {
		return err
	}
	for _, r := range rows {
		s.resetStaleCheckins(asStr(r, "id"))
	}
	return nil
}

// maxRoundNumber returns the highest existing round number for a division (0 if
// none). Used to append a perpetual league's new session AFTER the accumulated
// rounds rather than colliding with round 1.
func (s *Service) maxRoundNumber(eventID, bracketID string) int {
	rows, err := s.sb.Select("rounds",
		"event_id=eq."+store.Q(eventID)+"&bracket_id=eq."+store.Q(bracketID)+
			"&select=round_number&order=round_number.desc&limit=1")
	if err != nil || len(rows) == 0 {
		return 0
	}
	return asInt(rows[0], "round_number")
}

// generatePerpetualSession appends ONE session's round-robin — for the players
// CHECKED IN right now — to a perpetual (recurring-league) event, WITHOUT wiping
// any prior session's rounds/matches/scores. Each division's new rounds are
// numbered after its existing max, so games and standings accumulate season-long.
// Matches are placed on courts inline by the round-robin engine (no global
// re-spread that would disturb completed games).
// DeleteSessionRange removes an entire day's session from a perpetual league —
// every round whose created_at falls in [fromUTC, toUTC) plus all of its
// matches. This is the "coach forgot to skip the day" fix: the games were
// accidentally recorded into history, and because standings are computed live
// from matches, deleting them pulls the session out of the Leaderboard
// automatically — no void bookkeeping. History groups sessions by ROUND
// created_at (the seeder backdates rounds, not matches), so we filter on rounds.
// Returns the number of rounds removed.
func (s *Service) DeleteSessionRange(eventID, fromUTC, toUTC string) (int, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" || strings.TrimSpace(fromUTC) == "" || strings.TrimSpace(toUTC) == "" {
		return 0, errors.New("event id and from/to bounds are required")
	}
	defer s.lockDuprEvent(eventID)() // serialize the dupr_submissions delete vs a flush
	rows, err := s.sb.Select("rounds",
		"event_id=eq."+store.Q(eventID)+
			"&created_at=gte."+store.Q(fromUTC)+
			"&created_at=lt."+store.Q(toUTC)+"&select=id")
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	roundIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		roundIDs = append(roundIDs, asStr(r, "id"))
	}
	// Best-effort DUPR reversal for the matches we're about to delete so we never
	// orphan a real result on official ratings (mirrors the full-event wipe;
	// usually a no-op for coaching/social leagues that don't submit to DUPR).
	if matchRows, err := s.sb.Select("matches",
		"round_id="+store.In(roundIDs)+"&select=id"); err == nil && len(matchRows) > 0 {
		matchIDs := make([]string, 0, len(matchRows))
		for _, m := range matchRows {
			matchIDs = append(matchIDs, asStr(m, "id"))
		}
		if subs, err := s.sb.Select("dupr_submissions",
			"match_id="+store.In(matchIDs)+"&status=eq.submitted&select=match_id,provider_ref,dupr_gen"); err == nil && len(subs) > 0 {
			var pend []map[string]any
			for _, sub := range subs {
				if code := asStr(sub, "provider_ref"); code != "" {
					pend = append(pend, map[string]any{
						"event_id":     eventID,
						"provider_ref": code,
						"identifier":   duprIdentifier(asStr(sub, "match_id"), asInt(sub, "dupr_gen")),
					})
				}
			}
			if len(pend) > 0 {
				if _, err := s.sb.Insert("dupr_pending_deletes", pend); err == nil {
					go func() { _ = s.DrainDuprPendingDeletes() }()
				}
			}
			_ = s.sb.Delete("dupr_submissions", "match_id="+store.In(matchIDs))
		}
	}
	// Matches carry the FK to rounds, so delete them first, then the rounds.
	if err := s.sb.Delete("matches", "round_id="+store.In(roundIDs)); err != nil {
		return 0, err
	}
	if err := s.sb.Delete("rounds", "id="+store.In(roundIDs)); err != nil {
		return 0, err
	}
	return len(roundIDs), nil
}

// clearCurrentUnscoredSession removes the CURRENT session's rounds + matches
// (the newest date bucket) IF none of them are scored — so a substitution made
// after the day's schedule was already built can be re-seeded by a plain "Build
// schedule" (which appends) without duplicating the session. No-op when there's
// no session, or when the current session already has a recorded score (you
// can't re-draw played games — the sub takes effect on the next build instead).
// Returns whether it cleared anything.
func (s *Service) clearCurrentUnscoredSession(eventID string) (bool, error) {
	defer s.lockPerpetualBuild(eventID)() // serialize vs a concurrent Build
	rows, err := s.sb.Select("rounds",
		"event_id=eq."+store.Q(eventID)+"&select=id,created_at")
	if err != nil || len(rows) == 0 {
		return false, err
	}
	// Bucket sessions by the event's LOCAL day (sessions are a local-day concept),
	// derived from the configured start time-of-day — the same approach
	// resetStaleCheckins uses. A pure UTC bucket would mis-classify an evening
	// league's session in any non-UTC timezone and could delete a genuinely-prior
	// LOCAL session.
	loc := time.UTC
	if ev, _ := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=starts_at"); ev != nil {
		if st := strings.TrimSpace(asStr(ev, "starts_at")); st != "" {
			if t, e := time.Parse(time.RFC3339, st); e == nil {
				loc = t.Location()
			}
		}
	}
	day := func(ts string) (time.Time, bool) {
		t, e := time.Parse(time.RFC3339, ts)
		if e != nil {
			if t, e = time.Parse("2006-01-02T15:04:05", ts); e != nil {
				return time.Time{}, false
			}
		}
		l := t.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc), true
	}
	var maxDay time.Time
	for _, r := range rows {
		if d, ok := day(asStr(r, "created_at")); ok && d.After(maxDay) {
			maxDay = d
		}
	}
	// Only clear TODAY's session (in the event's local day). This is a same-day
	// substitute helper — it must NEVER delete an older, prior session that was
	// built but not scored (it's real data the app already shows in History; the
	// sub just applies to the next build). A session that ran across local
	// midnight is still in_progress/scored and caught by the guards below anyway;
	// if truly unscored, not clearing is safe (the Build confirm catches a double).
	nowLocal := time.Now().In(loc)
	todayLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
	if !maxDay.Equal(todayLocal) {
		return false, nil
	}
	cur := make([]string, 0, len(rows))
	for _, r := range rows {
		if d, ok := day(asStr(r, "created_at")); ok && d.Equal(maxDay) {
			cur = append(cur, asStr(r, "id"))
		}
	}
	if len(cur) == 0 {
		return false, nil
	}
	// Refuse to clear a session that has any game already STARTED (in_progress) or
	// SCORED — a sub added mid-play must not wipe live games. Byes (completed with
	// no score) don't block, so a freshly-built-but-unplayed session still clears.
	live, err := s.sb.SelectOne("matches",
		"round_id="+store.In(cur)+"&status=eq.in_progress&select=id")
	if err != nil {
		return false, err
	}
	if live != nil {
		return false, nil
	}
	scored, err := s.sb.SelectOne("matches",
		"round_id="+store.In(cur)+"&status=eq.completed&team1_score=not.is.null&select=id")
	if err != nil {
		return false, err
	}
	if scored != nil {
		return false, nil
	}
	if err := s.sb.Delete("matches", "round_id="+store.In(cur)); err != nil {
		return false, err
	}
	if err := s.sb.Delete("rounds", "id="+store.In(cur)); err != nil {
		return false, err
	}
	return true, nil
}

// cleanupStaleSubstitutes expires one-night substitutes from PRIOR sessions at
// the next session build: a tagged sub who isn't checked in for the new session
// is removed from the roster (their played games + standings line survive, since
// matches carry player_id). For fixed-partner leagues, the benched member they
// stood in for is re-paired with the sub's partner. No-op before the migration
// (column-guarded) or when there are no stale subs.
func (s *Service) cleanupStaleSubstitutes(eventID, partnerMode string) {
	if !s.columnReady("registrations", "is_substitute") {
		return
	}
	subs, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+
			"&is_substitute=eq.true&checked_in=eq.false"+
			"&select=id,player_id,partner_id,substitute_for")
	if err != nil || len(subs) == 0 {
		return
	}
	fixed := partnerMode == "fixed"
	// Map each sub's player_id → the member they stood in for, so a pairing whose
	// partner is ITSELF a departing sub restores to the real member, not the sub
	// (both members of a pair subbed the same night would otherwise cross-wire).
	subFor := map[string]string{}
	for _, sub := range subs {
		subFor[asStr(sub, "player_id")] = asStr(sub, "substitute_for")
	}
	for _, sub := range subs {
		if fixed {
			member := asStr(sub, "substitute_for")
			partner := asStr(sub, "partner_id")
			if real, ok := subFor[partner]; ok && real != "" {
				partner = real // partner was a sub too → restore to its real member
			}
			if member != "" && partner != "" && member != partner {
				if mr, _ := s.sb.SelectOne("registrations",
					"event_id=eq."+store.Q(eventID)+"&player_id=eq."+store.Q(member)+"&select=id"); mr != nil {
					_, _ = s.sb.Update("registrations", "id=eq."+store.Q(asStr(mr, "id")),
						map[string]any{"partner_id": partner})
				}
				if pr, _ := s.sb.SelectOne("registrations",
					"event_id=eq."+store.Q(eventID)+"&player_id=eq."+store.Q(partner)+"&select=id"); pr != nil {
					_, _ = s.sb.Update("registrations", "id=eq."+store.Q(asStr(pr, "id")),
						map[string]any{"partner_id": member})
				}
			}
		}
		_ = s.sb.Delete("registrations", "id=eq."+store.Q(asStr(sub, "id")))
	}
}

func (s *Service) generatePerpetualSession(ev model.Event) (model.ScheduleResult, error) {
	// Serialize appends for this event: the round-offset is a read-modify-write on
	// the max round number, so two concurrent builds would otherwise both read the
	// same offset and append two overlapping round-robins for the same session.
	defer s.lockPerpetualBuild(ev.ID)()
	// Expire last session's one-night substitutes before seeding this session.
	s.cleanupStaleSubstitutes(ev.ID, ev.PartnerMode)
	bks, err := s.GetBrackets(ev.ID)
	if err != nil {
		return model.ScheduleResult{}, err
	}
	courtByNum, err := s.courtIDsByNumber(ev.ID)
	if err != nil {
		return model.ScheduleResult{}, err
	}
	skill, err := s.playerSkills()
	if err != nil {
		return model.ScheduleResult{}, err
	}
	total := 0
	var droppedIDs []string
	for _, b := range bks {
		regs, err := s.bracketRegs(ev.ID, b.ID, true) // checked-in players only
		if err != nil {
			return model.ScheduleResult{}, err
		}
		minPlayers := 2
		if ev.Format == "doubles" {
			minPlayers = 4
		}
		if len(regs) < minPlayers {
			continue // not enough checked in for a game in this division
		}
		droppedIDs = append(droppedIDs, droppedDoublesPlayers(ev, regs)...)
		offset := s.maxRoundNumber(ev.ID, b.ID)
		n, err := s.persistRoundRobin(ev, b.ID, regs, courtByNum, skill, offset)
		if err != nil {
			return model.ScheduleResult{}, err
		}
		total += n
	}
	if _, err := s.sb.Update("events", "id=eq."+store.Q(ev.ID),
		map[string]any{"status": "in_progress"}); err != nil {
		return model.ScheduleResult{}, err
	}
	unscheduled, err := s.playerNamesByID(ev.ID, droppedIDs)
	if err != nil {
		return model.ScheduleResult{}, err
	}
	return model.ScheduleResult{Matches: total, Unscheduled: unscheduled}, nil
}
