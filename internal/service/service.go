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
	// v2: distance-ranked + reverse-geocoded labels. The version prefix bypasses
	// stale v1 entries ("Pickleball court", no distance) before the 14d TTL.
	return fmt.Sprintf("v2:%.3f:%.3f:%.1f", lat, lng, radiusKm)
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

	ev, err := s.sb.Insert("events", map[string]any{
		"name":                   req.Name,
		"format":                 format,
		"partner_mode":           partner,
		"tournament_format":      tf,
		"scoring_mode":           scoring,
		"num_courts":             courts,
		"points_to_win":          ptw,
		"registration_fee_cents": req.RegistrationFeeCents,
		"currency":               "USD",
		"location":               orNull(req.Location),
		"dupr_sanctioned":        req.DuprSanctioned,
		"admin_passcode":         orNull(req.AdminPasscode),
		"owner_id":               orNull(ownerID),
		"status":                 "open",
	})
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

func (s *Service) ListEvents() ([]model.Event, error) {
	rows, err := s.sb.Select("events", "select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapEvent(r))
	}
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
	return mapEvent(row), nil
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
		})
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

	if _, err := s.GenerateSchedule(eid); err != nil {
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
			err = s.RecordScore(poolIDs[i], 11, loser)
		} else {
			err = s.RecordScore(poolIDs[i], loser, 11)
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
func (s *Service) RegisterPlayer(eventID string, req model.RegisterRequest) (model.Registration, error) {
	if strings.TrimSpace(req.FullName) == "" {
		return model.Registration{}, errors.New("fullName is required")
	}
	pl, err := s.sb.Insert("players", map[string]any{
		"full_name":        req.FullName,
		"phone":            orNull(req.Phone),
		"email":            orNull(req.Email),
		"skill_level":      fOrNull(req.SkillLevel),
		"dupr_id":          orNull(req.DuprID),
		"dupr_rating":      fOrNull(req.DuprRating),
		"dupr_reliability": fOrNull(req.DuprReliability),
	})
	if err != nil {
		return model.Registration{}, err
	}
	if len(pl) == 0 {
		return model.Registration{}, errors.New("player insert returned no row")
	}
	playerID := asStr(pl[0], "id")

	bracketID := req.BracketID
	if bracketID == "" {
		b, err := s.autoAssignBracket(eventID, req.SkillLevel)
		if err != nil {
			return model.Registration{}, err
		}
		bracketID = b
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
			"player:players!player_id(full_name,phone,dupr_id,dupr_rating),"+
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

// ---------------------------------------------------------- scheduling
func (s *Service) GenerateSchedule(eventID string) (int, error) {
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
		if len(regs) < 2 {
			continue
		}
		if ev.TournamentFormat == "single_elim" {
			sides := seedSides(sidesForBracket(ev, regs), skill)
			n, err := s.persistBracket(ev, b.ID, sides)
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
	schedule := engine.GenerateSchedule(ids, format, partner, ev.NumCourts, fixedPairs, 7)

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

func (s *Service) persistBracket(ev model.Event, bracketID string, seededSides [][]string) (int, error) {
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
	_ = topN // medal format is fixed at the top 4
	if len(sides) < 4 {
		return 0, errors.New("need at least 4 teams in this division to build the playoff")
	}
	if err := s.wipeBracketStage(bracketID); err != nil {
		return 0, err
	}
	return s.persistMedalBracket(ev, bracketID, sides[:4])
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
			rate[st.PlayerID] = st.Wins*1000 + st.PointsFor
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
	// Already seeded? (sf1 has participants)
	seeded, err := s.sb.Select("match_participants", "match_id=eq."+store.Q(sf1)+"&select=match_id")
	if err != nil {
		return err
	}
	if len(seeded) > 0 {
		return nil
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

// ----------------------------------------------------------- scoring
func (s *Service) RecordScore(matchID string, t1, t2 int) error {
	if t1 < 0 || t2 < 0 {
		return errors.New("scores must be non-negative")
	}
	if t1 == t2 {
		return errors.New("a pickleball game cannot end in a tie")
	}
	winner := 1
	if t2 > t1 {
		winner = 2
	}
	out, err := s.sb.Update("matches", "id=eq."+store.Q(matchID), map[string]any{
		"team1_score": t1, "team2_score": t2, "winning_team": winner,
		"status": "completed", "completed_at": now(),
	})
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return ErrNotFound
	}
	// The updated row (return=representation) carries the routing columns.
	m := out[0]
	stage := asStr(m, "stage")
	eventID := asStr(m, "event_id")
	if stage == "bracket" {
		loser := 3 - winner
		// Winner advances (e.g. semifinal -> gold game).
		if fm := asStrPtr(m, "feeds_match_id"); fm != nil {
			if err := s.advanceTeam(matchID, winner, *fm, asInt(m, "feeds_slot")); err != nil {
				return err
			}
		}
		// Loser drops down (e.g. semifinal loser -> bronze game).
		if lm := asStrPtr(m, "loser_feeds_match_id"); lm != nil {
			if err := s.advanceTeam(matchID, loser, *lm, asInt(m, "loser_feeds_slot")); err != nil {
				return err
			}
		}
	}
	// A completed pool match may have finished the pools — seed the playoff.
	if stage == "pool" {
		if bc := asStrPtr(m, "bracket_id"); bc != nil {
			if err := s.maybeSeedPlayoff(*bc); err != nil {
				return err
			}
		}
	}
	// DUPR-sanctioned events queue completed results for later import.
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=dupr_sanctioned")
	if err != nil {
		return err
	}
	if ev != nil && asBool(ev, "dupr_sanctioned") {
		if err := s.queueDuprSubmission(matchID, eventID); err != nil {
			return err
		}
	}
	return nil
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
	res, err := s.Pay.Charge(registrationID, fee, currency, provider)
	if err != nil {
		return false, err
	}
	status, regStatus := "failed", "pending"
	var ref, paidAt any
	if res.OK {
		status, regStatus = "paid", "paid"
		ref, paidAt = res.ProviderRef, now()
	}
	if _, err := s.sb.Insert("payments", map[string]any{
		"registration_id": registrationID,
		"provider":        provider,
		"provider_ref":    ref,
		"amount_cents":    fee,
		"currency":        currency,
		"status":          status,
		"paid_at":         paidAt,
	}); err != nil {
		return false, err
	}
	if _, err := s.sb.Update("registrations", "id=eq."+store.Q(registrationID),
		map[string]any{"payment_status": regStatus}); err != nil {
		return false, err
	}
	return res.OK, nil
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

// CheckIn marks a registration checked in. (#1)
func (s *Service) CheckIn(registrationID, method string) error {
	if method == "" {
		method = "manual"
	}
	out, err := s.sb.Update("registrations", "id=eq."+store.Q(registrationID), map[string]any{
		"checked_in":      true,
		"checked_in_at":   now(),
		"check_in_method": method,
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
	return regID, s.CheckIn(regID, "qr")
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

// CheckInByPhone checks a player in by the phone number they registered with.
// Returns the registration id and the player's display name. Matching is on
// digits only and tolerates a country-code prefix (a suffix match), so
// "+1 (555) 100-0000" matches a stored "5551000000".
func (s *Service) CheckInByPhone(eventID, phone string) (string, string, error) {
	want := digitsOnly(phone)
	if len(want) < 7 {
		return "", "", errors.New("enter the phone number you registered with")
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
		have := digitsOnly(asStr(p, "phone"))
		if have == "" {
			continue
		}
		if have == want || strings.HasSuffix(have, want) || strings.HasSuffix(want, have) {
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
	return matchID, matchName, s.CheckIn(matchID, "code")
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
		body := fmt.Sprintf("PlanMyPickle: You are up! Head to %s for round %d.", court, roundNumber)
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
			"id=eq."+store.Q(matchID)+"&select=team1_score,team2_score,winning_team")
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
		res, err := s.Dupr.SubmitMatch(gateway.DuprPayload{
			EventID: eventID, DuprEventID: duprEventID,
			Team1DuprIDs: t1, Team2DuprIDs: t2,
			Team1Score: *t1s, Team2Score: *t2s,
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
	return s.insertSide(feedsMatchID, feedsSlot, side)
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
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if byWins {
			if a.Wins != b.Wins {
				return a.Wins > b.Wins
			}
			if a.Losses != b.Losses {
				return a.Losses < b.Losses
			}
			if a.PointsFor != b.PointsFor {
				return a.PointsFor > b.PointsFor
			}
			return a.PointDiff > b.PointDiff
		}
		if a.PointsFor != b.PointsFor {
			return a.PointsFor > b.PointsFor
		}
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		return a.PointDiff > b.PointDiff
	})
	return out, nil
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
