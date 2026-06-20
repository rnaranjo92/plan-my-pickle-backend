// Package service holds PlanMyPickle's business operations: event setup,
// registration, schedule/bracket generation, scoring, and standings.
// Ported from the Flutter app's repository; uses the verified engine package.
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/courts"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/engine"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

type Service struct {
	sb     *store.Client // Supabase REST (the sole data store)
	Pay    gateway.PaymentGateway
	Sms    gateway.SmsGateway
	Dupr   gateway.DuprGateway
	Courts courts.Finder
}

func New() *Service {
	return &Service{
		sb:     store.NewClient(),
		Pay:    gateway.NewMockPayment(),
		Sms:    gateway.NewMockSms(),
		Dupr:   gateway.NewMockDupr(),
		Courts: courts.NewFinder(), // Google Places if PMP_PLACES_KEY set, else OSM
	}
}

// courtCacheTTL bounds how long a cached nearby-courts lookup stays fresh.
const courtCacheTTL = 14 * 24 * time.Hour

// courtCacheKey buckets a lookup to ~110m so nearby searches share a cache row.
func courtCacheKey(lat, lng, radiusKm float64) string {
	if radiusKm <= 0 {
		radiusKm = 25
	}
	// Version prefix invalidates stale cached results when the data shape or
	// source changes (v2: distance-rank; v3: Google Places; v4: rating/category).
	return fmt.Sprintf("v4:%.3f:%.3f:%.1f", lat, lng, radiusKm)
}

// NearbyCourts finds pickleball courts near a point (for the create-event venue
// picker). Results are cached in Supabase (table court_lookups) so repeat
// searches over the same area skip the slow external API. When Supabase isn't
// configured — or any cache op fails — it falls back to a live lookup.
func (s *Service) NearbyCourts(lat, lng, radiusKm float64) ([]courts.Court, error) {
	key := courtCacheKey(lat, lng, radiusKm)

	if s.sb.Ready() {
		if cached, ok := s.cachedCourts(key); ok {
			return cached, nil
		}
	}

	found, err := s.Courts.Nearby(lat, lng, radiusKm)
	if err != nil {
		return nil, err
	}

	// Rank nearest-first and keep the closest 20 (per the courts spec), then
	// reverse-geocode any that are still nameless so the list shows a street or
	// park name instead of "Pickleball court". The enriched result is cached.
	found = courts.Rank(found, lat, lng, 20)
	courts.EnrichLabels(found)

	if s.sb.Ready() {
		s.cacheCourts(key, lat, lng, radiusKm, found)
	}
	return found, nil
}

// cachedCourts returns a fresh cached lookup for key, or ok=false on miss /
// stale / any decode error (the caller then does a live lookup).
func (s *Service) cachedCourts(key string) ([]courts.Court, bool) {
	rows, err := s.sb.Select("court_lookups", "cache_key=eq."+store.Q(key)+"&select=courts,created_at")
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	if ts, ok := rows[0]["created_at"].(string); ok {
		if t, perr := time.Parse(time.RFC3339, ts); perr == nil && time.Since(t) > courtCacheTTL {
			return nil, false
		}
	}
	raw, err := json.Marshal(rows[0]["courts"])
	if err != nil {
		return nil, false
	}
	var out []courts.Court
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// cacheCourts best-effort upserts a lookup result. A cache-write failure is
// logged but never fails the user's request.
func (s *Service) cacheCourts(key string, lat, lng, radiusKm float64, found []courts.Court) {
	if found == nil {
		found = []courts.Court{} // never write jsonb null into a NOT NULL column
	}
	row := map[string]any{
		"cache_key": key,
		"lat":       lat,
		"lng":       lng,
		"radius_km": radiusKm,
		"courts":    found,
	}
	if _, err := s.sb.Upsert("court_lookups", "cache_key", row); err != nil {
		log.Printf("court cache write failed: %v", err)
	}
}

// Geocode resolves a city / address / zip to a point (venue picker search).
func (s *Service) Geocode(query string) (*courts.GeoResult, error) {
	return courts.Geocode(query)
}

func now() string   { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
func newID() string { return uuid.NewString() }

var ErrNotFound = errors.New("not found")

// ErrForbidden means the caller is authenticated but not allowed to act on the
// resource (e.g. deleting someone else's comment when not the event owner).
var ErrForbidden = errors.New("forbidden")

// ErrScheduleHasResults guards against silently wiping recorded scores: a
// re-generate is refused (409) once any match is completed, unless forced.
var ErrScheduleHasResults = errors.New("schedule already has recorded results")

// ------------------------------------------------------------------ events
// CreateEvent inserts an event owned by ownerID (the authenticated organizer).
// ownerID may be empty for internal/demo seeding, leaving the event unowned.
func (s *Service) CreateEvent(req model.CreateEventRequest, ownerID string) (string, error) {
	if strings.TrimSpace(req.Name) == "" {
		return "", errors.New("name is required")
	}
	format := req.Format
	if format == "" {
		format = "doubles"
	}
	partner := req.PartnerMode
	if format == "singles" {
		partner = "na"
	} else if partner == "" {
		partner = "rotating"
	}
	tf := req.TournamentFormat
	if tf == "" {
		tf = "round_robin"
	}
	scoring := req.ScoringMode
	if scoring == "" {
		scoring = "wins"
	}
	courts := req.NumCourts
	if courts < 1 {
		courts = 1
	}
	ptw := req.PointsToWin
	if ptw < 1 {
		ptw = 11
	}
	winBy := req.WinBy
	if winBy < 1 {
		winBy = 2
	}
	bestOf := normalizeBestOf(req.BestOf)
	gameMin := clampGameDuration(req.GameDurationMinutes)

	payload := map[string]any{
		"name":                   req.Name,
		"format":                 format,
		"partner_mode":           partner,
		"tournament_format":      tf,
		"scoring_mode":           scoring,
		"num_courts":             courts,
		"points_to_win":          ptw,
		"win_by":                 winBy,
		"best_of":                bestOf,
		"game_duration_minutes":  gameMin,
		"registration_fee_cents": req.RegistrationFeeCents,
		"currency":               "USD",
		"location":               orNull(req.Location),
		"contact_phone":          orNull(req.ContactPhone),
		"dupr_sanctioned":        req.DuprSanctioned,
		"consolation":            req.Consolation,
		"admin_passcode":         orNull(req.AdminPasscode),
		"owner_id":               orNull(ownerID),
		"venue_name":             orNull(req.VenueName),
		"venue_address":          orNull(req.VenueAddress),
		"venue_phone":            orNull(req.VenuePhone),
		"venue_website":          orNull(req.VenueWebsite),
		"venue_lat":              fOrNull(req.VenueLat),
		"venue_lng":              fOrNull(req.VenueLng),
		"status":                 "open",
	}
	// Only reference starts_at when the organizer set one. The column ships in
	// migration 0012; a date-less create never touches it, so this works before
	// and after the migration is applied.
	if req.StartsAt != "" {
		payload["starts_at"] = req.StartsAt
	}
	// ends_at column ships in migration 0015; only reference it when set.
	if req.EndsAt != "" {
		payload["ends_at"] = req.EndsAt
	}
	// description column ships in migration 0014; only reference it when set so
	// creates work before and after the migration.
	if req.Description != "" {
		payload["description"] = req.Description
	}
	ev, err := s.sb.Insert("events", payload)
	if err != nil {
		return "", err
	}
	if len(ev) == 0 {
		return "", errors.New("event insert returned no row")
	}
	id := asStr(ev[0], "id")

	divs := req.Brackets
	if len(divs) == 0 {
		divs = []model.BracketInput{{Name: "Open"}}
	}
	brackets := make([]map[string]any, 0, len(divs))
	for i, d := range divs {
		brackets = append(brackets, map[string]any{
			"event_id":   id,
			"name":       d.Name,
			"min_rating": fOrNull(d.MinRating),
			"max_rating": fOrNull(d.MaxRating),
			"min_age":    iOrNull(d.MinAge),
			"max_age":    iOrNull(d.MaxAge),
			"sort_order": i,
		})
	}
	if _, err := s.sb.Insert("brackets", brackets); err != nil {
		return "", err
	}
	courtRows := make([]map[string]any, 0, courts)
	for i := 1; i <= courts; i++ {
		courtRows = append(courtRows, map[string]any{
			"event_id":     id,
			"label":        "Court " + strconv.Itoa(i),
			"court_number": i,
		})
	}
	if _, err := s.sb.Insert("courts", courtRows); err != nil {
		return "", err
	}
	return id, nil
}

// ListEvents returns the events OWNED by ownerID (the organizer dashboard list),
// newest first. An empty ownerID (anonymous caller) returns nothing — unowned
// and other-organizers' events are never listed here, so the dashboard only ever
// shows events the caller can actually manage. Spectators/registrants use
// GetEvent (single) instead.
func (s *Service) ListEvents(ownerID string) ([]model.Event, error) {
	if ownerID == "" {
		return []model.Event{}, nil
	}
	rows, err := s.sb.Select("events",
		"owner_id=eq."+store.Q(ownerID)+"&select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapEvent(r))
	}

	// Attach registered-player counts in ONE batched query (idiomatic
	// select-then-tally, no N+1 and no PostgREST count-embed): pull every
	// registration's event_id for these events and group by it.
	if len(out) > 0 {
		ids := make([]string, len(out))
		for i, e := range out {
			ids[i] = e.ID
		}
		// Best-effort: a count failure must not blank the whole dashboard, so on
		// error we leave the counts at 0 and still return the events.
		regs, err := s.sb.Select("registrations",
			"event_id=in.("+strings.Join(ids, ",")+")&select=event_id")
		if err == nil {
			counts := make(map[string]int, len(out))
			for _, r := range regs {
				counts[asStr(r, "event_id")]++
			}
			for i := range out {
				out[i].RegisteredCount = counts[out[i].ID]
			}
		}
	}
	s.attachActivity(out)
	return out, nil
}

// attachActivity fills LiveCount + LastActivity* for a slice of events in two
// batched queries (no N+1). Best-effort: a failure leaves the fields at their
// zero value so the dashboard still renders.
func (s *Service) attachActivity(events []model.Event) {
	if len(events) == 0 {
		return
	}
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	inList := "in.(" + strings.Join(ids, ",") + ")"

	// Newest feed item per event. Rows come back created_at-desc, so the first
	// row seen for an event id is its latest activity.
	if rows, err := s.sb.Select("feed_items",
		"event_id="+inList+"&select=event_id,type,text,created_at&order=created_at.desc"); err == nil {
		latest := make(map[string]map[string]any, len(events))
		for _, r := range rows {
			eid := asStr(r, "event_id")
			if _, ok := latest[eid]; !ok {
				latest[eid] = r
			}
		}
		for i := range events {
			if r, ok := latest[events[i].ID]; ok {
				typ, txt, at := asStr(r, "type"), asStr(r, "text"), asStr(r, "created_at")
				events[i].LastActivityType = &typ
				events[i].LastActivity = &txt
				events[i].LastActivityAt = &at
			}
		}
	}

	// Count of matches currently in progress per event (the "live" pill).
	if rows, err := s.sb.Select("matches",
		"event_id="+inList+"&status=eq.in_progress&select=event_id"); err == nil {
		live := make(map[string]int, len(events))
		for _, r := range rows {
			live[asStr(r, "event_id")]++
		}
		for i := range events {
			events[i].LiveCount = live[events[i].ID]
		}
	}
}

// MyEvents returns the events the user is registered to PLAY in (via a player
// row linked to their account), newest first. Empty if they have no linked
// player or no registrations.
// MyEvents returns the events the caller is registered to PLAY in. A
// registration counts when its player is linked to the account (players.user_id)
// OR its player's email matches the caller's verified account email — so signing
// up with your login email surfaces the event here even if you weren't signed in
// at registration time. (email comes from the verified JWT, so it can't be
// spoofed to claim someone else's registrations.)
func (s *Service) MyEvents(userID, email string) ([]model.Event, error) {
	playerIDs := map[string]bool{}
	if userID != "" {
		pl, err := s.sb.SelectOne("players",
			"user_id=eq."+store.Q(userID)+"&select=id")
		if err != nil {
			return nil, err
		}
		if pl != nil {
			playerIDs[asStr(pl, "id")] = true
		}
	}
	if email != "" {
		// PostgREST ilike uses * as its only wildcard, so a raw email is a safe
		// case-insensitive exact match.
		pls, err := s.sb.Select("players",
			"email=ilike."+store.Q(email)+"&select=id")
		if err != nil {
			return nil, err
		}
		for _, p := range pls {
			playerIDs[asStr(p, "id")] = true
		}
	}
	if len(playerIDs) == 0 {
		return []model.Event{}, nil
	}
	pidList := make([]string, 0, len(playerIDs))
	for id := range playerIDs {
		pidList = append(pidList, id)
	}
	regs, err := s.sb.Select("registrations",
		"player_id=in.("+strings.Join(pidList, ",")+")&select=event_id")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(regs))
	seen := map[string]bool{}
	for _, r := range regs {
		if eid := asStr(r, "event_id"); eid != "" && !seen[eid] {
			seen[eid] = true
			ids = append(ids, eid)
		}
	}
	if len(ids) == 0 {
		return []model.Event{}, nil
	}
	rows, err := s.sb.Select("events",
		"id=in.("+strings.Join(ids, ",")+")&select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapEvent(r))
	}
	s.attachActivity(out)
	return out, nil
}

func (s *Service) GetEvent(id string) (model.Event, error) {
	row, err := s.sb.SelectOne("events", "id=eq."+store.Q(id)+"&select=*")
	if err != nil {
		return model.Event{}, err
	}
	if row == nil {
		return model.Event{}, ErrNotFound
	}
	ev := mapEvent(row)
	// Best-effort registered + checked-in counts (mirrors ListEvents) so the
	// event-detail header shows the real numbers; a count failure must not fail
	// the read — single reads otherwise leave the counts at 0.
	if regs, rerr := s.sb.Select("registrations",
		"event_id=eq."+store.Q(id)+"&select=checked_in"); rerr == nil {
		ev.RegisteredCount = len(regs)
		for _, r := range regs {
			if asBool(r, "checked_in") {
				ev.CheckedInCount++
			}
		}
	}
	return ev, nil
}

// Roster returns the PUBLIC list of players who joined an event — names,
// division, and check-in status only (no phone/email/DUPR) — in join order, so
// players/spectators can see who's playing.
func (s *Service) Roster(eventID string) ([]model.RosterEntry, error) {
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&order=created_at.asc"+
			"&select=checked_in,player:players!player_id(full_name),bracket:brackets!bracket_id(name)")
	if err != nil {
		return nil, err
	}
	out := make([]model.RosterEntry, 0, len(rows))
	for _, r := range rows {
		name := ""
		if p := asMap(r, "player"); p != nil {
			name = strings.TrimSpace(asStr(p, "full_name"))
		}
		if name == "" {
			continue
		}
		div := ""
		if b := asMap(r, "bracket"); b != nil {
			div = asStr(b, "name")
		}
		out = append(out, model.RosterEntry{
			FullName:  name,
			Division:  div,
			CheckedIn: asBool(r, "checked_in"),
		})
	}
	return out, nil
}

// DeleteEvent removes an event and (via ON DELETE CASCADE) all its brackets,
// courts, registrations, payments, rounds, matches, match_participants,
// notifications and DUPR submissions. Players are global and are not deleted.
func (s *Service) DeleteEvent(id string) error {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(id)+"&select=id")
	if err != nil {
		return err
	}
	if ev == nil {
		return ErrNotFound
	}
	return s.sb.Delete("events", "id=eq."+store.Q(id))
}

// DeleteAccount permanently erases a user: the events they organize (FK cascade
// removes those events' brackets, matches, registrations and feed), their player
// profile/registrations in OTHER organizers' events (anonymized + unlinked so
// those events' draws stay valid), and finally their Supabase auth login. Order
// matters: data first, login last, so a mid-way failure leaves an account the
// user can still sign into and retry — never orphaned data with no owner. Idempotent,
// so a retry after a partial failure is safe. Satisfies App Store Guideline 5.1.1(v).
func (s *Service) DeleteAccount(userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return errors.New("not signed in")
	}
	// 1. Their tournaments. Cascade handles everything beneath each event.
	if err := s.sb.Delete("events", "owner_id=eq."+store.Q(userID)); err != nil {
		return err
	}
	// 2. Scrub PII from + unlink their player rows in events they DON'T own, so
	//    those organizers' brackets/matches keep working. Best-effort: a hiccup
	//    here must not block erasing the login (the part Apple checks).
	if _, err := s.sb.Update("players", "user_id=eq."+store.Q(userID), map[string]any{
		"full_name": "Former player",
		"email":     nil,
		"phone":     nil,
		"dupr_id":   nil,
		"user_id":   nil,
	}); err != nil {
		log.Printf("DeleteAccount: anonymize players for %s failed (continuing): %v", userID, err)
	}
	// 3. Erase the login last.
	return s.sb.DeleteAuthUser(userID)
}

