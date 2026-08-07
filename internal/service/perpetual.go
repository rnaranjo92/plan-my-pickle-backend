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
// Returns the ongoing event id (nil only if creation genuinely fails). The
// perpetual flag is set in place on the passed events.
func (s *Service) ensurePerpetualLeagueEvent(league model.League, brackets []model.LeagueBracket, events []model.Event) *string {
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
	cutoff := time.Now().UTC().Add(-perpetualCheckinWindow).Format(time.RFC3339)
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
func (s *Service) generatePerpetualSession(ev model.Event) (model.ScheduleResult, error) {
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
