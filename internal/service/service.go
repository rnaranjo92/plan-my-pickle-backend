// Package service holds PlanMyPickle's business operations: event setup,
// registration, schedule/bracket generation, scoring, and standings.
// Ported from the Flutter app's repository; uses the verified engine package.
package service

import (
	"bytes"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// PlaceAutocomplete returns location suggestions for club/venue fields. kind
// "city" → cities ("City, State"); "place" → full addresses/POIs. Empty when no
// geocoder key is configured (the field then works as plain free text).
func (s *Service) PlaceAutocomplete(query, kind string) []string {
	return courts.PlaceAutocomplete(query, kind)
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

// ErrDuprIDTaken means the DUPR account is already linked to a different
// PlanMyPickle account (one DUPR id maps to one account).
var ErrDuprIDTaken = errors.New("this DUPR account is already connected to another PlanMyPickle account")

// ErrDuprNotConnected means a player tried to SELF-register for a DUPR-sanctioned
// event without a connected DUPR account (their results must be submittable).
var ErrDuprNotConnected = errors.New("connect your DUPR account to register for this DUPR-sanctioned event")

// ErrPremiumRequired means a Premium-only action (organizing events, DUPR
// sanctioning) was requested by a non-premium account.
var ErrPremiumRequired = errors.New("a Premium subscription is required")

// ErrDuprEntitlementRequired means a player tried to SELF-register for a DUPR
// event that gates on a higher entitlement tier (DUPR+ Premium or Verified) that
// their DUPR account does not hold. The message names the tier.
var ErrDuprEntitlementRequired = errors.New("your DUPR account isn't eligible for this event's tier")

// normalizeDuprEntitlement validates an event's required DUPR gate, returning
// "" for anything that isn't a gated tier (so a stray value can't silently lock
// an event nobody can join). Per DUPR's guidance there is ONE user-facing tier:
// DUPR_PLUS ("DUPR+"), which requires BOTH the PREMIUM_L1 and VERIFIED_L1
// entitlements. The legacy per-entitlement values fold into it.
func normalizeDuprEntitlement(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "DUPR_PLUS", "PREMIUM_L1", "VERIFIED_L1":
		return "DUPR_PLUS"
	default:
		return ""
	}
}

// duprPlusEntitlements are the DUPR entitlement codes a player must ALL hold to
// enter a DUPR+ event.
var duprPlusEntitlements = []string{"PREMIUM_L1", "VERIFIED_L1"}

// duprEntCache caches each user's entitlement codes — DUPR's integration doc
// allows caching entitlements for up to 24 hours, which keeps registration
// bursts from hammering /subscription/active. Keyed by our auth user id.
var duprEntCache sync.Map // userID -> duprEntEntry

type duprEntEntry struct {
	codes []string
	at    time.Time
}

// userEntitlements returns the DUPR entitlement codes for a connected user,
// using the 24h cache, the stored SSO user token, and a refresh-once retry when
// the access token has expired (persisting the refreshed token).
func (s *Service) userEntitlements(userID string) ([]string, error) {
	if e, ok := duprEntCache.Load(userID); ok {
		if ent := e.(duprEntEntry); time.Since(ent.at) < 24*time.Hour {
			return ent.codes, nil
		}
	}
	conn, err := s.sb.SelectOne("dupr_connections",
		"user_id=eq."+store.Q(userID)+"&select=user_token,refresh_token")
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("no dupr connection")
	}
	codes, err := s.Dupr.GetEntitlements(asStr(conn, "user_token"))
	if errors.Is(err, gateway.ErrDuprUserTokenExpired) {
		// Access token expired (UAT 7d / prod 30d): mint a fresh one from the
		// refresh token, persist it, and retry once.
		fresh, rerr := s.Dupr.RefreshUserToken(asStr(conn, "refresh_token"))
		if rerr != nil {
			return nil, fmt.Errorf("user token expired and refresh failed: %w", rerr)
		}
		_, _ = s.sb.Update("dupr_connections", "user_id=eq."+store.Q(userID),
			map[string]any{"user_token": fresh})
		codes, err = s.Dupr.GetEntitlements(fresh)
	}
	if err != nil {
		return nil, err
	}
	duprEntCache.Store(userID, duprEntEntry{codes: codes, at: time.Now()})
	return codes, nil
}

// containsFold reports whether want appears in list, case-insensitively.
func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

// duprEntitlementLabel is the human name for a gated tier, for error messages.
func duprEntitlementLabel(code string) string {
	if code == "DUPR_PLUS" {
		return "DUPR+"
	}
	return code
}

// ------------------------------------------------------------------ events
// CreateEvent inserts an event owned by ownerID (the authenticated organizer).
// ownerID may be empty for internal/demo seeding, leaving the event unowned.
func (s *Service) CreateEvent(req model.CreateEventRequest, ownerID string) (string, error) {
	if strings.TrimSpace(req.Name) == "" {
		return "", errors.New("name is required")
	}
	// A premium/verified DUPR tier implies a sanctioned event (BASIC_L1 baseline
	// plus the gated tier), so normalize it up front.
	minEnt := normalizeDuprEntitlement(req.DuprMinEntitlement)
	sanctioned := req.DuprSanctioned || minEnt != ""
	// DUPR sanctioning is a Premium feature — enforce server-side so the UI lock
	// can't be bypassed.
	if sanctioned && !s.IsPremium(ownerID) {
		return "", ErrPremiumRequired
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

	// Auto-geocode: if the organizer typed a text location but didn't pick a
	// venue on the map (no coords), resolve the text to a point so the event can
	// surface in Nearby. Best-effort — never fails the create.
	venueLat, venueLng := req.VenueLat, req.VenueLng
	if venueLat == nil && venueLng == nil {
		if lat, lng := bestEffortGeocode(req.Location); lat != nil && lng != nil {
			venueLat, venueLng = lat, lng
		}
	}

	// An event can be created under a club only by that club's owner.
	if req.ClubID != "" && !s.OwnsClub(req.ClubID, ownerID) {
		return "", ErrForbidden
	}
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
		"zelle_handle":           orNull(req.ZelleHandle),
		"club_id":                orNull(req.ClubID),
		"dupr_sanctioned":        sanctioned,
		"dupr_min_entitlement":   orNull(minEnt),
		"cash_prize":             req.CashPrize,
		"cash_prize_amount":      fOrNull(req.CashPrizeAmount),
		"consolation":            req.Consolation,
		"auto_adjust":            req.AutoAdjust,
		"team_size":              req.TeamSize,
		"admin_passcode":         orNull(req.AdminPasscode),
		"owner_id":               orNull(ownerID),
		"listed":                 req.Listed,
		"poster_url":             orNull(req.PosterURL),
		"venue_name":             orNull(req.VenueName),
		"venue_address":          orNull(req.VenueAddress),
		"venue_phone":            orNull(req.VenuePhone),
		"venue_website":          orNull(req.VenueWebsite),
		"venue_lat":              fOrNull(venueLat),
		"venue_lng":              fOrNull(venueLng),
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
	// venue_notes / waiver_url ship in add_venue_info.sql — same migration-safe
	// pattern (only reference when set).
	if req.VenueNotes != "" {
		payload["venue_notes"] = req.VenueNotes
	}
	if req.WaiverURL != "" {
		payload["waiver_url"] = req.WaiverURL
	}
	// min/max_pool_rounds ship in add_pool_rounds.sql — only reference when set.
	if req.MinPoolRounds > 0 {
		payload["min_pool_rounds"] = req.MinPoolRounds
	}
	if req.MaxPoolRounds > 0 {
		payload["max_pool_rounds"] = req.MaxPoolRounds
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
		dt := d.DivisionType
		if dt == "" {
			dt = "open"
		}
		brackets = append(brackets, map[string]any{
			"event_id":      id,
			"name":          d.Name,
			"min_rating":    fOrNull(d.MinRating),
			"max_rating":    fOrNull(d.MaxRating),
			"min_age":       iOrNull(d.MinAge),
			"max_age":       iOrNull(d.MaxAge),
			"division_type": dt,
			"dupr_min":      fOrNull(d.DuprMin),
			"dupr_max":      fOrNull(d.DuprMax),
			"sort_order":    i,
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
			"event_id="+store.In(ids)+"&select=event_id")
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

// publicFeedTestName spots QA/test event names ("Test", "Bday Smash Test 2",
// "TEST · Doubles 3.0-4.0 · 150", "Demo Open Slam", "dbg", "authcheck") so the
// marketing feed never shows them. Word-boundary match keeps real names like
// "SoCal Contest" visible; junk on the homepage costs more than the rare
// false positive (an organizer with "demo" in a real event name can rename).
var publicFeedTestName = regexp.MustCompile(`(?i)\b(test|demo|dbg|debug|authcheck)\b`)

// PublicEvents returns up to `limit` publicly-listed events (listed=eq.true),
// ordered by scheduled start, mapped to the SAFE public projection for the
// planmypickle.com marketing feed. No owner scoping, no PII — anyone may read it.
// Registered counts are attached with the same batched select-then-tally as
// ListEvents (no N+1); a count failure is best-effort and leaves counts at 0.
func (s *Service) PublicEvents(limit int) ([]model.PublicEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	// starts_at NULLs sort last so dated tournaments lead the feed. Order by
	// start ascending (soonest first), then created_at desc as a stable tiebreak
	// for events that have no scheduled start yet. Over-fetch 3× because the
	// test-name filter below drops rows after the query.
	rows, err := s.sb.Select("events",
		"listed=eq.true&select=*&order=starts_at.asc.nullslast,created_at.desc&limit="+strconv.Itoa(limit*3))
	if err != nil {
		return nil, err
	}
	events := make([]model.Event, 0, len(rows))
	for _, r := range rows {
		e := mapEvent(r)
		// The marketing feed skips events that are obviously QA/test runs —
		// "Test", "Bday Smash Test", "TEST · Doubles…" — even if their owner left
		// the public-listing toggle on. Word-boundary match so a legit name like
		// "SoCal Contest" or "Tested Champions" still shows.
		if publicFeedTestName.MatchString(e.Name) {
			continue
		}
		events = append(events, e)
		if len(events) == limit {
			break
		}
	}

	// Batched registered-player counts (mirrors ListEvents): one query for every
	// event id, grouped client-side. Best-effort — a failure leaves counts at 0.
	if len(events) > 0 {
		ids := make([]string, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		if regs, err := s.sb.Select("registrations",
			"event_id="+store.In(ids)+"&select=event_id"); err == nil {
			counts := make(map[string]int, len(events))
			for _, r := range regs {
				counts[asStr(r, "event_id")]++
			}
			for i := range events {
				events[i].RegisteredCount = counts[events[i].ID]
			}
		}
	}

	out := make([]model.PublicEvent, 0, len(events))
	for _, e := range events {
		out = append(out, model.PublicEvent{
			ID:               e.ID,
			Name:             e.Name,
			TournamentFormat: e.TournamentFormat,
			Format:           e.Format,
			StartsAt:         e.StartsAt,
			EndsAt:           e.EndsAt,
			Location:         e.Location,
			VenueName:        e.VenueName,
			PosterURL:        e.PosterURL,
			DuprSanctioned:   e.DuprSanctioned,
			RegisteredCount:  e.RegisteredCount,
		})
	}
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
	inList := store.In(ids)

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
// playerIDsForUser resolves the set of player rows that belong to the caller:
// a player linked to the account (players.user_id) PLUS any player whose email
// matches the caller's VERIFIED account email — so signing up with your login
// email surfaces your registrations even when you weren't signed in at the time.
// (email comes from the verified JWT, so it can't be spoofed to claim someone
// else's player rows.) Shared by MyEvents, MyLeagues and IsLeagueParticipant so
// the "what is mine" rule is defined in exactly one place.
func (s *Service) playerIDsForUser(userID, email string) ([]string, error) {
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
	out := make([]string, 0, len(playerIDs))
	for id := range playerIDs {
		out = append(out, id)
	}
	return out, nil
}

func (s *Service) MyEvents(userID, email string) ([]model.Event, error) {
	pidList, err := s.playerIDsForUser(userID, email)
	if err != nil {
		return nil, err
	}
	if len(pidList) == 0 {
		return []model.Event{}, nil
	}
	regs, err := s.sb.Select("registrations",
		"player_id="+store.In(pidList)+"&select=event_id")
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
		"id="+store.In(ids)+"&select=*&order=created_at.desc")
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

// MyNextMatch returns the signed-in user's next match in an event — the one
// in progress, else the soonest scheduled — or nil when they have none or aren't
// registered. Powers the player view's "your next match" banner.
func (s *Service) MyNextMatch(eventID, userID, email string) (*model.Match, error) {
	pidList, err := s.playerIDsForUser(userID, email)
	if err != nil || len(pidList) == 0 {
		return nil, err
	}
	parts, err := s.sb.Select("match_participants",
		"player_id="+store.In(pidList)+"&select=match_id")
	if err != nil {
		return nil, err
	}
	idset := map[string]bool{}
	for _, p := range parts {
		if mid := asStr(p, "match_id"); mid != "" {
			idset[mid] = true
		}
	}
	if len(idset) == 0 {
		return nil, nil
	}
	mids := make([]string, 0, len(idset))
	for id := range idset {
		mids = append(mids, id)
	}
	// In-progress first (status sorts 'in_progress' < 'scheduled'), then by play
	// order so the soonest upcoming game wins.
	rows, err := s.sb.Select("matches",
		"id="+store.In(mids)+"&event_id=eq."+store.Q(eventID)+
			"&status=in.(scheduled,in_progress)"+
			"&order=status.asc,play_order.asc.nullslast,created_at.asc&select="+matchSelect)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	m := mapMatch(rows[0])
	return &m, nil
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
	ev.OwnerPremium = s.eventPremiumUnlocked(row)
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
			"&select=checked_in,player:players!player_id(id,full_name),bracket:brackets!bracket_id(name)")
	if err != nil {
		return nil, err
	}
	out := make([]model.RosterEntry, 0, len(rows))
	for _, r := range rows {
		name, pid := "", ""
		if p := asMap(r, "player"); p != nil {
			name = strings.TrimSpace(asStr(p, "full_name"))
			pid = asStr(p, "id")
		}
		if name == "" {
			continue
		}
		div := ""
		if b := asMap(r, "bracket"); b != nil {
			div = asStr(b, "name")
		}
		out = append(out, model.RosterEntry{
			PlayerID:  pid,
			FullName:  name,
			Division:  div,
			CheckedIn: asBool(r, "checked_in"),
		})
	}
	return out, nil
}

// PlayerProfile builds the PUBLIC profile for a player: their DUPR id/ratings
// (when connected) plus an across-events box score aggregated from every
// completed match they've played. Returns ErrNotFound for an unknown player.
func (s *Service) PlayerProfile(playerID string) (model.PlayerProfile, error) {
	prof := model.PlayerProfile{PlayerID: playerID, RecentEvents: []string{}}
	prow, err := s.sb.SelectOne("players",
		"id=eq."+store.Q(playerID)+"&select=id,full_name,dupr_id,user_id")
	if err != nil {
		return prof, err
	}
	if prow == nil {
		return prof, ErrNotFound
	}
	prof.FullName = strings.TrimSpace(asStr(prow, "full_name"))
	prof.DuprID = asStr(prow, "dupr_id")

	// Photo + ratings live on the account (keyed by the linked auth user). The
	// photo read is best-effort so a missing profiles table never errors.
	if uid := asStr(prow, "user_id"); uid != "" {
		if pr, err := s.sb.SelectOne("pmp_profiles",
			"user_id=eq."+store.Q(uid)+"&select=photo_url"); err == nil && pr != nil {
			prof.PhotoURL = asStr(pr, "photo_url")
		}
		if c, _ := s.sb.SelectOne("dupr_connections",
			"user_id=eq."+store.Q(uid)+"&select=doubles_rating,singles_rating"); c != nil {
			prof.DoublesRating = asFloatPtr(c, "doubles_rating")
			prof.SinglesRating = asFloatPtr(c, "singles_rating")
		}
	}

	// Box score: every completed match this player took part in, attributed to
	// the side they played on.
	parts, err := s.sb.Select("match_participants",
		"player_id=eq."+store.Q(playerID)+"&select=team,match_id")
	if err != nil {
		return prof, err
	}
	teamByMatch := map[string]int{}
	mids := make([]string, 0, len(parts))
	for _, p := range parts {
		if mid := asStr(p, "match_id"); mid != "" {
			teamByMatch[mid] = asInt(p, "team")
			mids = append(mids, mid)
		}
	}
	if len(mids) > 0 {
		rows, err := s.sb.Select("matches",
			"id="+store.In(mids)+"&status=eq.completed"+
				"&select=id,team1_score,team2_score,winning_team")
		if err != nil {
			return prof, err
		}
		for _, m := range rows {
			t1, t2 := asIntPtr(m, "team1_score"), asIntPtr(m, "team2_score")
			if t1 == nil || t2 == nil {
				continue // a bye is a completed match with no score — not a game
			}
			team := teamByMatch[asStr(m, "id")]
			mine, opp := *t2, *t1
			if team == 1 {
				mine, opp = *t1, *t2
			}
			prof.GamesPlayed++
			prof.PointsFor += mine
			prof.PointsAgainst += opp
			if wt := asIntPtr(m, "winning_team"); wt != nil && *wt == team {
				prof.Wins++
			} else {
				prof.Losses++
			}
		}
	}

	// Tournaments played (most recent first) come from their registrations.
	regs, err := s.sb.Select("registrations",
		"player_id=eq."+store.Q(playerID)+"&order=created_at.desc"+
			"&select=event:events!event_id(name)")
	if err != nil {
		return prof, err
	}
	seen := map[string]bool{}
	for _, r := range regs {
		if e := asMap(r, "event"); e != nil {
			n := strings.TrimSpace(asStr(e, "name"))
			if n != "" && !seen[n] {
				seen[n] = true
				if len(prof.RecentEvents) < 8 {
					prof.RecentEvents = append(prof.RecentEvents, n)
				}
			}
		}
	}
	prof.EventsPlayed = len(seen)
	return prof, nil
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
// NearbyEvents returns publicly-listed events that have a venue location,
// sorted by distance from (lat,lng) ascending, paginated (0-based page).
func (s *Service) NearbyEvents(lat, lng float64, page, pageSize int) ([]model.Event, error) {
	rows, err := s.sb.Select("events",
		"listed=eq.true&venue_lat=not.is.null&venue_lng=not.is.null&select=*")
	if err != nil {
		return nil, err
	}
	type withDist struct {
		e model.Event
		d float64
	}
	now := time.Now()
	list := make([]withDist, 0, len(rows))
	for _, r := range rows {
		e := mapEvent(r)
		if e.VenueLat == nil || e.VenueLng == nil {
			continue
		}
		// Discovery shows today + upcoming only — drop events already over.
		if eventEnded(e, now) {
			continue
		}
		d := haversineKm(lat, lng, *e.VenueLat, *e.VenueLng)
		dd := d
		e.DistanceKm = &dd
		list = append(list, withDist{e, d})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].d < list[j].d })
	start := page * pageSize
	if start < 0 || start >= len(list) {
		return []model.Event{}, nil
	}
	end := start + pageSize
	if end > len(list) {
		end = len(list)
	}
	out := make([]model.Event, 0, end-start)
	for _, x := range list[start:end] {
		out = append(out, x.e)
	}
	return out, nil
}

// eventEnded reports whether an event is over (so nearby discovery hides it). It
// uses the scheduled end; if no end is set, it assumes the event runs ~a day from
// its start so a same-day no-end event isn't hidden the moment it begins. Undated
// events (no start and no end) are never "ended".
func eventEnded(e model.Event, now time.Time) bool {
	end := parseTS(e.EndsAt)
	if end.IsZero() {
		if start := parseTS(e.StartsAt); !start.IsZero() {
			end = start.Add(24 * time.Hour)
		}
	}
	return !end.IsZero() && end.Before(now)
}

// parseTS tolerantly parses a stored timestamp string (RFC3339, with or without
// fractional seconds / timezone, or date-only). Returns the zero time on failure.
func parseTS(p *string) time.Time {
	if p == nil || strings.TrimSpace(*p) == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, strings.TrimSpace(*p)); err == nil {
			return t
		}
	}
	return time.Time{}
}

// haversineKm is the great-circle distance between two lat/lng points, in km.
func haversineKm(lat1, lng1, lat2, lng2 float64) float64 {
	const r = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

func (s *Service) UpdateEvent(id string, req model.CreateEventRequest) error {
	// select=* (not a column list) so this read stays valid before migration 0044
	// adds premium_pass — until then eventPremiumUnlocked simply sees it as false.
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(id)+"&select=*")
	if err != nil {
		return err
	}
	if ev == nil {
		return ErrNotFound
	}
	// A premium/verified DUPR tier implies a sanctioned event.
	minEnt := normalizeDuprEntitlement(req.DuprMinEntitlement)
	sanctioned := req.DuprSanctioned || minEnt != ""
	// DUPR sanctioning is Premium — allowed if the owner subscribes OR a one-time
	// per-event pass was bought for this event.
	if sanctioned && !s.eventPremiumUnlocked(ev) {
		return ErrPremiumRequired
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
	// poster_url is ALSO intentionally NOT updated here: the poster is set/cleared
	// only via the dedicated POST /events/{id}/poster endpoint, so a metadata edit
	// (which always sends posterUrl="") never erases an uploaded banner.
	upd := map[string]any{
		"name":                   req.Name,
		"num_courts":             courts,
		"points_to_win":          ptw,
		"win_by":                 winBy,
		"best_of":                normalizeBestOf(req.BestOf),
		"game_duration_minutes":  clampGameDuration(req.GameDurationMinutes),
		"registration_fee_cents": req.RegistrationFeeCents,
		"location":               orNull(req.Location),
		"dupr_sanctioned":        sanctioned,
		"dupr_min_entitlement":   orNull(minEnt),
		"auto_adjust":            req.AutoAdjust,
		// On edit the form always sends these, so write them unconditionally —
		// an empty value clears the field (orNull → SQL NULL).
		"listed":        req.Listed,
		"contact_phone": orNull(req.ContactPhone),
		"zelle_handle":  orNull(req.ZelleHandle),
		"starts_at":     orNull(req.StartsAt),
		"ends_at":       orNull(req.EndsAt),
		"description":   orNull(req.Description),
	}
	// Rotate the scorekeeper passcode on edit (plaintext, mirrors create). Set-only:
	// an empty field leaves the current passcode untouched — we never wipe it, since
	// a blank passcode disables volunteer scoring (see VerifyAdminPasscode). The edit
	// form starts this box blank, so most edits intentionally leave it unchanged.
	if pc := strings.TrimSpace(req.AdminPasscode); pc != "" {
		upd["admin_passcode"] = pc
	}
	// Set-only: an edit never un-teams an event (0 = not sent), so a metadata edit
	// can't accidentally wipe the team flag.
	if req.TeamSize > 0 {
		upd["team_size"] = req.TeamSize
	}
	// venue_notes / waiver_url ship in add_venue_info.sql — reference them only
	// when set so an event edit never breaks before the migration is applied (and
	// so it can't fail app-wide if that manual step is missed). Trade-off: blanking
	// them won't clear an existing value (acceptable until the column is live).
	if req.VenueNotes != "" {
		upd["venue_notes"] = req.VenueNotes
	}
	if req.WaiverURL != "" {
		upd["waiver_url"] = req.WaiverURL
	}
	// min/max_pool_rounds ship in add_pool_rounds.sql — reference only when set so
	// an edit never breaks before the migration is applied. (Trade-off: clearing
	// a bound back to 0 won't persist until the column is live; acceptable.)
	if req.MinPoolRounds > 0 {
		upd["min_pool_rounds"] = req.MinPoolRounds
	}
	if req.MaxPoolRounds > 0 {
		upd["max_pool_rounds"] = req.MaxPoolRounds
	}
	// Auto-geocode on edit: ONLY when the event has no coords yet (a map-picked
	// venue is left untouched — we can't distinguish it from a prior geocode, so
	// we never overwrite existing coords) and the organizer has a text location.
	// Best-effort: a failure leaves coords as-is and never fails the update.
	if asFloatPtr(ev, "venue_lat") == nil && asFloatPtr(ev, "venue_lng") == nil {
		if lat, lng := bestEffortGeocode(req.Location); lat != nil && lng != nil {
			upd["venue_lat"] = *lat
			upd["venue_lng"] = *lng
		}
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

// SetEventPoster sets (or clears, when empty) the event's banner/poster URL — the
// public Supabase Storage URL the client uploaded the image to. An empty url
// stores SQL NULL, removing the banner.
func (s *Service) SetEventPoster(eventID, posterURL string) error {
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"poster_url": orNull(posterURL)})
	return err
}

// SetLeaguePoster sets (or clears, when empty) the league's banner/poster URL.
// An empty url stores SQL NULL, removing the banner.
func (s *Service) SetLeaguePoster(leagueID, posterURL string) error {
	_, err := s.sb.Update("leagues", "id=eq."+store.Q(leagueID),
		map[string]any{"poster_url": orNull(posterURL)})
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
	case m < 10:
		return 10
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
	}, 0.5, 6, ownerID)
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
	if err := s.registerDemoPlayers(eid, 8); err != nil {
		return "", err
	}
	return eid, nil
}

// registerDemoPlayers registers perDiv demo players into each of the two rating
// divisions (3.0-3.5 and 3.5-4.0, strictly split so the auto-assigner produces
// even, bye-free divisions). perDiv is clamped to the name pool (12/division).
func (s *Service) registerDemoPlayers(eventID string, perDiv int) error {
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

	// Cap each division at perDiv players so the round-robin demo (which also
	// scores most of its pool) stays small enough to finish inside the request
	// timeout; the playoff demo passes a larger perDiv.
	if perDiv <= 0 || perDiv > len(div1) {
		perDiv = len(div1)
	}
	if perDiv > len(div2) {
		perDiv = len(div2)
	}
	for i := 0; i < perDiv; i++ {
		if err := reg(div1[i], 3.0+float64(i)*0.03); err != nil { // 3.00–3.33
			return err
		}
	}
	for i := 0; i < perDiv; i++ {
		if err := reg(div2[i], 3.55+float64(i)*0.03); err != nil { // 3.55–3.88
			return err
		}
	}
	return nil
}

// seedDuprDemo stands up a ready-to-demo DUPR-SANCTIONED singles event for the
// DUPR integration review: one open division, four players named after the UAT
// test accounts, and a generated single-elim bracket — so scoring a match and
// then "Import to DUPR" (create / update / delete, owner-only) is one tap away.
// The players are seeded with ratings but NO DUPR id; during the live demo you
// connect the real UAT accounts (SSO) so their consented DUPR ids attach.
func (s *Service) seedDuprDemo(ownerID string) (string, error) {
	eid, err := s.CreateEvent(model.CreateEventRequest{
		Name:             "DEMO · DUPR · sanctioned",
		Format:           "singles",
		PartnerMode:      "na",
		TournamentFormat: "single_elim",
		NumCourts:        2,
		DuprSanctioned:   true,
		Location:         "DUPR Demo Courts",
		Brackets: []model.BracketInput{
			{Name: "Open 3.0-4.5", MinRating: ratingPtr(3.0), MaxRating: ratingPtr(4.5)},
		},
	}, ownerID)
	if err != nil {
		return "", err
	}
	names := []string{"UAT Player 1", "UAT Player 2", "UAT Player 3", "UAT Player 4"}
	for i, n := range names {
		if _, err := s.RegisterPlayer(eid, model.RegisterRequest{
			FullName:   n,
			Phone:      fmt.Sprintf("+1555%07d", 2000000+i),
			SkillLevel: ratingPtr(3.6 + float64(i)*0.1),
			DuprRating: ratingPtr(3.6 + float64(i)*0.1),
		}, ""); err != nil {
			return eid, err
		}
	}
	if _, err := s.GenerateSchedule(eid, true, true); err != nil {
		return eid, err
	}
	return eid, nil
}

// SeedTestTournament creates a ready-to-run TEST tournament owned by ownerID with
// a single 3.0-4.0 rating bracket. ~1 in 5 players is given a DUPR rating ABOVE
// 4.0 so the "outside the bracket rating" flag can be exercised. kind selects the
// shape: "mixed30" (15 fixed M/F pairs, round-robin), "doubles150" (75 fixed
// pairs, single-elim), "singles80" (single-elim). Direct bulk inserts (4 calls)
// so a 150-player field seeds in one request. Returns the new event id.
func (s *Service) SeedTestTournament(ownerID, kind string) (string, error) {
	var (
		name, format, partnerMode, tournFmt, divType string
		count, courts                                int
		doubles, mixed                               bool
	)
	switch kind {
	case "mlp6":
		// MLP Challenger: 6 teams of 16 (8M/8W), round-robin of ties.
		return s.seedMlp(ownerID, false)
	case "mlp6prem":
		// MLP Premier: 6 teams of 6 (3M/3W), team_size=6.
		return s.seedMlp(ownerID, true)
	case "mlpscored":
		// MLP with scores filled in — live ties + standings for the team board.
		return s.seedMlpScored(ownerID)
	case "mlpchamp":
		// MLP played to completion — pools + playoff done, has a champion.
		return s.seedMlpComplete(ownerID)
	case "duprdemo":
		// A ready-to-demo DUPR-SANCTIONED singles event for the DUPR review.
		return s.seedDuprDemo(ownerID)
	case "pikelbol":
		// Real event: Pikelbol Adiks 2026 Masters — 9 teams of 8, Premier.
		return s.seedPikelbol(ownerID)
	case "greensretro":
		// Real event: GREENS vs RETRO club day (Jul 4) — 4 courts, manual schedule.
		return s.seedGreensRetro(ownerID)
	case "podium":
		// A small, pre-played single-elim showing gold/silver/bronze.
		return s.seedPodium(ownerID)
	case "mixedmulti150":
		// Multi-division mixed doubles, own builder (3 brackets).
		return s.seedMultiDivMixed(ownerID, "single_elim", "single-elim")
	case "poolsmulti150":
		// Same field, NON-elimination: pools then a playoff bracket per division.
		return s.seedMultiDivMixed(ownerID, "pools_playoff", "pools-playoff")
	case "mixed30":
		name, format, partnerMode, tournFmt, divType =
			"TEST · Mixed Doubles 3.0-4.0 · 30", "doubles", "fixed", "round_robin", "mixed_doubles"
		count, courts, doubles, mixed = 30, 6, true, true
	case "doubles150":
		name, format, partnerMode, tournFmt, divType =
			"TEST · Doubles 3.0-4.0 · 150", "doubles", "fixed", "single_elim", "open"
		count, courts, doubles = 150, 12, true
	case "singles80":
		name, format, partnerMode, tournFmt, divType =
			"TEST · Singles 3.0-4.0 · 80", "singles", "na", "single_elim", "singles"
		count, courts = 80, 10
	default:
		return "", fmt.Errorf("unknown seed kind %q (want podium|mixed30|mixedmulti150|poolsmulti150|doubles150|singles80|mlp6)", kind)
	}

	evRows, err := s.sb.Insert("events", map[string]any{
		"name": name, "format": format, "partner_mode": partnerMode,
		"scoring_mode": "wins", "tournament_format": tournFmt, "num_courts": courts,
		"points_to_win": 11, "dupr_sanctioned": false, "status": "open",
		"location": "Test Courts", "owner_id": ownerID, "listed": false,
	})
	if err != nil || len(evRows) == 0 {
		return "", fmt.Errorf("seed event: %w", err)
	}
	eventID := asStr(evRows[0], "id")

	// Direct inserts bypass CreateEvent's ensureCourts, so create the court rows
	// ourselves — without them spreadCourts/spreadBracketCourts find 0 courts and
	// arrange nothing on Build schedule.
	if err := s.ensureCourts(eventID, courts); err != nil {
		return "", fmt.Errorf("seed courts: %w", err)
	}

	brRows, err := s.sb.Insert("brackets", map[string]any{
		"event_id": eventID, "name": "3.0-4.0", "division_type": divType,
		"min_rating": 3.0, "max_rating": 4.0, "dupr_min": 3.0, "dupr_max": 4.0, "sort_order": 0,
	})
	if err != nil || len(brRows) == 0 {
		return "", fmt.Errorf("seed bracket: %w", err)
	}
	bracketID := asStr(brRows[0], "id")

	male := []string{"Mike", "John", "Dave", "Carl", "Sam", "Tom", "Alex", "Ben", "Will", "Jake", "Luis", "Ray", "Nick", "Paul", "Kev"}
	female := []string{"Mia", "Jen", "Sara", "Ana", "Kim", "Liz", "Emma", "Beth", "Nina", "Tara", "Lucy", "Rosa", "Dana", "Pam", "Kate"}
	neutral := []string{"Alex", "Sam", "Jordan", "Casey", "Taylor", "Drew", "Pat", "Lee", "Morgan", "Quinn", "Riley", "Jamie", "Avery", "Reese", "Sky"}
	last := []string{"Lee", "Ng", "Diaz", "Park", "Cruz", "Hall", "Reed", "Shaw", "Vance", "Wood"}
	prefix := eventID[:8] // per-run unique (a fresh event uuid) → unique dupr_id

	playerRows := make([]map[string]any, count)
	for i := 0; i < count; i++ {
		n := i + 1
		var rating float64
		if n%5 == 0 {
			rating = 4.1 + float64(n%8)*0.1 // EXCEEDS the 4.0 cap
		} else {
			rating = 3.0 + float64(n%10)*0.1 // 3.0-3.9 (in band)
		}
		first := neutral[n%len(neutral)]
		if mixed {
			if n%2 == 1 {
				first = male[n%len(male)]
			} else {
				first = female[n%len(female)]
			}
		}
		playerRows[i] = map[string]any{
			"full_name":        fmt.Sprintf("%s %s %d", first, last[n%len(last)], n),
			"dupr_id":          fmt.Sprintf("TST-%s-%d", prefix, n),
			"dupr_rating":      rating,
			"dupr_reliability": 85,
			"phone":            fmt.Sprintf("+1555%07d", n),
		}
	}
	plRows, err := s.sb.Insert("players", playerRows)
	if err != nil || len(plRows) != count {
		return "", fmt.Errorf("seed players (%d/%d): %w", len(plRows), count, err)
	}
	// Map player index n -> id by parsing the dupr_id, robust against row order.
	idByN := make(map[int]string, count)
	for _, row := range plRows {
		parts := strings.Split(asStr(row, "dupr_id"), "-")
		if len(parts) < 3 {
			continue
		}
		if n, e := strconv.Atoi(parts[len(parts)-1]); e == nil {
			idByN[n] = asStr(row, "id")
		}
	}

	ts := now()
	mk := func(player, partner string) map[string]any {
		r := map[string]any{
			"event_id": eventID, "player_id": player, "bracket_id": bracketID,
			"payment_status": "comped", "checked_in": true, "checked_in_at": ts,
			"check_in_method": "manual",
		}
		if partner != "" {
			r["partner_id"] = partner
		}
		return r
	}
	var regRows []map[string]any
	if doubles {
		for k := 1; k+1 <= count; k += 2 { // pair (1,2),(3,4),... mutually
			p1, p2 := idByN[k], idByN[k+1]
			if p1 == "" || p2 == "" {
				continue
			}
			regRows = append(regRows, mk(p1, p2), mk(p2, p1))
		}
	} else {
		for k := 1; k <= count; k++ {
			if pid := idByN[k]; pid != "" {
				regRows = append(regRows, mk(pid, ""))
			}
		}
	}
	if _, err := s.sb.Insert("registrations", regRows); err != nil {
		return "", fmt.Errorf("seed registrations: %w", err)
	}
	return eventID, nil
}

// seedMultiDivMixed builds a 150-player MIXED DOUBLES, SINGLE-ELIM test event split
// across 3 rating divisions (3.0-3.5 / 3.5-4.0 / 4.0-4.5), 25 fixed M/F pairs each,
// with ~1 in 6 players rated over their division's cap. Exercises multi-division
// bracket scheduling. Returns the new event id.
func (s *Service) seedMultiDivMixed(ownerID, tournFmt, label string) (string, error) {
	evRows, err := s.sb.Insert("events", map[string]any{
		"name": "TEST · Mixed Doubles · 150 · 3 div · " + label, "format": "doubles",
		"partner_mode": "fixed", "scoring_mode": "wins", "tournament_format": tournFmt,
		"num_courts": 12, "points_to_win": 11, "dupr_sanctioned": false, "status": "open",
		"location": "Test Courts", "owner_id": ownerID, "listed": false,
	})
	if err != nil || len(evRows) == 0 {
		return "", fmt.Errorf("seed event: %w", err)
	}
	eventID := asStr(evRows[0], "id")
	prefix := eventID[:8]
	if err := s.ensureCourts(eventID, 12); err != nil {
		return "", fmt.Errorf("seed courts: %w", err)
	}

	divs := []struct {
		name   string
		lo, hi float64
	}{
		{"Mixed 3.0-3.5", 3.0, 3.5},
		{"Mixed 3.5-4.0", 3.5, 4.0},
		{"Mixed 4.0-4.5", 4.0, 4.5},
	}
	startN := 0
	for di, d := range divs {
		brRows, err := s.sb.Insert("brackets", map[string]any{
			"event_id": eventID, "name": d.name, "division_type": "mixed_doubles",
			"min_rating": d.lo, "max_rating": d.hi, "dupr_min": d.lo, "dupr_max": d.hi,
			"sort_order": di,
		})
		if err != nil || len(brRows) == 0 {
			return "", fmt.Errorf("seed bracket: %w", err)
		}
		if err := s.seedDivPairs(eventID, asStr(brRows[0], "id"), prefix, startN, 25, d.lo, d.hi); err != nil {
			return "", err
		}
		startN += 50 // 25 pairs = 50 players per division
	}
	return eventID, nil
}

// seedDivPairs bulk-inserts `pairs` fixed M/F mixed-doubles pairs (2*pairs players)
// rated within [lo, hi) — ~1 in 6 over hi — and registers them to bracketID. n runs
// globally (startN+1 ..) so dupr_id/phone stay unique across divisions.
func (s *Service) seedDivPairs(eventID, bracketID, prefix string, startN, pairs int, lo, hi float64) error {
	male := []string{"Mike", "John", "Dave", "Carl", "Sam", "Tom", "Alex", "Ben", "Will", "Jake", "Luis", "Ray", "Nick", "Paul", "Kev"}
	female := []string{"Mia", "Jen", "Sara", "Ana", "Kim", "Liz", "Emma", "Beth", "Nina", "Tara", "Lucy", "Rosa", "Dana", "Pam", "Kate"}
	last := []string{"Lee", "Ng", "Diaz", "Park", "Cruz", "Hall", "Reed", "Shaw", "Vance", "Wood"}
	count := pairs * 2
	playerRows := make([]map[string]any, count)
	for i := 0; i < count; i++ {
		n := startN + i + 1
		rating := lo + float64(n%5)*0.1 // within [lo, hi)
		if n%6 == 0 {
			rating = hi + 0.2 // over this division's cap
		}
		first := female[n%len(female)]
		if n%2 == 1 {
			first = male[n%len(male)]
		}
		playerRows[i] = map[string]any{
			"full_name":        fmt.Sprintf("%s %s %d", first, last[n%len(last)], n),
			"dupr_id":          fmt.Sprintf("TST-%s-%d", prefix, n),
			"dupr_rating":      rating,
			"dupr_reliability": 85,
			"phone":            fmt.Sprintf("+1555%07d", n),
		}
	}
	plRows, err := s.sb.Insert("players", playerRows)
	if err != nil || len(plRows) != count {
		return fmt.Errorf("seed div players (%d/%d): %w", len(plRows), count, err)
	}
	idByN := make(map[int]string, count)
	for _, row := range plRows {
		parts := strings.Split(asStr(row, "dupr_id"), "-")
		if len(parts) >= 3 {
			if k, e := strconv.Atoi(parts[len(parts)-1]); e == nil {
				idByN[k] = asStr(row, "id")
			}
		}
	}
	ts := now()
	mk := func(p, partner string) map[string]any {
		return map[string]any{
			"event_id": eventID, "player_id": p, "partner_id": partner,
			"bracket_id": bracketID, "payment_status": "comped", "checked_in": true,
			"checked_in_at": ts, "check_in_method": "manual",
		}
	}
	var regRows []map[string]any
	for k := startN + 1; k+1 <= startN+count; k += 2 {
		p1, p2 := idByN[k], idByN[k+1]
		if p1 == "" || p2 == "" {
			continue
		}
		regRows = append(regRows, mk(p1, p2), mk(p2, p1))
	}
	_, err = s.sb.Insert("registrations", regRows)
	return err
}

// seedPodium builds a SMALL pools→playoff doubles event and AUTO-PLAYS every match
// (pools, then the 4-team medal bracket) to completion, so it opens already on a gold /
// silver / bronze podium — no manual scoring. The medal bracket (top-4 playoff) is what
// creates the in-bracket bronze game the podium reads; single-elim's back-draw does not.
func (s *Service) seedPodium(ownerID string) (string, error) {
	evRows, err := s.sb.Insert("events", map[string]any{
		"name": "TEST · Podium · gold/silver/bronze", "format": "doubles",
		"partner_mode": "fixed", "scoring_mode": "wins", "tournament_format": "pools_playoff",
		"num_courts": 4, "points_to_win": 11, "dupr_sanctioned": false, "status": "open",
		"location": "Test Courts", "owner_id": ownerID, "listed": false,
	})
	if err != nil || len(evRows) == 0 {
		return "", fmt.Errorf("seed event: %w", err)
	}
	eventID := asStr(evRows[0], "id")
	if err := s.ensureCourts(eventID, 4); err != nil {
		return "", fmt.Errorf("seed courts: %w", err)
	}
	brRows, err := s.sb.Insert("brackets", map[string]any{
		"event_id": eventID, "name": "Open", "division_type": "open",
		"min_rating": 3.0, "max_rating": 5.0, "dupr_min": 3.0, "dupr_max": 5.0, "sort_order": 0,
	})
	if err != nil || len(brRows) == 0 {
		return "", fmt.Errorf("seed bracket: %w", err)
	}
	if err := s.seedDivPairs(eventID, asStr(brRows[0], "id"), eventID[:8], 0, 8, 3.0, 5.0); err != nil {
		return "", err
	}
	if _, err := s.GenerateSchedule(eventID, false, true); err != nil {
		return "", fmt.Errorf("seed schedule: %w", err)
	}
	if err := s.autoPlayEvent(eventID); err != nil {
		return "", fmt.Errorf("seed autoplay: %w", err)
	}
	// Mark the event finished so it reads as completed (champion treatment).
	_, _ = s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"status": "completed"})
	return eventID, nil
}