// UpdateEvent edits an existing event's metadata (name, description, date,
// venue/location, fee, courts, scoring, DUPR). It deliberately does NOT change
// the structural format / tournament_format / brackets — those are fixed once
// the draw/schedule exists. starts_at + description are only written when set
// (their columns ship in migrations 0012/0014), so editing works before and
// after those migrations.
func (s *Service) UpdateEvent(id string, req model.CreateEventRequest) error {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(id)+"&select=id")
	if err != nil {
		return err
	}
	if ev == nil {
		return ErrNotFound
	}

	ptw := req.PointsToWin
	if ptw <= 0 {
		ptw = 11
	}
	winBy := req.WinBy
	if winBy != 1 {
		winBy = 2
	}
	courts := req.NumCourts
	if courts < 1 {
		courts = 1
	}

	// Note: the structured venue_* columns are intentionally NOT updated here —
	// EventDto doesn't carry them, so the edit form can't round-trip them, and
	// blanking them would wipe the venue. `location` (free text) IS round-tripped.
	upd := map[string]any{
		"name":                   req.Name,
		"num_courts":             courts,
		"points_to_win":          ptw,
		"win_by":                 winBy,
		"best_of":                normalizeBestOf(req.BestOf),
		"game_duration_minutes":  clampGameDuration(req.GameDurationMinutes),
		"registration_fee_cents": req.RegistrationFeeCents,
		"location":               orNull(req.Location),
		"dupr_sanctioned":        req.DuprSanctioned,
		// On edit the form always sends these, so write them unconditionally —
		// an empty value clears the field (orNull → SQL NULL).
		"contact_phone": orNull(req.ContactPhone),
		"starts_at":     orNull(req.StartsAt),
		"ends_at":       orNull(req.EndsAt),
		"description":   orNull(req.Description),
	}
	if _, err = s.sb.Update("events", "id=eq."+store.Q(id), upd); err != nil {
		return err
	}

	// Reconcile the courts table so every lane the board renders (1..num_courts)
	// maps to a real court row. Court rows are otherwise created only at event
	// creation, so RAISING the count here would leave "phantom" lanes that reject
	// a drag (SetMatchCourt looks courts up by court_number). We only ADD missing
	// numbers; LOWERING the count leaves the extra rows in place — harmless, since
	// the board hides out-of-range lanes and they may still hold played matches.
	return s.ensureCourts(id, courts)
}

// ensureCourts inserts any missing court rows for numbers 1..n on the event so
// the schedule board's lanes always resolve to a real court.
func (s *Service) ensureCourts(eventID string, n int) error {
	existing, err := s.sb.Select("courts",
		"event_id=eq."+store.Q(eventID)+"&select=court_number")
	if err != nil {
		return err
	}
	have := make(map[int]bool, len(existing))
	for _, c := range existing {
		have[asInt(c, "court_number")] = true
	}
	rows := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		if have[i] {
			continue
		}
		rows = append(rows, map[string]any{
			"event_id":     eventID,
			"label":        "Court " + strconv.Itoa(i),
			"court_number": i,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	_, err = s.sb.Insert("courts", rows)
	return err
}

// SetStartTime sets (or clears, when empty) the tournament start (RFC3339 UTC).
func (s *Service) SetStartTime(eventID, startsAt string) error {
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"starts_at": orNull(startsAt)})
	return err
}

// SetGameDuration updates just the per-game slot length (minutes) and returns
// the clamped value actually stored.
func (s *Service) SetGameDuration(eventID string, minutes int) (int, error) {
	m := clampGameDuration(minutes)
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"game_duration_minutes": m})
	return m, err
}

// clampGameDuration bounds the per-game slot length to the form's 15..90 range,
// defaulting an unset value to the researched 25-minute slot.
func clampGameDuration(m int) int {
	switch {
	case m <= 0:
		return 25
	case m < 15:
		return 15
	case m > 90:
		return 90
	default:
		return m
	}
}

// normalizeBestOf coerces the games-per-match setting to a supported odd value
// (1 = single game, 3 = best of 3). Anything else defaults to a single game.
func normalizeBestOf(n int) int {
	if n == 3 {
		return 3
	}
	return 1
}

func ratingPtr(v float64) *float64 { return &v }

// SeedDemo creates a fully-populated round-robin demo tournament so the app has
// data to explore (dev convenience). ~60% of pool matches are scored. Returns
// the new event id.
func (s *Service) SeedDemo(ownerID string) (string, error) {
	return s.seedTournament(model.CreateEventRequest{
		Name:                 "Demo Open Slam",
		Format:               "doubles",
		PartnerMode:          "rotating",
		TournamentFormat:     "round_robin",
		NumCourts:            3,
		RegistrationFeeCents: 2500,
		DuprSanctioned:       true,
		Location:             "Riverside Pickleball Center",
		Brackets: []model.BracketInput{
			{Name: "3.0-3.5", MinRating: ratingPtr(3.0), MaxRating: ratingPtr(3.5)},
			{Name: "3.5-4.0 50+", MinRating: ratingPtr(3.5), MaxRating: ratingPtr(4.0), MinAge: agePtr(50)},
		},
	}, 0.6, ownerID)
}

// SeedPlayoffDemo creates a pools->playoff demo at the very first step: 16 players
// registered across two divisions, with NO schedule generated and NO playoff
// bracket yet. The coordinator drives every step from the UI — Generate schedule,
// start matches, score the pools, then Build playoff. Returns the new event id.
func (s *Service) SeedPlayoffDemo(ownerID string) (string, error) {
	eid, err := s.CreateEvent(model.CreateEventRequest{
		Name:                 "Demo Pickle Cup",
		Format:               "doubles",
		PartnerMode:          "rotating",
		TournamentFormat:     "pools_playoff",
		NumCourts:            3,
		RegistrationFeeCents: 3000,
		DuprSanctioned:       true,
		Location:             "Lakeside Pickleball Courts",
		Brackets: []model.BracketInput{
			{Name: "3.0-3.5", MinRating: ratingPtr(3.0), MaxRating: ratingPtr(3.5)},
			{Name: "3.5-4.0 50+", MinRating: ratingPtr(3.5), MaxRating: ratingPtr(4.0), MinAge: agePtr(50)},
		},
	}, ownerID)
	if err != nil {
		return "", err
	}
	if err := s.registerDemoPlayers(eid); err != nil {
		return "", err
	}
	return eid, nil
}

// registerDemoPlayers registers the 16 standard demo players, split cleanly 8/8
// across the two rating divisions (first 8 at 3.0-3.35, next 8 at 3.55-3.90 —
// strictly above 3.5 so the auto-assigner produces even, bye-free divisions).
func (s *Service) registerDemoPlayers(eventID string) error {
	// 12 players per division. Ratings keep each group inside its bracket's band
	// (3.0-3.5 and 3.5-4.0) so RegisterPlayer auto-assigns the right division.
	div1 := []string{ // 3.0-3.5
		"Ana Rivera", "Ben Carter", "Cara Lopez", "Dan Patel",
		"Evan Brooks", "Fae Nguyen", "Gus Holt", "Hana Park",
		"Iris Cole", "Jay Mercer", "Kira Bose", "Liam Frost",
	}
	div2 := []string{ // 3.5-4.0 50+
		"Ivy Stone", "Jon Webb", "Kim Ross", "Leo Diaz",
		"Mara Quinn", "Nora Vale", "Omar Reed", "Pia Shah",
		"Quinn Ames", "Ravi Shah", "Sky Tran", "Tom Yorke",
	}

	idx := 0
	reg := func(name string, skill float64) error {
		_, err := s.RegisterPlayer(eventID, model.RegisterRequest{
			FullName:        name,
			Phone:           fmt.Sprintf("+1555%07d", 1000000+idx),
			SkillLevel:      ratingPtr(skill),
			DuprID:          fmt.Sprintf("DUPR-%04d", 1000+idx),
			DuprRating:      ratingPtr(skill),
			DuprReliability: ratingPtr(float64(60 + (idx%4)*10)),
		}, "")
		idx++
		return err
	}

	for i, n := range div1 {
		if err := reg(n, 3.0+float64(i)*0.03); err != nil { // 3.00–3.33
			return err
		}
	}
	for i, n := range div2 {
		if err := reg(n, 3.55+float64(i)*0.03); err != nil { // 3.55–3.88
			return err
		}
	}
	return nil
}

// FillRandomPlayers seeds the given EXISTING event with a batch of demo players
// spread across its divisions — enough to run a full day. Each player's rating
// lands inside its bracket's band (so RegisterPlayer keeps it in that division),
// and a deterministic slice are marked paid / checked-in so the roster shows a
// realistic mix of states. Returns the number added. Temporary organizer
// convenience for demos/testing.
func (s *Service) FillRandomPlayers(eventID string) (int, error) {
	bks, err := s.GetBrackets(eventID)
	if err != nil {
		return 0, err
	}
	existing, err := s.Registrations(eventID)
	if err != nil {
		return 0, err
	}
	// Offset names/phones past any prior demo fill so repeats stay distinct even
	// after some seeded players were deleted — a HIGH-WATER MARK over the demo
	// phone range, not the live count (which would regress and recycle values).
	base := 0
	for _, r := range existing {
		if !strings.HasPrefix(r.Phone, "+15553") {
			continue
		}
		if n, perr := strconv.Atoi(strings.TrimPrefix(r.Phone, "+1555")); perr == nil {
			if seq := n - 3000000 + 1; seq > base {
				base = seq
			}
		}
	}

	first := []string{
		"Ava", "Noah", "Mia", "Liam", "Zoe", "Ethan", "Lucy", "Mason",
		"Ella", "Owen", "Nina", "Cole", "Ruby", "Finn", "Tess", "Jude",
		"Wren", "Reed", "Sage", "Beau", "Lena", "Tate", "Cleo", "Maya",
		"Rhys", "Iris", "Knox", "Vera", "Dane", "Faye",
	}
	last := []string{
		"Hill", "Ford", "Vance", "Pope", "Lane", "Cross", "Wells", "Dean",
		"Boyd", "Reyes", "Knox", "Page", "Frye", "Sosa", "Hale", "Nash",
		"Banks", "Cobb", "Diaz", "Estes", "Fox", "Gold", "Pratt", "Quill",
		"Roth", "Sims", "True", "Vega", "Webb", "York",
	}

	const perDiv = 16
	idx := 0
	added := 0

	regOne := func(bracketID string, rating float64) error {
		rt := ratingPtr(rating)
		reg, err := s.RegisterPlayer(eventID, model.RegisterRequest{
			FullName:        first[(base+idx)%len(first)] + " " + last[(base*3+idx*7)%len(last)],
			Phone:           fmt.Sprintf("+1555%07d", 3000000+base+idx),
			SkillLevel:      rt,
			DuprID:          fmt.Sprintf("DUPR-%05d", 30000+base+idx),
			DuprRating:      rt,
			DuprReliability: ratingPtr(float64(50 + (idx%5)*10)),
			BracketID:       bracketID,
		}, "")
		idx++
		if err != nil {
			return err
		}
		added++
		// Realistic roster mix: ~1/3 paid, ~1/4 checked in (best-effort).
		if idx%3 == 0 {
			_ = s.CollectPaymentManually(reg.ID)
		}
		if idx%4 == 0 {
			_, _ = s.CheckIn(reg.ID, "manual")
		}
		return nil
	}

	// Best-effort: one bad insert shouldn't abort the whole fill. Keep going and
	// report how many landed; only surface an error if NOTHING could be added.
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if len(bks) == 0 {
		// No divisions yet — auto-assign across a general spread.
		for i := 0; i < perDiv+8; i++ {
			note(regOne("", 3.0+0.05*float64(i%20)))
		}
	} else {
		for _, b := range bks {
			for i := 0; i < perDiv; i++ {
				note(regOne(b.ID, ratingInBand(b.MinRating, b.MaxRating, i, perDiv)))
			}
		}
	}
	if added == 0 && firstErr != nil {
		return 0, firstErr
	}
	return added, nil
}

// ratingInBand returns a rating strictly inside [min,max] (defaults 2.5–5.0 when
// a bound is open), spread by position i of n, rounded to 2 decimals.
func ratingInBand(min, max *float64, i, n int) float64 {
	lo, hi := 2.5, 5.0
	if min != nil {
		lo = *min
	}
	if max != nil {
		hi = *max
	}
	if hi <= lo {
		hi = lo + 0.5
	}
	denom := n - 1
	if denom < 1 {
		denom = 1
	}
	v := lo + (hi-lo)*(0.05+0.9*float64(i)/float64(denom))
	return float64(int(v*100+0.5)) / 100
}

// seedTournament creates the event, registers the demo players, generates the
// pool schedule, scores a `poolCompletion` fraction (0..1) of the pool matches,
// and reconciles each round's status to match. Used by SeedDemo (round-robin).
func (s *Service) seedTournament(req model.CreateEventRequest, poolCompletion float64, ownerID string) (string, error) {
	eid, err := s.CreateEvent(req, ownerID)
	if err != nil {
		return "", err
	}
	if err := s.registerDemoPlayers(eid); err != nil {
		return "", err
	}

	if _, err := s.GenerateSchedule(eid, true); err != nil {
		return "", err
	}

	// Score the requested fraction of pool matches so standings/live have content.
	poolIDs, err := s.listPoolMatchIDs(eid)
	if err != nil {
		return "", err
	}
	cut := int(float64(len(poolIDs))*poolCompletion + 1e-9)
	for i := 0; i < cut; i++ {
		loser := 5 + (i*3)%6 // 5..10, deterministic
		if i%2 == 0 {
			err = s.applyScore(poolIDs[i], 11, loser)
		} else {
			err = s.applyScore(poolIDs[i], loser, 11)
		}
		if err != nil {
			return "", err
		}
	}

	// The seed records scores directly (bypassing StartRound), so bring each
	// round's status in line with its matches — a round must not read
	// "scheduled" while its matches read "final".
	if err := s.reconcileRoundStatuses(eid); err != nil {
		return "", err
	}
	return eid, nil
}

// reconcileRoundStatuses sets a round to 'completed' when all of its matches are
// completed and 'active' when only some are; rounds with no completed matches are
// left 'pending'. Used after seeding, which records scores without the usual
// StartRound flow.
// reconcileRoundStatuses marks each round completed (all its matches done),
// active (some done), or leaves it pending (none done). PostgREST can't do the
// GROUP BY/HAVING, so we pull each round with its matches' statuses embedded and
// compute the transitions in Go, batching the two UPDATEs by target status.
func (s *Service) reconcileRoundStatuses(eventID string) error {
	rows, err := s.sb.Select("rounds",
		"event_id=eq."+store.Q(eventID)+"&select=id,status,matches(status)")
	if err != nil {
		return err
	}
	var toCompleted, toActive []string
	for _, r := range rows {
		ms, _ := r["matches"].([]any)
		total, done := 0, 0
		for _, m := range ms {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			total++
			if asStr(mm, "status") == "completed" {
				done++
			}
		}
		if total == 0 {
			continue
		}
		switch {
		case done == total:
			if asStr(r, "status") != "completed" {
				toCompleted = append(toCompleted, asStr(r, "id"))
			}
		case done > 0:
			if asStr(r, "status") != "active" {
				toActive = append(toActive, asStr(r, "id"))
			}
		}
	}
	if len(toCompleted) > 0 {
		if _, err := s.sb.Update("rounds", "id=in.("+strings.Join(toCompleted, ",")+")",
			map[string]any{"status": "completed"}); err != nil {
			return err
		}
	}
	if len(toActive) > 0 {
		if _, err := s.sb.Update("rounds", "id=in.("+strings.Join(toActive, ",")+")",
			map[string]any{"status": "active"}); err != nil {
			return err
		}
	}
	return nil
}

// listPoolMatchIDs returns the ids of every pool-stage match for an event, in a
// stable insertion order.
func (s *Service) listPoolMatchIDs(eventID string) ([]string, error) {
	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&stage=eq.pool&select=id&order=created_at,id")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, asStr(r, "id"))
	}
	return ids, nil
}

func (s *Service) GetBrackets(eventID string) ([]model.Bracket, error) {
	rows, err := s.sb.Select("brackets",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=sort_order")
	if err != nil {
		return nil, err
	}
	out := make([]model.Bracket, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapBracket(r))
	}
	return out, nil
}

// ------------------------------------------------------------ registration
// RegisterPlayer files a registration. When linkUserID is non-empty (a
// logged-in user registering THEMSELVES), the player is tied to that account
// (players.user_id) — reusing the account's existing player row if it has one
// (the user_id column is unique) rather than creating a duplicate.
func (s *Service) RegisterPlayer(eventID string, req model.RegisterRequest, linkUserID string) (model.Registration, error) {
	if strings.TrimSpace(req.FullName) == "" {
		return model.Registration{}, errors.New("fullName is required")
	}
	fields := map[string]any{
		"full_name":        req.FullName,
		"phone":            orNull(req.Phone),
		"email":            orNull(req.Email),
		"skill_level":      fOrNull(req.SkillLevel),
		"dupr_id":          orNull(req.DuprID),
		"dupr_rating":      fOrNull(req.DuprRating),
		"dupr_reliability": fOrNull(req.DuprReliability),
	}
	var playerID string
	if linkUserID != "" {
		// Reuse this account's player row if it exists (unique user_id), else
		// create one tagged with the account.
		existing, err := s.sb.SelectOne("players",
			"user_id=eq."+store.Q(linkUserID)+"&select=id")
		if err != nil {
			return model.Registration{}, err
		}
		if existing != nil {
			playerID = asStr(existing, "id")
			// Update the linked profile, but only with values actually provided so
			// a registration with blank optional fields can't wipe a phone/rating
			// saved from a prior event (one player row is shared across events).
			upd := map[string]any{"full_name": req.FullName}
			if req.Phone != "" {
				upd["phone"] = req.Phone
			}
			if req.Email != "" {
				upd["email"] = req.Email
			}
			if req.SkillLevel != nil {
				upd["skill_level"] = *req.SkillLevel
			}
			if req.DuprID != "" {
				upd["dupr_id"] = req.DuprID
			}
			if req.DuprRating != nil {
				upd["dupr_rating"] = *req.DuprRating
			}
			if req.DuprReliability != nil {
				upd["dupr_reliability"] = *req.DuprReliability
			}
			if _, err := s.sb.Update("players", "id=eq."+store.Q(playerID), upd); err != nil {
				return model.Registration{}, err
			}
		} else {
			fields["user_id"] = linkUserID
			pl, err := s.sb.Insert("players", fields)
			if err != nil {
				return model.Registration{}, err
			}
			if len(pl) == 0 {
				return model.Registration{}, errors.New("player insert returned no row")
			}
			playerID = asStr(pl[0], "id")
		}
	} else {
		pl, err := s.sb.Insert("players", fields)
		if err != nil {
			return model.Registration{}, err
		}
		if len(pl) == 0 {
			return model.Registration{}, errors.New("player insert returned no row")
		}
		playerID = asStr(pl[0], "id")
	}

	// Resolve the division. An explicitly-chosen bracket must actually belong to
	// this event (otherwise a crafted request could file a registration under
	// another event's division); an empty choice is auto-assigned by rating.
	bks, err := s.GetBrackets(eventID)
	if err != nil {
		return model.Registration{}, err
	}
	bracketID := req.BracketID
	if bracketID == "" {
		b, err := s.autoAssignBracket(eventID, req.SkillLevel)
		if err != nil {
			return model.Registration{}, err
		}
		bracketID = b
	}
	var chosen *model.Bracket
	for i := range bks {
		if bks[i].ID == bracketID {
			chosen = &bks[i]
			break
		}
	}
	if bracketID != "" && chosen == nil {
		return model.Registration{}, errors.New("selected division is not part of this event")
	}
	// Soft eligibility flag: surface (don't block) when the player's rating falls
	// outside the division's band. Prefer DUPR; fall back to self-reported skill.
	playerRating := req.DuprRating
	if playerRating == nil {
		playerRating = req.SkillLevel
	}
	outside := false
	if chosen != nil && playerRating != nil {
		if (chosen.MinRating != nil && *playerRating < *chosen.MinRating) ||
			(chosen.MaxRating != nil && *playerRating > *chosen.MaxRating) {
			outside = true
		}
	}
	// A linked account already registered for this event would collide on the
	// unique (event_id, player_id) — surface a friendly message, not a raw 409.
	if linkUserID != "" {
		dup, err := s.sb.SelectOne("registrations",
			"event_id=eq."+store.Q(eventID)+"&player_id=eq."+store.Q(playerID)+"&select=id")
		if err != nil {
			return model.Registration{}, err
		}
		if dup != nil {
			return model.Registration{}, errors.New("you're already registered for this event")
		}
	}
	token := newID()
	reg, err := s.sb.Insert("registrations", map[string]any{
		"event_id":       eventID,
		"player_id":      playerID,
		"partner_id":     orNull(req.PartnerID),
		"bracket_id":     orNull(bracketID),
		"check_in_token": token,
	})
	if err != nil {
		return model.Registration{}, err
	}
	if len(reg) == 0 {
		return model.Registration{}, errors.New("registration insert returned no row")
	}
	return model.Registration{
		ID: asStr(reg[0], "id"), EventID: eventID, PlayerID: playerID, FullName: req.FullName,
		BracketID: strp(bracketID), PaymentStatus: "unpaid", CheckedIn: false, CheckInToken: &token,
		OutsideRating: outside,
	}, nil
}

func (s *Service) autoAssignBracket(eventID string, rating *float64) (string, error) {
	bks, err := s.GetBrackets(eventID)
	if err != nil || len(bks) == 0 {
		return "", err
	}
	if len(bks) == 1 {
		return bks[0].ID, nil
	}
	if rating != nil {
		for _, b := range bks {
			okMin := b.MinRating == nil || *rating >= *b.MinRating
			okMax := b.MaxRating == nil || *rating <= *b.MaxRating
			if okMin && okMax {
				return b.ID, nil
			}
		}
	}
	return bks[0].ID, nil
}

func (s *Service) Registrations(eventID string) ([]model.Registration, error) {
	// registrations has two FKs to players (player_id, partner_id) so the embed
	// must name the FK column; alias both embeds to stable keys.
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+
			"&select=id,event_id,player_id,bracket_id,payment_status,checked_in,check_in_token,"+
			"player:players!player_id(full_name,phone,dupr_id,dupr_rating,skill_level),"+
			"bracket:brackets(min_rating,max_rating)")
	if err != nil {
		return nil, err
	}
	out := make([]model.Registration, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapRegistration(r))
	}
	return out, nil
}

// UpdateRegistrationDetails edits the player behind a registration (organizer
// only) — the shared players row holds the name + rating, so this writes there.
func (s *Service) UpdateRegistrationDetails(regID, fullName string, duprRating *float64) error {
	if strings.TrimSpace(fullName) == "" {
		return errors.New("name is required")
	}
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(regID)+"&select=player_id")
	if err != nil {
		return err
	}
	if reg == nil {
		return ErrNotFound
	}
	playerID := asStr(reg, "player_id")
	_, err = s.sb.Update("players", "id=eq."+store.Q(playerID), map[string]any{
		"full_name":   strings.TrimSpace(fullName),
		"dupr_rating": fOrNull(duprRating),
	})
	return err
}

// DeleteRegistration removes a player's registration from an event (organizer
// only). The global players row is left intact (it may be used elsewhere); FK
// cascades clean up this registration's payments/check-ins.
func (s *Service) DeleteRegistration(regID string) error {
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(regID)+"&select=id")
	if err != nil {
		return err
	}
	if reg == nil {
		return ErrNotFound
	}
	return s.sb.Delete("registrations", "id=eq."+store.Q(regID))
}

// BusyCourts returns the distinct court numbers that currently have an
// in-progress match in this event. The schedule UI uses this to dim other
// scheduled matches assigned to a court that's already in play.
func (s *Service) BusyCourts(eventID string) ([]int, error) {
	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&status=eq.in_progress&select=court:courts!court_id(court_number)")
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	out := []int{}
	for _, r := range rows {
		c := asMap(r, "court")
		if c == nil {
			continue
		}
		n := asIntPtr(c, "court_number")
		if n == nil || seen[*n] {
			continue
		}
		seen[*n] = true
		out = append(out, *n)
	}
	sort.Ints(out)
	return out, nil
}

// completedMatchCount counts an event's scored matches (guards re-generate).
func (s *Service) completedMatchCount(eventID string) (int, error) {
	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&status=eq.completed&select=id")
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// ---------------------------------------------------------- scheduling
func (s *Service) GenerateSchedule(eventID string, force bool) (int, error) {
	// Refuse to wipe an in-progress event's scores unless explicitly forced.
	if !force {
		done, err := s.completedMatchCount(eventID)
		if err != nil {
			return 0, err
		}
		if done > 0 {
			return done, fmt.Errorf("%w: %d match(es) already scored", ErrScheduleHasResults, done)
		}
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	bks, err := s.GetBrackets(eventID)
	if err != nil {
		return 0, err
	}
	courtByNum, err := s.courtIDsByNumber(eventID)
	if err != nil {
		return 0, err
	}
	skill, err := s.playerSkills()
	if err != nil {
		return 0, err
	}
	if err := s.wipeAllMatches(eventID); err != nil {
		return 0, err
	}

	total := 0
	for _, b := range bks {
		regs, err := s.bracketRegs(eventID, b.ID)
		if err != nil {
			return 0, err
		}
		// Doubles needs at least 4 players (a full game); singles 2. Skip
		// undersized divisions instead of persisting empty rounds.
		minPlayers := 2
		if ev.Format == "doubles" {
			minPlayers = 4
		}
		if len(regs) < minPlayers {
			continue
		}
		if ev.TournamentFormat == "single_elim" {
			sides := seedSides(sidesForBracket(ev, regs), skill)
			n, err := s.persistBracket(ev, b.ID, sides, ev.Consolation)
			if err != nil {
				return 0, err
			}
			total += n
		} else if ev.TournamentFormat == "double_elim" {
			sides := seedSides(sidesForBracket(ev, regs), skill)
			n, err := s.persistDoubleElim(ev, b.ID, sides)
			if err != nil {
				return 0, err
			}
			total += n
		} else {
			n, err := s.persistRoundRobin(ev, b.ID, regs, courtByNum)
			if err != nil {
				return 0, err
			}
			total += n
			// pools_playoff: lay down the (empty) medal bracket now so it shows
			// in the Standings tab immediately; it auto-seeds when pools finish.
			if ev.TournamentFormat == "pools_playoff" {
				seeds, err := s.seedTopTeams(ev, eventID, b.ID)
				if err != nil {
					return 0, err
				}
				if len(seeds) >= 4 {
					if _, err := s.persistMedalBracket(ev, b.ID, nil); err != nil {
						return 0, err
					}
				}
			}
		}
	}
	if err := s.spreadCourts(eventID); err != nil {
		return 0, err
	}
	_, err = s.sb.Update("events", "id=eq."+store.Q(eventID), map[string]any{"status": "in_progress"})
	return total, err
}

// spreadCourts distributes pool matches across every available court. Each
// division generates its round-robin independently (each starting at court 1),
// so without this both divisions would pile onto courts 1-2 and leave the rest
// idle. We reassign court numbers per round number across all divisions, cycling
// through the available courts, so concurrent matches use distinct courts.
func (s *Service) spreadCourts(eventID string) error {
	courtByNum, err := s.courtIDsByNumber(eventID)
	if err != nil {
		return err
	}
	if len(courtByNum) == 0 {
		return nil
	}
	courtNums := make([]int, 0, len(courtByNum))
	for n := range courtByNum {
		courtNums = append(courtNums, n)
	}
	sort.Ints(courtNums)

	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&stage=eq.pool&select=id,bracket_id,created_at,round:rounds!round_id(round_number)")
	if err != nil {
		return err
	}
	type mr struct {
		id, bracket, created string
		round                int
	}
	list := make([]mr, 0, len(rows))
	for _, r := range rows {
		round := 0
		if rd := asMap(r, "round"); rd != nil {
			round = asInt(rd, "round_number")
		}
		list = append(list, mr{
			id: asStr(r, "id"), bracket: asStr(r, "bracket_id"),
			created: asStr(r, "created_at"), round: round,
		})
	}
	// Match the old ORDER BY r.round_number, m.bracket_id, insertion order.
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.round != b.round {
			return a.round < b.round
		}
		if a.bracket != b.bracket {
			return a.bracket < b.bracket
		}
		return a.created < b.created
	})

	prevRound, idx := -1, 0
	for _, m := range list {
		if m.round != prevRound {
			prevRound = m.round
			idx = 0
		}
		cid := courtByNum[courtNums[idx%len(courtNums)]]
		idx++
		if _, err := s.sb.Update("matches", "id=eq."+store.Q(m.id),
			map[string]any{"court_id": cid}); err != nil {
			return err
		}
	}
	return nil
}

// AutoScheduleByRating lays every pool game onto courts + time-slots ordered by
// division rating band (lowest first → highest last). Conflict-safety: each
// (division, round) occupies its own slot(s), so two games that could share a
// player are never put in the same slot — within a round all games are already
// player-disjoint, and different rounds of a division land in different slots.
// Games in a round fan out across the courts. play_order is the slot index, so
// the calendar can place a game at day_start + slot*game_duration. Returns the
// number of games scheduled.
func (s *Service) AutoScheduleByRating(eventID string, interleave bool, minRestSlots int) (int, error) {
	courtByNum, err := s.courtIDsByNumber(eventID)
	if err != nil {
		return 0, err
	}
	if len(courtByNum) == 0 {
		return 0, errors.New("no courts for this event")
	}
	courtNums := make([]int, 0, len(courtByNum))
	for n := range courtByNum {
		courtNums = append(courtNums, n)
	}
	sort.Ints(courtNums)

	// Division order: rated bands ascending by min_rating; unrated ("Open")
	// divisions sort last; sort_order breaks ties.
	brackets, err := s.sb.Select("brackets",
		"event_id=eq."+store.Q(eventID)+"&select=id,min_rating,sort_order")
	if err != nil {
		return 0, err
	}
	type brk struct {
		id   string
		min  *float64
		sort int
	}
	blist := make([]brk, 0, len(brackets))
	for _, b := range brackets {
		blist = append(blist, brk{id: asStr(b, "id"), min: asFloatPtr(b, "min_rating"), sort: asInt(b, "sort_order")})
	}
	sort.SliceStable(blist, func(i, j int) bool {
		a, b := blist[i], blist[j]
		aNull, bNull := a.min == nil, b.min == nil
		if aNull != bNull {
			return !aNull // rated divisions before unrated
		}
		if !aNull && !bNull && *a.min != *b.min {
			return *a.min < *b.min
		}
		return a.sort < b.sort
	})
	rank := make(map[string]int, len(blist))
	for i, b := range blist {
		rank[b.id] = i
	}

	// Pool matches with their round number.
	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&stage=eq.pool&select=id,bracket_id,created_at,round:rounds!round_id(round_number)")
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	type mr struct {
		id, bracket, created string
		round                int
	}
	list := make([]mr, 0, len(rows))
	for _, r := range rows {
		round := 0
		if rd := asMap(r, "round"); rd != nil {
			round = asInt(rd, "round_number")
		}
		list = append(list, mr{
			id: asStr(r, "id"), bracket: asStr(r, "bracket_id"),
			created: asStr(r, "created_at"), round: round,
		})
	}
	// Lowest division first, then round order, then a stable insertion tiebreak.
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := rank[a.bracket], rank[b.bracket]; ra != rb {
			return ra < rb
		}
		if a.round != b.round {
			return a.round < b.round
		}
		return a.created < b.created
	})

	// Group into division -> ordered rounds -> ordered match ids. `list` is
	// already sorted by (bracketRank, round, created), so divOrder comes out in
	// rating order and each division's rounds are in play order.
	divOrder := make([]string, 0)
	seenDiv := make(map[string]bool)
	divRounds := make(map[string][][]string)
	curKey := ""
	for _, m := range list {
		if !seenDiv[m.bracket] {
			seenDiv[m.bracket] = true
			divOrder = append(divOrder, m.bracket)
		}
		key := m.bracket + "|" + strconv.Itoa(m.round)
		if key != curKey {
			curKey = key
			divRounds[m.bracket] = append(divRounds[m.bracket], []string{})
		}
		rs := divRounds[m.bracket]
		rs[len(rs)-1] = append(rs[len(rs)-1], m.id)
	}

	// Player -> participants, for cross-division conflict avoidance: a player
	// entered in TWO divisions must not be placed in the same time slot (a player
	// can't be on two courts at once — USAP 12.K). Sequential mode is already
	// conflict-free (divisions get separate slots); the interleave/packed path
	// shares slots across divisions, so it needs this. minRestSlots additionally
	// keeps a player's consecutive matches at least that many slots apart.
	matchPlayers := map[string][]string{}
	if len(list) > 0 {
		mids := make([]string, len(list))
		for i, m := range list {
			mids[i] = m.id
		}
		// Fetch participants in CHUNKS: one in.(...) over a big event could exceed
		// the URL length or PostgREST's default row cap (a silent 200 truncation),
		// leaving matchPlayers partial — which would quietly disable conflict
		// avoidance while still reporting success. Keep each chunk well under any
		// row cap (<=100 matches => <=~400 participant rows) and PROPAGATE errors
		// rather than scheduling on incomplete data. This runs before any match is
		// placed, so an early return leaves play_order untouched.
		const chunk = 100
		for start := 0; start < len(mids); start += chunk {
			end := start + chunk
			if end > len(mids) {
				end = len(mids)
			}
			parts, e := s.sb.Select("match_participants",
				"match_id=in.("+strings.Join(mids[start:end], ",")+")&select=match_id,player_id")
			if e != nil {
				return 0, e
			}
			for _, p := range parts {
				mid := asStr(p, "match_id")
				matchPlayers[mid] = append(matchPlayers[mid], asStr(p, "player_id"))
			}
		}
	}
	occupied := map[int]map[string]bool{} // slot -> player ids playing that slot
	lastSlot := map[string]int{}          // player id -> their latest placed slot
	// conflictAt reports whether placing matchID at slot would double-book a
	// player (hard) or violate the min-rest gap (soft — only when minRestSlots>0).
	conflictAt := func(slot int, matchID string) bool {
		occ := occupied[slot]
		for _, pid := range matchPlayers[matchID] {
			if occ != nil && occ[pid] {
				return true
			}
			if minRestSlots > 0 {
				if prev, ok := lastSlot[pid]; ok && slot-prev <= minRestSlots {
					return true
				}
			}
		}
		return false
	}
	markSlot := func(slot int, matchID string) {
		if occupied[slot] == nil {
			occupied[slot] = map[string]bool{}
		}
		for _, pid := range matchPlayers[matchID] {
			occupied[slot][pid] = true
			lastSlot[pid] = slot
		}
	}

	scheduled := 0
	var perr error
	place := func(matchID string, courtNumber, slot int) {
		if perr != nil {
			return
		}
		if _, err := s.sb.Update("matches", "id=eq."+store.Q(matchID),
			map[string]any{"court_id": courtByNum[courtNumber], "play_order": float64(slot)}); err != nil {
			perr = err
			return
		}
		scheduled++
	}

	if interleave {
		// Slot-filling: at each slot, fill courts division-by-division (lowest
		// first) from each division's CURRENT round only — so a division never
		// has two rounds in one slot (no double-booking), while idle courts get
		// used by higher divisions, shortening the day.
		roundIdx := make(map[string]int)
		pos := make(map[string]int)
		remaining := len(list)
		slot := 0
		// Conflicts (and the min-rest gap) ALWAYS self-resolve as `slot` grows: a
		// fresh slot has no occupied players, and the rest gap widens with time. So
		// the loop terminates and the hard double-booking check is never bypassed.
		// maxSlots is only a defensive bound; if it were somehow hit (it can't for
		// valid round-robin data), the few unplaced matches just stay un-arranged
		// (play_order null) — never an infinite loop and never a double-booked slot.
		maxSlots := len(list)*(minRestSlots+1) + len(courtNums) + 10
		for remaining > 0 && perr == nil && slot <= maxSlots {
			courtCursor := 0
			for _, div := range divOrder {
				if courtCursor >= len(courtNums) {
					break
				}
				rounds := divRounds[div]
				ri := roundIdx[div]
				if ri >= len(rounds) {
					continue // division done
				}
				round := rounds[ri]
				for pos[div] < len(round) && courtCursor < len(courtNums) {
					mid := round[pos[div]]
					// Defer this division if its next match would double-book a
					// player at this slot (hard) or break the min-rest gap (soft);
					// it retries at a later slot. Never bypassed — a player is never
					// placed on two courts at once.
					if conflictAt(slot, mid) {
						break
					}
					place(mid, courtNums[courtCursor], slot)
					markSlot(slot, mid)
					courtCursor++
					pos[div]++
					remaining--
				}
				if pos[div] >= len(round) {
					roundIdx[div]++ // next round goes to a later slot
					pos[div] = 0
				}
			}
			slot++
		}
	} else {
		// Sequential: each division fully before the next; each round gets its
		// own slot(s) and spills to the next slot when it overflows the courts.
		slot := 0
		for _, div := range divOrder {
			for _, round := range divRounds[div] {
				courtIdx := 0
				for _, mid := range round {
					if courtIdx == len(courtNums) {
						slot++
						courtIdx = 0
					}
					place(mid, courtNums[courtIdx], slot)
					courtIdx++
				}
				slot++
			}
		}
	}
	if perr != nil {
		return scheduled, perr
	}
	return scheduled, nil
}