// autoPlayEvent scores every ready match (both sides filled) 11-7 to the higher slot,
// repeating as pools complete (which auto-seeds the playoff) and bracket winners
// advance, until no scheduled match has two teams. Deterministic → a stable podium.
func (s *Service) autoPlayEvent(eventID string) error {
	for iter := 0; iter < 80; iter++ {
		rows, err := s.sb.SelectAll("matches",
			"event_id=eq."+store.Q(eventID)+"&status=eq.scheduled"+
				"&select=id,match_participants(team)")
		if err != nil {
			return err
		}
		scoredAny := false
		for _, r := range rows {
			teams := map[int]bool{}
			if ps, ok := r["match_participants"].([]any); ok {
				for _, p := range ps {
					if pm, ok := p.(map[string]any); ok {
						teams[asInt(pm, "team")] = true
					}
				}
			}
			if teams[1] && teams[2] { // both sides known → playable
				if err := s.applyScore(asStr(r, "id"), 11, 7); err != nil {
					return err
				}
				scoredAny = true
			}
		}
		if !scoredAny {
			break
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
func (s *Service) FillRandomPlayers(eventID, bracketID string) (int, error) {
	bks, err := s.GetBrackets(eventID)
	if err != nil {
		return 0, err
	}
	// When a specific division is requested (the Players tab passes the division
	// the organizer is viewing), seed ONLY into it; otherwise spread across all.
	if bracketID != "" {
		var only []model.Bracket
		for _, b := range bks {
			if b.ID == bracketID {
				only = append(only, b)
				break
			}
		}
		if len(only) == 0 {
			return 0, errors.New("division not found for this event")
		}
		bks = only
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
			// Append the running player number so demo names stay unique: the
			// name arrays wrap at len(first)/len(last), so without this, player
			// #1 in one division and #31 in another collide on the same name and
			// look like one player appearing in two divisions.
			FullName:        first[(base+idx)%len(first)] + " " + last[(base*3+idx*7)%len(last)] + " " + fmt.Sprintf("%d", base+idx+1),
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

// seedMlp creates a TEST MLP-style team event: 6 teams of 16 players (8 men + 8
// women each), then generates the round-robin of ties. Lines are left unscored
// so QA can score them on the board and watch the tie roll up.
func (s *Service) seedMlp(ownerID string, premier bool) (string, error) {
	teamSize, perGender := 4, 8
	name := "TEST · MLP · 6 teams"
	if premier {
		teamSize, perGender = 6, 3
		name = "TEST · MLP Premier · 6 teams"
	}
	evRows, err := s.sb.Insert("events", map[string]any{
		"name": name, "format": "doubles", "partner_mode": "fixed",
		"scoring_mode": "wins", "tournament_format": "round_robin", "num_courts": 8,
		"points_to_win": 11, "win_by": 2, "best_of": 1, "team_size": teamSize,
		"dupr_sanctioned": false, "status": "open",
		"location": "Test Courts", "owner_id": ownerID, "listed": false,
	})
	if err != nil || len(evRows) == 0 {
		return "", fmt.Errorf("seed mlp event: %w", err)
	}
	eventID := asStr(evRows[0], "id")
	if err := s.ensureCourts(eventID, 8); err != nil {
		return "", fmt.Errorf("seed mlp courts: %w", err)
	}

	male := []string{"Mike", "John", "Dave", "Carl", "Sam", "Tom", "Alex", "Ben"}
	female := []string{"Mia", "Jen", "Sara", "Ana", "Kim", "Liz", "Emma", "Beth"}
	last := []string{"Lee", "Ng", "Diaz", "Park", "Cruz", "Hall", "Reed", "Shaw"}

	for t := 0; t < 6; t++ {
		teamRows, err := s.sb.Insert("event_teams", []map[string]any{
			{"event_id": eventID, "name": fmt.Sprintf("Team %d", t+1)},
		})
		if err != nil || len(teamRows) == 0 {
			return "", fmt.Errorf("seed mlp team: %w", err)
		}
		teamID := asStr(teamRows[0], "id")

		// 16 per team = 8 men + 8 women. Bulk-mint players, then bulk-link members
		// (insert order is preserved, so the returned rows align with names/genders).
		players := make([]map[string]any, 0, 16)
		names := make([]string, 0, 16)
		genders := make([]string, 0, 16)
		for g := 0; g < perGender; g++ {
			mn := fmt.Sprintf("%s %s", male[g], last[(t+g)%len(last)])
			fn := fmt.Sprintf("%s %s", female[g], last[(t+g+1)%len(last)])
			rating := 3.0 + float64(g%6)*0.2
			players = append(players, map[string]any{
				"full_name":   mn,
				"phone":       fmt.Sprintf("+1555%03d%04d", t, g*2),
				"dupr_id":     fmt.Sprintf("TST-%s-%dm%d", eventID[:8], t, g),
				"dupr_rating": rating,
			})
			names = append(names, mn)
			genders = append(genders, "M")
			players = append(players, map[string]any{
				"full_name":   fn,
				"phone":       fmt.Sprintf("+1555%03d%04d", t, g*2+1),
				"dupr_id":     fmt.Sprintf("TST-%s-%df%d", eventID[:8], t, g),
				"dupr_rating": rating,
			})
			names = append(names, fn)
			genders = append(genders, "F")
		}
		plRows, err := s.sb.Insert("players", players)
		if err != nil || len(plRows) != len(players) {
			return "", fmt.Errorf("seed mlp players: %w", err)
		}
		members := make([]map[string]any, len(plRows))
		for i, pr := range plRows {
			members[i] = map[string]any{
				"team_id": teamID, "player_id": asStr(pr, "id"),
				"full_name": names[i], "gender": genders[i],
			}
		}
		if _, err := s.sb.Insert("event_team_members", members); err != nil {
			return "", fmt.Errorf("seed mlp members: %w", err)
		}
	}

	if _, err := s.GenerateTeamTies(eventID); err != nil {
		return "", fmt.Errorf("seed mlp schedule: %w", err)
	}
	return eventID, nil
}

// seedPikelbol sets up the real "Pikelbol Adiks 2026 Masters" event: 9 teams of
// 8, Premier (team_size 6). Genders are a BEST-GUESS from first names — the
// organizer reviews/fixes them in Manage Teams, then generates the schedule. It
// deliberately does NOT auto-generate ties (lineups depend on corrected genders).
// seedGreensRetro builds the real "GREENS vs RETRO" club day (Saturday, July 4,
// 8:35 AM) from the printed schedule: four division courts (3-6) with named
// doubles pairings at set times, cumulative-points scoring ("add all scores in
// all 3 games", win by 2, no playoffs). The two Court-3 matchups annotated
// "3x rounds" repeat three times; every other listed matchup is one game.
// Matches are created MANUALLY (CreateManualGame) so the printed court + order
// are preserved exactly — no Build schedule needed (in-app slot times are the
// standard cascade approximation of the printed times).
func (s *Service) seedGreensRetro(ownerID string) (string, error) {
	evRows, err := s.sb.Insert("events", map[string]any{
		"name": "GREENS vs RETRO", "format": "doubles",
		"partner_mode": "rotating", "scoring_mode": "points",
		"tournament_format": "round_robin", "num_courts": 6,
		"points_to_win": 11, "win_by": 2, "best_of": 1,
		// 15-minute games: each Int-3 matchup plays its 3 rounds inside one
		// printed 45-minute window (8:35-9:20 etc.) on a shared 15-min wave grid.
		"game_duration_minutes": 15,
		"dupr_sanctioned":       false, "status": "open",
		"starts_at": "2026-07-04T15:35:00Z", // Sat Jul 4, 8:35 AM PDT
		"description": "Official scorers: Myles and Kay — please report your score " +
			"to them after each game. Each team plays 3 games per match; add all " +
			"scores in all 3 games; win by 2 points. No playoffs. Open play after " +
			"the games on Courts 5 and 6.",
		"owner_id": ownerID,
	})
	if err != nil || len(evRows) == 0 {
		return "", fmt.Errorf("seed greensretro event: %w", err)
	}
	eventID := asStr(evRows[0], "id")
	if err := s.ensureCourts(eventID, 6); err != nil {
		return eventID, fmt.Errorf("seed greensretro courts: %w", err)
	}

	// One division per court, in the printed order.
	divs := []struct {
		name  string
		court int
	}{
		{"Intermediate 3", 3},
		{"Intermediate 2", 4},
		{"Intermediate 1", 5},
		{"Intermediate 1 + Senior", 6},
	}
	bracketByCourt := map[int]string{}
	for i, d := range divs {
		brRows, err := s.sb.Insert("brackets", map[string]any{
			"event_id": eventID, "name": d.name, "division_type": "open",
			"min_rating": 2.5, "max_rating": 5.0, "dupr_min": 2.5, "dupr_max": 5.0,
			"sort_order": i,
		})
		if err != nil || len(brRows) == 0 {
			return eventID, fmt.Errorf("seed greensretro bracket %q: %w", d.name, err)
		}
		bracketByCourt[d.court] = asStr(brRows[0], "id")
	}

	// Roster per division (players appear once even when they play twice;
	// Franze plays on Courts 5 AND 6 but is registered once, under Int 1).
	roster := map[int][]string{
		3: {"Angelica", "Pao", "Genergy", "Joyce", "Jon", "Lloyd", "Ed",
			"DocLet", "Twinkle", "Jane"},
		4: {"Sheila", "Rose Lefty", "Arleen", "Carina", "Mico", "Little Mario",
			"Carlos", "Raul", "Pete", "Araceli", "Marlon"},
		5: {"Francia", "Rose", "Ofel", "Chona", "Erin", "Jobert", "Franze"},
		6: {"Kuya Mario", "Bobby", "Rafa", "Pete Sr", "Edwin", "KC"},
	}
	idByName := map[string]string{}
	for court, names := range roster {
		playerRows := make([]map[string]any, len(names))
		for i, n := range names {
			playerRows[i] = map[string]any{"full_name": n}
		}
		plRows, err := s.sb.Insert("players", playerRows)
		if err != nil || len(plRows) != len(names) {
			return eventID, fmt.Errorf("seed greensretro players (court %d): %w", court, err)
		}
		regRows := make([]map[string]any, len(plRows))
		for i, pr := range plRows {
			idByName[asStr(pr, "full_name")] = asStr(pr, "id")
			regRows[i] = map[string]any{
				"event_id": eventID, "player_id": asStr(pr, "id"),
				"bracket_id": bracketByCourt[court], "payment_status": "comped",
			}
		}
		if _, err := s.sb.Insert("registrations", regRows); err != nil {
			return eventID, fmt.Errorf("seed greensretro regs (court %d): %w", court, err)
		}
	}

	// The schedule (organizer feedback 2026-07-03 v2): every Intermediate 3
	// matchup plays 3 rounds ON COURT 3, with all three rounds squeezed into its
	// printed 45-min window. The whole day runs on a shared 15-minute wave grid
	// (event game_duration=15), so wave n starts at 8:35 + 15n:
	//   w0 8:35 · w1 8:50 · w2 9:05 · w3 9:20 · w4 9:35 · w5 9:50
	//   w6 10:05 · w7 10:20 · w8 10:35
	// Int-3: rounds fill w0-2 (8:35-9:20), w3-5 (9:20-10:15), w6-8 (10:15-11:00).
	// Other courts pin their printed times to the nearest wave.
	type matchup struct {
		t1a, t1b, t2a, t2b string
		rounds             int
		wave               int // starting wave; rounds occupy wave, wave+1, ...
	}
	// Whole day on a 15-minute wave grid: wave n starts 8:35 + 15n, so the
	// 3-round blocks are w0-2 (8:35-9:20), w3-5 (9:20-10:05), w6-8 (10:05-10:50)
	// — each matchup's three rounds squeezed back-to-back on its own court, all
	// divisions aligned. (The printed 10:15/10:30 block edges can't BOTH sit on
	// one shared grid; blocks land within ~10 min and day-of starts are manual.)
	schedule := map[int][]matchup{
		3: { // Intermediate 3 — all on Court 3, 3 rounds per matchup
			{"Angelica", "Pao", "Genergy", "Joyce", 3, 0}, // block 1
			{"Jon", "Lloyd", "Ed", "Genergy", 3, 3},       // block 2
			{"DocLet", "Twinkle", "Jane", "Joyce", 3, 6},  // block 3
		},
		4: { // Intermediate 2 — all on Court 4, 3 rounds per matchup
			{"Sheila", "Rose Lefty", "Arleen", "Carina", 3, 0}, // block 1
			{"Mico", "Little Mario", "Carlos", "Raul", 3, 3},   // block 2
			{"Pete", "Araceli", "Marlon", "Carina", 3, 6},      // block 3
		},
		5: { // Intermediate 1 — all on Court 5, 3 rounds per matchup
			{"Francia", "Rose", "Ofel", "Chona", 3, 0},  // block 1 (8:35-)
			{"Erin", "Jobert", "Franze", "Chona", 3, 4}, // block 2 (~9:40-, open play after)
		},
		6: { // Intermediate 1 + Senior — all on Court 6, 3 rounds per matchup
			{"Kuya Mario", "Bobby", "Rafa", "Franze", 3, 0}, // block 1 (8:35-)
			{"Pete Sr", "Edwin", "Rafa", "KC", 3, 4},        // block 2 (~9:40-, open play after)
		},
	}
	for court, games := range schedule {
		for _, g := range games {
			for r := 0; r < g.rounds; r++ {
				if _, err := s.CreateManualGame(eventID, bracketByCourt[court],
					court, g.wave+r, 0, 0,
					[]string{idByName[g.t1a], idByName[g.t1b]},
					[]string{idByName[g.t2a], idByName[g.t2b]}); err != nil {
					return eventID, fmt.Errorf("seed greensretro game (court %d wave %d): %w", court, g.wave+r, err)
				}
			}
		}
	}
	return eventID, nil
}

func (s *Service) seedPikelbol(ownerID string) (string, error) {
	evRows, err := s.sb.Insert("events", map[string]any{
		"name": "Pikelbol Adiks 2026 Masters", "format": "doubles",
		"partner_mode": "fixed", "scoring_mode": "wins",
		"tournament_format": "round_robin", "num_courts": 6,
		"points_to_win": 11, "win_by": 2, "best_of": 1, "team_size": 6,
		"dupr_sanctioned": false, "status": "open",
		"location": "San Diego", "owner_id": ownerID, "listed": false,
	})
	if err != nil || len(evRows) == 0 {
		return "", fmt.Errorf("seed pikelbol event: %w", err)
	}
	eventID := asStr(evRows[0], "id")
	if err := s.ensureCourts(eventID, 6); err != nil {
		return "", fmt.Errorf("seed pikelbol courts: %w", err)
	}
	type pk struct{ n, g string }
	teams := [][]pk{
		{{"Gayle", "F"}, {"Lolit", "F"}, {"Twinkle", "F"}, {"Ivan", "M"}, {"Jessa B.", "F"}, {"Mario", "M"}, {"EJ", "M"}, {"Alex", "M"}},
		{{"Krizhia", "F"}, {"Janella", "F"}, {"Marissa", "F"}, {"Bobby", "M"}, {"Rose", "F"}, {"JP L.", "M"}, {"Leif D.", "M"}, {"Randy", "M"}},
		{{"Lhou", "F"}, {"Marife", "F"}, {"Sheila", "F"}, {"Sid", "M"}, {"Nikki", "F"}, {"Zavier", "M"}, {"Howie", "M"}, {"Joshua", "M"}},
		{{"Anne M.", "F"}, {"Angeli", "F"}, {"Mae", "F"}, {"Aaron", "M"}, {"Shirley", "F"}, {"Vicente", "M"}, {"Andrew", "M"}, {"Aris", "M"}},
		{{"Mafie", "F"}, {"Megen", "F"}, {"Alliyah", "F"}, {"Marvin", "M"}, {"Yen", "F"}, {"Allan", "M"}, {"Ivan Med.", "M"}, {"Amiel", "M"}},
		{{"Joan Y.", "F"}, {"Tin B.", "F"}, {"Janet", "F"}, {"Ian A.", "M"}, {"Myles", "M"}, {"David", "M"}, {"Rafael", "M"}, {"Chrix", "M"}},
		{{"Sarah", "F"}, {"Joan L.", "F"}, {"Jonelle", "F"}, {"Miguel", "M"}, {"Michelle", "F"}, {"Jon N.", "M"}, {"Joel", "M"}, {"Tristan", "M"}},
		{{"Belle", "F"}, {"Ysabella", "F"}, {"Angelica", "F"}, {"Pete", "M"}, {"Verna", "F"}, {"Nathan", "M"}, {"Jeff", "M"}, {"Jimmy", "M"}},
		{{"Eden", "F"}, {"Ann L.", "F"}, {"Carina", "F"}, {"Gilbert", "M"}, {"Arlene", "F"}, {"Ricky", "M"}, {"Ernest", "M"}, {"Quynton", "M"}},
	}
	for i, members := range teams {
		teamRows, err := s.sb.Insert("event_teams", []map[string]any{
			{"event_id": eventID, "name": fmt.Sprintf("Team %d", i+1)},
		})
		if err != nil || len(teamRows) == 0 {
			return eventID, fmt.Errorf("seed pikelbol team %d: %w", i+1, err)
		}
		teamID := asStr(teamRows[0], "id")
		players := make([]map[string]any, len(members))
		for j, m := range members {
			players[j] = map[string]any{"full_name": m.n}
		}
		plRows, err := s.sb.Insert("players", players)
		if err != nil || len(plRows) != len(players) {
			return eventID, fmt.Errorf("seed pikelbol players t%d: %w", i+1, err)
		}
		rows := make([]map[string]any, len(plRows))
		for j, pr := range plRows {
			rows[j] = map[string]any{
				"team_id": teamID, "player_id": asStr(pr, "id"),
				"full_name": members[j].n, "gender": members[j].g,
			}
		}
		if _, err := s.sb.Insert("event_team_members", rows); err != nil {
			return eventID, fmt.Errorf("seed pikelbol members t%d: %w", i+1, err)
		}
	}
	return eventID, nil
}

// seedMlpScored builds an MLP event and fills in scores so the live team
// scoreboard is populated: a mix of completed ties (3–1, no DreamBreaker needed),
// in-progress ties (a couple lines done + one playing), and scheduled ties.
func (s *Service) seedMlpScored(ownerID string) (string, error) {
	eventID, err := s.seedMlp(ownerID, false)
	if err != nil {
		return "", err
	}
	_, _ = s.sb.Update("events", "id=eq."+store.Q(eventID), map[string]any{
		"name":   "TEST · MLP · scored",
		"status": "in_progress",
	})
	ties, err := s.ListTies(eventID)
	if err != nil {
		return eventID, err
	}
	setLine := func(matchID string, t1, t2, win int, status string) {
		row := map[string]any{"status": status}
		if status == "completed" {
			row["team1_score"], row["team2_score"], row["winning_team"] = t1, t2, win
		}
		_, _ = s.sb.Update("matches", "id=eq."+store.Q(matchID), row)
	}
	for i, tie := range ties {
		reg := make([]model.TieLine, 0, 4)
		for _, ln := range tie.Lines {
			if ln.LineType != "dec" {
				reg = append(reg, ln)
			}
		}
		switch i % 5 {
		case 0:
			// leave scheduled
		case 1, 2:
			// in progress: 2 lines done (1–1), 1 line playing
			if len(reg) >= 1 {
				setLine(reg[0].MatchID, 11, 6, 1, "completed")
			}
			if len(reg) >= 2 {
				setLine(reg[1].MatchID, 7, 11, 2, "completed")
			}
			if len(reg) >= 3 {
				setLine(reg[2].MatchID, 0, 0, 0, "in_progress")
			}
			_ = s.rollupTie(tie.ID)
		default:
			// completed: winner takes 3 of 4 (no 2–2, so no decider needed)
			winnerSide := 1
			if i%2 == 1 {
				winnerSide = 2
			}
			for j, ln := range reg {
				win := winnerSide
				if j == 3 {
					win = 3 - winnerSide
				}
				t1, t2 := 11, 6
				if win == 2 {
					t1, t2 = 6, 11
				}
				setLine(ln.MatchID, t1, t2, win, "completed")
			}
			_ = s.rollupTie(tie.ID)
		}
	}
	return eventID, nil
}

// seedMlpComplete builds an MLP event, plays every pool tie to a 3-1 result (no
// DreamBreaker), seeds the playoff, and plays it to a champion — a fully
// finished event for demoing final standings + the gold/silver/bronze podium.
func (s *Service) seedMlpComplete(ownerID string) (string, error) {
	eventID, err := s.seedMlp(ownerID, false)
	if err != nil {
		return "", err
	}
	_, _ = s.sb.Update("events", "id=eq."+store.Q(eventID), map[string]any{
		"name":   "TEST · MLP · champion",
		"status": "in_progress",
	})
	pool, err := s.ListTies(eventID)
	if err != nil {
		return eventID, err
	}
	for i, tie := range pool {
		s.scoreTie31(tie, 1+(i%2)) // alternate which side wins for a spread
	}
	if _, err := s.GeneratePlayoff(eventID); err != nil {
		return eventID, err
	}
	if err := s.scorePlayoffRounds(eventID); err != nil {
		return eventID, err
	}
	_, _ = s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"status": "completed"})
	return eventID, nil
}

// scoreTie31 fills a tie's 4 regulation lines so winnerSide (1 or 2) takes it
// 3-1 — a clean result with no DreamBreaker. Best-effort (seed helper).
func (s *Service) scoreTie31(tie model.TeamTie, winnerSide int) {
	reg := make([]model.TieLine, 0, 4)
	for _, ln := range tie.Lines {
		if ln.LineType != "dec" {
			reg = append(reg, ln)
		}
	}
	for j, ln := range reg {
		win := winnerSide
		if j == 3 {
			win = 3 - winnerSide // loser takes the last line -> 3-1
		}
		t1, t2 := 11, 6
		if win == 2 {
			t1, t2 = 6, 11
		}
		_, _ = s.sb.Update("matches", "id=eq."+store.Q(ln.MatchID), map[string]any{
			"team1_score": t1, "team2_score": t2, "winning_team": win, "status": "completed",
		})
	}
	_ = s.rollupTie(tie.ID)
}

// scorePlayoffRounds plays out the playoff: score every open playoff tie, which
// grows the next round, and repeat until a champion remains.
func (s *Service) scorePlayoffRounds(eventID string) error {
	for iter := 0; iter < 6; iter++ {
		ties, err := s.ListTies(eventID)
		if err != nil {
			return err
		}
		var open []model.TeamTie
		for _, t := range ties {
			if t.Stage == "playoff" && t.WinnerTeamID == nil {
				open = append(open, t)
			}
		}
		if len(open) == 0 {
			return nil
		}
		for i, t := range open {
			s.scoreTie31(t, 1+(i%2))
		}
	}
	return nil
}

// seedTournament creates the event, registers the demo players, generates the
// pool schedule, scores a `poolCompletion` fraction (0..1) of the pool matches,
// and reconciles each round's status to match. Used by SeedDemo (round-robin).
func (s *Service) seedTournament(req model.CreateEventRequest, poolCompletion float64, perDiv int, ownerID string) (string, error) {
	eid, err := s.CreateEvent(req, ownerID)
	if err != nil {
		return "", err
	}
	if err := s.registerDemoPlayers(eid, perDiv); err != nil {
		return "", err
	}

	if _, err := s.GenerateSchedule(eid, true, true); err != nil {
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
		if _, err := s.sb.Update("rounds", "id="+store.In(toCompleted)+"",
			map[string]any{"status": "completed"}); err != nil {
			return err
		}
	}
	if len(toActive) > 0 {
		if _, err := s.sb.Update("rounds", "id="+store.In(toActive)+"",
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

// bracketHasRows reports whether any row in [table] references this bracket
// (used to guard division deletion — registrations or matches).
func (s *Service) bracketHasRows(table, bracketID string) (bool, error) {
	row, err := s.sb.SelectOne(table,
		"bracket_id=eq."+store.Q(bracketID)+"&select=id")
	if err != nil {
		return false, err
	}
	return row != nil, nil
}

// SyncDivisions reconciles an event's divisions (brackets) with [divs] — the
// edit-tournament flow. Each input WITH an ID updates that bracket; inputs
// without an ID are inserted; existing brackets absent from [divs] are DELETED,
// but ONLY when empty (no registrations and no matches). Non-empty divisions are
// kept and their names returned in `blocked` so the UI can explain why. Never
// leaves the event with zero divisions (re-creates an "Open" if all are gone).
func (s *Service) SyncDivisions(eventID string, divs []model.BracketInput) ([]string, error) {
	existing, err := s.sb.Select("brackets",
		"event_id=eq."+store.Q(eventID)+"&select=id,name")
	if err != nil {
		return nil, err
	}
	existingName := map[string]string{}
	for _, b := range existing {
		existingName[asStr(b, "id")] = asStr(b, "name")
	}
	keep := map[string]bool{}

	for i, d := range divs {
		dt := d.DivisionType
		if dt == "" {
			dt = "open"
		}
		fields := map[string]any{
			"name":          d.Name,
			"min_rating":    fOrNull(d.MinRating),
			"max_rating":    fOrNull(d.MaxRating),
			"min_age":       iOrNull(d.MinAge),
			"max_age":       iOrNull(d.MaxAge),
			"division_type": dt,
			"dupr_min":      fOrNull(d.DuprMin),
			"dupr_max":      fOrNull(d.DuprMax),
			"sort_order":    i,
		}
		if d.ID != "" {
			if _, ok := existingName[d.ID]; !ok {
				return nil, errors.New("division does not belong to this event")
			}
			keep[d.ID] = true
			if _, err := s.sb.Update("brackets",
				"id=eq."+store.Q(d.ID)+"&event_id=eq."+store.Q(eventID), fields); err != nil {
				return nil, err
			}
		} else {
			fields["event_id"] = eventID
			if _, err := s.sb.Insert("brackets", fields); err != nil {
				return nil, err
			}
		}
	}

	var blocked []string
	for id, name := range existingName {
		if keep[id] {
			continue
		}
		hasRegs, err := s.bracketHasRows("registrations", id)
		if err != nil {
			return nil, err
		}
		hasMatches, err := s.bracketHasRows("matches", id)
		if err != nil {
			return nil, err
		}
		if hasRegs || hasMatches {
			blocked = append(blocked, name)
			continue
		}
		if err := s.sb.Delete("brackets",
			"id=eq."+store.Q(id)+"&event_id=eq."+store.Q(eventID)); err != nil {
			return nil, err
		}
	}

	// Never leave the event with zero divisions.
	remaining, err := s.sb.Select("brackets",
		"event_id=eq."+store.Q(eventID)+"&select=id")
	if err != nil {
		return nil, err
	}
	if len(remaining) == 0 {
		if _, err := s.sb.Insert("brackets", map[string]any{
			"event_id": eventID, "name": "Open", "division_type": "open",
			"sort_order": 0,
		}); err != nil {
			return nil, err
		}
	}

	// Keep events.format in sync with the divisions' play type — the scheduler
	// reads events.format (singles vs doubles), NOT division_type. Only divisions
	// with an EXPLICIT play type carry a signal; "open"/blank do not, so when none
	// are typed we leave events.format exactly as the organizer set it (otherwise a
	// routine edit of a singles event with a default "Open" division would silently
	// flip it to doubles). Singles only when every typed division is singles.
	typed, allSingles := false, true
	for _, d := range divs {
		if d.DivisionType == "" || d.DivisionType == "open" {
			continue
		}
		typed = true
		if d.DivisionType != "singles" {
			allSingles = false
		}
	}
	if typed {
		format := "doubles"
		if allSingles {
			format = "singles"
		}
		if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
			map[string]any{"format": format}); err != nil {
			return blocked, err
		}
	}
	return blocked, nil
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

// ErrAlreadyRegistered is returned when the same person is already registered
// for the event (matched by linked account, or by phone/email). The HTTP layer
// maps it to 409 Conflict so the client can show a friendly "already
// registered" message instead of a generic error.
var ErrAlreadyRegistered = errors.New("already registered for this event")

// registrationExistsByContact reports whether this event already has a
// registration whose player shares the given phone or email. Used to block
// silent duplicates from the organizer-add and anonymous self-register flows,
// where every submit creates a fresh player row (so there's no account/player
// collision to rely on). Registrations has two FKs to players (player_id +
// partner_id), which makes a PostgREST embed ambiguous — so we resolve the
// matching player ids first, then look for a registration that uses one.
func (s *Service) registrationExistsByContact(eventID string, req model.RegisterRequest) (bool, error) {
	check := func(col, val string) (bool, error) {
		val = strings.TrimSpace(val)
		name := strings.TrimSpace(req.FullName)
		if val == "" || name == "" {
			return false, nil
		}
		// Require the SAME NAME as well as the same contact — the same person
		// re-registering. Without the name match, family members who share a
		// phone/email would be wrongly blocked. (ilike = case-insensitive.)
		players, err := s.sb.Select("players",
			col+"=eq."+store.Q(val)+"&full_name=ilike."+store.Q(name)+"&select=id")
		if err != nil {
			return false, err
		}
		ids := make([]string, 0, len(players))
		for _, p := range players {
			if id := asStr(p, "id"); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return false, nil
		}
		dup, err := s.sb.SelectOne("registrations",
			"event_id=eq."+store.Q(eventID)+"&player_id="+store.In(ids)+"&select=id")
		if err != nil {
			return false, err
		}
		return dup != nil, nil
	}
	if ok, err := check("phone", req.Phone); err != nil || ok {
		return ok, err
	}
	return check("email", req.Email)
}

// RegisterPlayer files a registration. When linkUserID is non-empty (a
// logged-in user registering THEMSELVES), the player is tied to that account
// (players.user_id) — reusing the account's existing player row if it has one
// (the user_id column is unique) rather than creating a duplicate.
// duprTestAccountEmails are the DUPR UAT test logins the demo "Register DUPR
// testers" button enrolls into a sanctioned event. Each is linked to its
// PlanMyPickle account, so RegisterPlayer attaches the account's SSO-connected
// dupr_id automatically — making the resulting matches submittable to DUPR.
var duprTestAccountEmails = []string{
	"player1@duprtest.com",
	"player2@duprtest.com",
	"player3@duprtest.com",
	"player4@duprtest.com",
}

// DuprTestSummary reports the outcome of RegisterDuprTestAccounts.
type DuprTestSummary struct {
	Registered        int      `json:"registered"`
	AlreadyRegistered int      `json:"alreadyRegistered"`
	NotFound          []string `json:"notFound"`     // no PlanMyPickle account for the email
	NotConnected      []string `json:"notConnected"` // registered, but DUPR not connected yet
}

// RegisterDuprTestAccounts enrolls the fixed DUPR UAT test logins into an event
// (a demo helper for the DUPR walkthrough). It resolves each email to its auth
// user via public.profiles and registers them linked to that account, so
// RegisterPlayer attaches their connected dupr_id. Idempotent — an already-
// registered tester is counted, not duplicated. Accounts that exist but haven't
// connected DUPR yet are still registered and flagged in NotConnected (their
// match can't be submitted to DUPR until they connect).
func (s *Service) RegisterDuprTestAccounts(eventID string) (DuprTestSummary, error) {
	var sum DuprTestSummary
	for _, email := range duprTestAccountEmails {
		prof, err := s.sb.SelectOne("profiles",
			"email=eq."+store.Q(email)+"&select=id,full_name")
		if err != nil {
			return sum, err
		}
		if prof == nil {
			sum.NotFound = append(sum.NotFound, email)
			continue
		}
		uid := asStr(prof, "id")
		name := asStr(prof, "full_name")
		if name == "" {
			name = email
		}
		_, err = s.RegisterPlayer(eventID,
			model.RegisterRequest{FullName: name, Email: email}, uid)
		switch {
		case err == nil:
			sum.Registered++
		case errors.Is(err, ErrAlreadyRegistered):
			sum.AlreadyRegistered++
		default:
			return sum, err
		}
		if c, e := s.DuprConnection(uid); e != nil || !c.Connected {
			sum.NotConnected = append(sum.NotConnected, email)
		}
	}
	return sum, nil
}

// eventIsSanctioned reports whether an event is DUPR-sanctioned (best-effort: a
// lookup failure returns false so it can't wrongly block a registration).
func (s *Service) eventIsSanctioned(eventID string) bool {
	sanctioned, _ := s.eventDuprGate(eventID)
	return sanctioned
}

// eventDuprGate returns an event's DUPR gating: whether it's sanctioned (requires
// a connected account) and the minimum entitlement tier a self-registrant must
// hold ("" | PREMIUM_L1 | VERIFIED_L1). Best-effort — a lookup failure returns
// (false, "") so it can't wrongly block a registration.
func (s *Service) eventDuprGate(eventID string) (sanctioned bool, minEnt string) {
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=dupr_sanctioned,dupr_min_entitlement")
	if err != nil || ev == nil {
		return false, ""
	}
	minEnt = normalizeDuprEntitlement(asStr(ev, "dupr_min_entitlement"))
	return asBool(ev, "dupr_sanctioned") || minEnt != "", minEnt
}

func (s *Service) RegisterPlayer(eventID string, req model.RegisterRequest, linkUserID string) (model.Registration, error) {
	if strings.TrimSpace(req.FullName) == "" {
		return model.Registration{}, errors.New("fullName is required")
	}
	// Block a duplicate up front (before creating a player row): someone already
	// registered for this event with the same phone/email. The linked-account
	// path is also guarded below via the unique (event_id, player_id).
	if dup, err := s.registrationExistsByContact(eventID, req); err != nil {
		return model.Registration{}, err
	} else if dup {
		return model.Registration{}, ErrAlreadyRegistered
	}
	// DUPR id/rating are never typed by hand (DUPR forbids it) — for a signed-in
	// user, attach them from their SSO-connected DUPR account, the source of truth.
	// Load the connection once; reuse it for the sanctioned-event gate below.
	var conn model.DuprConnection
	if linkUserID != "" {
		conn, _ = s.DuprConnection(linkUserID)
		if conn.Connected {
			req.DuprID = conn.DuprID
			if conn.DoublesRating != nil {
				req.DuprRating = conn.DoublesRating
			}
		}
	}
	// A DUPR-sanctioned event REQUIRES a self-registering player to have DUPR
	// connected — their results must be submittable to DUPR. An organizer adding
	// players (req.Self == false) is trusted and not blocked. Anonymous self-
	// registration (linkUserID == "") is also blocked here: no account, no connection.
	//
	// This is a UX/data-quality guardrail, not a hard security boundary: req.Self
	// is client-controlled, so a crafted req.Self=false lands the caller as an
	// unlinked player — but then no dupr_id is ever attached, and the real backstop
	// (SubmitPendingToDupr fails a match with any participant missing a dupr_id)
	// still keeps un-DUPR'd results out of DUPR.
	if req.Self {
		sanctioned, minEnt := s.eventDuprGate(eventID)
		if sanctioned && !conn.Connected {
			return model.Registration{}, ErrDuprNotConnected
		}
		// DUPR+ tier: the connected player must hold ALL of the DUPR+ entitlements
		// (Premium + Verified, per DUPR's guidance — one consumer-facing name).
		// Checked with the user's own SSO token (24h-cached, auto-refreshed).
		// Fail OPEN if DUPR can't be reached — a lookup error must never
		// wrongfully block a legitimate registration.
		if minEnt != "" && conn.Connected {
			ents, err := s.userEntitlements(linkUserID)
			if err != nil {
				log.Printf("dupr: entitlements lookup for user %s failed (allowing registration): %v", linkUserID, err)
			} else {
				for _, need := range duprPlusEntitlements {
					if !containsFold(ents, need) {
						return model.Registration{}, fmt.Errorf("%w: this event requires a %s membership",
							ErrDuprEntitlementRequired, duprEntitlementLabel(minEnt))
					}
				}
			}
		}
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
			return model.Registration{}, ErrAlreadyRegistered
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

// ImportRoster bulk-registers many players into an event (owner-only). All go
// into req.BracketID; rows already registered (by phone/email) are skipped, not
// duplicated. Blank-name rows are ignored. Returns a per-import summary.
func (s *Service) ImportRoster(eventID string, req model.ImportRosterRequest) (model.ImportRosterResult, error) {
	var res model.ImportRosterResult
	for _, p := range req.Players {
		name := strings.TrimSpace(p.FullName)
		if name == "" {
			continue
		}
		_, err := s.RegisterPlayer(eventID, model.RegisterRequest{
			FullName:  name,
			Phone:     strings.TrimSpace(p.Phone),
			Email:     strings.TrimSpace(p.Email),
			BracketID: req.BracketID,
		}, "")
		switch {
		case err == nil:
			res.Added++
		case errors.Is(err, ErrAlreadyRegistered):
			res.Skipped++
		default:
			res.Failed++
			if len(res.Errors) < 8 {
				res.Errors = append(res.Errors, name+": "+err.Error())
			}
		}
	}
	return res, nil
}

// ImportDuprClubToEvent fetches the DUPR club's members and registers each into
// the event with their DUPR id + doubles rating. duprClubID "" -> the platform's
// configured DUPR club. Already-registered players are skipped.
func (s *Service) ImportDuprClubToEvent(eventID, bracketID, duprClubID string) (model.ImportRosterResult, error) {
	// If no DUPR club was passed, use the event's club's DUPR id (when the event
	// belongs to a club that has one set). The gateway falls back to the platform
	// club. Tolerates the dupr_club_id column not existing yet (pre-migration).
	if strings.TrimSpace(duprClubID) == "" {
		if ev, e := s.sb.SelectOne("events",
			"id=eq."+store.Q(eventID)+"&select=club_id"); e == nil && ev != nil {
			if cid := asStr(ev, "club_id"); cid != "" {
				if c, e := s.sb.SelectOne("clubs",
					"id=eq."+store.Q(cid)+"&select=dupr_club_id"); e == nil && c != nil {
					duprClubID = asStr(c, "dupr_club_id")
				}
			}
		}
	}
	members, err := s.Dupr.ClubMembers(duprClubID)
	if err != nil {
		return model.ImportRosterResult{}, err
	}
	var res model.ImportRosterResult
	for _, m := range members {
		name := strings.TrimSpace(m.FullName)
		if name == "" {
			continue
		}
		var rating *float64
		if d, e := strconv.ParseFloat(strings.TrimSpace(m.Doubles), 64); e == nil {
			rating = &d
		}
		_, err := s.RegisterPlayer(eventID, model.RegisterRequest{
			FullName:   name,
			DuprID:     m.DuprID,
			DuprRating: rating,
			BracketID:  bracketID,
		}, "")
		switch {
		case err == nil:
			res.Added++
		case errors.Is(err, ErrAlreadyRegistered):
			res.Skipped++
		default:
			res.Failed++
			if len(res.Errors) < 8 {
				res.Errors = append(res.Errors, name+": "+err.Error())
			}
		}
	}
	return res, nil
}

func (s *Service) Registrations(eventID string) ([]model.Registration, error) {
	// registrations has two FKs to players (player_id, partner_id) so the embed
	// must name the FK column; alias both embeds to stable keys. partner is the
	// registered partner's player row (for paired doubles); partner_name is the
	// free-text partner column.
	base := "event_id=eq." + store.Q(eventID) +
		"&select=id,event_id,player_id,partner_id,bracket_id,payment_status,checked_in,check_in_token,%s" +
		"player:players!player_id(full_name,phone,dupr_id,dupr_rating,skill_level,user_id)," +
		"partner:players!partner_id(full_name)," +
		"bracket:brackets(min_rating,max_rating)"
	rows, err := s.sb.Select("registrations", fmt.Sprintf(base, "partner_name,"))
	if err != nil {
		// Tolerate the partner_name column not existing yet (pre-migration): retry
		// without it so the roster never breaks. Free-text partner notes simply
		// don't appear until the migration runs.
		rows, err = s.sb.Select("registrations", fmt.Sprintf(base, ""))
		if err != nil {
			return nil, err
		}
	}
	out := make([]model.Registration, 0, len(rows))
	uids := make([]string, len(rows))
	for i, r := range rows {
		out = append(out, mapRegistration(r))
		if p := asMap(r, "player"); p != nil {
			uids[i] = asStr(p, "user_id")
		}
	}
	// Batch-load account profile photos for registrants linked to an app account
	// (one query); name-only players have no user_id -> initials fallback.
	photos := s.photosByUser(uids)
	for i := range out {
		if uids[i] != "" {
			out[i].PhotoURL = photos[uids[i]]
		}
	}
	return out, nil
}

// photosByUser batch-loads account profile photos (pmp_profiles.photo_url) for a
// set of user ids in ONE query. Best-effort: returns an empty/partial map on
// error so the roster never breaks. Keyed by user_id; only non-empty photos.
func (s *Service) photosByUser(userIDs []string) map[string]string {
	out := map[string]string{}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(userIDs))
	for _, u := range userIDs {
		if u != "" && !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	if len(uniq) == 0 {
		return out
	}
	rows, err := s.sb.Select("pmp_profiles",
		"user_id="+store.In(uniq)+"&select=user_id,photo_url")
	if err != nil {
		return out
	}
	for _, r := range rows {
		if url := asStr(r, "photo_url"); url != "" {
			out[asStr(r, "user_id")] = url
		}
	}
	return out
}

// eventHasMatches reports whether a schedule has already been generated for the
// event (any match row). Used to warn that changing a pairing leaves the live
// schedule stale until it's regenerated.
func (s *Service) eventHasMatches(eventID string) (bool, error) {
	m, err := s.sb.SelectOne("matches", "event_id=eq."+store.Q(eventID)+"&select=id")
	if err != nil {
		return false, err
	}
	return m != nil, nil
}

type partnerRegRow struct {
	id, eventID, playerID, bracketID, partnerID string
}

func (s *Service) loadPartnerRegRow(regID string) (partnerRegRow, error) {
	row, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(regID)+"&select=id,event_id,player_id,bracket_id,partner_id")
	if err != nil {
		return partnerRegRow{}, err
	}
	if row == nil {
		return partnerRegRow{}, errors.New("registration not found")
	}
	return partnerRegRow{
		id:        asStr(row, "id"),
		eventID:   asStr(row, "event_id"),
		playerID:  asStr(row, "player_id"),
		bracketID: asStr(row, "bracket_id"),
		partnerID: asStr(row, "partner_id"),
	}, nil
}

// unlinkPartnerOf clears partner_id/partner_name on whichever registration in
// the event currently points BACK at playerID — i.e. the other side of a mutual
// link — so re-pairing or clearing never leaves a dangling one-way link.
func (s *Service) unlinkPartnerOf(eventID, playerID string) error {
	if playerID == "" {
		return nil
	}
	_, err := s.sb.Update("registrations",
		"event_id=eq."+store.Q(eventID)+"&partner_id=eq."+store.Q(playerID),
		map[string]any{"partner_id": nil, "partner_name": nil})
	return err
}

// SetPartner sets a doubles registration's partner and returns whether the
// event's schedule is now stale (a schedule already exists and a real pairing
// changed). Modes:
//   - partnerRegID set: pair two REGISTERED players. Both must be in the same
//     event + division; writes partner_id mutually on both, clears free-text
//     notes, and flips the event to fixed partners so the schedule keeps the
//     pair together.
//   - partnerName set (partnerRegID empty): record a free-text partner note for
//     an un-registered partner (no schedule effect, no mode change).
//   - both empty: clear the registration's partner.
func (s *Service) SetPartner(regID, partnerRegID, partnerName string) (bool, error) {
	reg, err := s.loadPartnerRegRow(regID)
	if err != nil {
		return false, err
	}
	ev, err := s.GetEvent(reg.eventID)
	if err != nil {
		return false, err
	}
	if ev.Format != "doubles" {
		return false, errors.New("partners can only be set on doubles events")
	}
	partnerName = strings.TrimSpace(partnerName)
	hasMatches, err := s.eventHasMatches(reg.eventID)
	if err != nil {
		return false, err
	}

	switch {
	case partnerRegID != "":
		if partnerRegID == regID {
			return false, errors.New("a player can't be their own partner")
		}
		other, err := s.loadPartnerRegRow(partnerRegID)
		if err != nil {
			return false, err
		}
		if other.eventID != reg.eventID {
			return false, errors.New("partner is not registered for this event")
		}
		if other.bracketID != reg.bracketID {
			return false, errors.New("partners must be in the same division")
		}
		// Whether this actually changes the pairing — re-confirming an existing
		// A+B pair shouldn't warn that the schedule is stale.
		pairingChanged :=
			reg.partnerID != other.playerID || other.partnerID != reg.playerID
		// Break any prior mutual links on either side before re-pairing.
		if reg.partnerID != "" && reg.partnerID != other.playerID {
			if err := s.unlinkPartnerOf(reg.eventID, reg.playerID); err != nil {
				return false, err
			}
		}
		if other.partnerID != "" && other.partnerID != reg.playerID {
			if err := s.unlinkPartnerOf(reg.eventID, other.playerID); err != nil {
				return false, err
			}
		}
		if _, err := s.sb.Update("registrations", "id=eq."+store.Q(reg.id),
			map[string]any{"partner_id": other.playerID, "partner_name": nil}); err != nil {
			return false, err
		}
		if _, err := s.sb.Update("registrations", "id=eq."+store.Q(other.id),
			map[string]any{"partner_id": reg.playerID, "partner_name": nil}); err != nil {
			return false, err
		}
		if ev.PartnerMode != "fixed" {
			if _, err := s.sb.Update("events", "id=eq."+store.Q(reg.eventID),
				map[string]any{"partner_mode": "fixed"}); err != nil {
				return false, err
			}
		}
		return hasMatches && pairingChanged, nil

	case partnerName != "":
		if reg.partnerID != "" {
			if err := s.unlinkPartnerOf(reg.eventID, reg.playerID); err != nil {
				return false, err
			}
		}
		if _, err := s.sb.Update("registrations", "id=eq."+store.Q(reg.id),
			map[string]any{"partner_id": nil, "partner_name": partnerName}); err != nil {
			return false, err
		}
		if err := s.revertToRotatingIfNoPairs(ev); err != nil {
			return false, err
		}
		return hasMatches && reg.partnerID != "", nil

	default:
		if reg.partnerID != "" {
			if err := s.unlinkPartnerOf(reg.eventID, reg.playerID); err != nil {
				return false, err
			}
		}
		if _, err := s.sb.Update("registrations", "id=eq."+store.Q(reg.id),
			map[string]any{"partner_id": nil, "partner_name": nil}); err != nil {
			return false, err
		}
		if err := s.revertToRotatingIfNoPairs(ev); err != nil {
			return false, err
		}
		return hasMatches && reg.partnerID != "", nil
	}
}

// revertToRotatingIfNoPairs flips a fixed-partner doubles event back to rotating
// once its last registered pair is removed. Pairing auto-switches a rotating
// event to fixed, so unpairing the final team should be symmetric — a fixed
// event with zero pairs is meaningless, and re-pairing flips it back to fixed.
func (s *Service) revertToRotatingIfNoPairs(ev model.Event) error {
	if ev.Format != "doubles" || ev.PartnerMode != "fixed" {
		return nil
	}
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(ev.ID)+"&partner_id=not.is.null&select=id")
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return nil
	}
	_, err = s.sb.Update("events", "id=eq."+store.Q(ev.ID),
		map[string]any{"partner_mode": "rotating"})
	return err
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
func (s *Service) GenerateSchedule(eventID string, force, arrange bool) (model.ScheduleResult, error) {
	// Refuse to wipe an in-progress event's scores unless explicitly forced.
	if !force {
		done, err := s.completedMatchCount(eventID)
		if err != nil {
			return model.ScheduleResult{}, err
		}
		if done > 0 {
			return model.ScheduleResult{Matches: done},
				fmt.Errorf("%w: %d match(es) already scored", ErrScheduleHasResults, done)
		}
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return model.ScheduleResult{}, err
	}
	// Team events: NEVER wipe + regenerate from registrations (there are none, so
	// that would just delete the tie lines). Instead (re)build the ties from the
	// rosters and arrange them onto courts — same as the Teams screen's "Rebuild
	// schedule", and self-healing if the lines were previously wiped.
	if ev.TeamSize > 0 {
		n, err := s.GenerateTeamTies(eventID)
		if err != nil {
			return model.ScheduleResult{}, err
		}
		return model.ScheduleResult{Matches: n * len(tieLineOrder)}, nil
	}
	bks, err := s.GetBrackets(eventID)
	if err != nil {
		return model.ScheduleResult{}, err
	}
	courtByNum, err := s.courtIDsByNumber(eventID)
	if err != nil {
		return model.ScheduleResult{}, err
	}
	skill, err := s.playerSkills()
	if err != nil {
		return model.ScheduleResult{}, err
	}
	if err := s.wipeAllMatches(eventID); err != nil {
		return model.ScheduleResult{}, err
	}

	total := 0
	var droppedIDs []string
	for _, b := range bks {
		regs, err := s.bracketRegs(eventID, b.ID)
		if err != nil {
			return model.ScheduleResult{}, err
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
		// A doubles player with no partner to pair with is left out of the draw —
		// collect them so the organizer is told instead of a silent drop.
		droppedIDs = append(droppedIDs, droppedDoublesPlayers(ev, regs)...)
		if ev.TournamentFormat == "single_elim" {
			sides := seedSides(sidesForBracket(ev, regs), skill)
			n, err := s.persistBracket(ev, b.ID, sides, ev.Consolation)
			if err != nil {
				return model.ScheduleResult{}, err
			}
			total += n
		} else if ev.TournamentFormat == "double_elim" {
			sides := seedSides(sidesForBracket(ev, regs), skill)
			n, err := s.persistDoubleElim(ev, b.ID, sides)
			if err != nil {
				return model.ScheduleResult{}, err
			}
			total += n
		} else if ev.TournamentFormat == "compass" {
			sides := seedSides(sidesForBracket(ev, regs), skill)
			n, err := s.persistCompass(ev, b.ID, sides)
			if err != nil {
				return model.ScheduleResult{}, err
			}
			total += n
		} else {
			n, err := s.persistRoundRobin(ev, b.ID, regs, courtByNum)
			if err != nil {
				return model.ScheduleResult{}, err
			}
			total += n
			// pools_playoff: lay down the (empty) medal bracket now so it shows
			// in the Standings tab immediately; it auto-seeds when pools finish.
			if ev.TournamentFormat == "pools_playoff" {
				seeds, err := s.seedTopTeams(ev, eventID, b.ID, "wins")
				if err != nil {
					return model.ScheduleResult{}, err
				}
				if len(seeds) >= 4 {
					if _, err := s.persistMedalBracket(ev, b.ID, nil); err != nil {
						return model.ScheduleResult{}, err
					}
				}
			}
		}
	}
	// Auto-arrange games onto courts/time-slots — SKIPPED for a manual build
	// (arrange=false), where the organizer places each game on the Board.
	if arrange {
		if err := s.spreadCourts(eventID); err != nil {
			return model.ScheduleResult{}, err
		}
		// Elimination draws also lay their matches onto courts/time-slots so they
		// show on the Game-tab grid. Skip pools_playoff: its medal bracket is empty
		// at build and gets arranged when the playoff is generated.
		switch ev.TournamentFormat {
		case "single_elim", "double_elim", "compass":
			if err := s.spreadBracketCourts(eventID); err != nil {
				return model.ScheduleResult{}, err
			}
		}
	}
	if _, err = s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"status": "in_progress"}); err != nil {
		return model.ScheduleResult{}, err
	}

	unscheduled, err := s.playerNamesByID(eventID, droppedIDs)
	if err != nil {
		return model.ScheduleResult{}, err
	}
	return model.ScheduleResult{Matches: total, Unscheduled: unscheduled}, nil
}

// CreateManualGame inserts an organizer-defined match — chosen teams on a chosen
// court + time slot (play_order). Powers manual scheduling (the "Add game"
// dialog); players come from the registered roster and it counts like a pool game.
func (s *Service) CreateManualGame(eventID, bracketID string, court, playOrder, durationMinutes, scheduledDay int, team1, team2 []string) (string, error) {
	row := map[string]any{
		"event_id":   eventID,
		"stage":      "pool",
		"status":     "scheduled",
		"play_order": playOrder,
	}
	if durationMinutes > 0 {
		row["duration_minutes"] = durationMinutes
	}
	if scheduledDay >= 0 {
		row["scheduled_day"] = scheduledDay
	}
	if bracketID != "" {
		row["bracket_id"] = bracketID
	}
	if court > 0 {
		// Resolve the court number to its FK — matches place via court_id, not a
		// court_number column (mirrors SetMatchCourt).
		c, err := s.sb.SelectOne("courts",
			"event_id=eq."+store.Q(eventID)+"&court_number=eq."+strconv.Itoa(court)+"&select=id")
		if err != nil {
			return "", err
		}
		if c != nil {
			courtID := asStr(c, "id")
			row["court_id"] = courtID
			// Double-booking guard (defense-in-depth; the dialog only offers free
			// courts): reject if a scheduled game already sits on this court at this
			// wave (play_order rounds to the same slot).
			lo := strconv.FormatFloat(float64(playOrder)-0.5, 'f', 1, 64)
			hi := strconv.FormatFloat(float64(playOrder)+0.5, 'f', 1, 64)
			busy, err := s.sb.Select("matches",
				"event_id=eq."+store.Q(eventID)+"&court_id=eq."+store.Q(courtID)+
					"&status=eq.scheduled&play_order=gte."+lo+"&play_order=lt."+hi+
					"&select=id&limit=1")
			if err != nil {
				return "", err
			}
			if len(busy) > 0 {
				return "", fmt.Errorf("%w: that court is already taken at that time", ErrScheduleHasResults)
			}
		}
	}
	ins, err := s.sb.Insert("matches", []map[string]any{row})
	if err != nil {
		return "", err
	}
	if len(ins) == 0 {
		return "", errors.New("manual game insert returned no row")
	}
	matchID := asStr(ins[0], "id")
	partRows := make([]map[string]any, 0, len(team1)+len(team2))
	for _, pid := range team1 {
		if pid != "" {
			partRows = append(partRows, map[string]any{"match_id": matchID, "player_id": pid, "team": 1})
		}
	}
	for _, pid := range team2 {
		if pid != "" {
			partRows = append(partRows, map[string]any{"match_id": matchID, "player_id": pid, "team": 2})
		}
	}
	if len(partRows) > 0 {
		if _, err := s.sb.Upsert("match_participants", "match_id,player_id", partRows); err != nil {
			return "", err
		}
	}
	return matchID, nil
}

// ClearArrangement un-places every SCHEDULED game (keeping the matchups) so the
// organizer can position them manually on the Board. court_id is the placement
// FK (SetMatchCourt writes it; the DTO resolves courtNumber back from it) and
// play_order is the per-court time slot — null both, exactly like the drag-to-
// Unassigned path. Started/scored games keep their court (status filter).
func (s *Service) ClearArrangement(eventID string) error {
	_, err := s.sb.Update("matches",
		"event_id=eq."+store.Q(eventID)+"&status=eq.scheduled",
		map[string]any{"court_id": nil, "play_order": nil})
	return err
}

// DeleteMatch removes a single SCHEDULED, non-bracket match (its participants +
// any dupr_submissions row cascade via the FK). Powers the edit-match sheet's
// Delete. Completed/bracket matches are rejected — deleting a scored match would
// leave a pushed DUPR rating un-reversed, and deleting a bracket game breaks
// advancement; those go through a wipe/regenerate path instead.
func (s *Service) DeleteMatch(matchID string) error {
	m, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=status,stage")
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	if asStr(m, "status") == "completed" || asStr(m, "stage") == "bracket" {
		return fmt.Errorf("%w: completed or bracket matches can't be deleted here", ErrScheduleHasResults)
	}
	// match_participants cascade via the FK on delete — just remove the match row.
	return s.sb.Delete("matches", "id=eq."+store.Q(matchID))
}

// playerNamesByID resolves player IDs to display names for the given event.
// Returns nil for an empty input (the common case — no lookup performed).
func (s *Service) playerNamesByID(eventID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	regs, err := s.Registrations(eventID)
	if err != nil {
		return nil, err
	}
	nameByID := map[string]string{}
	for _, r := range regs {
		nameByID[r.PlayerID] = r.FullName
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		n := nameByID[id]
		if n == "" {
			n = "A player"
		}
		names = append(names, n)
	}
	return names, nil
}

// spreadBracketCourts lays an elimination bracket's (non-bye) matches onto courts
// and time-slots the same way spreadCourts does for pools — so single/double-elim
// and compass draws also appear on the Game-tab time-grid, not only the Standings
// bracket. Byes are status='completed' at build (their winner already advanced),
// so the status=scheduled filter excludes them at the source. Matches order by
// bracket round (the time block) so round 1 plays before round 2; a later-round
// match with a TBD feeder still gets a slot (its name fills in as winners advance,
// keeping the same court/slot). play_order accumulates per round, cycling courts.
func (s *Service) spreadBracketCourts(eventID string) error {
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

	// Only the still-to-be-played matches; byes/decided games are status!=scheduled.
	rows, err := s.sb.SelectAll("matches",
		"event_id=eq."+store.Q(eventID)+"&stage=eq.bracket&status=eq.scheduled"+
			"&select=id,bracket_round,bracket_slot,bracket_tier,bracket_group")
	if err != nil {
		return err
	}
	type mr struct {
		id, tier, group string
		round, slot     int
	}
	list := make([]mr, 0, len(rows))
	for _, r := range rows {
		list = append(list, mr{
			id:    asStr(r, "id"),
			tier:  asStr(r, "bracket_tier"),
			group: asStr(r, "bracket_group"),
			round: asInt(r, "bracket_round"),
			slot:  asInt(r, "bracket_slot"),
		})
	}
	// Round is the time block; tier/group/slot keep a stable layout. Single-elim
	// has no tier/group so this reduces to (round, slot).
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.round != b.round {
			return a.round < b.round
		}
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.group != b.group {
			return a.group < b.group
		}
		return a.slot < b.slot
	})

	nc := len(courtNums)
	byCourt := map[string][]string{}
	bySlot := map[int][]string{}
	prevRound, idxInRound, baseSlot := -1, 0, 0
	for _, m := range list {
		if m.round != prevRound {
			if prevRound != -1 {
				baseSlot += (idxInRound + nc - 1) / nc
			}
			prevRound = m.round
			idxInRound = 0
		}
		cid := courtByNum[courtNums[idxInRound%nc]]
		slot := baseSlot + idxInRound/nc
		idxInRound++
		byCourt[cid] = append(byCourt[cid], m.id)
		bySlot[slot] = append(bySlot[slot], m.id)
	}
	const chunk = 80
	apply := func(ids []string, patch map[string]any) error {
		for i := 0; i < len(ids); i += chunk {
			end := i + chunk
			if end > len(ids) {
				end = len(ids)
			}
			if _, err := s.sb.Update("matches",
				"id="+store.In(ids[i:end])+"", patch); err != nil {
				return err
			}
		}
		return nil
	}
	for cid, ids := range byCourt {
		if err := apply(ids, map[string]any{"court_id": cid}); err != nil {
			return err
		}
	}
	for slot, ids := range bySlot {
		if err := apply(ids, map[string]any{"play_order": slot}); err != nil {
			return err
		}
	}
	return nil
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

	// SelectAll: a big field can have hundreds of pool matches — an unbounded
	// Select would truncate at PostgREST's row cap and leave matches unspread.
	rows, err := s.sb.SelectAll("matches",
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

	// Assign each match BOTH a court and a wave/slot (play_order). Within a round,
	// the i-th match goes to court i%nc and wave i/nc, so when a round has more
	// matches than courts the overflow lands in a LATER slot rather than a second
	// game on the same court at the same time. Slots accumulate across rounds so
	// the schedule cascade orders them sequentially. (Previously only court_id was
	// set and play_order left null, so two matches of one round read as
	// simultaneous on one court — the engine's wave assignment was discarded.)
	nc := len(courtNums)
	byCourt := map[string][]string{}
	bySlot := map[int][]string{}
	prevRound, idxInRound, baseSlot := -1, 0, 0
	for _, m := range list {
		if m.round != prevRound {
			if prevRound != -1 {
				baseSlot += (idxInRound + nc - 1) / nc // waves used by the prior round
			}
			prevRound = m.round
			idxInRound = 0
		}
		cid := courtByNum[courtNums[idxInRound%nc]]
		slot := baseSlot + idxInRound/nc
		idxInRound++
		byCourt[cid] = append(byCourt[cid], m.id)
		bySlot[slot] = append(bySlot[slot], m.id)
	}
	// Batched UPDATEs (chunked to keep the id=in.(...) URL bounded): one per court
	// for court_id, one per slot for play_order — instead of one round-trip per
	// match (a big field, e.g. 768 pool games, would otherwise time out / 502).
	const chunk = 80
	apply := func(ids []string, patch map[string]any) error {
		for i := 0; i < len(ids); i += chunk {
			end := i + chunk
			if end > len(ids) {
				end = len(ids)
			}
			if _, err := s.sb.Update("matches",
				"id="+store.In(ids[i:end])+"", patch); err != nil {
				return err
			}
		}
		return nil
	}
	for cid, ids := range byCourt {
		if err := apply(ids, map[string]any{"court_id": cid}); err != nil {
			return err
		}
	}
	for slot, ids := range bySlot {
		if err := apply(ids, map[string]any{"play_order": slot}); err != nil {
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
// SetDivisionOrder sets each division's sort_order to its position in the given
// list, so the organizer controls which division the auto-scheduler lays down
// first. Each id is bound to the event (the backend bypasses RLS, so app code
// is the only guard against a cross-event write). Ids omitted from the list keep
// their existing sort_order and fall after the listed ones at schedule time.
func (s *Service) SetDivisionOrder(eventID string, orderedIDs []string) error {
	for i, id := range orderedIDs {
		if _, err := s.sb.Update("brackets",
			"id=eq."+store.Q(id)+"&event_id=eq."+store.Q(eventID),
			map[string]any{"sort_order": i}); err != nil {
			return err
		}
	}
	return nil
}

// ConnectDupr upserts a user's DUPR account link captured by the SSO flow, then
// best-effort pulls authoritative ratings via the partner API (the user has now
// consented). userID is the authenticated caller.
func (s *Service) ConnectDupr(userID string, in model.DuprConnectInput) error {
	if userID == "" {
		return errors.New("must be signed in to connect DUPR")
	}
	duprID := strings.TrimSpace(in.DuprID)
	if duprID == "" {
		return errors.New("DUPR connection did not return a DUPR id")
	}
	// One DUPR id maps to one account: reject if it's already linked to a
	// DIFFERENT PlanMyPickle user (re-connecting your own account is fine).
	if taken, err := s.sb.SelectOne("dupr_connections",
		"dupr_id=eq."+store.Q(duprID)+"&user_id=neq."+store.Q(userID)+
			"&select=user_id&limit=1"); err == nil && taken != nil {
		return ErrDuprIDTaken
	}
	doubles, singles := in.DoublesRating, in.SinglesRating
	// Now that the user consented, prefer authoritative ratings from DUPR. Only
	// override with > 0 values — an unrated (NR) account comes back as 0, which
	// we leave null rather than show as "0.00".
	if r, err := s.Dupr.GetPlayerRating(duprID); err == nil && r.Found {
		if r.Doubles > 0 {
			d := r.Doubles
			doubles = &d
		}
		if r.Singles > 0 {
			sg := r.Singles
			singles = &sg
		}
	}
	row := map[string]any{
		"user_id":        userID,
		"dupr_id":        duprID,
		"user_token":     orNull(in.UserToken),
		"refresh_token":  orNull(in.RefreshToken),
		"doubles_rating": fOrNull(doubles),
		"singles_rating": fOrNull(singles),
		"updated_at":     now(),
	}
	existing, err := s.sb.SelectOne("dupr_connections",
		"user_id=eq."+store.Q(userID)+"&select=user_id")
	if err != nil {
		return err
	}
	if existing != nil {
		if _, err = s.sb.Update("dupr_connections", "user_id=eq."+store.Q(userID), row); err != nil {
			return err
		}
	} else {
		row["connected_at"] = now()
		if _, err = s.sb.Insert("dupr_connections", row); err != nil {
			return err
		}
	}
	// Subscribe to RATING webhooks for this user (best-effort): DUPR posts a
	// RATING_SEED with their current rating, which our webhook stores — so the
	// rating populates + stays fresh without polling.
	if e := s.Dupr.SubscribeUserRating(duprID); e != nil {
		log.Printf("dupr: subscribe %s failed (non-fatal): %v", duprID, e)
	}
	// Adopt any player rows imported from DUPR under this id (no account yet) onto
	// this user, and auto-join the clubs whose events those players are in — so a
	// migrated player who later signs up + connects DUPR sees their registrations
	// and clubs without a duplicate. Best-effort.
	s.linkDuprPlayers(userID, duprID)
	return nil
}

// DisconnectDupr unlinks a user's DUPR account by deleting the account-level
// connection (+ tokens). It deliberately does NOT touch the dupr_id already stamped
// on their player rows: that's match-history data, so already-played / pending
// sanctioned results still submit — a disconnect can't retroactively drop finished
// results. The card flips to "Connect DUPR account" and new sanctioned registrations
// are gated until they reconnect (DuprConnection now reports not-connected).
// Local unlink only: DUPR exposes no partner unsubscribe, and a stray rating webhook
// is a no-op once there's no matching connection row to apply it to.
func (s *Service) DisconnectDupr(userID string) error {
	if userID == "" {
		return errors.New("must be signed in to disconnect DUPR")
	}
	return s.sb.Delete("dupr_connections", "user_id=eq."+store.Q(userID))
}

// linkDuprPlayers adopts orphan player rows carrying duprID (created by a DUPR
// roster import, no account) onto the account, then auto-joins the clubs whose
// events those players are registered in. Best-effort — never breaks ConnectDupr.
func (s *Service) linkDuprPlayers(userID, duprID string) {
	if userID == "" || duprID == "" {
		return
	}
	// 1) Adopt orphan players with this DUPR id (leave already-linked ones alone).
	if _, err := s.sb.Update("players",
		"dupr_id=eq."+store.Q(duprID)+"&user_id=is.null",
		map[string]any{"user_id": userID}); err != nil {
		return
	}
	// 2) Auto-join the clubs of the events those players are registered in.
	pls, err := s.sb.Select("players", "user_id=eq."+store.Q(userID)+"&select=id")
	if err != nil || len(pls) == 0 {
		return
	}
	pids := make([]string, 0, len(pls))
	for _, p := range pls {
		if id := asStr(p, "id"); id != "" {
			pids = append(pids, id)
		}
	}
	regs, err := s.sb.Select("registrations",
		"player_id="+store.In(pids)+"&select=event_id")
	if err != nil || len(regs) == 0 {
		return
	}
	eset := map[string]bool{}
	for _, r := range regs {
		if eid := asStr(r, "event_id"); eid != "" {
			eset[eid] = true
		}
	}
	if len(eset) == 0 {
		return
	}
	eids := make([]string, 0, len(eset))
	for eid := range eset {
		eids = append(eids, eid)
	}
	evs, err := s.sb.Select("events",
		"id="+store.In(eids)+"&club_id=not.is.null&select=club_id")
	if err != nil {
		return
	}
	joined := map[string]bool{}
	for _, e := range evs {
		cid := asStr(e, "club_id")
		if cid == "" || joined[cid] {
			continue
		}
		joined[cid] = true
		_, _ = s.sb.Upsert("club_members", "club_id,user_id", map[string]any{
			"club_id": cid, "user_id": userID, "role": "member",
		})
	}
}

// ApplyDuprRating updates a connected user's cached ratings from a RATING
// webhook (matched by DUPR id). Only present (non-nil) values are written.
func (s *Service) ApplyDuprRating(duprID string, doubles, singles *float64) error {
	duprID = strings.TrimSpace(duprID)
	if duprID == "" {
		return nil
	}
	upd := map[string]any{"updated_at": now()}
	if doubles != nil {
		upd["doubles_rating"] = *doubles
	}
	if singles != nil {
		upd["singles_rating"] = *singles
	}
	_, err := s.sb.Update("dupr_connections", "dupr_id=eq."+store.Q(duprID), upd)
	return err
}

// RegisterDuprWebhook registers our webhook URL with DUPR (called at startup).
func (s *Service) RegisterDuprWebhook(url string) error {
	return s.Dupr.RegisterWebhook(url)
}

// DuprConnection returns the caller's DUPR link (token-free) for profile display.
func (s *Service) DuprConnection(userID string) (model.DuprConnection, error) {
	if userID == "" {
		return model.DuprConnection{}, nil
	}
	row, err := s.sb.SelectOne("dupr_connections",
		"user_id=eq."+store.Q(userID)+"&select=dupr_id,doubles_rating,singles_rating,connected_at")
	if err != nil {
		return model.DuprConnection{}, err
	}
	if row == nil {
		return model.DuprConnection{Connected: false}, nil
	}
	return model.DuprConnection{
		Connected:     true,
		DuprID:        asStr(row, "dupr_id"),
		DoublesRating: asFloatPtr(row, "doubles_rating"),
		SinglesRating: asFloatPtr(row, "singles_rating"),
		ConnectedAt:   asStr(row, "connected_at"),
	}, nil
}

// DuprSsoURL returns the iframe URL (+ origin to validate) for the connect flow.
func (s *Service) DuprSsoURL() (string, string) { return s.Dupr.SsoURL() }

func (s *Service) AutoScheduleByRating(eventID string, interleave bool, minRestSlots int) (int, error) {
	// Team events are already placed conflict-free by GenerateTeamTies — never
	// re-arrange them by rating (no registrations/divisions to sort by, and
	// re-arranging would pile each tie's 4 lines into a single slot).
	if ev, gerr := s.GetEvent(eventID); gerr == nil && ev.TeamSize > 0 {
		ids, lerr := s.listPoolMatchIDs(eventID)
		if lerr != nil {
			return 0, lerr
		}
		return len(ids), nil
	}
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

	// Division order: the organizer's chosen order (sort_order) wins, so they
	// control which division the scheduler lays down first. Rating only breaks
	// ties — rated bands ascending by min_rating, unrated ("Open") last — which
	// is also the default order divisions are created in.
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
		if a.sort != b.sort {
			return a.sort < b.sort
		}
		aNull, bNull := a.min == nil, b.min == nil
		if aNull != bNull {
			return !aNull // rated divisions before unrated
		}
		if !aNull && !bNull && *a.min != *b.min {
			return *a.min < *b.min
		}
		return false
	})
	rank := make(map[string]int, len(blist))
	for i, b := range blist {
		rank[b.id] = i
	}

	// Pool matches with their round number. SelectAll so a big field's pool games
	// aren't truncated at PostgREST's row cap (which would silently skip placing
	// the dropped matches onto courts/slots).
	rows, err := s.sb.SelectAll("matches",
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
				"match_id="+store.In(mids[start:end])+"&select=match_id,player_id")
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
	// Record placements in memory; flush them in batched UPDATEs after the loops
	// (grouped by court + slot) instead of one UPDATE per match — a big field
	// (768 games) otherwise fires 768 round-trips and the request times out (502).
	type placement struct{ court, slot int }
	placements := map[string]placement{}
	place := func(matchID string, courtNumber, slot int) {
		placements[matchID] = placement{courtNumber, slot}
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
	// Flush placements in batched UPDATEs: one per court (court_id) + one per slot
	// (play_order), chunked to bound the id=in.(...) URL — replaces one UPDATE per
	// match so a big field doesn't fire hundreds of round-trips and time out.
	byCourt := map[int][]string{}
	bySlot := map[int][]string{}
	for mid, p := range placements {
		byCourt[p.court] = append(byCourt[p.court], mid)
		bySlot[p.slot] = append(bySlot[p.slot], mid)
	}
	const flushChunk = 80
	flush := func(ids []string, data map[string]any) error {
		for i := 0; i < len(ids); i += flushChunk {
			end := i + flushChunk
			if end > len(ids) {
				end = len(ids)
			}
			if _, err := s.sb.Update("matches",
				"id="+store.In(ids[i:end])+"", data); err != nil {
				return err
			}
		}
		return nil
	}
	for courtNumber, ids := range byCourt {
		if err := flush(ids, map[string]any{"court_id": courtByNum[courtNumber]}); err != nil {
			return scheduled, err
		}
	}
	for slot, ids := range bySlot {
		if err := flush(ids, map[string]any{"play_order": float64(slot)}); err != nil {
			return scheduled, err
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
	schedule := engine.GenerateSchedule(ids, format, partner, ev.NumCourts, fixedPairs, rounds, ev.MinPoolRounds, ev.MaxPoolRounds)

	// Batch every insert (rounds, matches, participants) into 3 bulk calls
	// instead of ~3 per match. A big round-robin (e.g. 96 games) otherwise fires
	// hundreds of sequential PostgREST calls and the request times out (502).
	// PostgREST returns bulk-inserted rows in input order, so we zip ids by index.
	roundRows := make([]map[string]any, 0, len(schedule))
	for _, round := range schedule {
		roundRows = append(roundRows, map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID,
			"round_number": round.RoundNumber,
		})
	}
	if len(roundRows) == 0 {
		return 0, nil
	}
	insRounds, err := s.sb.Insert("rounds", roundRows)
	if err != nil {
		return 0, err
	}
	roundIDByNum := make(map[int]string, len(insRounds))
	for _, r := range insRounds {
		roundIDByNum[asInt(r, "round_number")] = asStr(r, "id")
	}

	type pend struct{ t1, t2 []string }
	matchRows := make([]map[string]any, 0)
	pending := make([]pend, 0)
	for _, round := range schedule {
		rid := roundIDByNum[round.RoundNumber]
		for _, m := range round.Matches {
			matchRows = append(matchRows, map[string]any{
				"event_id": ev.ID, "bracket_id": bracketID, "round_id": rid,
				"court_id": orNull(courtByNum[m.CourtNumber]), "stage": "pool",
			})
			pending = append(pending, pend{m.Team1, m.Team2})
		}
	}
	if len(matchRows) == 0 {
		return 0, nil
	}
	insMatches, err := s.sb.Insert("matches", matchRows)
	if err != nil {
		return 0, err
	}
	if len(insMatches) != len(pending) {
		return 0, errors.New("match insert count mismatch")
	}

	partRows := make([]map[string]any, 0)
	for i, im := range insMatches {
		mid := asStr(im, "id")
		sides := []struct {
			team int
			side []string
		}{{1, pending[i].t1}, {2, pending[i].t2}}
		for _, sd := range sides {
			if len(sd.side) == 0 || engine.IsBye(sd.side) {
				continue
			}
			for _, pid := range sd.side {
				partRows = append(partRows, map[string]any{
					"match_id": mid, "player_id": pid, "team": sd.team,
				})
			}
		}
	}
	if len(partRows) > 0 {
		if _, err := s.sb.Upsert("match_participants", "match_id,player_id",
			partRows); err != nil {
			return 0, err
		}
	}
	return len(insMatches), nil
}

func (s *Service) persistBracket(ev model.Event, bracketID string, seededSides [][]string, consolation bool) (int, error) {
	plan := engine.GenerateBracket(seededSides)

	// Mirror persistRoundRobin's batching: collect every match row, do ONE bulk
	// Insert (PostgREST returns rows in input order, so we zip ids by index), then
	// bulk-Upsert all participants and bulk-Update the feed links. A 64-team draw
	// otherwise fires 250+ sequential PostgREST calls and times out (502), leaving
	// a half-built bracket.
	matchRows := make([]map[string]any, 0, len(plan.Matches))
	for _, m := range plan.Matches {
		// Every row MUST carry the same keys: PostgREST rejects a bulk insert whose
		// objects differ ("All object keys must match"). Byes set completed_at /
		// winning_team; non-byes carry them as nil so the array stays homogeneous.
		row := map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
			"bracket_round": m.Round, "bracket_slot": m.Slot, "status": "scheduled",
			"completed_at": nil, "winning_team": nil,
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
		matchRows = append(matchRows, row)
	}
	if len(matchRows) == 0 {
		return 0, nil
	}
	insMatches, err := s.sb.Insert("matches", matchRows)
	if err != nil {
		return 0, err
	}
	if len(insMatches) != len(plan.Matches) {
		return 0, errors.New("bracket match insert count mismatch")
	}
	idByKey := make(map[string]string, len(plan.Matches))
	for i, m := range plan.Matches {
		idByKey[key(m.Round, m.Slot)] = asStr(insMatches[i], "id")
	}
	count := len(insMatches)

	// All participants (both sides of every match) in ONE upsert. insertSide's
	// skip rules (empty side / bye) are inlined here.
	partRows := make([]map[string]any, 0)
	for i, m := range plan.Matches {
		mid := asStr(insMatches[i], "id")
		partRows = appendSideParts(partRows, mid, 1, m.Side1)
		partRows = appendSideParts(partRows, mid, 2, m.Side2)
	}
	if len(partRows) > 0 {
		if _, err := s.sb.Upsert("match_participants", "match_id,player_id", partRows); err != nil {
			return 0, err
		}
	}

	// Winner-feed links, grouped so matches that feed the SAME (feeds_match_id,
	// feeds_slot) target share one Update (id=in.(...)) instead of one per match.
	feeds := newFeedUpdates()
	for _, m := range plan.Matches {
		if m.FeedsRound == 0 {
			continue
		}
		feeds.add(idByKey[key(m.Round, m.Slot)], map[string]any{
			"feeds_match_id": idByKey[key(m.FeedsRound, m.FeedsSlot)],
			"feeds_slot":     m.FeedsTeam,
		})
	}
	if err := feeds.flush(s); err != nil {
		return 0, err
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
		consRows := make([]map[string]any, 0, len(cons.Matches))
		for _, cm := range cons.Matches {
			consRows = append(consRows, map[string]any{
				"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
				"bracket_tier": "consolation", "bracket_round": cm.Round,
				"bracket_slot": cm.Slot, "status": "scheduled",
			})
		}
		consID := map[string]string{}
		if len(consRows) > 0 {
			insCons, err := s.sb.Insert("matches", consRows)
			if err != nil {
				return 0, err
			}
			if len(insCons) != len(cons.Matches) {
				return 0, errors.New("consolation match insert count mismatch")
			}
			for i, cm := range cons.Matches {
				consID[key(cm.Round, cm.Slot)] = asStr(insCons[i], "id")
			}
			count += len(insCons)
		}
		// Winner advancement within the consolation tree.
		consFeeds := newFeedUpdates()
		for _, cm := range cons.Matches {
			if cm.FeedsRound == 0 {
				continue
			}
			consFeeds.add(consID[key(cm.Round, cm.Slot)], map[string]any{
				"feeds_match_id": consID[key(cm.FeedsRound, cm.FeedsSlot)],
				"feeds_slot":     cm.FeedsTeam,
			})
		}
		if err := consFeeds.flush(s); err != nil {
			return 0, err
		}
		// Each main round-1 match's LOSER drops into the consolation tree.
		dropFeeds := newFeedUpdates()
		for _, d := range cons.Drops {
			mainID := idByKey[key(1, d.MainSlot)]
			if mainID == "" {
				continue
			}
			dropFeeds.add(mainID, map[string]any{
				"loser_feeds_match_id": consID[key(d.Round, d.Slot)],
				"loser_feeds_slot":     d.Team,
			})
		}
		if err := dropFeeds.flush(s); err != nil {
			return 0, err
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

	// Batch like persistRoundRobin/persistBracket: ONE bulk Insert (ids zipped back
	// by input order), one bulk participant Upsert, grouped feed Updates. A large
	// double-elim draw otherwise fires hundreds of sequential calls (502 timeout).
	matchRows := make([]map[string]any, 0, len(plan.Matches))
	for _, m := range plan.Matches {
		row := map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
			"bracket_tier": m.Tier, "bracket_round": m.Round, "bracket_slot": m.Slot,
			"status": "scheduled", "completed_at": nil, "winning_team": nil,
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
		matchRows = append(matchRows, row)
	}
	if len(matchRows) == 0 {
		return 0, nil
	}
	insMatches, err := s.sb.Insert("matches", matchRows)
	if err != nil {
		return 0, err
	}
	if len(insMatches) != len(plan.Matches) {
		return 0, errors.New("double-elim match insert count mismatch")
	}
	for i, m := range plan.Matches {
		idByKey[dkey(m.Tier, m.Round, m.Slot)] = asStr(insMatches[i], "id")
	}
	count := len(insMatches)

	partRows := make([]map[string]any, 0)
	for i, m := range plan.Matches {
		mid := asStr(insMatches[i], "id")
		partRows = appendSideParts(partRows, mid, 1, m.Side1)
		partRows = appendSideParts(partRows, mid, 2, m.Side2)
	}
	if len(partRows) > 0 {
		if _, err := s.sb.Upsert("match_participants", "match_id,player_id", partRows); err != nil {
			return 0, err
		}
	}

	// Winner + loser feed links. Both go in ONE payload per match (so the bulk
	// Update grouping keys on the combined fields, matching the original per-match
	// single PATCH that set whichever links applied).
	feeds := newFeedUpdates()
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
		feeds.add(idByKey[dkey(m.Tier, m.Round, m.Slot)], upd)
	}
	if err := feeds.flush(s); err != nil {
		return 0, err
	}
	return count, nil
}

// persistCompass lays down a Compass Draw: one EAST single-elimination bracket
// plus one single-elim CONSOLATION bracket per East losing round (West / North /
// South / East-R{n}). Every match is tagged with its compass direction in
// matches.bracket_group; all stay bracket_tier='main' (East and the
// consolations are all single-elim trees, so the existing 'main' medal/header
// rendering applies per direction). Mirrors persistDoubleElim's batching: ONE
// bulk match Insert (ids zipped by input order), one bulk participant Upsert, and
// grouped feed Updates. Winner feeds (feeds_match_id) advance within a bracket;
// East loser drops (loser_feeds_match_id) route a beaten team into its
// consolation — both are plain match ids, so the generic advanceAfterScore /
// advanceTeam path (incl. its re-score cascade) routes winners and losers across
// brackets unchanged.
func (s *Service) persistCompass(ev model.Event, bracketID string, seededSides [][]string) (int, error) {
	plan := engine.GenerateCompass(seededSides)
	ckey := func(group string, r, slot int) string {
		return group + ":" + strconv.Itoa(r) + ":" + strconv.Itoa(slot)
	}
	idByKey := make(map[string]string, len(plan.Matches))

	matchRows := make([]map[string]any, 0, len(plan.Matches))
	for _, m := range plan.Matches {
		row := map[string]any{
			"event_id": ev.ID, "bracket_id": bracketID, "stage": "bracket",
			"bracket_tier": "main", "bracket_group": m.Group,
			"bracket_round": m.Round, "bracket_slot": m.Slot,
			"status": "scheduled", "completed_at": nil, "winning_team": nil,
		}
		if m.ResolvedWinner != nil { // an East round-1 bye — auto-complete it
			row["status"] = "completed"
			row["completed_at"] = now()
			if m.Side1 != nil && !engine.IsBye(m.Side1) {
				row["winning_team"] = 1
			} else {
				row["winning_team"] = 2
			}
		}
		matchRows = append(matchRows, row)
	}
	if len(matchRows) == 0 {
		return 0, nil
	}
	insMatches, err := s.sb.Insert("matches", matchRows)
	if err != nil {
		return 0, err
	}
	if len(insMatches) != len(plan.Matches) {
		return 0, errors.New("compass match insert count mismatch")
	}
	for i, m := range plan.Matches {
		idByKey[ckey(m.Group, m.Round, m.Slot)] = asStr(insMatches[i], "id")
	}
	count := len(insMatches)

	partRows := make([]map[string]any, 0)
	for i, m := range plan.Matches {
		mid := asStr(insMatches[i], "id")
		partRows = appendSideParts(partRows, mid, 1, m.Side1)
		partRows = appendSideParts(partRows, mid, 2, m.Side2)
	}
	if len(partRows) > 0 {
		if _, err := s.sb.Upsert("match_participants", "match_id,player_id", partRows); err != nil {
			return 0, err
		}
	}

	// Winner feeds (within a bracket) + East loser drops (into a consolation), one
	// payload per match so the bulk Update grouping keys on the combined fields.
	feeds := newFeedUpdates()
	for _, m := range plan.Matches {
		upd := map[string]any{}
		if m.FeedsRound != 0 {
			upd["feeds_match_id"] = idByKey[ckey(m.Group, m.FeedsRound, m.FeedsSlot)]
			upd["feeds_slot"] = m.FeedsTeam
		}
		// A bye East match has no loser to drop.
		if m.LoserGroup != "" && m.ResolvedWinner == nil {
			upd["loser_feeds_match_id"] = idByKey[ckey(m.LoserGroup, m.LoserRound, m.LoserSlot)]
			upd["loser_feeds_slot"] = m.LoserTeam
		}
		if len(upd) == 0 {
			continue
		}
		feeds.add(idByKey[ckey(m.Group, m.Round, m.Slot)], upd)
	}
	if err := feeds.flush(s); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) GeneratePlayoffBracket(bracketID string, topN int, seeding string, manualSides [][]string) (int, error) {
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
	var sides [][]string
	if seeding == "manual" {
		// Manual requires an explicit order — never silently fall back to wins.
		if len(manualSides) == 0 {
			return 0, errors.New("manual seeding needs the team order")
		}
		// Honor the organizer's chosen order, but validate every team is a real
		// team in this division (no fabricated/duplicated pairs).
		valid, verr := s.seedTopTeams(ev, eventID, bracketID, "wins")
		if verr != nil {
			return 0, verr
		}
		if verr := validateManualSides(manualSides, valid); verr != nil {
			return 0, verr
		}
		sides = manualSides
	} else {
		sides, err = s.seedTopTeams(ev, eventID, bracketID, seeding)
		if err != nil {
			return 0, err
		}
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

// validateManualSides ensures every team in [manual] is a real team in [valid]
// (matched as an unordered player set) and that none repeats — so a manual
// playoff seed can reorder/trim teams but never invent or duplicate one.
func validateManualSides(manual, valid [][]string) error {
	key := func(t []string) string {
		c := append([]string(nil), t...)
		sort.Strings(c)
		return strings.Join(c, "|")
	}
	want := map[string]bool{}
	for _, t := range valid {
		want[key(t)] = true
	}
	seen := map[string]bool{}
	for _, t := range manual {
		k := key(t)
		if !want[k] {
			return errors.New("manual seeding includes a team that isn't in this division")
		}
		if seen[k] {
			return errors.New("manual seeding lists the same team twice")
		}
		seen[k] = true
	}
	return nil
}

// PlayoffSeedTeams returns this division's teams in seed order (seeding: "wins"
// default | "points"), each with its players' names and the team's combined
// pool record — so the organizer can review and reorder them before building
// the playoff.
func (s *Service) PlayoffSeedTeams(bracketID, seeding string) (model.PlayoffSeedInfo, error) {
	var info model.PlayoffSeedInfo
	b, err := s.sb.SelectOne("brackets", "id=eq."+store.Q(bracketID)+"&select=event_id")
	if err != nil {
		return info, err
	}
	if b == nil {
		return info, ErrNotFound
	}
	eventID := asStr(b, "event_id")
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return info, err
	}
	sides, err := s.seedTopTeams(ev, eventID, bracketID, seeding)
	if err != nil {
		return info, err
	}
	regs, err := s.Registrations(eventID)
	if err != nil {
		return info, err
	}
	nameByPlayer := map[string]string{}
	for _, r := range regs {
		nameByPlayer[r.PlayerID] = r.FullName
	}
	standings, err := s.Standings(eventID, bracketID, true)
	if err != nil {
		return info, err
	}
	type rec struct{ wins, diff, pf int }
	recByPlayer := map[string]rec{}
	for _, st := range standings {
		recByPlayer[st.PlayerID] = rec{st.Wins, st.PointDiff, st.PointsFor}
	}
	out := make([]model.PlayoffSeed, 0, len(sides))
	for _, side := range sides {
		seed := model.PlayoffSeed{PlayerIDs: side}
		for _, pid := range side {
			n := nameByPlayer[pid]
			if n == "" {
				n = "Player"
			}
			seed.Names = append(seed.Names, n)
			r := recByPlayer[pid]
			seed.Wins += r.wins
			seed.PointDiff += r.diff
			seed.PointsFor += r.pf
		}
		out = append(out, seed)
	}
	total, open, err := s.poolProgress(bracketID)
	if err != nil {
		return info, err
	}
	info.Teams = out
	info.PoolsTotal = total
	info.PoolsOpen = open
	return info, nil
}

// seedTopTeams returns this division's teams ordered best-first by pool
// standings. Before any pools are played the order is arbitrary but the team
// SET is complete, so callers can use len() to gate (>= 4 for a medal bracket)
// and take the top 4 once standings exist.
// seedTopTeams orders the division's teams best-first. seeding == "points" ranks
// by total points scored (then differential, then wins); anything else ("wins",
// the default) ranks by record (wins, then differential, then points).
func (s *Service) seedTopTeams(ev model.Event, eventID, bracketID, seeding string) ([][]string, error) {
	byPoints := seeding == "points"
	// Standings already orders by wins (byWins=true) or by points (false); reuse
	// that for the singles seed order.
	standings, err := s.Standings(eventID, bracketID, !byPoints)
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
			if byPoints {
				// Total points scored first, then differential, then wins.
				rate[st.PlayerID] = st.PointsFor*1_000_000 + st.PointDiff*1_000 + st.Wins
			} else {
				// Record first, then differential, then points scored — the same
				// priority the standings table ranks by. Wide multipliers keep the
				// tiers from bleeding into each other.
				rate[st.PlayerID] = st.Wins*1_000_000 + st.PointDiff*1_000 + st.PointsFor
			}
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
	sides, err := s.seedTopTeams(ev, eventID, bracketID, "wins")
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
		"id=eq."+store.Q(matchID)+"&select=line_type,event:events!event_id(points_to_win,win_by,best_of)")
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
	// MLP DreamBreaker: the decider line is a single game to 21, win by 2 (not the
	// event's per-line target).
	if asStr(fmtRow, "line_type") == "dec" {
		ptw, winBy, bestOf = 21, 2, 1
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
	if err := s.advanceAfterScore(out[0]); err != nil {
		return err
	}
	// A scored tie line re-evaluates its tie (lines won -> winner; decider on 2-2).
	if tieID := asStr(out[0], "tie_id"); tieID != "" {
		return s.rollupTie(tieID)
	}
	return nil
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
		// Team-event tie lines are also stage="pool" on the division bracket, but
		// they belong to a tie (not a registration pool) — skip pool->playoff seeding.
		if bc := asStrPtr(m, "bracket_id"); bc != nil && asStr(m, "tie_id") == "" {
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
	// walkovers aren't submitted (no genuine head-to-head score). The MLP
	// DreamBreaker (dec) is an 8-player singles rotation, not a DUPR-ratable game,
	// so it's excluded; the regulation 2-v-2 lines stay eligible.
	if rt := asStr(m, "result_type"); ev != nil && asBool(ev, "dupr_sanctioned") &&
		(rt == "" || rt == "normal") && asStr(m, "line_type") != "dec" {
		// Best-effort: this only ENQUEUES a row for the organizer's later DUPR
		// import (the real submit is SubmitPendingToDupr) — never fail an
		// already-committed score over the deferred queue write, mirroring the
		// syncEventStatus best-effort treatment below.
		_ = s.queueDuprSubmission(matchID, eventID)
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
	Submitted int `json:"submitted"` // newly created on DUPR
	Updated   int `json:"updated"`   // corrections to an existing DUPR match
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	// Queued counts rows deferred to the next background pass because this pass
	// hit the per-chunk DUPR call cap (rate-limit prudence on large events).
	Queued int `json:"queued,omitempty"`
	// Details carries the per-match reason for each skip/fail so the UI can show
	// WHY nothing was submitted (e.g. "bye / incomplete side", "dupr http 409 …").
	Details []string `json:"details,omitempty"`
}

// VerifyAdminPasscode gates the coordinator scoring page for a passcode-holding
// (non-owner) scorekeeper. A blank/unset passcode means delegated scoring is
// DISABLED — only the event owner's JWT can score — so an unset passcode must
// NOT grant access (returning true here was an auth bypass: any anonymous caller
// could score any event that never set a passcode, which is the default).
func (s *Service) VerifyAdminPasscode(eventID, code string) (bool, error) {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=admin_passcode")
	if err != nil {
		return false, err
	}
	if ev == nil {
		return false, ErrNotFound
	}
	pass := strings.TrimSpace(asStr(ev, "admin_passcode"))
	if pass == "" {
		return false, nil // no passcode configured -> delegated scoring disabled
	}
	return pass == strings.TrimSpace(code), nil
}

// AuthorizeRegistrationAction reports whether a caller may mutate a registration
// via the public self-service endpoints (pay / shirt). Access is granted when
// EITHER the caller proves possession of the registration's check_in_token (the
// value handed to the registrant at registration, same secret checkinByToken
// uses) OR the caller is the authenticated owner of the registration's event.
// Returns ErrNotFound if the registration (or its event) is missing.
func (s *Service) AuthorizeRegistrationAction(registrationID, token, userID string) (bool, error) {
	row, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=event_id,check_in_token")
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, ErrNotFound
	}
	// Token match (constant-time) — the registrant's own proof of ownership.
	if tok := asStr(row, "check_in_token"); tok != "" && token != "" &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(strings.TrimSpace(token))) == 1 {
		return true, nil
	}
	// Otherwise fall back to event ownership (the organizer acting on a registrant).
	if userID == "" {
		return false, nil
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(asStr(row, "event_id"))+"&select=owner_id")
	if err != nil {
		return false, err
	}
	if ev == nil {
		return false, ErrNotFound
	}
	owner := asStr(ev, "owner_id")
	return owner != "" && owner == userID, nil
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

	// Free registration — nothing to charge, confirm immediately. Use
	// provider="manual" (method "free"); the payments.provider CHECK constraint
	// only allows stripe|paypal|venmo|manual, so "free" would be rejected.
	if fee <= 0 {
		return s.recordPayment(registrationID, "manual", "free", 0, currency, "paid", "paid")
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
//
// IDEMPOTENT: Stripe retries webhooks (checkout.session.completed → this path
// via CollectPaymentManually) and users can double-click "pay", so we guard
// against duplicate payments rows that would double-count in finance totals.
// The registration_status Update is naturally idempotent and always runs.
func (s *Service) recordPayment(registrationID, provider, ref string, fee int, currency, payStatus, regStatus string) (bool, error) {
	var refVal, paidAt any
	if ref != "" {
		refVal = ref
	}
	if payStatus == "paid" {
		paidAt = now()
	}
	dup, err := s.paymentAlreadyRecorded(registrationID, provider, payStatus)
	if err != nil {
		return false, err
	}
	if !dup {
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
	}
	if _, err := s.sb.Update("registrations", "id=eq."+store.Q(registrationID),
		map[string]any{"payment_status": regStatus}); err != nil {
		return false, err
	}
	return regStatus == "paid", nil
}

// paymentAlreadyRecorded reports whether a payments row already covers this
// write, keeping recordPayment idempotent under webhook retries / double-clicks:
//   - paid:    any existing paid row for the registration (one fee, paid once) —
//     covers Stripe-webhook retries (which come through with an empty ref).
//   - pending: an existing pending row for the same registration + provider,
//     so repeated public "pay" clicks don't stack duplicates.
//
// Other statuses (e.g. failed) are never deduped — each is informative.
func (s *Service) paymentAlreadyRecorded(registrationID, provider, payStatus string) (bool, error) {
	var query string
	switch payStatus {
	case "paid":
		query = "registration_id=eq." + store.Q(registrationID) +
			"&status=eq.paid&select=id"
	case "pending":
		query = "registration_id=eq." + store.Q(registrationID) +
			"&provider=eq." + store.Q(provider) + "&status=eq.pending&select=id"
	default:
		return false, nil
	}
	row, err := s.sb.SelectOne("payments", query)
	if err != nil {
		return false, err
	}
	return row != nil, nil
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

// PostChampionFeed posts (or UPDATES) the single "champions" feed item for a
// division's gold final, keyed on the final's match id. Idempotent: a re-score /
// correction updates the existing item to the current winner instead of
// inserting a duplicate — which could otherwise leave a stale, WRONG champion on
// the public feed. Best-effort; skips (rather than risks a dup) if it can't check.
func (s *Service) PostChampionFeed(eventID, matchID, text string) {
	if eventID == "" || matchID == "" || text == "" {
		return
	}
	existing, err := s.sb.SelectOne("feed_items",
		"event_id=eq."+store.Q(eventID)+"&type=eq.champions&ref_id=eq."+
			store.Q(matchID)+"&select=id,text")
	if err != nil {
		log.Printf("feed: champions dedup check for %s failed: %v", eventID, err)
		return
	}
	if existing == nil {
		s.AddFeedItem(eventID, "champions", text, matchID)
		return
	}
	if asStr(existing, "text") != text {
		if _, err := s.sb.Update("feed_items",
			"id=eq."+store.Q(asStr(existing, "id")),
			map[string]any{"text": text}); err != nil {
			log.Printf("feed: champions update for %s failed: %v", eventID, err)
		}
	}
}

// PostTeamChampionFeed upserts the single "champions" feed item for a TEAM
// event's playoff (keyed on event + type, since there's no single gold match).
func (s *Service) PostTeamChampionFeed(eventID, text string) {
	if eventID == "" || text == "" {
		return
	}
	existing, err := s.sb.SelectOne("feed_items",
		"event_id=eq."+store.Q(eventID)+"&type=eq.champions&select=id,text")
	if err != nil {
		return
	}
	if existing == nil {
		s.AddFeedItem(eventID, "champions", text, "")
		return
	}
	if asStr(existing, "text") != text {
		if _, err := s.sb.Update("feed_items",
			"id=eq."+store.Q(asStr(existing, "id")),
			map[string]any{"text": text}); err != nil {
			log.Printf("feed: team champions update for %s failed: %v", eventID, err)
		}
	}
}

// maybePostPlayoffPodium crowns the playoff podium on the feed once the final is
// decided: gold = champion, silver = runner-up, bronze = the semifinal losers.
func (s *Service) maybePostPlayoffPodium(eventID string, byRound map[int][]map[string]any, maxRound int) {
	final := byRound[maxRound]
	if len(final) != 1 {
		return
	}
	gold := asStr(final[0], "winner_team_id")
	if gold == "" {
		return
	}
	silver := asStr(final[0], "team_a_id")
	if silver == gold {
		silver = asStr(final[0], "team_b_id")
	}
	var bronze []string
	for _, semi := range byRound[maxRound-1] {
		w := asStr(semi, "winner_team_id")
		if w == "" {
			continue
		}
		loser := asStr(semi, "team_a_id")
		if loser == w {
			loser = asStr(semi, "team_b_id")
		}
		if loser != "" {
			bronze = append(bronze, loser)
		}
	}
	teams, err := s.ListTeams(eventID)
	if err != nil {
		return
	}
	name := map[string]string{}
	for _, t := range teams {
		name[t.ID] = t.Name
	}
	text := "Gold: " + name[gold]
	if silver != "" {
		text += " · Silver: " + name[silver]
	}
	if len(bronze) > 0 {
		bn := make([]string, 0, len(bronze))
		for _, id := range bronze {
			bn = append(bn, name[id])
		}
		text += " · Bronze: " + strings.Join(bn, " & ")
	}
	s.PostTeamChampionFeed(eventID, text)
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

// MyFeed returns a unified activity stream across every event the signed-in user
// organizes or plays in, newest first, with each item's event name attached for
// context. Powers the app's NewsFeed tab. Read-only + best-effort: a lookup miss
// on one source still returns what we have.
func (s *Service) MyFeed(userID string) ([]model.FeedItem, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return []model.FeedItem{}, nil
	}
	idSet := map[string]struct{}{}
	// Events they organize.
	if rows, err := s.sb.Select("events", "owner_id=eq."+store.Q(userID)+"&select=id"); err == nil {
		for _, r := range rows {
			if id := asStr(r, "id"); id != "" {
				idSet[id] = struct{}{}
			}
		}
	}
	// Events they're registered to play in. players is a GLOBAL identity table
	// (no event_id column), so go user -> their player rows -> those players'
	// registrations' event_id. (Mirrors linkDuprPlayers.)
	if pls, err := s.sb.Select("players", "user_id=eq."+store.Q(userID)+"&select=id"); err == nil && len(pls) > 0 {
		pids := make([]string, 0, len(pls))
		for _, p := range pls {
			if id := asStr(p, "id"); id != "" {
				pids = append(pids, id)
			}
		}
		if len(pids) > 0 {
			if regs, err := s.sb.Select("registrations",
				"player_id="+store.In(pids)+"&select=event_id"); err == nil {
				for _, r := range regs {
					if id := asStr(r, "event_id"); id != "" {
						idSet[id] = struct{}{}
					}
				}
			}
		}
	}
	if len(idSet) == 0 {
		return []model.FeedItem{}, nil
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	inList := store.In(ids)
	// Event id -> name, for the context label on each item.
	names := map[string]string{}
	if rows, err := s.sb.Select("events", "id="+inList+"&select=id,name"); err == nil {
		for _, r := range rows {
			names[asStr(r, "id")] = asStr(r, "name")
		}
	}
	rows, err := s.sb.Select("feed_items",
		"event_id="+inList+"&select=*&order=created_at.desc&limit=60")
	if err != nil {
		return nil, err
	}
	out := make([]model.FeedItem, 0, len(rows))
	itemIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		fi := mapFeedItem(r)
		fi.ReactionCounts = map[string]int{}
		fi.MyReactions = []string{}
		fi.EventName = names[fi.EventID]
		out = append(out, fi)
		itemIDs = append(itemIDs, fi.ID)
	}
	// My own community posts (standalone user posts, no event). County-scoped
	// visibility to OTHERS lands in a later phase; the author always sees theirs.
	seen := map[string]bool{}
	for _, id := range itemIDs {
		seen[id] = true
	}
	if crows, err := s.sb.Select("feed_items",
		"author_id=eq."+store.Q(userID)+"&select=*&order=created_at.desc&limit=60"); err == nil {
		for _, r := range crows {
			fi := mapFeedItem(r)
			if seen[fi.ID] {
				continue
			}
			seen[fi.ID] = true
			fi.ReactionCounts = map[string]int{}
			fi.MyReactions = []string{}
			out = append(out, fi)
		}
	}
	// Newest first across events + community posts (created_at is ISO → sorts lexically).
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if len(out) > 60 {
		out = out[:60]
	}
	finalIDs := make([]string, len(out))
	for i := range out {
		finalIDs[i] = out[i].ID
	}
	// Enrich with reaction counts / my reactions / comment counts (like ListFeed) —
	// otherwise the NewsFeed shows every post as un-reacted after a refresh.
	if len(finalIDs) > 0 {
		s.attachSocial(out, finalIDs, userID)
	}
	return out, nil
}

// attachSocial fills ReactionCounts/MyReactions/CommentCount for a set of feed
// items in two batched queries (no N+1; best-effort).
func (s *Service) attachSocial(items []model.FeedItem, ids []string, callerID string) {
	inList := store.In(ids)
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
	// Account-level photo first (independent of any players row, so it works for
	// organizers who never registered as a player). Best-effort: a missing
	// profiles table (pre-migration 0035) just leaves no photo, never errors.
	if pr, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=photo_url"); err == nil && pr != nil {
		p.PhotoURL = asStr(pr, "photo_url")
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

// SetMyPhoto uploads the caller's avatar to the "avatars" Storage bucket and
// stamps its public URL on every players row they own (a user's profile is
// denormalized across those rows). Accepts JPEG/PNG up to 5 MB and returns the
// cache-busted URL. The bucket must exist (public-read) — see migration 0034.
func (s *Service) SetMyPhoto(userID, contentType string, data []byte) (string, error) {
	if userID == "" {
		return "", errors.New("not signed in")
	}
	var ext string
	switch contentType {
	case "image/jpeg", "image/jpg":
		contentType, ext = "image/jpeg", "jpg"
	case "image/png":
		ext = "png"
	default:
		return "", errors.New("photo must be a JPEG or PNG")
	}
	if len(data) == 0 {
		return "", errors.New("empty photo")
	}
	if len(data) > 5*1024*1024 {
		return "", errors.New("photo too large (max 5 MB)")
	}
	url, err := s.sb.StorageUpload("avatars", userID+"."+ext, contentType, data)
	if err != nil {
		return "", err
	}
	// Content-addressed cache-bust: the URL only changes when the image does, so
	// a re-upload (same object path) is fetched fresh instead of served stale.
	url = fmt.Sprintf("%s?v=%08x", url, crc32.ChecksumIEEE(data))
	// Persist on the account-level profile (keyed by user_id) so it survives
	// regardless of whether the user has any players rows (migration 0035).
	if _, err := s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id":   userID,
		"photo_url": url,
	}); err != nil {
		return "", err
	}
	return url, nil
}

// ClearMyPhoto removes the caller's uploaded avatar URL from their profile so
// the app falls back to a chosen mascot / initials. The storage object is left
// in place (a later re-upload overwrites it).
func (s *Service) ClearMyPhoto(userID string) error {
	if userID == "" {
		return errors.New("not signed in")
	}
	_, err := s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id":   userID,
		"photo_url": nil,
	})
	return err
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
	// Render the score in winner/loser order (NOT sorted descending): for a
	// retirement the side that retired can be the one that was ahead, so the
	// winner's score is not necessarily the larger number.
	ws, ls := s1, s2
	if wt == 2 {
		ws, ls = s2, s1
	}
	switch m.ResultType {
	case "forfeit":
		return eventID, fmt.Sprintf("%s win — %s forfeited", winner, loser)
	case "walkover":
		return eventID, fmt.Sprintf("%s advance on a walkover", winner)
	case "retire":
		return eventID, fmt.Sprintf("%s def. %s %d–%d (%s retired)", winner, loser, ws, ls, loser)
	}
	if wt == 0 {
		return eventID, fmt.Sprintf("Final: %s vs %s, %d–%d", a, b, s1, s2)
	}
	return eventID, fmt.Sprintf("%s def. %s, %d–%d", winner, loser, ws, ls)
}

// ChampionFeedText returns a "champions" feed line + event id IF the given match
// is the just-decided GOLD final of its division (medal bracket, main tier,
// final round, slot 0, completed with a winner); both "" otherwise. Mirrors the
// app's podium logic so the public feed crowns the same champion the bracket
// shows — and only once it's actually decided. Plain text (the UI adds the icon).
func (s *Service) ChampionFeedText(matchID string) (eventID, text string) {
	row, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=event_id,"+matchSelect)
	if err != nil || row == nil {
		return "", ""
	}
	m := mapMatch(row)
	// Gold lives in the main/medal tier, slot 0, decided — and NOT a Compass draw
	// (bracket_group set), whose final the app's podium/banner doesn't crown, so
	// the feed must not disagree with the bracket UI.
	if m.Stage != "bracket" || m.BracketID == nil || m.WinningTeam == nil ||
		m.Status != "completed" || m.BracketGroup != "" {
		return "", ""
	}
	if m.BracketTier != "" && m.BracketTier != "main" {
		return "", ""
	}
	if m.BracketSlot == nil || *m.BracketSlot != 0 || m.BracketRound == nil {
		return "", ""
	}
	// Confirm it's the FINAL round of this bracket's main tier (not an early slot-0).
	rows, err := s.sb.SelectAll("matches",
		"bracket_id=eq."+store.Q(*m.BracketID)+
			"&stage=eq.bracket&select=bracket_round,bracket_tier")
	if err != nil {
		return "", ""
	}
	maxRound := 0
	for _, r := range rows {
		t := asStr(r, "bracket_tier")
		if t != "" && t != "main" {
			continue
		}
		if rr := asInt(r, "bracket_round"); rr > maxRound {
			maxRound = rr
		}
	}
	if *m.BracketRound != maxRound {
		return "", ""
	}
	// Winner (pair) name + division name.
	winner := "The champions"
	for _, sd := range m.Sides {
		if sd.Team == *m.WinningTeam {
			if n := strings.Join(sd.Players, " & "); n != "" {
				winner = n
			}
		}
	}
	division := ""
	if b, _ := s.sb.SelectOne("brackets",
		"id=eq."+store.Q(*m.BracketID)+"&select=name"); b != nil {
		division = asStr(b, "name")
	}
	eventID = asStr(row, "event_id")
	if division != "" {
		return eventID, fmt.Sprintf("%s win the %s division!", winner, division)
	}
	return eventID, fmt.Sprintf("%s are the champions!", winner)
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
		// Best-effort per match: the round is already active and its matches are
		// marked in_progress — a failed text/notification must NOT fail the whole
		// start (the organizer would re-tap and double-notify everyone else).
		n, _ := s.notifyMatchStart(asStr(m, "id"), asStr(m, "event_id"), court, roundNumber)
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
	// A match can only start once BOTH sides are decided — a bracket game whose
	// feeder hasn't finished has no participants on one team (shown as "TBD"),
	// and starting it would let a half-empty game go live and even get scored.
	parts, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(matchID)+"&select=team")
	if err != nil {
		return 0, err
	}
	teams := map[int]bool{}
	for _, p := range parts {
		teams[asInt(p, "team")] = true
	}
	if !teams[1] || !teams[2] {
		return 0, errors.New("this match can't start yet — its opponent is still TBD (waiting on an earlier result)")
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
	// Best-effort: the match is already live; a failed text/notification must not
	// report the start as failed (the organizer would re-tap, double-notifying).
	n, _ := s.notifyMatchStart(matchID, eventID, court, rn)
	return n, nil
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
		"match_id=eq."+store.Q(matchID)+"&select=player:players!player_id(phone,user_id)")
	if err != nil {
		return 0, err
	}
	var phones []string
	var userIDs []string // OneSignal external_ids = Supabase user ids
	seenUser := map[string]bool{}
	for _, r := range prows {
		if p := asMap(r, "player"); p != nil {
			if ph := asStr(p, "phone"); ph != "" {
				phones = append(phones, ph)
			}
			// Players with a linked account get a push too; skip those without
			// a user_id, and de-dupe (a user could be on both teams in odd setups).
			if uid := asStr(p, "user_id"); uid != "" && !seenUser[uid] {
				seenUser[uid] = true
				userIDs = append(userIDs, uid)
			}
		}
	}

	// One bulk push per match for players with a linked account — best-effort,
	// alongside the SMS below. sendPush logs and swallows its own errors.
	_ = s.sendPush(userIDs, "Match starting",
		fmt.Sprintf("You're up on %s", court), "")

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
		map[string]any{"status": "pending", "error": nil,
			"attempts": 0, "next_attempt_at": nil}) // re-scored → fresh retry budget
	return err
}

// duprIdentifier is the DUPR create identifier for a match at a given generation.
// DUPR forbids reusing an identifier (even after a delete → "Match with identifier
// already exists"), so each generation — bumped whenever a submission is reversed —
// yields a distinct identifier, while staying stable across retries within a
// generation for idempotency / partial-failure safety.
func duprIdentifier(matchID string, gen int) string {
	return matchID + "-g" + strconv.Itoa(gen)
}

// isDuprIdentifierConflict reports whether a DUPR error is an identifier-uniqueness
// rejection ("Match with identifier already exists" / "Provide a unique identifier
// for this match") — the signal to bump the generation and re-create with a fresh id.
func isDuprIdentifierConflict(errMsg string) bool {
	e := strings.ToLower(errMsg)
	return strings.Contains(e, "identifier") &&
		(strings.Contains(e, "exist") || strings.Contains(e, "unique"))
}

// SubmitPendingToDupr flushes queued results to DUPR for a sanctioned event —
// the organizer-initiated "Import to DUPR" action. (#11)
func (s *Service) SubmitPendingToDupr(eventID string) (DuprImportSummary, error) {
	return s.flushDuprSubmissions(eventID, false, nil)
}

// flushDuprSubmissions submits an event's due, still-pending DUPR rows. When
// retryOnly is true it processes only rows that were ALREADY attempted (attempts
// > 0) — used by the background reconciler to heal transient failures without
// auto-submitting a fresh, never-attempted result (which would push to official
// DUPR ratings without the organizer's manual Import).
// duprFlushLocks serializes flushDuprSubmissions per event so a manual "Import
// to DUPR" and the background reconciler can't race the same row (double-create /
// clobbered provider_ref). The identifier idempotency is a backstop, not the
// primary defense.
var duprFlushLocks sync.Map // eventID -> *sync.Mutex

// lockDuprEvent serializes ALL DUPR-submission mutations for one event — the
// import flush, "Remove from DUPR", and the schedule wipe — so they can't
// interleave (e.g. a flush's UpdateMatch racing a remove that clears provider_ref
// and bumps dupr_gen). Returns the unlock func: `defer s.lockDuprEvent(id)()`.
// None of these three paths call each other, so no nested acquisition / deadlock.
func (s *Service) lockDuprEvent(eventID string) func() {
	muAny, _ := duprFlushLocks.LoadOrStore(eventID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// duprSubmitChunk caps how many DUPR match calls (create/update) one flush pass
// makes. DUPR's bulk endpoint accepts 100 matches per request and documents no
// per-minute throttle; per their team's guidance we stay well under the ceiling
// (80) rather than riding it. A pass that hits the cap leaves the remainder
// pending and self-continues in the background after duprChunkPause — so a
// 300-game event submits in ~4 waves without holding the import HTTP request
// open past the server's 15s write timeout.
const duprSubmitChunk = 80

// duprChunkPause spaces the background continuation passes ("submit 80, wait,
// submit the next 80"), mirroring the reconciler's own 2-minute cadence.
const duprChunkPause = 2 * time.Minute

// onlyIDs, when non-nil, restricts the pass to those dupr_submissions row ids —
// used by the chunked background continuation so it finishes exactly the rows
// the organizer's Import covered, and can't sweep in results scored since.
func (s *Service) flushDuprSubmissions(eventID string, retryOnly bool, onlyIDs map[string]bool) (DuprImportSummary, error) {
	defer s.lockDuprEvent(eventID)()

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

	// A manual "Import to DUPR" (retryOnly == false) pushes EVERYTHING not already
	// on DUPR — both pending and previously-failed rows — and ignores the retry
	// backoff (the organizer asked for it now). The background reconciler only
	// retries still-pending, previously-attempted rows and respects the backoff.
	statusFilter := "status=in.(pending,failed)"
	if retryOnly {
		statusFilter = "status=eq.pending"
	}
	pendings, err := s.sb.Select("dupr_submissions",
		"event_id=eq."+store.Q(eventID)+"&"+statusFilter+"&select=id,match_id,provider_ref,attempts,next_attempt_at,dupr_gen")
	if err != nil {
		return DuprImportSummary{}, err
	}

	var sum DuprImportSummary
	calls := 0                     // DUPR API calls made this pass (create/update)
	queuedIDs := map[string]bool{} // rows deferred past the chunk cap
	for _, p := range pendings {
		subID := asStr(p, "id")
		if onlyIDs != nil && !onlyIDs[subID] {
			continue // a continuation pass finishes ITS rows only
		}
		// Chunk cap reached: leave the rest pending for the background
		// continuation (below) / the reconciler's next tick.
		if calls >= duprSubmitChunk {
			queuedIDs[subID] = true
			sum.Queued++
			continue
		}
		// The reconciler respects the backoff window and never auto-submits a fresh
		// (never-attempted) result; a manual import does neither.
		if retryOnly && !dueNow(asStr(p, "next_attempt_at")) {
			continue
		}
		if retryOnly && asInt(p, "attempts") == 0 {
			continue
		}
		matchID := asStr(p, "match_id")
		m, err := s.sb.SelectOne("matches",
			"id=eq."+store.Q(matchID)+"&select=team1_score,team2_score,winning_team,result_type,games,completed_at")
		if err != nil {
			return sum, err
		}
		t1s := asIntPtr(m, "team1_score")
		t2s := asIntPtr(m, "team2_score")
		wt := asIntPtr(m, "winning_team")
		if m == nil || wt == nil || t1s == nil || t2s == nil {
			s.markSubmission(subID, "failed", "", "match not completed")
			sum.Failed++
			sum.Details = append(sum.Details, "match not completed")
			continue
		}
		// Forfeits/retirements/walkovers aren't real played results — skip them
		// (belt-and-suspenders; advanceAfterScore no longer queues them).
		if rt := asStr(m, "result_type"); rt != "" && rt != "normal" {
			s.markSubmission(subID, "skipped", "", "not a played result ("+rt+")")
			sum.Skipped++
			sum.Details = append(sum.Details, "skipped: not a played result ("+rt+")")
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
			sum.Details = append(sum.Details, "missing DUPR id for "+missing)
			continue
		}
		// A bye (one side empty) is not a real match — skip rather than submit a
		// one-sided result to DUPR.
		if len(t1) == 0 || len(t2) == 0 {
			s.markSubmission(subID, "skipped", "", "bye / incomplete side")
			sum.Skipped++
			sum.Details = append(sum.Details, "skipped: bye / incomplete side")
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
		matchDate := ""
		if c := asStr(m, "completed_at"); len(c) >= 10 {
			matchDate = c[:10] // RFC3339 → yyyy-MM-dd
		}
		existingCode := asStr(p, "provider_ref")
		payload := gateway.DuprPayload{
			EventID: eventID, DuprEventID: duprEventID,
			EventName: ev.Name, MatchID: matchID, MatchCode: existingCode,
			// Fresh per-generation identifier so a re-create after a delete never
			// reuses an identifier (DUPR rejects that). Stable within a generation.
			Identifier:   duprIdentifier(matchID, asInt(p, "dupr_gen")),
			MatchDate:    matchDate,
			Team1DuprIDs: t1, Team2DuprIDs: t2,
			Team1Score: g1t1, Team2Score: g1t2, Games: pairs,
		}
		// Already submitted (re-scored) → update the DUPR match; else create it.
		calls++
		var res gateway.DuprResult
		if existingCode != "" {
			res, err = s.Dupr.UpdateMatch(payload)
		} else {
			res, err = s.Dupr.SubmitMatch(payload)
		}
		if err != nil {
			return sum, err
		}
		if res.OK {
			// CRITICAL write: the match is now on DUPR, so the provider_ref MUST
			// land locally — otherwise we lose the matchCode and can neither update
			// nor delete it, orphaning a real result on official ratings. Retry the
			// write once and alarm loudly if it still fails (far worse than a failed
			// submit — the reconciler can't recover a lost provider_ref).
			if e := s.markSubmission(subID, "submitted", res.DuprMatchID, ""); e != nil {
				log.Printf("dupr: ALARM submit succeeded (match=%s code=%s) but provider_ref write failed: %v — retrying",
					matchID, res.DuprMatchID, e)
				if e2 := s.markSubmission(subID, "submitted", res.DuprMatchID, ""); e2 != nil {
					log.Printf("dupr: ALARM provider_ref write STILL failing for match=%s code=%s: %v — result is on DUPR but locally unrecorded",
						matchID, res.DuprMatchID, e2)
				}
			}
			if existingCode != "" {
				sum.Updated++ // corrected an existing DUPR match
			} else {
				sum.Submitted++ // newly created
			}
		} else if res.Permanent && existingCode == "" && isDuprIdentifierConflict(res.Error) {
			// "Identifier already exists" on a CREATE = this match is already on DUPR
			// under the current generation's identifier, but we lost its matchCode
			// (drift, e.g. a create whose local write failed). Bump the generation and
			// keep it retryable so the next import creates cleanly with a FRESH
			// identifier rather than dead-ending on the collision.
			if _, err := s.sb.Update("dupr_submissions", "id=eq."+store.Q(subID),
				map[string]any{
					"status": "pending", "attempts": 0, "next_attempt_at": nil,
					"error": orNull(res.Error), "dupr_gen": asInt(p, "dupr_gen") + 1,
				}); err != nil {
				log.Printf("dupr: gen-bump write failed for %s: %v", subID, err)
			}
			sum.Failed++
			sum.Details = append(sum.Details, res.Error+" — will re-create with a fresh identifier on the next import")
		} else if res.Permanent {
			// A DUPR 4xx (bad payload, invalid dupr_id) won't fix itself — go terminal
			// now instead of burning 10 retries. PRESERVE provider_ref (an update that
			// permanently failed keeps its matchCode so a later retry updates the right
			// match; a create-fail has none anyway).
			_ = s.markSubmission(subID, "failed", existingCode, res.Error)
			sum.Failed++
			reason := res.Error
			if reason == "" {
				reason = "DUPR rejected the submission"
			}
			sum.Details = append(sum.Details, reason)
		} else {
			// Transient DUPR failure (5xx / 429 / timeout / network): keep the row
			// retryable with backoff until the attempt cap flips it to 'failed'.
			s.markSubmissionRetry(subID, asInt(p, "attempts"), res.Error)
			sum.Failed++
			reason := res.Error
			if reason == "" {
				reason = "DUPR rejected the submission"
			}
			sum.Details = append(sum.Details, reason)
		}
	}
	if sum.Queued > 0 {
		sum.Details = append(sum.Details, fmt.Sprintf(
			"%d more queued — submitting in the background in batches of %d every %d min",
			sum.Queued, duprSubmitChunk, int(duprChunkPause.Minutes())))
		// A manual import self-continues so the organizer doesn't have to re-tap
		// Import once per chunk — scoped to the snapshot of rows THIS import
		// covered. The per-event lock serializes overlapping passes; if the
		// process restarts mid-chain the leftovers stay pending and the next
		// manual Import picks them up. The reconciler (retryOnly) doesn't chain —
		// its 2-minute ticker already provides the cadence.
		if !retryOnly {
			go func() {
				time.Sleep(duprChunkPause)
				if _, err := s.flushDuprSubmissions(eventID, false, queuedIDs); err != nil {
					log.Printf("dupr: background chunk flush (event %s): %v", eventID, err)
				}
			}()
		}
	}
	return sum, nil
}

// DuprSubmissionStatuses returns each of the event's DUPR submission rows as
// {match_id, status} (status in pending|submitted|failed|skipped) so the UI can
// badge which matches have already been sent to DUPR.
func (s *Service) DuprSubmissionStatuses(eventID string) ([]map[string]any, error) {
	return s.sb.Select("dupr_submissions",
		"event_id=eq."+store.Q(eventID)+"&select=match_id,status")
}

// DuprRemoveSummary reports the outcome of RemoveEventFromDupr.
type DuprRemoveSummary struct {
	Removed int      `json:"removed"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

// RemoveEventFromDupr reverses an event's already-submitted results on DUPR — the
// delete leg of the create/update/delete round-trip. It deletes each submitted
// match on DUPR and "un-submits" it locally (back to pending, provider_ref/attempts
// cleared) so it can be re-imported. Local scores are left untouched — unlike a
// schedule wipe, this only touches DUPR. Owner-gated at the handler.
func (s *Service) RemoveEventFromDupr(eventID string) (DuprRemoveSummary, error) {
	defer s.lockDuprEvent(eventID)() // serialize vs the import flush / reconciler
	subs, err := s.sb.Select("dupr_submissions",
		"event_id=eq."+store.Q(eventID)+"&status=eq.submitted&select=id,match_id,provider_ref,dupr_gen")
	if err != nil {
		return DuprRemoveSummary{}, err
	}
	var sum DuprRemoveSummary
	for _, sub := range subs {
		code := asStr(sub, "provider_ref")
		if code == "" {
			continue // never actually landed on DUPR — nothing to delete
		}
		// DUPR delete validates the identifier against the one used at CREATE, so
		// pass the same generation identifier (not the raw match_id).
		ident := duprIdentifier(asStr(sub, "match_id"), asInt(sub, "dupr_gen"))
		if err := s.Dupr.DeleteMatch(code, ident); err != nil {
			sum.Failed++
			sum.Errors = append(sum.Errors, err.Error())
			continue
		}
		// Un-submit locally: gone from DUPR, re-importable with a fresh budget.
		// (Not auto-resubmitted — the reconciler only retries attempts>0 rows.)
		// Bump dupr_gen so a re-import uses a BRAND-NEW identifier — DUPR won't let
		// a deleted match's identifier be reused.
		_, _ = s.sb.Update("dupr_submissions", "id=eq."+store.Q(asStr(sub, "id")),
			map[string]any{
				"status": "pending", "provider_ref": nil, "submitted_at": nil,
				"attempts": 0, "next_attempt_at": nil, "error": nil,
				"dupr_gen": asInt(sub, "dupr_gen") + 1,
			})
		sum.Removed++
	}
	return sum, nil
}

// RemoveMatchFromDupr reverses a SINGLE match's already-submitted result on DUPR
// (the per-game delete leg). It deletes just that match on DUPR and "un-submits"
// it locally (back to pending, provider_ref/attempts cleared, dupr_gen bumped) so
// it can be re-imported. The local match + score are left untouched. Owner-gated
// at the handler. Returns a summary (Removed 0 / Failed 0 = nothing was on DUPR).
func (s *Service) RemoveMatchFromDupr(matchID string) (DuprRemoveSummary, error) {
	// Find the submission (and its event) so we can lock the right event.
	head, err := s.sb.SelectOne("dupr_submissions",
		"match_id=eq."+store.Q(matchID)+"&select=event_id")
	if err != nil {
		return DuprRemoveSummary{}, err
	}
	if head == nil {
		return DuprRemoveSummary{}, nil // never queued to DUPR — nothing to remove
	}
	eventID := asStr(head, "event_id")
	defer s.lockDuprEvent(eventID)() // serialize vs the import flush / reconciler
	// Re-read the authoritative row UNDER the lock (a concurrent flush may have
	// just changed status/provider_ref/dupr_gen).
	sub, err := s.sb.SelectOne("dupr_submissions",
		"match_id=eq."+store.Q(matchID)+"&select=id,match_id,provider_ref,dupr_gen")
	if err != nil {
		return DuprRemoveSummary{}, err
	}
	if sub == nil {
		return DuprRemoveSummary{}, nil
	}
	var sum DuprRemoveSummary
	code := asStr(sub, "provider_ref")
	if code == "" {
		return sum, nil // queued but never landed on DUPR — nothing to delete there
	}
	// DUPR delete validates the identifier against the one used at CREATE, so pass
	// the same generation identifier (not the raw match_id).
	ident := duprIdentifier(asStr(sub, "match_id"), asInt(sub, "dupr_gen"))
	if err := s.Dupr.DeleteMatch(code, ident); err != nil {
		sum.Failed++
		sum.Errors = append(sum.Errors, err.Error())
		return sum, nil
	}
	// Un-submit locally: gone from DUPR, re-importable. Bump dupr_gen so a re-import
	// uses a BRAND-NEW identifier — DUPR won't let a deleted match's id be reused.
	_, _ = s.sb.Update("dupr_submissions", "id=eq."+store.Q(asStr(sub, "id")),
		map[string]any{
			"status": "pending", "provider_ref": nil, "submitted_at": nil,
			"attempts": 0, "next_attempt_at": nil, "error": nil,
			"dupr_gen": asInt(sub, "dupr_gen") + 1,
		})
	sum.Removed++
	return sum, nil
}

func (s *Service) markSubmission(id, status, ref, errMsg string) error {
	var submittedAt any
	if status == "submitted" {
		submittedAt = now()
	}
	_, err := s.sb.Update("dupr_submissions", "id=eq."+store.Q(id), map[string]any{
		"status":          status,
		"provider_ref":    orNull(ref),
		"error":           orNull(errMsg),
		"submitted_at":    submittedAt,
		"next_attempt_at": nil, // terminal (or skipped) → no more retries
	})
	return err
}

// duprMaxAttempts caps retries of a transiently-failing submission before it is
// marked terminal 'failed'. With the backoff below this spans ~2.5h of outage.
const duprMaxAttempts = 10

// markSubmissionRetry records a transient DUPR failure. Below the attempt cap the
// row stays 'pending' with an exponential-backoff next_attempt_at so the
// reconciler retries it; at the cap it becomes terminal 'failed' (visible, so an
// organizer can investigate / re-import).
func (s *Service) markSubmissionRetry(id string, attempts int, errMsg string) {
	attempts++
	if attempts >= duprMaxAttempts {
		if _, err := s.sb.Update("dupr_submissions", "id=eq."+store.Q(id), map[string]any{
			"status":          "failed",
			"attempts":        attempts,
			"error":           orNull(errMsg + " — gave up after " + strconv.Itoa(attempts) + " attempts"),
			"next_attempt_at": nil,
		}); err != nil {
			log.Printf("dupr: markSubmissionRetry(terminal) write failed for %s: %v", id, err)
		}
		return
	}
	// If this write fails, the next flush re-attempts the DUPR create with the SAME
	// generation identifier — the identifier idempotency is the double-create guard.
	if _, err := s.sb.Update("dupr_submissions", "id=eq."+store.Q(id), map[string]any{
		"status":          "pending",
		"attempts":        attempts,
		"error":           orNull(errMsg),
		"next_attempt_at": duprNextAttempt(attempts),
	}); err != nil {
		log.Printf("dupr: markSubmissionRetry(backoff) write failed for %s: %v", id, err)
	}
}

// duprNextAttempt returns when to retry after `attempts` failures: exponential
// backoff 1m, 2m, 4m, … capped at 30m.
func duprNextAttempt(attempts int) string {
	d := time.Minute
	for i := 1; i < attempts && d < 30*time.Minute; i++ {
		d *= 2
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return time.Now().UTC().Add(d).Format("2006-01-02T15:04:05.000Z")
}

// dueNow reports whether a next_attempt_at timestamp has arrived (empty/unparsed
// → due, so a row can never get permanently stuck).
func dueNow(ts string) bool {
	if ts == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true
	}
	// UTC on both sides (duprNextAttempt writes UTC) — compare absolute instants.
	return !t.After(time.Now().UTC())
}

// ReconcileDuprSubmissions retries transient DUPR failures that are now due,
// across all sanctioned events — so a DUPR hiccup self-heals without the
// organizer re-clicking Import. Only already-attempted rows (attempts > 0) are
// retried; a fresh, never-imported result is left for the organizer's action.
func (s *Service) ReconcileDuprSubmissions() error {
	rows, err := s.sb.Select("dupr_submissions",
		"status=eq.pending&attempts=gt.0&select=event_id,next_attempt_at")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if !dueNow(asStr(r, "next_attempt_at")) {
			continue
		}
		eid := asStr(r, "event_id")
		if eid == "" || seen[eid] {
			continue
		}
		seen[eid] = true
		if _, err := s.flushDuprSubmissions(eid, true, nil); err != nil {
			log.Printf("dupr reconcile: event %s: %v", eid, err)
		}
	}
	return nil
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
	// SelectAll: across a full division the completed-pool-match count can pass the
	// row cap; truncation would drop head-to-head results and mis-break standings ties.
	rows, err := s.sb.SelectAll("matches", q)
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

// ResultsCSV builds a downloadable results export for an event: a per-division
// standings section (rank, players, GP/W/L, points for/against, diff) followed
// by a matches section (division, round/stage, both teams, score, winner). It
// reuses the same Standings + matches queries the live dashboard uses, so the
// export matches what organizers see on screen. Returns the CSV bytes.
// csvSafe defends a CSV cell against spreadsheet formula injection (CWE-1236): a
// cell whose first char is one a spreadsheet may treat as a formula (= + - @) or
// a control char (TAB/CR) is prefixed with an apostrophe so Excel/Sheets render
// it as text. Applied to every user-supplied cell in CSV exports — player names
// etc. are free text collected at (public) registration.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// RosterCSV streams the event's REGISTRANT roster (contact info + status) as a
// CSV — distinct from ResultsCSV (standings/matches). Owner-only. Uses its own
// query so player email can be included without putting it in the shared
// Registration JSON.
func (s *Service) RosterCSV(eventID string) ([]byte, error) {
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	// Team (MLP) events have no registrations — export the team rosters instead.
	if ev.TeamSize > 0 {
		return s.teamRosterCSV(ev)
	}
	brackets, err := s.GetBrackets(eventID)
	if err != nil {
		return nil, err
	}
	divName := make(map[string]string, len(brackets))
	for _, b := range brackets {
		divName[b.ID] = b.Name
	}

	base := "event_id=eq." + store.Q(eventID) +
		"&select=payment_status,checked_in,bracket_id,%s" +
		"player:players!player_id(full_name,phone,email,dupr_id)," +
		"partner:players!partner_id(full_name)"
	rows, err := s.sb.SelectAll("registrations", fmt.Sprintf(base, "partner_name,"))
	if err != nil {
		// Tolerate the partner_name column not existing yet (pre-migration).
		rows, err = s.sb.SelectAll("registrations", fmt.Sprintf(base, ""))
		if err != nil {
			return nil, err
		}
	}

	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	w := func(rec ...string) { _ = cw.Write(rec) }

	w("PlanMyPickle Roster", csvSafe(ev.Name))
	w()
	w("Name", "Phone", "Email", "Division", "Partner", "Paid", "Checked in", "DUPR ID")
	for _, r := range rows {
		var name, phone, email, dupr string
		if p := asMap(r, "player"); p != nil {
			name = asStr(p, "full_name")
			phone = asStr(p, "phone")
			email = asStr(p, "email")
			dupr = asStr(p, "dupr_id")
		}
		partner := ""
		if pp := asMap(r, "partner"); pp != nil {
			partner = asStr(pp, "full_name")
		}
		if partner == "" {
			partner = asStr(r, "partner_name")
		}
		paid := "No"
		if asStr(r, "payment_status") == "paid" {
			paid = "Yes"
		}
		checked := "No"
		if asBool(r, "checked_in") {
			checked = "Yes"
		}
		w(csvSafe(name), csvSafe(phone), csvSafe(email),
			csvSafe(divName[asStr(r, "bracket_id")]), csvSafe(partner),
			paid, checked, csvSafe(dupr))
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// teamRosterCSV exports an MLP team event's rosters (one row per member, grouped
// by team) — the registration-based RosterCSV reads empty for team events.
func (s *Service) teamRosterCSV(ev model.Event) ([]byte, error) {
	teams, err := s.ListTeams(ev.ID)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	w := func(rec ...string) { _ = cw.Write(rec) }
	w("PlanMyPickle Roster", csvSafe(ev.Name))
	w()
	w("Name", "Phone", "Team", "Gender", "Checked in", "DUPR ID")
	for _, t := range teams {
		for _, m := range t.Members {
			gender := "Man"
			if m.Gender == "F" {
				gender = "Woman"
			}
			checked := "No"
			if m.CheckedIn {
				checked = "Yes"
			}
			w(csvSafe(m.FullName), csvSafe(m.Phone), csvSafe(t.Name),
				gender, checked, csvSafe(m.DuprID))
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Service) ResultsCSV(eventID string) ([]byte, error) {
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	brackets, err := s.GetBrackets(eventID)
	if err != nil {
		return nil, err
	}
	// Division name lookup for the matches section (matches carry a bracket id).
	divName := make(map[string]string, len(brackets))
	for _, b := range brackets {
		divName[b.ID] = b.Name
	}

	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	write := func(rec ...string) error { return cw.Write(rec) }

	if err := write("PlanMyPickle Results", ev.Name); err != nil {
		return nil, err
	}
	_ = write() // blank separator line

	// ---- Standings, one block per division (in dashboard sort order). ----
	if err := write("STANDINGS"); err != nil {
		return nil, err
	}
	for _, b := range brackets {
		st, err := s.Standings(eventID, b.ID, true)
		if err != nil {
			return nil, err
		}
		if err := write("Division", b.Name); err != nil {
			return nil, err
		}
		if err := write("Rank", "Player", "GP", "W", "L", "PF", "PA", "Diff"); err != nil {
			return nil, err
		}
		for i, row := range st {
			if err := write(
				strconv.Itoa(i+1), csvSafe(row.FullName),
				strconv.Itoa(row.GamesPlayed), strconv.Itoa(row.Wins), strconv.Itoa(row.Losses),
				strconv.Itoa(row.PointsFor), strconv.Itoa(row.PointsAgainst), strconv.Itoa(row.PointDiff),
			); err != nil {
				return nil, err
			}
		}
		_ = write()
	}

	// ---- Matches: every pool game + every bracket game, across divisions. ----
	if err := write("MATCHES"); err != nil {
		return nil, err
	}
	if err := write("Division", "Round/Stage", "Team 1", "Team 2", "Score", "Winner"); err != nil {
		return nil, err
	}
	pool, err := s.EventPoolMatches(eventID)
	if err != nil {
		return nil, err
	}
	all := pool
	for _, b := range brackets {
		bm, err := s.BracketMatches(b.ID)
		if err != nil {
			return nil, err
		}
		all = append(all, bm...)
	}
	for _, m := range all {
		div := ""
		if m.BracketID != nil {
			div = divName[*m.BracketID]
		}
		if err := write(
			csvSafe(div), matchStageLabel(m),
			csvSafe(teamNames(m, 1)), csvSafe(teamNames(m, 2)),
			matchScoreText(m), csvSafe(matchWinnerText(m)),
		); err != nil {
			return nil, err
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SanctionCSV builds the sanction-ready export: one row per completed,
// really-played match with every player's name AND DUPR id, the per-game
// scores, the winner, and the match's DUPR submission state (status + match
// code). It's the paper trail a sanctioned event needs — verification of what
// was (or wasn't) pushed to DUPR, or the source sheet for a manual submission.
func (s *Service) SanctionCSV(eventID string) ([]byte, error) {
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	brackets, err := s.GetBrackets(eventID)
	if err != nil {
		return nil, err
	}
	divName := make(map[string]string, len(brackets))
	for _, b := range brackets {
		divName[b.ID] = b.Name
	}
	pool, err := s.EventPoolMatches(eventID)
	if err != nil {
		return nil, err
	}
	all := pool
	for _, b := range brackets {
		bm, err := s.BracketMatches(b.ID)
		if err != nil {
			return nil, err
		}
		all = append(all, bm...)
	}

	// Participants with DUPR ids, one bulk query (the match Sides projection is
	// spectator-safe and carries names only).
	type part struct{ name, dupr string }
	sides := map[string]map[int][]part{} // match id -> team -> players
	ids := make([]string, 0, len(all))
	for _, m := range all {
		ids = append(ids, m.ID)
	}
	if len(ids) > 0 {
		rows, err := s.sb.SelectAll("match_participants",
			"match_id="+store.In(ids)+
				"&select=match_id,team,player:players!player_id(full_name,dupr_id)")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			mid := asStr(r, "match_id")
			team := asInt(r, "team")
			p := part{}
			if pl := asMap(r, "player"); pl != nil {
				p = part{name: asStr(pl, "full_name"), dupr: asStr(pl, "dupr_id")}
			}
			if sides[mid] == nil {
				sides[mid] = map[int][]part{}
			}
			sides[mid][team] = append(sides[mid][team], p)
		}
	}

	// DUPR submission state per match (absent rows = never queued).
	type subState struct{ status, code string }
	subs := map[string]subState{}
	if rows, err := s.sb.Select("dupr_submissions",
		"event_id=eq."+store.Q(eventID)+"&select=match_id,status,provider_ref"); err == nil {
		for _, r := range rows {
			subs[asStr(r, "match_id")] = subState{
				status: asStr(r, "status"), code: asStr(r, "provider_ref"),
			}
		}
	}

	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	write := func(rec ...string) error { return cw.Write(rec) }
	if err := write("PlanMyPickle Sanction-Ready Export", ev.Name); err != nil {
		return nil, err
	}
	sanctioned := "no"
	if ev.DuprSanctioned {
		sanctioned = "yes"
	}
	_ = write("DUPR sanctioned", sanctioned)
	_ = write()
	if err := write("Division", "Stage", "Date",
		"Team 1 Player A", "DUPR ID", "Team 1 Player B", "DUPR ID",
		"Team 2 Player A", "DUPR ID", "Team 2 Player B", "DUPR ID",
		"Game scores", "Winner", "DUPR status", "DUPR match code"); err != nil {
		return nil, err
	}
	// cell returns the nth player/dupr pair of a side, blank-padded (singles
	// leave the B columns empty).
	cell := func(ps []part, n int) (string, string) {
		if n >= len(ps) {
			return "", ""
		}
		return ps[n].name, ps[n].dupr
	}
	for _, m := range all {
		// Only completed, really-played results belong on a sanction sheet —
		// no byes, forfeits, retirements, walkovers, or unplayed games.
		if m.Status != "completed" || m.Team1Score == nil || m.Team2Score == nil {
			continue
		}
		if m.ResultType != "" && m.ResultType != "normal" {
			continue
		}
		div := ""
		if m.BracketID != nil {
			div = divName[*m.BracketID]
		}
		date := ""
		if m.CompletedAt != nil && len(*m.CompletedAt) >= 10 {
			date = (*m.CompletedAt)[:10]
		}
		t1 := sides[m.ID][1]
		t2 := sides[m.ID][2]
		if len(t1) == 0 || len(t2) == 0 {
			continue // bye / incomplete side
		}
		t1a, t1aID := cell(t1, 0)
		t1b, t1bID := cell(t1, 1)
		t2a, t2aID := cell(t2, 0)
		t2b, t2bID := cell(t2, 1)
		sub := subs[m.ID]
		if sub.status == "" {
			sub.status = "not queued"
		}
		if err := write(
			csvSafe(div), matchStageLabel(m), date,
			csvSafe(t1a), csvSafe(t1aID), csvSafe(t1b), csvSafe(t1bID),
			csvSafe(t2a), csvSafe(t2aID), csvSafe(t2b), csvSafe(t2bID),
			matchScoreText(m), csvSafe(matchWinnerText(m)),
			sub.status, sub.code,
		); err != nil {
			return nil, err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// matchStageLabel describes where a match sits: a pool round number, or the
// bracket tier + round for a playoff match.
func matchStageLabel(m model.Match) string {
	if m.Stage == "pool" {
		if m.RoundNumber != nil {
			return "Pool R" + strconv.Itoa(*m.RoundNumber)
		}
		return "Pool"
	}
	tier := m.BracketTier
	if tier == "" {
		tier = "main"
	}
	if m.BracketRound != nil {
		return tier + " R" + strconv.Itoa(*m.BracketRound)
	}
	return tier
}

// teamNames joins the display names of a match's side (team 1 or 2). Empty for a
// TBD / bye side.
func teamNames(m model.Match, team int) string {
	for _, sd := range m.Sides {
		if sd.Team == team {
			return strings.Join(sd.Players, " / ")
		}
	}
	return ""
}

// matchScoreText renders the recorded score ("11-7"), blank when unplayed.
func matchScoreText(m model.Match) string {
	if m.Team1Score == nil || m.Team2Score == nil {
		return ""
	}
	return strconv.Itoa(*m.Team1Score) + "-" + strconv.Itoa(*m.Team2Score)
}

// matchWinnerText returns the winning team's player names, or "" if undecided.
func matchWinnerText(m model.Match) string {
	if m.WinningTeam == nil {
		return ""
	}
	return teamNames(m, *m.WinningTeam)
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

// EventScheduleMatches returns the matches that belong on the Game-tab time-grid.
// For elimination draws (single/double-elim, compass) that's the bracket matches
// (they carry court/play_order from spreadBracketCourts); for every other format
// it's the pool games only — pools_playoff's medal bracket stays out of the grid
// (it lives on Standings, and its bracket_round would collide with pool rounds).
// Distinct from EventPoolMatches (kept pool-only for the CSV export, which appends
// bracket games itself).
func (s *Service) EventScheduleMatches(eventID string) ([]model.Match, error) {
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=tournament_format")
	if err != nil {
		return nil, err
	}
	stageFilter := "&stage=eq.pool"
	switch asStr(ev, "tournament_format") {
	case "single_elim", "double_elim", "compass":
		stageFilter = "" // bracket-only event → return its bracket matches
	case "pools_playoff":
		// Pools AND the medal bracket — so the live TV can detect the playoff
		// phase and show the bracket + champions celebration once pools finish.
		stageFilter = ""
	}
	rows, err := s.sb.SelectAll("matches",
		"event_id=eq."+store.Q(eventID)+stageFilter+"&select="+matchSelect)
	if err != nil {
		return nil, err
	}
	out := make([]model.Match, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapMatch(r))
	}
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
	// The replacement must be REGISTERED in THIS match's event — not merely an
	// existing player row. Player rows are shared across events, so without
	// binding the body-supplied in-player to the path match's event an organizer
	// could swap in another organizer's player (cross-event IDOR / confused
	// deputy — same class as the ladder/team fixes).
	mrow, err := s.sb.SelectOne("matches", "id=eq."+store.Q(matchID)+"&select=event_id")
	if err != nil {
		return err
	}
	if mrow == nil {
		return ErrNotFound
	}
	reg, err := s.sb.SelectOne("registrations",
		"event_id=eq."+store.Q(asStr(mrow, "event_id"))+
			"&player_id=eq."+store.Q(inPlayerID)+"&select=id")
	if err != nil {
		return err
	}
	if reg == nil {
		return errors.New("replacement player is not registered in this event")
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

// SwapPlayersAcrossMatches exchanges two players who each sit in a DIFFERENT
// match: playerA (currently in matchA) takes playerB's match/team slot and
// vice-versa. Each player keeps the OTHER's team number on the destination side
// (i.e. team slots stay filled). Both matches must be un-scored.
//
// It returns a non-empty warning string (with a nil error) when the swap leaves
// a player double-booked in the same time slot — the swap is still performed so
// organizers can fix scheduling in steps. A non-nil error means nothing changed.
func (s *Service) SwapPlayersAcrossMatches(matchA, playerA, matchB, playerB string) (string, error) {
	if matchA == "" || playerA == "" || matchB == "" || playerB == "" {
		return "", errors.New("matchA, playerA, matchB and playerB are required")
	}
	if matchA == matchB {
		return "", errors.New("both players are in the same match; use the same-match swap instead")
	}
	if playerA == playerB {
		return "", errors.New("cannot swap a player with themselves")
	}

	// Both matches must exist and be un-scored. Block once either is completed.
	rowA, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchA)+"&select=id,status,scheduled_day,play_order")
	if err != nil {
		return "", err
	}
	if rowA == nil {
		return "", ErrNotFound
	}
	rowB, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchB)+"&select=id,status,scheduled_day,play_order")
	if err != nil {
		return "", err
	}
	if rowB == nil {
		return "", ErrNotFound
	}
	if asStr(rowA, "status") == "completed" || asStr(rowB, "status") == "completed" {
		return "", errors.New("cannot swap players in a match that already has a recorded score")
	}

	// Each player must currently be in their stated match (read their team slot).
	partA, err := s.sb.SelectOne("match_participants",
		"match_id=eq."+store.Q(matchA)+"&player_id=eq."+store.Q(playerA)+"&select=team")
	if err != nil {
		return "", err
	}
	if partA == nil {
		return "", ErrNotFound
	}
	partB, err := s.sb.SelectOne("match_participants",
		"match_id=eq."+store.Q(matchB)+"&player_id=eq."+store.Q(playerB)+"&select=team")
	if err != nil {
		return "", err
	}
	if partB == nil {
		return "", ErrNotFound
	}
	teamA := asInt(partA, "team")
	teamB := asInt(partB, "team")

	// Reject if a player would end up in a match they're already in (would create
	// a duplicate participant row / collapse the unique (match,player) key).
	dupA, err := s.sb.SelectOne("match_participants",
		"match_id=eq."+store.Q(matchB)+"&player_id=eq."+store.Q(playerA)+"&select=match_id")
	if err != nil {
		return "", err
	}
	if dupA != nil {
		return "", errors.New("that player is already in the other match")
	}
	dupB, err := s.sb.SelectOne("match_participants",
		"match_id=eq."+store.Q(matchA)+"&player_id=eq."+store.Q(playerB)+"&select=match_id")
	if err != nil {
		return "", err
	}
	if dupB != nil {
		return "", errors.New("that player is already in the other match")
	}

	// Perform the exchange. playerA -> matchB on playerB's old team slot; playerB
	// -> matchA on playerA's old team slot. Two-step to dodge the unique
	// (match_id,player_id) constraint mid-swap: move A out to matchB first, then B.
	if _, err := s.sb.Update("match_participants",
		"match_id=eq."+store.Q(matchA)+"&player_id=eq."+store.Q(playerA),
		map[string]any{"match_id": matchB, "team": teamB}); err != nil {
		return "", err
	}
	if _, err := s.sb.Update("match_participants",
		"match_id=eq."+store.Q(matchB)+"&player_id=eq."+store.Q(playerB),
		map[string]any{"match_id": matchA, "team": teamA}); err != nil {
		// Best-effort rollback of the first move so we don't leave a half-swap.
		_, _ = s.sb.Update("match_participants",
			"match_id=eq."+store.Q(matchB)+"&player_id=eq."+store.Q(playerA),
			map[string]any{"match_id": matchA, "team": teamA})
		return "", err
	}

	// Soft conflict: if the two matches share a (day, slot) the swapped player is
	// now double-booked. Warn but don't fail — organizers fix this in steps.
	if sameSlot(rowA, rowB) {
		return "heads up: these matches are in the same time slot, so a player may now be double-booked", nil
	}
	return "", nil
}

// sameSlot reports whether two match rows occupy the same scheduling slot
// (same tournament day and same within-court play order), in which case any
// player shared between them is double-booked. Rows must carry scheduled_day
// and play_order.
func sameSlot(a, b map[string]any) bool {
	pa, pb := asFloatPtr(a, "play_order"), asFloatPtr(b, "play_order")
	if pa == nil || pb == nil || *pa != *pb {
		return false
	}
	return asInt(a, "scheduled_day") == asInt(b, "scheduled_day")
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
// SetEventBreaks replaces an event's blocked time ranges (e.g. lunch). The
// schedule timeline skips over these.
func (s *Service) SetEventBreaks(eventID string, breaks []model.ScheduleBreak) error {
	arr := make([]map[string]any, 0, len(breaks))
	for _, b := range breaks {
		arr = append(arr, map[string]any{
			"startMin": b.StartMin,
			"endMin":   b.EndMin,
			"label":    b.Label,
		})
	}
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"schedule_breaks": arr})
	return err
}

// SetDayCap sets the latest time-of-day games may start (minutes from midnight);
// a negative value clears it. Past the cap, games roll to the next day.
func (s *Service) SetDayCap(eventID string, cap int) error {
	var val any
	if cap >= 0 {
		val = cap
	}
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"day_cap_minutes": val})
	return err
}

// SetDayEnds stores per-day court closing times (minutes from midnight, indexed
// by tournament day; -1 in a slot = no close that day). An empty list clears them.
func (s *Service) SetDayEnds(eventID string, ends []int) error {
	var val any
	if len(ends) > 0 {
		val = ends
	}
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"day_end_minutes": val})
	return err
}

// SetMatchDay assigns a match to a 0-based tournament day. A negative day clears
// the assignment (the schedule falls back to its automatic even split).
func (s *Service) SetMatchDay(matchID string, day int) error {
	var val any // nil clears the assignment
	if day >= 0 {
		val = day
	}
	_, err := s.sb.Update("matches", "id=eq."+store.Q(matchID),
		map[string]any{"scheduled_day": val})
	return err
}

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
	// SelectAll: the players table grows across all events, so an unbounded Select
	// would truncate at PostgREST's row cap and skew skill lookups.
	rows, err := s.sb.SelectAll("players", "select=id,skill_level")
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

// appendSideParts appends one match_participants row per player on a side,
// applying insertSide's skip rules (empty side / bye). Used by the batched
// bracket builders to collect every participant for a single bulk Upsert.
func appendSideParts(rows []map[string]any, matchID string, team int, side []string) []map[string]any {
	if len(side) == 0 || engine.IsBye(side) {
		return rows
	}
	for _, pid := range side {
		rows = append(rows, map[string]any{"match_id": matchID, "player_id": pid, "team": team})
	}
	return rows
}

// feedUpdates batches the bracket feed-link writes. Many matches feed the same
// target (feeds_match_id/feeds_slot or loser_feeds_*), so it groups match ids by
// an identical update payload and issues ONE Update per distinct payload, joining
// the ids with id=in.(...) (chunked to keep the URL bounded). This replaces the
// old per-match Update loop that fired hundreds of sequential PostgREST calls.
type feedUpdates struct {
	order  []string                  // payload keys, in first-seen order (stable)
	fields map[string]map[string]any // payload key -> the update fields
	ids    map[string][]string       // payload key -> match ids to apply it to
}

func newFeedUpdates() *feedUpdates {
	return &feedUpdates{fields: map[string]map[string]any{}, ids: map[string][]string{}}
}

func (f *feedUpdates) add(matchID string, fields map[string]any) {
	if matchID == "" {
		return
	}
	// Stable key for identical payloads: sorted "k=v" pairs. Values here are match
	// ids (string) and small ints, so %v is a faithful, collision-safe encoding.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, fields[k]))
	}
	pk := strings.Join(parts, "&")
	if _, ok := f.fields[pk]; !ok {
		f.fields[pk] = fields
		f.order = append(f.order, pk)
	}
	f.ids[pk] = append(f.ids[pk], matchID)
}

// flush issues one chunked Update per distinct payload. Chunk size matches the
// established spreadCourts limit so the id=in.(...) URL stays bounded.
func (f *feedUpdates) flush(s *Service) error {
	const chunk = 80
	for _, pk := range f.order {
		ids := f.ids[pk]
		fields := f.fields[pk]
		for i := 0; i < len(ids); i += chunk {
			end := i + chunk
			if end > len(ids) {
				end = len(ids)
			}
			if _, err := s.sb.Update("matches",
				"id="+store.In(ids[i:end])+"", fields); err != nil {
				return err
			}
		}
	}
	return nil
}

// wipeAllMatches clears an event's schedule. Deleting matches/rounds cascades to
// match_participants via the FK ON DELETE CASCADE, so no explicit child delete.
func (s *Service) wipeAllMatches(eventID string) error {
	defer s.lockDuprEvent(eventID)() // serialize the dupr_submissions delete vs a flush
	// Snapshot any results already pushed to DUPR so we can reverse them after the
	// local wipe (best-effort, async — never block/​slow regeneration).
	var toDelete [][2]string // {matchCode, identifier}
	if subs, err := s.sb.Select("dupr_submissions",
		"event_id=eq."+store.Q(eventID)+"&status=eq.submitted&select=match_id,provider_ref,dupr_gen"); err == nil {
		for _, sub := range subs {
			if code := asStr(sub, "provider_ref"); code != "" {
				// Delete identifier must match the CREATE identifier (generation).
				ident := duprIdentifier(asStr(sub, "match_id"), asInt(sub, "dupr_gen"))
				toDelete = append(toDelete, [2]string{code, ident})
			}
		}
	}
	_ = s.sb.Delete("dupr_submissions", "event_id=eq."+store.Q(eventID))
	if err := s.sb.Delete("matches", "event_id=eq."+store.Q(eventID)); err != nil {
		return err
	}
	if err := s.sb.Delete("rounds", "event_id=eq."+store.Q(eventID)); err != nil {
		return err
	}
	if len(toDelete) > 0 {
		go func() {
			for _, d := range toDelete {
				// The local rows are already gone, so this is the only chance to
				// reverse these on DUPR — retry a transient failure a few times
				// (404 = already gone counts as success in DeleteMatch).
				var e error
				for attempt := 0; attempt < 3; attempt++ {
					if e = s.Dupr.DeleteMatch(d[0], d[1]); e == nil {
						break
					}
					time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
				}
				if e != nil {
					log.Printf("dupr: delete match %s on wipe FAILED after retries: %v (may still be live on DUPR)", d[0], e)
				}
			}
		}()
	}
	return nil
}

func (s *Service) wipeBracketStage(bracketID string) error {
	return s.sb.Delete("matches", "bracket_id=eq."+store.Q(bracketID)+"&stage=eq.bracket")
}

// DeleteRound removes a single round and all of its matches (participants cascade
// with the matches). Refuses with ErrScheduleHasResults if ANY match in the round
// has a recorded result (completed with both scores) so played games can't be
// silently lost — a bye (completed with no score) does NOT block deletion.
func (s *Service) DeleteRound(roundID string) error {
	rows, err := s.sb.Select("matches",
		"round_id=eq."+store.Q(roundID)+"&select=status,team1_score,team2_score")
	if err != nil {
		return err
	}
	for _, m := range rows {
		if asStr(m, "status") == "completed" &&
			m["team1_score"] != nil && m["team2_score"] != nil {
			return ErrScheduleHasResults
		}
	}
	if err := s.sb.Delete("matches", "round_id=eq."+store.Q(roundID)); err != nil {
		return err
	}
	return s.sb.Delete("rounds", "id=eq."+store.Q(roundID))
}

// poolProgress reports how many pool matches a division has (total) and how many
// are not yet completed (open). Replaces a COUNT/SUM aggregation by tallying the
// fetched statuses in Go.
func (s *Service) poolProgress(bracketID string) (total, open int, err error) {
	// SelectAll (paged) — a large single division can exceed PostgREST's 1000-row
	// cap; a plain Select would silently miss the tail and read pools as complete.
	rows, err := s.sb.SelectAll("matches",
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

// droppedDoublesPlayers returns the player IDs that pairsFromRegs leaves out of
// every team — the odd one when a fixed/elimination doubles field has an odd
// count. Empty for singles and rotating round-robins (those seat everyone).
func droppedDoublesPlayers(ev model.Event, regs []reg) []string {
	if ev.Format != "doubles" {
		return nil
	}
	preformed := ev.PartnerMode == "fixed" ||
		ev.TournamentFormat == "single_elim" ||
		ev.TournamentFormat == "double_elim" ||
		ev.TournamentFormat == "compass"
	if !preformed {
		return nil
	}
	inTeam := map[string]bool{}
	for _, p := range pairsFromRegs(regs) {
		for _, id := range p {
			inTeam[id] = true
		}
	}
	var dropped []string
	for _, r := range regs {
		if !inTeam[r.playerID] {
			dropped = append(dropped, r.playerID)
		}
	}
	return dropped
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