func (s *Service) persistRoundRobin(ev model.Event, bracketID string, regs []reg, courtByNum map[int]string) (int, error) {
	format := engine.Doubles
	if ev.Format == "singles" {
		format = engine.Singles
	}
	partner := engine.Rotating
	if ev.PartnerMode == "fixed" {
		partner = engine.Fixed
	}
	var fixedPairs [][]string
	if format == engine.Doubles && partner == engine.Fixed {
		fixedPairs = pairsFromRegs(regs)
	}
	ids := make([]string, len(regs))
	for i, r := range regs {
		ids[i] = r.playerID
	}
	// Rounds only affects rotating doubles (singles & fixed doubles always run a
	// full N-1 round-robin and ignore this). Scale the social mixer with the
	// field instead of a magic 7: ~N-1, clamped to a practical 3..12 so small
	// fields don't over-repeat and huge fields don't run all day.
	rounds := 7
	if format == engine.Doubles && partner == engine.Rotating {
		rounds = len(ids) - 1
		if rounds < 3 {
			rounds = 3
		}
		if rounds > 12 {
			rounds = 12
		}
	}
	schedule := engine.GenerateSchedule(ids, format, partner, ev.NumCourts, fixedPairs, rounds)

	count := 0
	for _, round := range schedule {
		rd, err := s.sb.Insert("rounds", map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "round_number": round.RoundNumber,
		})
		if err != nil {
			return 0, err
		}
		if len(rd) == 0 {
			return 0, errors.New("round insert returned no row")
		}
		roundID := asStr(rd[0], "id")
		for _, m := range round.Matches {
			mt, err := s.sb.Insert("matches", map[string]any{
				"event_id": ev.ID, "bracket_id": bracketID, "round_id": roundID,
				"court_id": orNull(courtByNum[m.CourtNumber]), "stage": "pool",
			})
			if err != nil {
				return 0, err
			}
			if len(mt) == 0 {
				return 0, errors.New("match insert returned no row")
			}
			matchID := asStr(mt[0], "id")
			if err := s.insertSide(matchID, 1, m.Team1); err != nil {
				return 0, err
			}
			if err := s.insertSide(matchID, 2, m.Team2); err != nil {
				return 0, err
			}
			count++
		}
	}
	return count, nil
}

func (s *Service) persistBracket(ev model.Event, bracketID string, seededSides [][]string, consolation bool) (int, error) {
	plan := engine.GenerateBracket(seededSides)
	idByKey := map[string]string{}
	count := 0
	for _, m := range plan.Matches {
		row := map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
			"bracket_round": m.Round, "bracket_slot": m.Slot, "status": "scheduled",
		}
		if m.ResolvedWinner != nil { // a bye — auto-complete it
			row["status"] = "completed"
			row["completed_at"] = now()
			if !engine.IsBye(m.Side1) && m.Side1 != nil {
				row["winning_team"] = 1
			} else {
				row["winning_team"] = 2
			}
		}
		out, err := s.sb.Insert("matches", row)
		if err != nil {
			return 0, err
		}
		if len(out) == 0 {
			return 0, errors.New("bracket match insert returned no row")
		}
		mid := asStr(out[0], "id")
		idByKey[key(m.Round, m.Slot)] = mid
		if err := s.insertSide(mid, 1, m.Side1); err != nil {
			return 0, err
		}
		if err := s.insertSide(mid, 2, m.Side2); err != nil {
			return 0, err
		}
		count++
	}
	for _, m := range plan.Matches {
		if m.FeedsRound == 0 {
			continue
		}
		mid := idByKey[key(m.Round, m.Slot)]
		feedID := idByKey[key(m.FeedsRound, m.FeedsSlot)]
		if _, err := s.sb.Update("matches", "id=eq."+store.Q(mid), map[string]any{
			"feeds_match_id": feedID, "feeds_slot": m.FeedsTeam,
		}); err != nil {
			return 0, err
		}
	}

	// Consolation back-draw: first-round losers play down to a consolation
	// champion (bronze). A main round-1 match that auto-completed at generation
	// is a bye (no loser); GenerateConsolation routes the surviving losers past
	// those byes and emits only matches that will have two real players.
	if consolation {
		bye := make(map[int]bool)
		for _, m := range plan.Matches {
			if m.Round == 1 && m.ResolvedWinner != nil {
				bye[m.Slot] = true
			}
		}
		cons := engine.GenerateConsolation(plan.Size, func(slot int) bool { return bye[slot] })
		consID := map[string]string{}
		for _, cm := range cons.Matches {
			out, err := s.sb.Insert("matches", map[string]any{
				"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
				"bracket_tier": "consolation", "bracket_round": cm.Round,
				"bracket_slot": cm.Slot, "status": "scheduled",
			})
			if err != nil {
				return 0, err
			}
			if len(out) == 0 {
				return 0, errors.New("consolation match insert returned no row")
			}
			consID[key(cm.Round, cm.Slot)] = asStr(out[0], "id")
			count++
		}
		// Winner advancement within the consolation tree.
		for _, cm := range cons.Matches {
			if cm.FeedsRound == 0 {
				continue
			}
			if _, err := s.sb.Update("matches", "id=eq."+store.Q(consID[key(cm.Round, cm.Slot)]),
				map[string]any{
					"feeds_match_id": consID[key(cm.FeedsRound, cm.FeedsSlot)],
					"feeds_slot":     cm.FeedsTeam,
				}); err != nil {
				return 0, err
			}
		}
		// Each main round-1 match's LOSER drops into the consolation tree.
		for _, d := range cons.Drops {
			mainID := idByKey[key(1, d.MainSlot)]
			if mainID == "" {
				continue
			}
			if _, err := s.sb.Update("matches", "id=eq."+store.Q(mainID), map[string]any{
				"loser_feeds_match_id": consID[key(d.Round, d.Slot)],
				"loser_feeds_slot":     d.Team,
			}); err != nil {
				return 0, err
			}
		}
	}
	return count, nil
}

// persistMedalBracket builds the 4-team medal playoff:
//
//	SF1 (slot 0): seed 1 vs seed 4      SF2 (slot 1): seed 2 vs seed 3
//	Gold (round 2, slot 0): SF winners  Bronze (round 2, slot 1): SF losers
//
// Each semifinal routes its winner to gold and its loser to bronze. When sides
// is empty (or has < 4 teams) it lays down an unseeded skeleton (TBD semis) —
// used so the bracket shows the moment the schedule is generated; it auto-seeds
// once the pools finish (see maybeSeedPlayoff).
func (s *Service) persistMedalBracket(ev model.Event, bracketID string, sides [][]string) (int, error) {
	var s1, s2, s3, s4 []string
	if len(sides) >= 4 {
		s1, s2, s3, s4 = sides[0], sides[1], sides[2], sides[3]
	}

	// Round 2 medal games (TBD until the semifinals resolve). Insert these first
	// so we have their ids to point the semifinals' winner/loser feeds at.
	medal := func(slot int) (string, error) {
		out, err := s.sb.Insert("matches", map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
			"bracket_round": 2, "bracket_slot": slot, "status": "scheduled",
		})
		if err != nil {
			return "", err
		}
		if len(out) == 0 {
			return "", errors.New("medal game insert returned no row")
		}
		return asStr(out[0], "id"), nil
	}
	goldID, err := medal(0)
	if err != nil {
		return 0, err
	}
	bronzeID, err := medal(1)
	if err != nil {
		return 0, err
	}

	// Round 1 semifinals (1v4, 2v3). Winner -> gold, loser -> bronze, each into
	// team-slot (sf slot + 1) of the round-2 game.
	semis := []struct {
		slot int
		a, b []string
	}{
		{0, s1, s4}, // #1 vs #4 (nil = TBD skeleton)
		{1, s2, s3}, // #2 vs #3
	}
	for _, sf := range semis {
		feedSlot := sf.slot + 1
		out, err := s.sb.Insert("matches", map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
			"bracket_round": 1, "bracket_slot": sf.slot, "status": "scheduled",
			"feeds_match_id": goldID, "feeds_slot": feedSlot,
			"loser_feeds_match_id": bronzeID, "loser_feeds_slot": feedSlot,
		})
		if err != nil {
			return 0, err
		}
		if len(out) == 0 {
			return 0, errors.New("semifinal insert returned no row")
		}
		sfID := asStr(out[0], "id")
		if err := s.insertSide(sfID, 1, sf.a); err != nil {
			return 0, err
		}
		if err := s.insertSide(sfID, 2, sf.b); err != nil {
			return 0, err
		}
	}
	return 4, nil
}

// persistDoubleElim lays down a full double-elimination draw: a winners bracket
// (tier 'winners'), a losers/back-draw bracket ('losers'), and the grand final
// ('grand_final'). All matches are inserted first to obtain ids, then winner
// feeds (feeds_match_id) and the WB->LB loser feeds (loser_feeds_match_id) are
// wired — both are plain match ids, so the existing advanceAfterScore routes
// winners and losers across tiers unchanged. The conditional "if necessary"
// reset game is created at score time, not here.
func (s *Service) persistDoubleElim(ev model.Event, bracketID string, seededSides [][]string) (int, error) {
	plan := engine.GenerateDoubleElim(seededSides)
	dkey := func(tier string, r, slot int) string {
		return tier + ":" + strconv.Itoa(r) + ":" + strconv.Itoa(slot)
	}
	idByKey := make(map[string]string, len(plan.Matches))

	count := 0
	for _, m := range plan.Matches {
		row := map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
			"bracket_tier": m.Tier, "bracket_round": m.Round, "bracket_slot": m.Slot,
			"status": "scheduled",
		}
		if m.ResolvedWinner != nil { // a WB bye — auto-complete it
			row["status"] = "completed"
			row["completed_at"] = now()
			if m.Side1 != nil && !engine.IsBye(m.Side1) {
				row["winning_team"] = 1
			} else {
				row["winning_team"] = 2
			}
		}
		out, err := s.sb.Insert("matches", row)
		if err != nil {
			return 0, err
		}
		if len(out) == 0 {
			return 0, errors.New("double-elim match insert returned no row")
		}
		mid := asStr(out[0], "id")
		idByKey[dkey(m.Tier, m.Round, m.Slot)] = mid
		if err := s.insertSide(mid, 1, m.Side1); err != nil {
			return 0, err
		}
		if err := s.insertSide(mid, 2, m.Side2); err != nil {
			return 0, err
		}
		count++
	}

	for _, m := range plan.Matches {
		upd := map[string]any{}
		if m.WinTier != "" {
			upd["feeds_match_id"] = idByKey[dkey(m.WinTier, m.WinRound, m.WinSlot)]
			upd["feeds_slot"] = m.WinTeam
		}
		// A bye WB match has no loser to drop.
		if m.LoseTier != "" && m.ResolvedWinner == nil {
			upd["loser_feeds_match_id"] = idByKey[dkey(m.LoseTier, m.LoseRound, m.LoseSlot)]
			upd["loser_feeds_slot"] = m.LoseTeam
		}
		if len(upd) == 0 {
			continue
		}
		if _, err := s.sb.Update("matches", "id=eq."+store.Q(idByKey[dkey(m.Tier, m.Round, m.Slot)]), upd); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (s *Service) GeneratePlayoffBracket(bracketID string, topN int) (int, error) {
	b, err := s.sb.SelectOne("brackets", "id=eq."+store.Q(bracketID)+"&select=event_id")
	if err != nil {
		return 0, err
	}
	if b == nil {
		return 0, ErrNotFound
	}
	eventID := asStr(b, "event_id")

	// A single-elimination playoff is seeded from pool standings, so the pools
	// must be fully played first. Otherwise "Build playoff" would seed a
	// meaningless bracket off all-zero standings.
	poolTotal, poolOpen, err := s.poolProgress(bracketID)
	if err != nil {
		return 0, err
	}
	if poolTotal == 0 {
		return 0, errors.New("generate the pool schedule and play the pool matches before building the playoff")
	}
	if poolOpen > 0 {
		return 0, fmt.Errorf("finish all pool matches in this division before building the playoff (%d of %d still unplayed)", poolOpen, poolTotal)
	}

	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	sides, err := s.seedTopTeams(ev, eventID, bracketID)
	if err != nil {
		return 0, err
	}
	if len(sides) < 4 {
		return 0, errors.New("need at least 4 teams in this division to build the playoff")
	}
	// Draw size: default to the top 4; honor a larger request (8/16…) but never
	// ask for more teams than the division has.
	if topN < 4 {
		topN = 4
	}
	if topN > len(sides) {
		topN = len(sides)
	}
	if err := s.wipeBracketStage(bracketID); err != nil {
		return 0, err
	}
	// Top-4 uses the medal bracket (adds a 3rd-place / bronze game). Larger draws
	// use a standard single-elimination bracket, seeded 1-vs-N with byes padding
	// to the next power of two (the engine handles both).
	if topN == 4 {
		return s.persistMedalBracket(ev, bracketID, sides[:4])
	}
	// No consolation here: pool players already played >=2 games, so the playoff
	// bracket doesn't need a back-draw to satisfy the 2-match guarantee.
	return s.persistBracket(ev, bracketID, sides[:topN], false)
}

// seedTopTeams returns this division's teams ordered best-first by pool
// standings. Before any pools are played the order is arbitrary but the team
// SET is complete, so callers can use len() to gate (>= 4 for a medal bracket)
// and take the top 4 once standings exist.
func (s *Service) seedTopTeams(ev model.Event, eventID, bracketID string) ([][]string, error) {
	standings, err := s.Standings(eventID, bracketID, true)
	if err != nil {
		return nil, err
	}
	regs, err := s.bracketRegs(eventID, bracketID)
	if err != nil {
		return nil, err
	}
	var sides [][]string
	if ev.Format == "singles" {
		seen := map[string]bool{}
		for _, st := range standings {
			sides = append(sides, []string{st.PlayerID})
			seen[st.PlayerID] = true
		}
		for _, r := range regs {
			if !seen[r.playerID] {
				sides = append(sides, []string{r.playerID})
			}
		}
	} else {
		pairs := pairsFromRegs(regs)
		rate := map[string]int{}
		for _, st := range standings {
			// Seed by record first, then point differential, then points scored —
			// the same priority the standings table ranks by. Wide multipliers keep
			// the tiers from bleeding into each other.
			rate[st.PlayerID] = st.Wins*1_000_000 + st.PointDiff*1_000 + st.PointsFor
		}
		sort.SliceStable(pairs, func(i, j int) bool {
			return pairScore(pairs[i], rate) > pairScore(pairs[j], rate)
		})
		sides = pairs
	}
	return sides, nil
}

// maybeSeedPlayoff fills the medal-bracket skeleton's semifinals from standings
// once every pool match in the division is complete. No-op when the pools
// aren't done, there's no skeleton, or it's already seeded.
func (s *Service) maybeSeedPlayoff(bracketID string) error {
	b, err := s.sb.SelectOne("brackets", "id=eq."+store.Q(bracketID)+"&select=event_id")
	if err != nil {
		return err
	}
	if b == nil {
		return ErrNotFound
	}
	eventID := asStr(b, "event_id")
	total, open, err := s.poolProgress(bracketID)
	if err != nil {
		return err
	}
	if total == 0 || open > 0 {
		return nil
	}
	// Locate the skeleton semifinals (round 1).
	semis, err := s.sb.Select("matches",
		"bracket_id=eq."+store.Q(bracketID)+"&stage=eq.bracket&bracket_round=eq.1&select=id,bracket_slot&order=bracket_slot")
	if err != nil {
		return err
	}
	semiBySlot := map[int]string{}
	for _, m := range semis {
		semiBySlot[asInt(m, "bracket_slot")] = asStr(m, "id")
	}
	sf1, ok1 := semiBySlot[0]
	sf2, ok2 := semiBySlot[1]
	if !ok1 || !ok2 {
		return nil // no skeleton (e.g. fewer than 4 teams)
	}
	// Already seeded? A pool re-score can change the standings, so re-seed — but
	// ONLY while the playoff is still pristine. Once any semifinal is underway or
	// played we leave the bracket alone (the organizer regenerates manually)
	// rather than silently yanking teams out of a live playoff.
	seeded, err := s.sb.Select("match_participants", "match_id=eq."+store.Q(sf1)+"&select=match_id")
	if err != nil {
		return err
	}
	alreadySeeded := len(seeded) > 0
	if alreadySeeded {
		started, err := s.bracketStarted(bracketID)
		if err != nil {
			return err
		}
		if started {
			return nil
		}
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return err
	}
	sides, err := s.seedTopTeams(ev, eventID, bracketID)
	if err != nil {
		return err
	}
	if len(sides) < 4 {
		return nil
	}
	// Drop any stale seeds before writing the fresh ones (re-seed path).
	if alreadySeeded {
		for _, sf := range []string{sf1, sf2} {
			if err := s.sb.Delete("match_participants", "match_id=eq."+store.Q(sf)); err != nil {
				return err
			}
		}
	}
	// SF1: seed 1 vs seed 4 ; SF2: seed 2 vs seed 3.
	if err := s.insertSide(sf1, 1, sides[0]); err != nil {
		return err
	}
	if err := s.insertSide(sf1, 2, sides[3]); err != nil {
		return err
	}
	if err := s.insertSide(sf2, 1, sides[1]); err != nil {
		return err
	}
	return s.insertSide(sf2, 2, sides[2])
}

// bracketStarted reports whether any of a division's playoff matches are already
// underway or completed (so a re-seed would disturb live play).
func (s *Service) bracketStarted(bracketID string) (bool, error) {
	rows, err := s.sb.Select("matches",
		"bracket_id=eq."+store.Q(bracketID)+"&stage=eq.bracket&select=status")
	if err != nil {
		return false, err
	}
	for _, m := range rows {
		switch asStr(m, "status") {
		case "in_progress", "completed":
			return true, nil
		}
	}
	return false, nil
}

// ----------------------------------------------------------- scoring
// RecordSeries validates a best-of-N result for a match against the event's
// format (points_to_win / win_by / best_of) and writes it. games holds the
// per-game scores; the winner is decided by games won.
func (s *Service) RecordSeries(matchID string, games []model.GameScore) error {
	// Defaults (11, win by 2, single game) apply if the event predates a column.
	ptw, winBy, bestOf := 11, 2, 1
	fmtRow, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=event:events!event_id(points_to_win,win_by,best_of)")
	if err != nil {
		return err
	}
	if fmtRow == nil {
		return ErrNotFound
	}
	if ev, ok := fmtRow["event"].(map[string]any); ok {
		if g := asInt(ev, "points_to_win"); g > 0 {
			ptw = g
		}
		if w := asInt(ev, "win_by"); w > 0 {
			winBy = w
		}
		if b := asInt(ev, "best_of"); b > 0 {
			bestOf = b
		}
	}
	winner, t1, t2, err := validateSeries(games, bestOf, ptw, winBy)
	if err != nil {
		return err
	}
	return s.applySeries(matchID, games, winner, t1, t2)
}

// RecordScore records a single-game result (the best-of-1 / legacy path).
func (s *Service) RecordScore(matchID string, t1, t2 int) error {
	return s.RecordSeries(matchID, []model.GameScore{{Team1: t1, Team2: t2}})
}

// applySeries writes a validated result — per-game scores, the total points each
// side scored (so standings differential stays correct), and the series winner —
// marks the match completed and runs advancement. It does NOT validate; callers
// that already trust the result (demo seeding via applyScore) use it directly.
func (s *Service) applySeries(matchID string, games []model.GameScore, winner, t1total, t2total int) error {
	out, err := s.sb.Update("matches", "id=eq."+store.Q(matchID), map[string]any{
		"team1_score": t1total, "team2_score": t2total, "winning_team": winner,
		"games":  games,
		"status": "completed", "completed_at": now(), "result_type": "normal",
		// A real played result always counts toward differential — also resets a
		// match that had previously been a forfeit/walkover (counts_for_diff=false).
		"counts_for_diff": true,
	})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return ErrNotFound
	}
	// The updated row (return=representation) carries the routing columns.
	return s.advanceAfterScore(out[0])
}

// applyScore writes a single-game result without format validation (demo seeding).
func (s *Service) applyScore(matchID string, t1, t2 int) error {
	winner := 1
	if t2 > t1 {
		winner = 2
	}
	return s.applySeries(matchID, []model.GameScore{{Team1: t1, Team2: t2}}, winner, t1, t2)
}

// advanceAfterScore runs the post-completion routing for a just-finished match
// (the updated row carries the routing columns): advance the winner and drop
// the loser in medal play, auto-seed the playoff when pools complete, and queue
// DUPR submissions for sanctioned events. Shared by RecordScore and
// ForfeitMatch so a forfeit advances brackets exactly like a played result.
func (s *Service) advanceAfterScore(m map[string]any) error {
	matchID := asStr(m, "id")
	winner := asInt(m, "winning_team")
	stage := asStr(m, "stage")
	eventID := asStr(m, "event_id")
	if stage == "bracket" {
		loser := 3 - winner
		if fm := asStrPtr(m, "feeds_match_id"); fm != nil {
			if err := s.advanceTeam(matchID, winner, *fm, asInt(m, "feeds_slot")); err != nil {
				return err
			}
		}
		if lm := asStrPtr(m, "loser_feeds_match_id"); lm != nil {
			if err := s.advanceTeam(matchID, loser, *lm, asInt(m, "loser_feeds_slot")); err != nil {
				return err
			}
		}
		// Double-elim grand final: the WB champion (team 1) is undefeated, so if
		// they win game 1 they're champion; if the LB champion (team 2) wins, both
		// have one loss and a deciding RESET game is played.
		if asStr(m, "bracket_tier") == "grand_final" && asInt(m, "bracket_round") == 1 {
			if err := s.resolveGrandFinal(m); err != nil {
				return err
			}
		}
	}
	if stage == "pool" {
		if bc := asStrPtr(m, "bracket_id"); bc != nil {
			if err := s.maybeSeedPlayoff(*bc); err != nil {
				return err
			}
		}
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=dupr_sanctioned")
	if err != nil {
		return err
	}
	// Only real, played results are eligible for DUPR — forfeits, retirements and
	// walkovers aren't submitted (no genuine head-to-head score).
	if rt := asStr(m, "result_type"); ev != nil && asBool(ev, "dupr_sanctioned") && (rt == "" || rt == "normal") {
		if err := s.queueDuprSubmission(matchID, eventID); err != nil {
			return err
		}
	}
	// Flip the event to "completed" once nothing is left to play (so it stops
	// showing a live badge), or back to "in_progress" if a re-score/undo reopens
	// a match. Best-effort — never fail a recorded result over a status sync.
	s.syncEventStatus(eventID)
	return nil
}

// resolveGrandFinal applies the "if necessary" rule after grand-final game 1.
// gf1's team 1 is the winners-bracket champion (undefeated); team 2 is the
// losers-bracket champion (one loss). If team 1 wins, they're the champion and
// no reset is played; if team 2 wins, a deciding reset game (round 2) with the
// same two teams is created. Re-scoring game 1 reconciles the reset game.
func (s *Service) resolveGrandFinal(gf1 map[string]any) error {
	eventID := asStr(gf1, "event_id")
	// Scope the reset lookup to THIS bracket: separate divisions in one event each
	// have their own grand_final round 2, all sharing the same tier+round, so an
	// event-only query would collide across divisions.
	bid := asStr(gf1, "bracket_id")
	if bid == "" {
		return errors.New("grand final has no bracket_id; cannot resolve the reset game")
	}
	winner := asInt(gf1, "winning_team")
	q := "event_id=eq." + store.Q(eventID) +
		"&bracket_id=eq." + store.Q(bid) +
		"&bracket_tier=eq.grand_final&bracket_round=eq.2&select=id,status&limit=1"
	existing, err := s.sb.Select("matches", q)
	if err != nil {
		return err
	}

	if winner == 1 {
		// WB champion took it — no reset is played, so any round-2 reset must go,
		// even if it was already played (a corrected game-1 result makes it moot).
		if len(existing) > 0 {
			id := asStr(existing[0], "id")
			if err := s.sb.Delete("match_participants", "match_id=eq."+store.Q(id)); err != nil {
				return err
			}
			return s.sb.Delete("matches", "id=eq."+store.Q(id))
		}
		return nil
	}

	// LB champion forced a reset — create game 2 (once) with the same two teams.
	var gf2ID string
	if len(existing) > 0 {
		gf2ID = asStr(existing[0], "id")
	} else {
		// bid validated above — the reset shares the bracket so the UI (which
		// filters by bracket_id) shows it.
		row := map[string]any{
			"event_id": eventID, "bracket_id": bid, "stage": "bracket",
			"bracket_tier": "grand_final", "bracket_round": 2, "bracket_slot": 0,
			"status": "scheduled",
		}
		out, err := s.sb.Insert("matches", row)
		if err != nil {
			return err
		}
		if len(out) == 0 {
			return errors.New("reset game insert returned no row")
		}
		gf2ID = asStr(out[0], "id")
	}
	return s.copyGrandFinalTeams(asStr(gf1, "id"), gf2ID)
}

// copyGrandFinalTeams mirrors game 1's two sides into the reset game (same teams,
// same team numbers). It no-ops when the reset already holds those exact sides,
// so a re-score doesn't briefly empty the reset's roster for a concurrent reader.
func (s *Service) copyGrandFinalTeams(fromID, toID string) error {
	parts, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(fromID)+"&select=player_id,team")
	if err != nil {
		return err
	}
	cur, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(toID)+"&select=player_id,team")
	if err != nil {
		return err
	}
	if sameParticipants(parts, cur) {
		return nil
	}
	if err := s.sb.Delete("match_participants", "match_id=eq."+store.Q(toID)); err != nil {
		return err
	}
	if len(parts) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		rows = append(rows, map[string]any{
			"match_id": toID, "player_id": asStr(p, "player_id"), "team": asInt(p, "team"),
		})
	}
	_, err = s.sb.Upsert("match_participants", "match_id,player_id", rows)
	return err
}

// sameParticipants reports whether two participant rows describe the same set of
// (player_id, team) pairs.
func sameParticipants(a, b []map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(p map[string]any) string {
		return asStr(p, "player_id") + ":" + strconv.Itoa(asInt(p, "team"))
	}
	set := make(map[string]bool, len(a))
	for _, p := range a {
		set[key(p)] = true
	}
	for _, p := range b {
		if !set[key(p)] {
			return false
		}
	}
	return true
}

// syncEventStatus reconciles events.status with its matches: completed when no
// match is unfinished, in_progress if a completed event has a match reopened.
func (s *Service) syncEventStatus(eventID string) {
	open, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&status=neq.completed&select=id&limit=1")
	if err != nil {
		return
	}
	if len(open) == 0 {
		_, _ = s.sb.Update("events",
			"id=eq."+store.Q(eventID)+"&status=neq.completed",
			map[string]any{"status": "completed"})
		return
	}
	// A match is unfinished — make sure a previously-completed event reopens.
	_, _ = s.sb.Update("events",
		"id=eq."+store.Q(eventID)+"&status=eq.completed",
		map[string]any{"status": "in_progress"})
}

// ForfeitMatch resolves a match with no played score — a no-show forfeit, a
// mid-match retirement, or a walkover. The winning team is credited a
// conventional win (points_to_win to 0); kind labels how it ended. Bracket
// advancement and playoff seeding run exactly as for a normal result.
func (s *Service) ForfeitMatch(matchID string, winningTeam int, kind string, t1Score, t2Score *int) error {
	if winningTeam != 1 && winningTeam != 2 {
		return errors.New("winning team must be 1 or 2")
	}
	if kind == "" {
		kind = "forfeit"
	}
	if kind != "forfeit" && kind != "retire" && kind != "walkover" {
		return errors.New("result type must be forfeit, retire, or walkover")
	}
	ptw := 11
	if row, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=event:events!event_id(points_to_win)"); err != nil {
		return err
	} else if row == nil {
		return ErrNotFound
	} else if ev, ok := row["event"].(map[string]any); ok {
		if g := asInt(ev, "points_to_win"); g > 0 {
			ptw = g
		}
	}
	// A retirement keeps the actual partial score (and counts toward point
	// differential). Forfeits/walkovers fabricate a conventional points_to_win-0
	// win that is excluded from differential.
	t1, t2 := ptw, 0
	if winningTeam == 2 {
		t1, t2 = 0, ptw
	}
	countsForDiff := false
	if kind == "retire" && t1Score != nil && t2Score != nil {
		t1, t2, countsForDiff = *t1Score, *t2Score, true
	}
	out, err := s.sb.Update("matches", "id=eq."+store.Q(matchID), map[string]any{
		"team1_score": t1, "team2_score": t2, "winning_team": winningTeam,
		// Clear any per-game scores from a prior played result — a forfeit/retire/
		// walkover isn't a series, so the UI must not show game-by-game results.
		"games":  nil,
		"status": "completed", "completed_at": now(), "result_type": kind,
		"counts_for_diff": countsForDiff,
	})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return ErrNotFound
	}
	return s.advanceAfterScore(out[0])
}

// DuprImportSummary is the result of flushing queued results to DUPR.
type DuprImportSummary struct {
	Submitted int `json:"submitted"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// VerifyAdminPasscode gates the coordinator scoring page. No passcode = open.
func (s *Service) VerifyAdminPasscode(eventID, code string) (bool, error) {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=admin_passcode")
	if err != nil {
		return false, err
	}
	if ev == nil {
		return false, ErrNotFound
	}
	pass := asStr(ev, "admin_passcode")
	if pass == "" {
		return true, nil
	}
	return pass == strings.TrimSpace(code), nil
}

// CollectPayment charges the registration fee via the payment gateway. (#4)
func (s *Service) CollectPayment(registrationID, provider string) (bool, error) {
	if provider == "" {
		provider = "manual"
	}
	reg, err := s.sb.SelectOne("registrations", "id=eq."+store.Q(registrationID)+"&select=event_id")
	if err != nil {
		return false, err
	}
	if reg == nil {
		return false, ErrNotFound
	}
	eventID := asStr(reg, "event_id")
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=registration_fee_cents,currency")
	if err != nil {
		return false, err
	}
	if ev == nil {
		return false, ErrNotFound
	}
	fee := asInt(ev, "registration_fee_cents")
	currency := asStr(ev, "currency")

	// Free registration — nothing to charge, confirm immediately.
	if fee <= 0 {
		return s.recordPayment(registrationID, "free", "", 0, currency, "paid", "paid")
	}

	// Fee-bearing, but no real payment processor is wired up (the mock always
	// "succeeds"). Marking the registration paid here would let anyone
	// self-confirm payment from the public endpoint, so instead record a pending
	// intent. The organizer confirms receipt via the owner-only mark-paid action
	// (CollectPaymentManually), or a real gateway's webhook once one is added.
	if !s.Pay.Live() {
		return s.recordPayment(registrationID, provider, "", fee, currency, "pending", "pending")
	}

	res, err := s.Pay.Charge(registrationID, fee, currency, provider)
	if err != nil {
		return false, err
	}
	if res.OK {
		return s.recordPayment(registrationID, provider, res.ProviderRef, fee, currency, "paid", "paid")
	}
	return s.recordPayment(registrationID, provider, "", fee, currency, "failed", "pending")
}

// CollectPaymentManually is the organizer's owner-only confirmation that a
// fee-bearing registration was paid out of band (cash, e-transfer, etc.). It
// marks the registration paid without going through the (mock) gateway.
func (s *Service) CollectPaymentManually(registrationID string) error {
	reg, err := s.sb.SelectOne("registrations", "id=eq."+store.Q(registrationID)+"&select=event_id")
	if err != nil {
		return err
	}
	if reg == nil {
		return ErrNotFound
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(asStr(reg, "event_id"))+"&select=registration_fee_cents,currency")
	if err != nil {
		return err
	}
	fee, currency := 0, "usd"
	if ev != nil {
		fee, currency = asInt(ev, "registration_fee_cents"), asStr(ev, "currency")
	}
	_, err = s.recordPayment(registrationID, "manual", "", fee, currency, "paid", "paid")
	return err
}

// recordPayment writes a payments row and syncs the registration's
// payment_status. Returns whether the registration ended up paid.
func (s *Service) recordPayment(registrationID, provider, ref string, fee int, currency, payStatus, regStatus string) (bool, error) {
	var refVal, paidAt any
	if ref != "" {
		refVal = ref
	}
	if payStatus == "paid" {
		paidAt = now()
	}
	if _, err := s.sb.Insert("payments", map[string]any{
		"registration_id": registrationID,
		"provider":        provider,
		"provider_ref":    refVal,
		"amount_cents":    fee,
		"currency":        currency,
		"status":          payStatus,
		"paid_at":         paidAt,
	}); err != nil {
		return false, err
	}
	if _, err := s.sb.Update("registrations", "id=eq."+store.Q(registrationID),
		map[string]any{"payment_status": regStatus}); err != nil {
		return false, err
	}
	return regStatus == "paid", nil
}

// SaveShirtOrder creates or updates the (optional) tournament-shirt order a
// player picks after registering. One order per registration.
func (s *Service) SaveShirtOrder(registrationID string, req model.ShirtRequest) (model.ShirtOrder, error) {
	if strings.TrimSpace(req.Size) == "" {
		return model.ShirtOrder{}, errors.New("shirt size is required")
	}
	r, err := s.sb.SelectOne("registrations", "id=eq."+store.Q(registrationID)+"&select=id")
	if err != nil {
		return model.ShirtOrder{}, err
	}
	if r == nil {
		return model.ShirtOrder{}, ErrNotFound
	}

	existing, err := s.sb.SelectOne("shirt_orders", "registration_id=eq."+store.Q(registrationID)+"&select=id")
	if err != nil {
		return model.ShirtOrder{}, err
	}
	fields := map[string]any{
		"size":          req.Size,
		"name_on_shirt": orNull(req.NameOnShirt),
		"number":        orNull(req.Number),
		"color":         orNull(req.Color),
	}
	var id string
	if existing == nil {
		fields["registration_id"] = registrationID
		out, ierr := s.sb.Insert("shirt_orders", fields)
		if ierr != nil {
			return model.ShirtOrder{}, ierr
		}
		if len(out) > 0 {
			id = asStr(out[0], "id")
		}
	} else {
		id = asStr(existing, "id")
		if _, uerr := s.sb.Update("shirt_orders", "id=eq."+store.Q(id), fields); uerr != nil {
			return model.ShirtOrder{}, uerr
		}
	}
	return model.ShirtOrder{
		ID: id, RegistrationID: registrationID, Size: req.Size,
		NameOnShirt: req.NameOnShirt, Number: req.Number, Color: req.Color,
		Status: "requested",
	}, nil
}

// ---------------------------------------------------------- finances

var financeKinds = map[string]bool{"income": true, "expense": true}

// AddFinanceEntry records an income or expense line for an event's ledger.
func (s *Service) AddFinanceEntry(eventID string, req model.FinanceEntryRequest) (model.FinanceEntry, error) {
	if !financeKinds[req.Kind] {
		return model.FinanceEntry{}, errors.New("kind must be 'income' or 'expense'")
	}
	category := strings.TrimSpace(req.Category)
	if category == "" {
		return model.FinanceEntry{}, errors.New("category is required")
	}
	if req.AmountCents <= 0 {
		return model.FinanceEntry{}, errors.New("amount must be greater than zero")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=id")
	if err != nil {
		return model.FinanceEntry{}, err
	}
	if ev == nil {
		return model.FinanceEntry{}, ErrNotFound
	}
	out, err := s.sb.Insert("finance_entries", map[string]any{
		"event_id":     eventID,
		"kind":         req.Kind,
		"category":     category,
		"amount_cents": req.AmountCents,
		"note":         strings.TrimSpace(req.Note),
	})
	if err != nil {
		return model.FinanceEntry{}, err
	}
	if len(out) == 0 {
		return model.FinanceEntry{}, errors.New("insert returned no row")
	}
	return mapFinanceEntry(out[0]), nil
}

// FinanceEntries lists an event's ledger lines, newest first.
func (s *Service) FinanceEntries(eventID string) ([]model.FinanceEntry, error) {
	rows, err := s.sb.Select("finance_entries",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=created_at.desc,id.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.FinanceEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapFinanceEntry(r))
	}
	return out, nil
}

// DeleteFinanceEntry removes a ledger line (idempotent).
func (s *Service) DeleteFinanceEntry(id string) error {
	return s.sb.Delete("finance_entries", "id=eq."+store.Q(id))
}

// ---------------------------------------------------------- checklist

// defaultChecklist is the common tournament-day must-haves a new event's
// checklist is seeded with on first access.
var defaultChecklist = []string{
	"Monitor / scoreboard screen",
	"Tables",
	"Chairs",
	"First aid kit",
	"Water coolers / hydration",
	"Extra pickleballs",
	"Portable nets",
	"Scorecards & clipboards",
	"Pens & markers",
	"Sound system / speaker",
	"Extension cords & power",
	"Canopy / shade tent",
	"Signage & banners",
	"Trash bags",
	"Hand sanitizer",
	"Cash box / payment terminal",
	"Name tags / wristbands",
	"Prizes / medals",
}

// Checklist returns an event's prep checklist, seeding the common must-haves the
// first time it's opened so a new event starts with a useful default list.
func (s *Service) Checklist(eventID string) ([]model.ChecklistItem, error) {
	items, err := s.listChecklist(eventID)
	if err != nil {
		return nil, err
	}
	if len(items) > 0 {
		return items, nil
	}
	// Empty — seed the default must-haves (after confirming the event exists).
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=id")
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, ErrNotFound
	}
	seed := make([]map[string]any, 0, len(defaultChecklist))
	for i, label := range defaultChecklist {
		seed = append(seed, map[string]any{
			"event_id": eventID, "label": label, "checked": false, "sort_order": i,
		})
	}
	if _, err := s.sb.Insert("checklist_items", seed); err != nil {
		return nil, err
	}
	return s.listChecklist(eventID)
}

func (s *Service) listChecklist(eventID string) ([]model.ChecklistItem, error) {
	rows, err := s.sb.Select("checklist_items",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=sort_order,created_at")
	if err != nil {
		return nil, err
	}
	out := make([]model.ChecklistItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapChecklistItem(r))
	}
	return out, nil
}

// AddChecklistItem appends a custom item to the end of the list.
func (s *Service) AddChecklistItem(eventID, label string) (model.ChecklistItem, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return model.ChecklistItem{}, errors.New("label is required")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=id")
	if err != nil {
		return model.ChecklistItem{}, err
	}
	if ev == nil {
		return model.ChecklistItem{}, ErrNotFound
	}
	order := 0
	last, err := s.sb.Select("checklist_items",
		"event_id=eq."+store.Q(eventID)+"&select=sort_order&order=sort_order.desc&limit=1")
	if err != nil {
		return model.ChecklistItem{}, err
	}
	if len(last) > 0 {
		order = asInt(last[0], "sort_order") + 1
	}
	out, err := s.sb.Insert("checklist_items", map[string]any{
		"event_id": eventID, "label": label, "checked": false, "sort_order": order,
	})
	if err != nil {
		return model.ChecklistItem{}, err
	}
	if len(out) == 0 {
		return model.ChecklistItem{}, errors.New("insert returned no row")
	}
	return mapChecklistItem(out[0]), nil
}

// SetChecklistChecked sets an item's checked state.
func (s *Service) SetChecklistChecked(id string, checked bool) error {
	out, err := s.sb.Update("checklist_items", "id=eq."+store.Q(id),
		map[string]any{"checked": checked})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteChecklistItem removes an item (idempotent).
func (s *Service) DeleteChecklistItem(id string) error {
	return s.sb.Delete("checklist_items", "id=eq."+store.Q(id))
}

// ------------------------------------------------------------------- Feed

// AddFeedItem appends one activity row to a tournament's feed. BEST-EFFORT: a
// feed write must never break the action that triggered it, so any error is
// logged and swallowed.
func (s *Service) AddFeedItem(eventID, typ, text, refID string) {
	if eventID == "" || text == "" {
		return
	}
	if _, err := s.sb.Insert("feed_items", map[string]any{
		"event_id": eventID,
		"type":     typ,
		"text":     text,
		"ref_id":   orNull(refID),
	}); err != nil {
		log.Printf("feed: add %q for %s failed: %v", typ, eventID, err)
	}
}

// ListFeed returns an event's feed, newest first.
// ListFeed returns an event's feed (newest first) enriched with reaction counts,
// the caller's own reactions (callerID may be "" for anonymous), and comment
// counts.
func (s *Service) ListFeed(eventID, callerID string) ([]model.FeedItem, error) {
	rows, err := s.sb.Select("feed_items",
		"event_id=eq."+store.Q(eventID)+"&select=*&order=created_at.desc&limit=100")
	if err != nil {
		return nil, err
	}
	out := make([]model.FeedItem, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		fi := mapFeedItem(r)
		fi.ReactionCounts = map[string]int{}
		fi.MyReactions = []string{}
		out = append(out, fi)
		ids = append(ids, fi.ID)
	}
	if len(ids) > 0 {
		s.attachSocial(out, ids, callerID)
	}
	return out, nil
}

// attachSocial fills ReactionCounts/MyReactions/CommentCount for a set of feed
// items in two batched queries (no N+1; best-effort).
func (s *Service) attachSocial(items []model.FeedItem, ids []string, callerID string) {
	inList := "in.(" + strings.Join(ids, ",") + ")"
	idx := make(map[string]int, len(items))
	for i := range items {
		idx[items[i].ID] = i
	}
	if rows, err := s.sb.Select("feed_reactions",
		"feed_item_id="+inList+"&select=feed_item_id,user_id,type"); err == nil {
		for _, r := range rows {
			i, ok := idx[asStr(r, "feed_item_id")]
			if !ok {
				continue
			}
			typ := asStr(r, "type")
			items[i].ReactionCounts[typ]++
			if callerID != "" && asStr(r, "user_id") == callerID {
				items[i].MyReactions = append(items[i].MyReactions, typ)
			}
		}
	}
	if rows, err := s.sb.Select("feed_comments",
		"feed_item_id="+inList+"&select=feed_item_id"); err == nil {
		for _, r := range rows {
			if i, ok := idx[asStr(r, "feed_item_id")]; ok {
				items[i].CommentCount++
			}
		}
	}
}

var validReactions = map[string]bool{"like": true, "love": true, "fire": true}

// ToggleReaction adds or removes the caller's reaction of typ on a feed item and
// returns the new reacted state + per-type counts.
func (s *Service) ToggleReaction(feedItemID, userID, typ string) (model.ReactionResult, error) {
	if userID == "" {
		return model.ReactionResult{}, errors.New("sign in to react")
	}
	if !validReactions[typ] {
		return model.ReactionResult{}, errors.New("unknown reaction type")
	}
	q := "feed_item_id=eq." + store.Q(feedItemID) + "&user_id=eq." + store.Q(userID) +
		"&type=eq." + store.Q(typ)
	existing, err := s.sb.Select("feed_reactions", q+"&select=id")
	if err != nil {
		return model.ReactionResult{}, err
	}
	reacted := false
	if len(existing) > 0 {
		if err := s.sb.Delete("feed_reactions", q); err != nil {
			return model.ReactionResult{}, err
		}
	} else {
		if _, err := s.sb.Insert("feed_reactions", map[string]any{
			"feed_item_id": feedItemID,
			"user_id":      userID,
			"type":         typ,
		}); err != nil {
			return model.ReactionResult{}, err
		}
		reacted = true
	}
	counts := map[string]int{}
	if rows, err := s.sb.Select("feed_reactions",
		"feed_item_id=eq."+store.Q(feedItemID)+"&select=type"); err == nil {
		for _, r := range rows {
			counts[asStr(r, "type")]++
		}
	}
	return model.ReactionResult{Reacted: reacted, Counts: counts}, nil
}

// ListComments returns a feed item's comments (oldest first), flagging which the
// caller may delete (own comments, or any if they own the event).
func (s *Service) ListComments(feedItemID, callerID string) ([]model.FeedComment, error) {
	owner := s.feedItemOwner(feedItemID)
	rows, err := s.sb.Select("feed_comments",
		"feed_item_id=eq."+store.Q(feedItemID)+"&select=*&order=created_at.asc&limit=500")
	if err != nil {
		return nil, err
	}
	out := make([]model.FeedComment, 0, len(rows))
	for _, r := range rows {
		c := mapFeedComment(r)
		mine := callerID != "" && asStr(r, "user_id") == callerID
		c.Mine = mine
		c.CanDelete = mine || (callerID != "" && callerID == owner)
		out = append(out, c)
	}
	return out, nil
}

// AddComment posts a comment as the signed-in user; the display name resolves to
// their linked player name, else their email's local part.
func (s *Service) AddComment(feedItemID, userID, email, text string) (model.FeedComment, error) {
	if userID == "" {
		return model.FeedComment{}, errors.New("sign in to comment")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return model.FeedComment{}, errors.New("comment text is required")
	}
	if len(text) > 1000 {
		text = text[:1000]
	}
	rows, err := s.sb.Insert("feed_comments", map[string]any{
		"feed_item_id": feedItemID,
		"user_id":      userID,
		"author_name":  s.resolveDisplayName(userID, email),
		"text":         text,
	})
	if err != nil {
		return model.FeedComment{}, err
	}
	if len(rows) == 0 {
		return model.FeedComment{}, errors.New("comment insert returned no row")
	}
	c := mapFeedComment(rows[0])
	c.Mine = true
	c.CanDelete = true
	return c, nil
}

// DeleteComment removes a comment if the caller authored it OR owns the event.
func (s *Service) DeleteComment(commentID, userID string) error {
	if userID == "" {
		return ErrForbidden
	}
	row, err := s.sb.SelectOne("feed_comments",
		"id=eq."+store.Q(commentID)+"&select=user_id,feed_item_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "user_id") != userID {
		if owner := s.feedItemOwner(asStr(row, "feed_item_id")); owner == "" || owner != userID {
			return ErrForbidden
		}
	}
	return s.sb.Delete("feed_comments", "id=eq."+store.Q(commentID))
}

// feedItemOwner returns the auth-user id that owns the event behind a feed item,
// or "" if it can't be resolved.
func (s *Service) feedItemOwner(feedItemID string) string {
	row, err := s.sb.SelectOne("feed_items", "id=eq."+store.Q(feedItemID)+"&select=event_id")
	if err != nil || row == nil {
		return ""
	}
	owner, _ := s.OwnerOf("event", asStr(row, "event_id"))
	return owner
}

// resolveDisplayName picks a friendly author name for a commenter: their linked
// player's full name, else the email's local part, else "Player".
func (s *Service) resolveDisplayName(userID, email string) string {
	if row, err := s.sb.SelectOne("players",
		"user_id=eq."+store.Q(userID)+"&select=full_name&limit=1"); err == nil && row != nil {
		if n := strings.TrimSpace(asStr(row, "full_name")); n != "" {
			return n
		}
	}
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return "Player"
}

// MyProfile returns the signed-in user's saved player details (from their linked
// player row, if any) so the registration form can pre-fill. Email always comes
// from the verified token; missing a player row just yields an email-only
// profile. Best-effort — a lookup error still returns the email.
func (s *Service) MyProfile(userID, email string) model.Profile {
	p := model.Profile{Email: email}
	if userID == "" {
		return p
	}
	row, err := s.sb.SelectOne("players",
		"user_id=eq."+store.Q(userID)+
			"&select=full_name,phone,email,dupr_id,dupr_rating,skill_level&limit=1")
	if err != nil || row == nil {
		return p
	}
	p.FullName = asStr(row, "full_name")
	p.Phone = asStr(row, "phone")
	if e := asStr(row, "email"); e != "" {
		p.Email = e
	}
	p.DuprID = asStr(row, "dupr_id")
	p.DuprRating = asFloatPtr(row, "dupr_rating")
	p.SkillLevel = asFloatPtr(row, "skill_level")
	return p
}

// AccountExists reports whether a Supabase auth account already exists for the
// email (used to tailor the post-registration nudge: sign in vs sign up). Calls
// the locked-down account_exists RPC. BEST-EFFORT — any error returns false, so
// we just fall back to the "create account" path.
func (s *Service) AccountExists(email string) bool {
	email = strings.TrimSpace(email)
	if email == "" {
		return false
	}
	body, err := s.sb.RPC("account_exists", map[string]any{"p_email": email})
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) == "true"
}

// PostAnnouncement adds an organizer announcement to the feed.
func (s *Service) PostAnnouncement(eventID, text, actorName string) (model.FeedItem, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return model.FeedItem{}, errors.New("post text is required")
	}
	if len(text) > 1000 {
		text = text[:1000]
	}
	rows, err := s.sb.Insert("feed_items", map[string]any{
		"event_id":   eventID,
		"type":       "announcement",
		"text":       text,
		"actor_name": orNull(actorName),
	})
	if err != nil {
		return model.FeedItem{}, err
	}
	if len(rows) == 0 {
		return model.FeedItem{}, errors.New("feed insert returned no row")
	}
	return mapFeedItem(rows[0]), nil
}

func (s *Service) DeleteFeedItem(id string) error {
	return s.sb.Delete("feed_items", "id=eq."+store.Q(id))
}

// MatchFeedText loads a match and composes the feed line for it going live or
// finishing. Returns the event id (so the caller can file the feed item) and
// the rendered text; either is "" if the match can't be read. Plain text only —
// the UI adds the type icon.
func (s *Service) MatchFeedText(matchID string, final bool) (eventID, text string) {
	row, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=event_id,"+matchSelect)
	if err != nil || row == nil {
		return "", ""
	}
	eventID = asStr(row, "event_id")
	m := mapMatch(row)
	team := func(t int) string {
		for _, sd := range m.Sides {
			if sd.Team == t {
				if n := strings.Join(sd.Players, " & "); n != "" {
					return n
				}
			}
		}
		return "TBD"
	}
	a, b := team(1), team(2)
	if !final {
		court := "a court"
		if m.CourtNumber != nil {
			court = fmt.Sprintf("Court %d", *m.CourtNumber)
		}
		return eventID, fmt.Sprintf("Now live on %s — %s vs %s", court, a, b)
	}
	wt := 0
	if m.WinningTeam != nil {
		wt = *m.WinningTeam
	}
	winner, loser := a, b
	if wt == 2 {
		winner, loser = b, a
	}
	s1, s2 := 0, 0
	if m.Team1Score != nil {
		s1 = *m.Team1Score
	}
	if m.Team2Score != nil {
		s2 = *m.Team2Score
	}
	hi, lo := s1, s2
	if s2 > s1 {
		hi, lo = s2, s1
	}
	switch m.ResultType {
	case "forfeit":
		return eventID, fmt.Sprintf("%s win — %s forfeited", winner, loser)
	case "walkover":
		return eventID, fmt.Sprintf("%s advance on a walkover", winner)
	case "retire":
		return eventID, fmt.Sprintf("%s def. %s %d–%d (%s retired)", winner, loser, hi, lo, loser)
	}
	if wt == 0 {
		return eventID, fmt.Sprintf("Final: %s vs %s, %d–%d", a, b, s1, s2)
	}
	return eventID, fmt.Sprintf("%s def. %s, %d–%d", winner, loser, hi, lo)
}

// CheckIn marks a registration checked in. (#1)
// CheckIn marks a registration checked in and reports whether this was a NEW
// check-in (false if it was already checked in), so callers can post a one-time
// feed update without duplicating it on a re-scan. (#1)
func (s *Service) CheckIn(registrationID, method string) (bool, error) {
	if method == "" {
		method = "manual"
	}
	prior, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=checked_in")
	if err != nil {
		return false, err
	}
	if prior == nil {
		return false, ErrNotFound
	}
	already := asBool(prior, "checked_in")
	out, err := s.sb.Update("registrations", "id=eq."+store.Q(registrationID), map[string]any{
		"checked_in":      true,
		"checked_in_at":   now(),
		"check_in_method": method,
	})
	if err != nil {
		return false, err
	}
	if len(out) == 0 {
		return false, ErrNotFound
	}
	return !already, nil
}

// CheckinFeedText composes the feed line for a check-in ("<name> checked in").
// Returns the event id + text, both "" if the registration can't be read.
func (s *Service) CheckinFeedText(registrationID string) (eventID, text string) {
	row, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=event_id,player:players!player_id(full_name)")
	if err != nil || row == nil {
		return "", ""
	}
	name := "A player"
	if p := asMap(row, "player"); p != nil {
		if n := strings.TrimSpace(asStr(p, "full_name")); n != "" {
			name = n
		}
	}
	return asStr(row, "event_id"), name + " checked in"
}

// UncheckIn reverses a check-in (e.g. checked in by mistake), clearing the
// timestamp + method.
func (s *Service) UncheckIn(registrationID string) error {
	out, err := s.sb.Update("registrations", "id=eq."+store.Q(registrationID), map[string]any{
		"checked_in":      false,
		"checked_in_at":   nil,
		"check_in_method": nil,
	})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return ErrNotFound
	}
	return nil
}

// CheckInByToken redeems a player's QR/check-in token. Returns the registration id.
func (s *Service) CheckInByToken(eventID, token string) (string, error) {
	row, err := s.sb.SelectOne("registrations",
		"event_id=eq."+store.Q(eventID)+"&check_in_token=eq."+store.Q(token)+"&select=id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	regID := asStr(row, "id")
	changed, err := s.CheckIn(regID, "qr")
	if err == nil && changed {
		if eid, txt := s.CheckinFeedText(regID); txt != "" {
			s.AddFeedItem(eid, "checked_in", txt, regID)
		}
	}
	return regID, err
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normPhone reduces a phone string to comparable digits, dropping a leading
// North-American "1" country code so "+1 (555) 100-0000" and "5551000000"
// compare equal. Used for an EXACT match (not a suffix match) so a short or
// partial number can't be used to fish for other registrants' names.
func normPhone(s string) string {
	d := digitsOnly(s)
	if len(d) == 11 && d[0] == '1' {
		return d[1:]
	}
	return d
}

// CheckInByPhone checks a player in by the phone number they registered with.
// Returns the registration id and the player's display name. Matching is EXACT
// on the normalized number (country-code tolerant): a partial/short number
// never matches, so this can't be used as an oracle to enumerate registrants.
func (s *Service) CheckInByPhone(eventID, phone string) (string, string, error) {
	want := normPhone(phone)
	if len(want) < 10 {
		return "", "", errors.New("enter the full phone number you registered with")
	}
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&select=id,player:players!player_id(full_name,phone)")
	if err != nil {
		return "", "", err
	}
	var matchID, matchName string
	found := false
	for _, r := range rows {
		p := asMap(r, "player")
		if p == nil {
			continue
		}
		have := normPhone(asStr(p, "phone"))
		if have == "" {
			continue
		}
		if have == want {
			matchID, matchName = asStr(r, "id"), asStr(p, "full_name")
			found = true
			break
		}
	}
	if !found {
		return "", "", ErrNotFound
	}
	// "code" is the allowed check_in_method that fits a player self-identifying
	// by entering their phone number (see the registrations CHECK constraint).
	changed, err := s.CheckIn(matchID, "code")
	if err == nil && changed {
		if eid, txt := s.CheckinFeedText(matchID); txt != "" {
			s.AddFeedItem(eid, "checked_in", txt, matchID)
		}
	}
	return matchID, matchName, err
}

// StartRound activates a round and texts each player their court. (#5)
func (s *Service) StartRound(roundID string) (int, error) {
	rd, err := s.sb.SelectOne("rounds", "id=eq."+store.Q(roundID)+"&select=round_number,event_id")
	if err != nil {
		return 0, err
	}
	if rd == nil {
		return 0, ErrNotFound
	}
	roundNumber := asInt(rd, "round_number")
	if _, err := s.sb.Update("rounds", "id=eq."+store.Q(roundID),
		map[string]any{"status": "active", "started_at": now()}); err != nil {
		return 0, err
	}
	// Mark every not-yet-played match in the round as in progress, so starting a
	// whole round behaves like starting each match individually.
	if _, err := s.sb.Update("matches",
		"round_id=eq."+store.Q(roundID)+"&status=eq.scheduled",
		map[string]any{"status": "in_progress"}); err != nil {
		return 0, err
	}

	matches, err := s.sb.Select("matches",
		"round_id=eq."+store.Q(roundID)+"&select=id,event_id,court:courts!court_id(label)")
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, m := range matches {
		court := "your court"
		if c := asMap(m, "court"); c != nil {
			if l := asStr(c, "label"); l != "" {
				court = l
			}
		}
		n, err := s.notifyMatchStart(asStr(m, "id"), asStr(m, "event_id"), court, roundNumber)
		if err != nil {
			return 0, err
		}
		sent += n
	}
	return sent, nil
}

// StartMatch starts a single match (one court): marks it in progress, brings its
// pool round to 'active' if it was still pending, and texts that match's players.
// Returns the number of SMS sent.
func (s *Service) StartMatch(matchID string) (int, error) {
	m, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=event_id,round_id,court:courts!court_id(label),round:rounds!round_id(round_number)")
	if err != nil {
		return 0, err
	}
	if m == nil {
		return 0, ErrNotFound
	}
	eventID := asStr(m, "event_id")
	court := "your court"
	if c := asMap(m, "court"); c != nil {
		if l := asStr(c, "label"); l != "" {
			court = l
		}
	}
	if _, err := s.sb.Update("matches",
		"id=eq."+store.Q(matchID)+"&status=eq.scheduled",
		map[string]any{"status": "in_progress"}); err != nil {
		return 0, err
	}
	// Reflect that play has begun on the parent pool round (if it was pending).
	// (started_at is null on a pending round, so setting it here matches the old
	// COALESCE(started_at, now()).)
	if rid := asStrPtr(m, "round_id"); rid != nil {
		if _, err := s.sb.Update("rounds",
			"id=eq."+store.Q(*rid)+"&status=eq.pending",
			map[string]any{"status": "active", "started_at": now()}); err != nil {
			return 0, err
		}
	}
	rn := 0
	if r := asMap(m, "round"); r != nil {
		rn = asInt(r, "round_number")
	}
	return s.notifyMatchStart(matchID, eventID, court, rn)
}

// UnstartMatch reverts a match that was marked live (in_progress) back to
// scheduled — e.g. the organizer tapped "start" by accident. A completed match
// keeps its result; reset/regenerate the score to undo that instead.
func (s *Service) UnstartMatch(matchID string) error {
	m, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=status")
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	switch asStr(m, "status") {
	case "in_progress":
		_, err := s.sb.Update("matches",
			"id=eq."+store.Q(matchID)+"&status=eq.in_progress",
			map[string]any{"status": "scheduled"})
		return err
	case "completed":
		return errors.New("this match already has a result — reset the score to undo it")
	default:
		return nil // already scheduled — nothing to do
	}
}

// notifyMatchStart texts every player in a match that they're up, recording each
// notification. Returns the count successfully sent.
func (s *Service) notifyMatchStart(matchID, eventID, court string, roundNumber int) (int, error) {
	prows, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(matchID)+"&select=player:players!player_id(phone)")
	if err != nil {
		return 0, err
	}
	var phones []string
	for _, r := range prows {
		if p := asMap(r, "player"); p != nil {
			if ph := asStr(p, "phone"); ph != "" {
				phones = append(phones, ph)
			}
		}
	}

	sent := 0
	for _, phone := range phones {
		// Wording mirrors the registered A2P sample; the STOP footer is required
		// for compliance (the Messaging Service also auto-handles STOP/HELP).
		body := fmt.Sprintf("PlanMyPickle: You're up! Head to %s for round %d. Reply STOP to opt out.", court, roundNumber)
		ins, err := s.sb.Insert("notifications", map[string]any{
			"event_id": eventID, "match_id": matchID, "type": "game_starting",
			"to_address": phone, "body": body,
		})
		if err != nil {
			return 0, err
		}
		if len(ins) == 0 {
			return 0, errors.New("notification insert returned no row")
		}
		notifID := asStr(ins[0], "id")
		r, err := s.Sms.Send(phone, body)
		if err != nil {
			return 0, err
		}
		st := "failed"
		var ref, sentAt any
		if r.OK {
			st, ref, sentAt = "sent", r.ProviderRef, now()
			sent++
		}
		if _, err := s.sb.Update("notifications", "id=eq."+store.Q(notifID), map[string]any{
			"status": st, "provider_ref": ref, "sent_at": sentAt,
		}); err != nil {
			return 0, err
		}
	}
	return sent, nil
}

func (s *Service) queueDuprSubmission(matchID, eventID string) error {
	existing, err := s.sb.SelectOne("dupr_submissions", "match_id=eq."+store.Q(matchID)+"&select=id")
	if err != nil {
		return err
	}
	if existing == nil {
		_, err := s.sb.Insert("dupr_submissions", map[string]any{
			"event_id": eventID, "match_id": matchID,
		})
		return err
	}
	_, err = s.sb.Update("dupr_submissions", "id=eq."+store.Q(asStr(existing, "id")),
		map[string]any{"status": "pending", "error": nil})
	return err
}

// SubmitPendingToDupr flushes queued results to DUPR for a sanctioned event. (#11)
func (s *Service) SubmitPendingToDupr(eventID string) (DuprImportSummary, error) {
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return DuprImportSummary{}, err
	}
	if !ev.DuprSanctioned {
		return DuprImportSummary{}, nil
	}
	duprEventID := ""
	if row, _ := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=dupr_event_id"); row != nil {
		duprEventID = asStr(row, "dupr_event_id")
	}

	pendings, err := s.sb.Select("dupr_submissions",
		"event_id=eq."+store.Q(eventID)+"&status=eq.pending&select=id,match_id")
	if err != nil {
		return DuprImportSummary{}, err
	}

	var sum DuprImportSummary
	for _, p := range pendings {
		subID := asStr(p, "id")
		matchID := asStr(p, "match_id")
		m, err := s.sb.SelectOne("matches",
			"id=eq."+store.Q(matchID)+"&select=team1_score,team2_score,winning_team,result_type,games")
		if err != nil {
			return sum, err
		}
		t1s := asIntPtr(m, "team1_score")
		t2s := asIntPtr(m, "team2_score")
		wt := asIntPtr(m, "winning_team")
		if m == nil || wt == nil || t1s == nil || t2s == nil {
			s.markSubmission(subID, "failed", "", "match not completed")
			sum.Failed++
			continue
		}
		// Forfeits/retirements/walkovers aren't real played results — skip them
		// (belt-and-suspenders; advanceAfterScore no longer queues them).
		if rt := asStr(m, "result_type"); rt != "" && rt != "normal" {
			s.markSubmission(subID, "skipped", "", "not a played result ("+rt+")")
			sum.Skipped++
			continue
		}
		parts, err := s.sb.Select("match_participants",
			"match_id=eq."+store.Q(matchID)+"&select=team,player:players!player_id(dupr_id,full_name)")
		if err != nil {
			return sum, err
		}
		var t1, t2 []string
		missing := ""
		for _, pr := range parts {
			team := asInt(pr, "team")
			did, name := "", ""
			if pl := asMap(pr, "player"); pl != nil {
				did = asStr(pl, "dupr_id")
				name = asStr(pl, "full_name")
			}
			if did == "" {
				missing = name
			}
			if team == 1 {
				t1 = append(t1, did)
			} else {
				t2 = append(t2, did)
			}
		}
		if missing != "" {
			s.markSubmission(subID, "failed", "", "Missing DUPR id for "+missing)
			sum.Failed++
			continue
		}
		// A bye (one side empty) is not a real match — skip rather than submit a
		// one-sided result to DUPR.
		if len(t1) == 0 || len(t2) == 0 {
			s.markSubmission(subID, "skipped", "", "bye / incomplete side")
			sum.Skipped++
			continue
		}
		// Best-of-N: submit each game; the legacy single-game fields carry game 1
		// (team1_score/team2_score are point TOTALS, not a valid single game).
		games := asGames(m, "games")
		g1t1, g1t2 := *t1s, *t2s
		var pairs [][2]int
		for _, gg := range games {
			pairs = append(pairs, [2]int{gg.Team1, gg.Team2})
		}
		if len(games) > 0 {
			g1t1, g1t2 = games[0].Team1, games[0].Team2
		}
		res, err := s.Dupr.SubmitMatch(gateway.DuprPayload{
			EventID: eventID, DuprEventID: duprEventID,
			Team1DuprIDs: t1, Team2DuprIDs: t2,
			Team1Score: g1t1, Team2Score: g1t2, Games: pairs,
		})
		if err != nil {
			return sum, err
		}
		if res.OK {
			s.markSubmission(subID, "submitted", res.DuprMatchID, "")
			sum.Submitted++
		} else {
			s.markSubmission(subID, "failed", "", res.Error)
			sum.Failed++
		}
	}
	return sum, nil
}

func (s *Service) markSubmission(id, status, ref, errMsg string) {
	var submittedAt any
	if status == "submitted" {
		submittedAt = now()
	}
	_, _ = s.sb.Update("dupr_submissions", "id=eq."+store.Q(id), map[string]any{
		"status":       status,
		"provider_ref": orNull(ref),
		"error":        orNull(errMsg),
		"submitted_at": submittedAt,
	})
}

// advanceTeam copies one side (by team number) of a finished match into its
// next match's slot — used to advance a winner (e.g. to the gold game) or drop
// a loser (e.g. to the bronze game). It first clears any players previously
// advanced into that exact (feed match, slot) so a re-scored match that flips
// the result does not leave both teams' players on one side.
func (s *Service) advanceTeam(matchID string, team int, feedsMatchID string, feedsSlot int) error {
	// Who currently occupies the target slot — so we can tell whether this
	// advancement actually CHANGES who moves on (a flipped result) versus a
	// no-op re-score (e.g. fixing 11-5 to 11-6, same winner).
	before, err := s.slotPlayers(feedsMatchID, feedsSlot)
	if err != nil {
		return err
	}
	// Clear any team previously advanced into this exact (feed match, slot) so a
	// re-scored match that flips the result doesn't pile both teams onto one side.
	if err := s.sb.Delete("match_participants",
		"match_id=eq."+store.Q(feedsMatchID)+"&team=eq."+strconv.Itoa(feedsSlot)); err != nil {
		return err
	}
	rows, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(matchID)+"&team=eq."+strconv.Itoa(team)+"&select=player_id")
	if err != nil {
		return err
	}
	side := make([]string, 0, len(rows))
	for _, r := range rows {
		side = append(side, asStr(r, "player_id"))
	}
	if err := s.insertSide(feedsMatchID, feedsSlot, side); err != nil {
		return err
	}
	// If a DIFFERENT team now advances into this slot, any already-played match
	// downstream is based on a stale participant — reset it and cascade. (When
	// the same team advances we leave a played downstream game untouched.)
	if !sameSet(before, side) {
		return s.resetCompletedDownstream(feedsMatchID)
	}
	return nil
}

// resetCompletedDownstream reverts a bracket match that was already played but
// whose participants just changed (a re-scored upstream result flipped). It
// clears the score/winner back to unplayed, clears whatever it had advanced into
// ITS own feeds, and recurses so a whole downstream chain unwinds. No-op when
// the match wasn't completed.
func (s *Service) resetCompletedDownstream(matchID string) error {
	m, err := s.sb.SelectOne("matches", "id=eq."+store.Q(matchID)+
		"&select=status,feeds_match_id,feeds_slot,loser_feeds_match_id,loser_feeds_slot")
	if err != nil {
		return err
	}
	if m == nil || asStr(m, "status") != "completed" {
		return nil
	}
	if _, err := s.sb.Update("matches", "id=eq."+store.Q(matchID), map[string]any{
		"team1_score": nil, "team2_score": nil, "winning_team": nil,
		"status": "scheduled", "completed_at": nil,
		// result_type is NOT NULL — reset to its default rather than NULL (NULL
		// fails the constraint and aborts the whole re-score cascade mid-write).
		"result_type": "normal", "counts_for_diff": true,
	}); err != nil {
		return err
	}
	for _, f := range []struct {
		idKey, slotKey string
	}{{"feeds_match_id", "feeds_slot"}, {"loser_feeds_match_id", "loser_feeds_slot"}} {
		fm := asStrPtr(m, f.idKey)
		if fm == nil {
			continue
		}
		if err := s.sb.Delete("match_participants",
			"match_id=eq."+store.Q(*fm)+"&team=eq."+strconv.Itoa(asInt(m, f.slotKey))); err != nil {
			return err
		}
		if err := s.resetCompletedDownstream(*fm); err != nil {
			return err
		}
	}
	return nil
}

// slotPlayers returns the player ids occupying a match's team slot.
func (s *Service) slotPlayers(matchID string, team int) ([]string, error) {
	rows, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(matchID)+"&team=eq."+strconv.Itoa(team)+"&select=player_id")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, asStr(r, "player_id"))
	}
	return out, nil
}

// sameSet reports whether a and b contain the same player ids (order-agnostic).
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, y := range b {
		seen[y]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------- standings
func (s *Service) Standings(eventID, bracketID string, byWins bool) ([]model.Standing, error) {
	// Pool standings are a GROUP BY aggregation PostgREST can't express, so they
	// live in the pmp_standings SQL function (see 0002_rpc.sql). It returns rows
	// unordered; we apply the wins-vs-points sort here.
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

	// Head-to-head map for breaking ties when the box-score stats are equal —
	// the USAP convention (whoever won the meeting ranks higher). Best-effort:
	// on any error h2h is nil and we fall back to the stat-only order.
	h2h, _ := s.headToHead(eventID, bracketID)

	// h2hCmp: +1 if a beat b head-to-head more, -1 if b did, 0 if even/none.
	// Pairwise (resolves the common 2-way tie; multi-way groups fall through).
	h2hCmp := func(a, b model.Standing) int {
		if h2h == nil {
			return 0
		}
		aw, bw := h2h[a.PlayerID][b.PlayerID], h2h[b.PlayerID][a.PlayerID]
		if aw > bw {
			return 1
		}
		if bw > aw {
			return -1
		}
		return 0
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if byWins {
			// USAP order: record, then HEAD-TO-HEAD, then point differential,
			// then fewest points allowed, then points scored.
			if a.Wins != b.Wins {
				return a.Wins > b.Wins
			}
			if a.Losses != b.Losses {
				return a.Losses < b.Losses
			}
			if c := h2hCmp(a, b); c != 0 {
				return c > 0
			}
			if a.PointDiff != b.PointDiff {
				return a.PointDiff > b.PointDiff
			}
			if a.PointsAgainst != b.PointsAgainst {
				return a.PointsAgainst < b.PointsAgainst
			}
			return a.PointsFor > b.PointsFor
		}
		// Points leaderboard (a user view, not USAP standings): points first.
		if a.PointsFor != b.PointsFor {
			return a.PointsFor > b.PointsFor
		}
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		if a.PointDiff != b.PointDiff {
			return a.PointDiff > b.PointDiff
		}
		if c := h2hCmp(a, b); c != 0 {
			return c > 0
		}
		return false
	})
	return out, nil
}

// headToHead returns wins[a][b] = the number of completed pool matches in which
// player a's team beat player b's team (event-wide, or scoped to a bracket).
// Used only to break standings ties; pairwise, which resolves the common 2-way
// tie correctly (multi-way ties fall back to stable stat order).
func (s *Service) headToHead(eventID, bracketID string) (map[string]map[string]int, error) {
	q := "event_id=eq." + store.Q(eventID) +
		"&stage=eq.pool&status=eq.completed" +
		"&select=winning_team,participants:match_participants(player_id,team)"
	if bracketID != "" {
		q += "&bracket_id=eq." + store.Q(bracketID)
	}
	rows, err := s.sb.Select("matches", q)
	if err != nil {
		return nil, err
	}
	h := map[string]map[string]int{}
	for _, r := range rows {
		wt := asInt(r, "winning_team")
		if wt != 1 && wt != 2 {
			continue
		}
		parts, _ := r["participants"].([]any)
		var winners, losers []string
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			pid := asStr(pm, "player_id")
			if pid == "" {
				continue
			}
			if asInt(pm, "team") == wt {
				winners = append(winners, pid)
			} else {
				losers = append(losers, pid)
			}
		}
		for _, w := range winners {
			if h[w] == nil {
				h[w] = map[string]int{}
			}
			for _, l := range losers {
				h[w][l]++
			}
		}
	}
	return h, nil
}

// ------------------------------------------------------ bracket dashboard
func (s *Service) BracketMatches(bracketID string) ([]model.Match, error) {
	rows, err := s.sb.Select("matches",
		"bracket_id=eq."+store.Q(bracketID)+"&stage=eq.bracket&select="+matchSelect+
			"&order=bracket_round,bracket_slot")
	if err != nil {
		return nil, err
	}
	out := make([]model.Match, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapMatch(r))
	}
	return out, nil
}

func (s *Service) Rounds(eventID string) ([]model.RoundView, error) {
	rows, err := s.sb.Select("rounds",
		"event_id=eq."+store.Q(eventID)+"&select=id,bracket_id,round_number,status&order=bracket_id,round_number")
	if err != nil {
		return nil, err
	}
	out := make([]model.RoundView, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapRoundView(r))
	}
	return out, nil
}

// MatchesForRound returns a pool round's matches with resolved sides + court #.
func (s *Service) MatchesForRound(roundID string) ([]model.Match, error) {
	rows, err := s.sb.Select("matches", "round_id=eq."+store.Q(roundID)+"&select="+matchSelect)
	if err != nil {
		return nil, err
	}
	out := make([]model.Match, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapMatch(r))
	}
	// Order by court number (the court is an embed, so PostgREST can't sort on it).
	sort.SliceStable(out, func(i, j int) bool {
		return intOr(out[i].CourtNumber, 1<<30) < intOr(out[j].CourtNumber, 1<<30)
	})
	return out, nil
}

// EventPoolMatches returns every pool match in the event with resolved sides,
// court number, and round context (id/number/status). The Game tab loads this
// as one stream so it can group + filter (search, status, division) in memory.
func (s *Service) EventPoolMatches(eventID string) ([]model.Match, error) {
	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&stage=eq.pool&select="+matchSelect)
	if err != nil {
		return nil, err
	}
	out := make([]model.Match, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapMatch(r))
	}
	// Order by round number, then division, then court (round/court are embeds).
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := intOr(a.RoundNumber, 1<<30), intOr(b.RoundNumber, 1<<30); ra != rb {
			return ra < rb
		}
		if ba, bb := strOr(a.BracketID), strOr(b.BracketID); ba != bb {
			return ba < bb
		}
		return intOr(a.CourtNumber, 1<<30) < intOr(b.CourtNumber, 1<<30)
	})
	return out, nil
}

// SwapMatchPlayer replaces one player in a match with another (keeping the same
// team). Used for last-minute substitutions — works on any match, scored or not.
func (s *Service) SwapMatchPlayer(matchID, outPlayerID, inPlayerID string) error {
	if outPlayerID == "" || inPlayerID == "" {
		return errors.New("outPlayerId and inPlayerId are required")
	}
	if outPlayerID == inPlayerID {
		return nil
	}
	pl, err := s.sb.SelectOne("players", "id=eq."+store.Q(inPlayerID)+"&select=id")
	if err != nil {
		return err
	}
	if pl == nil {
		return errors.New("replacement player not found")
	}
	// The player being swapped out must currently be in the match.
	cur, err := s.sb.SelectOne("match_participants",
		"match_id=eq."+store.Q(matchID)+"&player_id=eq."+store.Q(outPlayerID)+"&select=team")
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrNotFound
	}
	// Don't swap in someone already playing in this match.
	dup, err := s.sb.SelectOne("match_participants",
		"match_id=eq."+store.Q(matchID)+"&player_id=eq."+store.Q(inPlayerID)+"&select=match_id")
	if err != nil {
		return err
	}
	if dup != nil {
		return errors.New("that player is already in this match")
	}
	out, err := s.sb.Update("match_participants",
		"match_id=eq."+store.Q(matchID)+"&player_id=eq."+store.Q(outPlayerID),
		map[string]any{"player_id": inPlayerID})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMatchCourt reassigns a match to the court with the given number within its
// event (courtNumber <= 0 clears the assignment). Powers drag-to-reassign on
// the schedule board.
func (s *Service) SetMatchCourt(matchID string, courtNumber int, playOrder *float64) error {
	m, err := s.sb.SelectOne("matches", "id=eq."+store.Q(matchID)+"&select=event_id")
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}

	eventID := asStr(m, "event_id")
	var courtID any // nil => unassigned
	courtIDStr := ""
	if courtNumber > 0 {
		c, err := s.sb.SelectOne("courts",
			"event_id=eq."+store.Q(eventID)+
				"&court_number=eq."+strconv.Itoa(courtNumber)+"&select=id")
		if err != nil {
			return err
		}
		if c == nil {
			return errors.New("no such court for this event")
		}
		courtIDStr = asStr(c, "id")
		courtID = courtIDStr
	}

	upd := map[string]any{"court_id": courtID}
	switch {
	case playOrder != nil:
		upd["play_order"] = *playOrder
	case courtIDStr != "":
		// A plain drag-to-court with no explicit order: append to the END of the
		// target court's queue so the calendar never lands two games on the same
		// court+slot (the stale-slot bug).
		next, err := s.nextPlayOrder(eventID, courtIDStr)
		if err != nil {
			return err
		}
		upd["play_order"] = next
	default:
		// Unassigned: clear the slot so it reads as not-yet-scheduled.
		upd["play_order"] = nil
	}
	_, err = s.sb.Update("matches", "id=eq."+store.Q(matchID), upd)
	return err
}

// nextPlayOrder returns one past the highest play_order currently on a court, so
// a game moved onto that court appends after its games (no calendar collision).
func (s *Service) nextPlayOrder(eventID, courtID string) (float64, error) {
	rows, err := s.sb.Select("matches",
		"event_id=eq."+store.Q(eventID)+"&court_id=eq."+store.Q(courtID)+
			"&play_order=not.is.null&select=play_order&order=play_order.desc&limit=1")
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if po := asFloatPtr(rows[0], "play_order"); po != nil {
		return *po + 1, nil
	}
	return 0, nil
}

// SetMatchDuration overrides one game's length (minutes); minutes <= 0 clears it
// back to the event default. Returns the clamped value (0 = cleared).
func (s *Service) SetMatchDuration(matchID string, minutes int) (int, error) {
	var val any // nil clears the override
	out := 0
	if minutes > 0 {
		out = clampGameDuration(minutes)
		val = out
	}
	_, err := s.sb.Update("matches", "id=eq."+store.Q(matchID),
		map[string]any{"duration_minutes": val})
	return out, err
}

// --------------------------------------------------------------- helpers
type reg struct {
	id        string
	playerID  string
	partnerID string
}

func (s *Service) bracketRegs(eventID, bracketID string) ([]reg, error) {
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&bracket_id=eq."+store.Q(bracketID)+"&select=id,player_id,partner_id")
	if err != nil {
		return nil, err
	}
	out := make([]reg, 0, len(rows))
	for _, r := range rows {
		out = append(out, reg{
			id:        asStr(r, "id"),
			playerID:  asStr(r, "player_id"),
			partnerID: asStr(r, "partner_id"), // "" when null
		})
	}
	return out, nil
}

func (s *Service) playerSkills() (map[string]float64, error) {
	rows, err := s.sb.Select("players", "select=id,skill_level")
	if err != nil {
		return nil, err
	}
	m := map[string]float64{}
	for _, r := range rows {
		sk := 0.0
		if p := asFloatPtr(r, "skill_level"); p != nil {
			sk = *p
		}
		m[asStr(r, "id")] = sk
	}
	return m, nil
}

func (s *Service) courtIDsByNumber(eventID string) (map[int]string, error) {
	rows, err := s.sb.Select("courts", "event_id=eq."+store.Q(eventID)+"&select=court_number,id")
	if err != nil {
		return nil, err
	}
	m := map[int]string{}
	for _, r := range rows {
		m[asInt(r, "court_number")] = asStr(r, "id")
	}
	return m, nil
}

func (s *Service) insertSide(matchID string, team int, side []string) error {
	if len(side) == 0 || engine.IsBye(side) {
		return nil
	}
	rows := make([]map[string]any, 0, len(side))
	for _, pid := range side {
		rows = append(rows, map[string]any{"match_id": matchID, "player_id": pid, "team": team})
	}
	// Upsert (merge-duplicates) mirrors the old INSERT OR IGNORE: re-seeding the
	// same (match_id,player_id) is a no-op rather than a unique-constraint error.
	_, err := s.sb.Upsert("match_participants", "match_id,player_id", rows)
	return err
}

// wipeAllMatches clears an event's schedule. Deleting matches/rounds cascades to
// match_participants via the FK ON DELETE CASCADE, so no explicit child delete.
func (s *Service) wipeAllMatches(eventID string) error {
	if err := s.sb.Delete("matches", "event_id=eq."+store.Q(eventID)); err != nil {
		return err
	}
	return s.sb.Delete("rounds", "event_id=eq."+store.Q(eventID))
}

func (s *Service) wipeBracketStage(bracketID string) error {
	return s.sb.Delete("matches", "bracket_id=eq."+store.Q(bracketID)+"&stage=eq.bracket")
}

// poolProgress reports how many pool matches a division has (total) and how many
// are not yet completed (open). Replaces a COUNT/SUM aggregation by tallying the
// fetched statuses in Go.
func (s *Service) poolProgress(bracketID string) (total, open int, err error) {
	rows, err := s.sb.Select("matches",
		"bracket_id=eq."+store.Q(bracketID)+"&stage=eq.pool&select=status")
	if err != nil {
		return 0, 0, err
	}
	for _, m := range rows {
		total++
		if asStr(m, "status") != "completed" {
			open++
		}
	}
	return total, open, nil
}

func sidesForBracket(ev model.Event, regs []reg) [][]string {
	if ev.Format == "singles" {
		out := make([][]string, len(regs))
		for i, r := range regs {
			out[i] = []string{r.playerID}
		}
		return out
	}
	return pairsFromRegs(regs)
}

func seedSides(sides [][]string, skill map[string]float64) [][]string {
	rate := func(s []string) float64 {
		if len(s) == 0 {
			return 0
		}
		sum := 0.0
		for _, id := range s {
			sum += skill[id]
		}
		return sum / float64(len(s))
	}
	out := make([][]string, len(sides))
	copy(out, sides)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rate(out[i]), rate(out[j])
		if ri != rj {
			return ri > rj
		}
		return strings.Join(out[i], "") < strings.Join(out[j], "")
	})
	return out
}

func pairsFromRegs(regs []reg) [][]string {
	used := map[string]bool{}
	present := map[string]bool{}
	for _, r := range regs {
		present[r.playerID] = true
	}
	var pairs [][]string
	for _, r := range regs {
		if used[r.playerID] {
			continue
		}
		if r.partnerID != "" && present[r.partnerID] && !used[r.partnerID] {
			pairs = append(pairs, []string{r.playerID, r.partnerID})
			used[r.playerID] = true
			used[r.partnerID] = true
		}
	}
	var leftover []string
	for _, r := range regs {
		if !used[r.playerID] {
			leftover = append(leftover, r.playerID)
		}
	}
	for i := 0; i+1 < len(leftover); i += 2 {
		pairs = append(pairs, []string{leftover[i], leftover[i+1]})
	}
	return pairs
}

func pairScore(pair []string, rate map[string]int) int {
	s := 0
	for _, id := range pair {
		s += rate[id]
	}
	return s
}

func key(round, slot int) string { return strconv.Itoa(round) + ":" + strconv.Itoa(slot) }

func agePtr(v int) *int { return &v }

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
