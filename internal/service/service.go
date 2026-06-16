// Package service holds PlanMyPickle's business operations: event setup,
// registration, schedule/bracket generation, scoring, and standings.
// Ported from the Flutter app's repository; uses the verified engine package.
package service

import (
	"database/sql"
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
	db     *store.DB
	sb     *store.Client // Supabase REST client (data-layer migration in progress)
	Pay    gateway.PaymentGateway
	Sms    gateway.SmsGateway
	Dupr   gateway.DuprGateway
	Courts courts.Finder
}

func New(db *store.DB) *Service {
	return &Service{
		db:     db,
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
	return fmt.Sprintf("%.3f:%.3f:%.1f", lat, lng, radiusKm)
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
func (s *Service) CreateEvent(req model.CreateEventRequest) (string, error) {
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

	id := newID()
	_, err := s.db.Exec(`INSERT INTO events
		(id,name,format,partner_mode,tournament_format,scoring_mode,num_courts,points_to_win,
		 registration_fee_cents,currency,location,dupr_sanctioned,admin_passcode,status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?, 'open')`,
		id, req.Name, format, partner, tf, scoring, courts, ptw,
		req.RegistrationFeeCents, "USD", nullStr(req.Location), b2i(req.DuprSanctioned), nullStr(req.AdminPasscode))
	if err != nil {
		return "", err
	}

	divs := req.Brackets
	if len(divs) == 0 {
		divs = []model.BracketInput{{Name: "Open"}}
	}
	for i, d := range divs {
		if _, err := s.db.Exec(
			`INSERT INTO brackets (id,event_id,name,min_rating,max_rating,min_age,max_age,sort_order) VALUES (?,?,?,?,?,?,?,?)`,
			newID(), id, d.Name, nullF(d.MinRating), nullF(d.MaxRating),
			nullI(d.MinAge), nullI(d.MaxAge), i); err != nil {
			return "", err
		}
	}
	for i := 1; i <= courts; i++ {
		if _, err := s.db.Exec(
			`INSERT INTO courts (id,event_id,label,court_number) VALUES (?,?,?,?)`,
			newID(), id, "Court "+strconv.Itoa(i), i); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (s *Service) ListEvents() ([]model.Event, error) {
	rows, err := s.db.Query(`SELECT id,name,format,partner_mode,tournament_format,scoring_mode,
		num_courts,points_to_win,registration_fee_cents,currency,location,dupr_sanctioned,status
		FROM events ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) GetEvent(id string) (model.Event, error) {
	row := s.db.QueryRow(`SELECT id,name,format,partner_mode,tournament_format,scoring_mode,
		num_courts,points_to_win,registration_fee_cents,currency,location,dupr_sanctioned,status
		FROM events WHERE id=?`, id)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// DeleteEvent removes an event and (via ON DELETE CASCADE) all its brackets,
// courts, registrations, payments, rounds, matches, match_participants,
// notifications and DUPR submissions. Players are global and are not deleted.
func (s *Service) DeleteEvent(id string) error {
	res, err := s.db.Exec(`DELETE FROM events WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func ratingPtr(v float64) *float64 { return &v }

// SeedDemo creates a fully-populated round-robin demo tournament so the app has
// data to explore (dev convenience). ~60% of pool matches are scored. Returns
// the new event id.
func (s *Service) SeedDemo() (string, error) {
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
	}, 0.6)
}

// SeedPlayoffDemo creates a pools->playoff demo at the very first step: 16 players
// registered across two divisions, with NO schedule generated and NO playoff
// bracket yet. The coordinator drives every step from the UI — Generate schedule,
// start matches, score the pools, then Build playoff. Returns the new event id.
func (s *Service) SeedPlayoffDemo() (string, error) {
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
	})
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
func (s *Service) seedTournament(req model.CreateEventRequest, poolCompletion float64) (string, error) {
	eid, err := s.CreateEvent(req)
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
func (s *Service) reconcileRoundStatuses(eventID string) error {
	if _, err := s.db.Exec(`
		UPDATE rounds SET status='completed', updated_at=?
		WHERE event_id=? AND id IN (
			SELECT r.id FROM rounds r JOIN matches m ON m.round_id=r.id
			GROUP BY r.id
			HAVING SUM(CASE WHEN m.status='completed' THEN 0 ELSE 1 END)=0
		)`, now(), eventID); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		UPDATE rounds SET status='active', updated_at=?
		WHERE event_id=? AND id IN (
			SELECT r.id FROM rounds r JOIN matches m ON m.round_id=r.id
			GROUP BY r.id
			HAVING SUM(CASE WHEN m.status='completed' THEN 1 ELSE 0 END) > 0
			   AND SUM(CASE WHEN m.status='completed' THEN 0 ELSE 1 END) > 0
		)`, now(), eventID)
	return err
}

// listPoolMatchIDs returns the ids of every pool-stage match for an event, in a
// stable insertion order.
func (s *Service) listPoolMatchIDs(eventID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM matches WHERE event_id=? AND stage='pool' ORDER BY rowid`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) GetBrackets(eventID string) ([]model.Bracket, error) {
	rows, err := s.db.Query(
		`SELECT id,event_id,name,min_rating,max_rating,min_age,max_age,sort_order FROM brackets WHERE event_id=? ORDER BY sort_order`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Bracket
	for rows.Next() {
		var b model.Bracket
		var mn, mx sql.NullFloat64
		var mnA, mxA sql.NullInt64
		if err := rows.Scan(&b.ID, &b.EventID, &b.Name, &mn, &mx, &mnA, &mxA, &b.SortOrder); err != nil {
			return nil, err
		}
		b.MinRating = nf(mn)
		b.MaxRating = nf(mx)
		b.MinAge = ni(mnA)
		b.MaxAge = ni(mxA)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------ registration
func (s *Service) RegisterPlayer(eventID string, req model.RegisterRequest) (model.Registration, error) {
	if strings.TrimSpace(req.FullName) == "" {
		return model.Registration{}, errors.New("fullName is required")
	}
	playerID := newID()
	if _, err := s.db.Exec(
		`INSERT INTO players (id,full_name,phone,email,skill_level,dupr_id,dupr_rating,dupr_reliability)
		 VALUES (?,?,?,?,?,?,?,?)`,
		playerID, req.FullName, nullStr(req.Phone), nullStr(req.Email), nullF(req.SkillLevel),
		nullStr(req.DuprID), nullF(req.DuprRating), nullF(req.DuprReliability)); err != nil {
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
	regID := newID()
	token := newID()
	if _, err := s.db.Exec(
		`INSERT INTO registrations (id,event_id,player_id,partner_id,bracket_id,check_in_token) VALUES (?,?,?,?,?,?)`,
		regID, eventID, playerID, nullStr(req.PartnerID), nullStr(bracketID), token); err != nil {
		return model.Registration{}, err
	}
	return model.Registration{
		ID: regID, EventID: eventID, PlayerID: playerID, FullName: req.FullName,
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
	rows, err := s.db.Query(`SELECT r.id,r.event_id,r.player_id,p.full_name,r.bracket_id,
		r.payment_status,r.checked_in,r.check_in_token,p.phone,p.dupr_id,p.dupr_rating,b.min_rating,b.max_rating
		FROM registrations r
		JOIN players p ON p.id=r.player_id
		LEFT JOIN brackets b ON b.id=r.bracket_id
		WHERE r.event_id=?`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Registration
	for rows.Next() {
		var r model.Registration
		var bid, tok, phone, duprID sql.NullString
		var checked int
		var rating, bMin, bMax sql.NullFloat64
		if err := rows.Scan(&r.ID, &r.EventID, &r.PlayerID, &r.FullName, &bid,
			&r.PaymentStatus, &checked, &tok, &phone, &duprID, &rating, &bMin, &bMax); err != nil {
			return nil, err
		}
		r.BracketID = ns(bid)
		r.CheckInToken = ns(tok)
		r.Phone = phone.String
		r.DuprID = ns(duprID)
		r.CheckedIn = checked == 1
		r.DuprRating = nf(rating)
		// Flag (don't block) a player whose DUPR rating is outside their division.
		if rating.Valid {
			if (bMin.Valid && rating.Float64 < bMin.Float64) ||
				(bMax.Valid && rating.Float64 > bMax.Float64) {
				r.OutsideRating = true
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BusyCourts returns the distinct court numbers that currently have an
// in-progress match in this event. The schedule UI uses this to dim other
// scheduled matches assigned to a court that's already in play.
func (s *Service) BusyCourts(eventID string) ([]int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT c.court_number
		FROM matches m JOIN courts c ON c.id=m.court_id
		WHERE m.event_id=? AND m.status='in_progress' AND c.court_number IS NOT NULL
		ORDER BY c.court_number`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
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
	_, err = s.db.Exec(`UPDATE events SET status='in_progress', updated_at=? WHERE id=?`, now(), eventID)
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

	rows, err := s.db.Query(`
		SELECT m.id, r.round_number
		FROM matches m JOIN rounds r ON r.id=m.round_id
		WHERE m.event_id=? AND m.stage='pool'
		ORDER BY r.round_number, m.bracket_id, m.rowid`, eventID)
	if err != nil {
		return err
	}
	type mr struct {
		id    string
		round int
	}
	var list []mr
	for rows.Next() {
		var x mr
		if err := rows.Scan(&x.id, &x.round); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	prevRound, idx := -1, 0
	for _, m := range list {
		if m.round != prevRound {
			prevRound = m.round
			idx = 0
		}
		cid := courtByNum[courtNums[idx%len(courtNums)]]
		idx++
		if _, err := s.db.Exec(
			`UPDATE matches SET court_id=?, updated_at=? WHERE id=?`, cid, now(), m.id); err != nil {
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
		roundID := newID()
		if _, err := s.db.Exec(
			`INSERT INTO rounds (id,event_id,bracket_id,round_number) VALUES (?,?,?,?)`,
			roundID, ev.ID, bracketID, round.RoundNumber); err != nil {
			return 0, err
		}
		for _, m := range round.Matches {
			matchID := newID()
			if _, err := s.db.Exec(
				`INSERT INTO matches (id,event_id,bracket_id,round_id,court_id,stage) VALUES (?,?,?,?,?, 'pool')`,
				matchID, ev.ID, bracketID, roundID, nullStr(courtByNum[m.CourtNumber])); err != nil {
				return 0, err
			}
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
		mid := newID()
		idByKey[key(m.Round, m.Slot)] = mid
		isBye := m.ResolvedWinner != nil
		var winning sql.NullInt64
		status := "scheduled"
		var completed sql.NullString
		if isBye {
			status = "completed"
			completed = sql.NullString{String: now(), Valid: true}
			if !engine.IsBye(m.Side1) && m.Side1 != nil {
				winning = sql.NullInt64{Int64: 1, Valid: true}
			} else {
				winning = sql.NullInt64{Int64: 2, Valid: true}
			}
		}
		if _, err := s.db.Exec(
			`INSERT INTO matches (id,event_id,bracket_id,stage,bracket_round,bracket_slot,status,winning_team,completed_at)
			 VALUES (?,?,?, 'bracket', ?,?,?,?,?)`,
			mid, ev.ID, bracketID, m.Round, m.Slot, status, winning, completed); err != nil {
			return 0, err
		}
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
		if _, err := s.db.Exec(
			`UPDATE matches SET feeds_match_id=?, feeds_slot=?, updated_at=? WHERE id=?`,
			feedID, m.FeedsTeam, now(), mid); err != nil {
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
	goldID, bronzeID, sf1ID, sf2ID := newID(), newID(), newID(), newID()

	// Round 2 medal games (TBD until the semifinals resolve).
	for _, g := range []struct {
		id   string
		slot int
	}{{goldID, 0}, {bronzeID, 1}} {
		if _, err := s.db.Exec(
			`INSERT INTO matches (id,event_id,bracket_id,stage,bracket_round,bracket_slot,status)
			 VALUES (?,?,?, 'bracket', 2, ?, 'scheduled')`,
			g.id, ev.ID, bracketID, g.slot); err != nil {
			return 0, err
		}
	}

	// Round 1 semifinals (1v4, 2v3). Winner -> gold, loser -> bronze, each into
	// team-slot (sf slot + 1) of the round-2 game.
	semis := []struct {
		id   string
		slot int
		a, b []string
	}{
		{sf1ID, 0, s1, s4}, // #1 vs #4 (nil = TBD skeleton)
		{sf2ID, 1, s2, s3}, // #2 vs #3
	}
	for _, sf := range semis {
		feedSlot := sf.slot + 1
		if _, err := s.db.Exec(
			`INSERT INTO matches
			 (id,event_id,bracket_id,stage,bracket_round,bracket_slot,status,
			  feeds_match_id,feeds_slot,loser_feeds_match_id,loser_feeds_slot)
			 VALUES (?,?,?, 'bracket', 1, ?, 'scheduled', ?,?,?,?)`,
			sf.id, ev.ID, bracketID, sf.slot, goldID, feedSlot, bronzeID, feedSlot); err != nil {
			return 0, err
		}
		if err := s.insertSide(sf.id, 1, sf.a); err != nil {
			return 0, err
		}
		if err := s.insertSide(sf.id, 2, sf.b); err != nil {
			return 0, err
		}
	}
	return 4, nil
}

func (s *Service) GeneratePlayoffBracket(bracketID string, topN int) (int, error) {
	var eventID string
	if err := s.db.QueryRow(`SELECT event_id FROM brackets WHERE id=?`, bracketID).Scan(&eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}

	// A single-elimination playoff is seeded from pool standings, so the pools
	// must be fully played first. Otherwise "Build playoff" would seed a
	// meaningless bracket off all-zero standings.
	var poolTotal, poolOpen int
	if err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN status!='completed' THEN 1 ELSE 0 END), 0)
		FROM matches WHERE bracket_id=? AND stage='pool'`, bracketID).Scan(&poolTotal, &poolOpen); err != nil {
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
	var eventID string
	if err := s.db.QueryRow(`SELECT event_id FROM brackets WHERE id=?`, bracketID).Scan(&eventID); err != nil {
		return err
	}
	var total, open int
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status!='completed' THEN 1 ELSE 0 END),0)
		FROM matches WHERE bracket_id=? AND stage='pool'`, bracketID).Scan(&total, &open); err != nil {
		return err
	}
	if total == 0 || open > 0 {
		return nil
	}
	// Locate the skeleton semifinals (round 1).
	rows, err := s.db.Query(`SELECT id, bracket_slot FROM matches
		WHERE bracket_id=? AND stage='bracket' AND bracket_round=1 ORDER BY bracket_slot`, bracketID)
	if err != nil {
		return err
	}
	semiBySlot := map[int]string{}
	for rows.Next() {
		var id string
		var slot int
		if err := rows.Scan(&id, &slot); err != nil {
			rows.Close()
			return err
		}
		semiBySlot[slot] = id
	}
	rows.Close()
	sf1, ok1 := semiBySlot[0]
	sf2, ok2 := semiBySlot[1]
	if !ok1 || !ok2 {
		return nil // no skeleton (e.g. fewer than 4 teams)
	}
	// Already seeded? (sf1 has participants)
	var seeded int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM match_participants WHERE match_id=?`, sf1).Scan(&seeded); err != nil {
		return err
	}
	if seeded > 0 {
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
	res, err := s.db.Exec(
		`UPDATE matches SET team1_score=?, team2_score=?, winning_team=?, status='completed', completed_at=?, updated_at=? WHERE id=?`,
		t1, t2, winner, now(), now(), matchID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	var stage, eventID string
	var feedsMatch, loserFeedsMatch, bracketCol sql.NullString
	var feedsSlot, loserFeedsSlot sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT stage, event_id, bracket_id, feeds_match_id, feeds_slot, loser_feeds_match_id, loser_feeds_slot
		 FROM matches WHERE id=?`, matchID).
		Scan(&stage, &eventID, &bracketCol, &feedsMatch, &feedsSlot, &loserFeedsMatch, &loserFeedsSlot); err != nil {
		return err
	}
	if stage == "bracket" {
		loser := 3 - winner
		// Winner advances (e.g. semifinal -> gold game).
		if feedsMatch.Valid {
			if err := s.advanceTeam(matchID, winner, feedsMatch.String, int(feedsSlot.Int64)); err != nil {
				return err
			}
		}
		// Loser drops down (e.g. semifinal loser -> bronze game).
		if loserFeedsMatch.Valid {
			if err := s.advanceTeam(matchID, loser, loserFeedsMatch.String, int(loserFeedsSlot.Int64)); err != nil {
				return err
			}
		}
	}
	// A completed pool match may have finished the pools — seed the playoff.
	if stage == "pool" && bracketCol.Valid {
		if err := s.maybeSeedPlayoff(bracketCol.String); err != nil {
			return err
		}
	}
	// DUPR-sanctioned events queue completed results for later import.
	var sanctioned int
	if err := s.db.QueryRow(`SELECT dupr_sanctioned FROM events WHERE id=?`, eventID).Scan(&sanctioned); err != nil {
		return err
	}
	if sanctioned == 1 {
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
	var pass sql.NullString
	err := s.db.QueryRow(`SELECT admin_passcode FROM events WHERE id=?`, eventID).Scan(&pass)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if !pass.Valid || pass.String == "" {
		return true, nil
	}
	return pass.String == strings.TrimSpace(code), nil
}

// CollectPayment charges the registration fee via the payment gateway. (#4)
func (s *Service) CollectPayment(registrationID, provider string) (bool, error) {
	if provider == "" {
		provider = "manual"
	}
	var eventID string
	if err := s.db.QueryRow(`SELECT event_id FROM registrations WHERE id=?`, registrationID).Scan(&eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	var fee int
	var currency string
	if err := s.db.QueryRow(`SELECT registration_fee_cents, currency FROM events WHERE id=?`, eventID).Scan(&fee, &currency); err != nil {
		return false, err
	}
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
	if _, err := s.db.Exec(
		`INSERT INTO payments (id,registration_id,provider,provider_ref,amount_cents,currency,status,paid_at) VALUES (?,?,?,?,?,?,?,?)`,
		newID(), registrationID, provider, ref, fee, currency, status, paidAt); err != nil {
		return false, err
	}
	if _, err := s.db.Exec(`UPDATE registrations SET payment_status=?, updated_at=? WHERE id=?`, regStatus, now(), registrationID); err != nil {
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
	var rid string
	err := s.db.QueryRow(`SELECT id FROM registrations WHERE id=?`, registrationID).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ShirtOrder{}, ErrNotFound
	}
	if err != nil {
		return model.ShirtOrder{}, err
	}

	var id string
	err = s.db.QueryRow(`SELECT id FROM shirt_orders WHERE registration_id=?`, registrationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = newID()
		_, err = s.db.Exec(
			`INSERT INTO shirt_orders (id,registration_id,size,name_on_shirt,number,color) VALUES (?,?,?,?,?,?)`,
			id, registrationID, req.Size, nullStr(req.NameOnShirt), nullStr(req.Number), nullStr(req.Color))
	} else if err == nil {
		_, err = s.db.Exec(
			`UPDATE shirt_orders SET size=?, name_on_shirt=?, number=?, color=?, updated_at=? WHERE id=?`,
			req.Size, nullStr(req.NameOnShirt), nullStr(req.Number), nullStr(req.Color), now(), id)
	}
	if err != nil {
		return model.ShirtOrder{}, err
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
	var eid string
	err := s.db.QueryRow(`SELECT id FROM events WHERE id=?`, eventID).Scan(&eid)
	if errors.Is(err, sql.ErrNoRows) {
		return model.FinanceEntry{}, ErrNotFound
	}
	if err != nil {
		return model.FinanceEntry{}, err
	}
	id := newID()
	ts := now()
	note := strings.TrimSpace(req.Note)
	if _, err := s.db.Exec(
		`INSERT INTO finance_entries (id,event_id,kind,category,amount_cents,note,created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		id, eventID, req.Kind, category, req.AmountCents, note, ts); err != nil {
		return model.FinanceEntry{}, err
	}
	return model.FinanceEntry{
		ID: id, EventID: eventID, Kind: req.Kind, Category: category,
		AmountCents: req.AmountCents, Note: note, CreatedAt: ts,
	}, nil
}

// FinanceEntries lists an event's ledger lines, newest first.
func (s *Service) FinanceEntries(eventID string) ([]model.FinanceEntry, error) {
	rows, err := s.db.Query(
		`SELECT id,event_id,kind,category,amount_cents,COALESCE(note,''),created_at
		 FROM finance_entries WHERE event_id=? ORDER BY created_at DESC, id DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.FinanceEntry{}
	for rows.Next() {
		var e model.FinanceEntry
		if err := rows.Scan(&e.ID, &e.EventID, &e.Kind, &e.Category,
			&e.AmountCents, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteFinanceEntry removes a ledger line.
func (s *Service) DeleteFinanceEntry(id string) error {
	res, err := s.db.Exec(`DELETE FROM finance_entries WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM checklist_items WHERE event_id=?`, eventID).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		var eid string
		err := s.db.QueryRow(`SELECT id FROM events WHERE id=?`, eventID).Scan(&eid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		for i, label := range defaultChecklist {
			if _, err := s.db.Exec(
				`INSERT INTO checklist_items (id,event_id,label,checked,sort_order) VALUES (?,?,?,0,?)`,
				newID(), eventID, label, i); err != nil {
				return nil, err
			}
		}
	}
	return s.listChecklist(eventID)
}

func (s *Service) listChecklist(eventID string) ([]model.ChecklistItem, error) {
	rows, err := s.db.Query(
		`SELECT id,event_id,label,checked,sort_order FROM checklist_items
		 WHERE event_id=? ORDER BY sort_order, rowid`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ChecklistItem{}
	for rows.Next() {
		var c model.ChecklistItem
		var checked int
		if err := rows.Scan(&c.ID, &c.EventID, &c.Label, &checked, &c.SortOrder); err != nil {
			return nil, err
		}
		c.Checked = checked == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddChecklistItem appends a custom item to the end of the list.
func (s *Service) AddChecklistItem(eventID, label string) (model.ChecklistItem, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return model.ChecklistItem{}, errors.New("label is required")
	}
	var eid string
	err := s.db.QueryRow(`SELECT id FROM events WHERE id=?`, eventID).Scan(&eid)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ChecklistItem{}, ErrNotFound
	}
	if err != nil {
		return model.ChecklistItem{}, err
	}
	var maxOrder sql.NullInt64
	_ = s.db.QueryRow(`SELECT MAX(sort_order) FROM checklist_items WHERE event_id=?`, eventID).Scan(&maxOrder)
	order := int(maxOrder.Int64) + 1
	id := newID()
	if _, err := s.db.Exec(
		`INSERT INTO checklist_items (id,event_id,label,checked,sort_order) VALUES (?,?,?,0,?)`,
		id, eventID, label, order); err != nil {
		return model.ChecklistItem{}, err
	}
	return model.ChecklistItem{ID: id, EventID: eventID, Label: label, SortOrder: order}, nil
}

// SetChecklistChecked sets an item's checked state.
func (s *Service) SetChecklistChecked(id string, checked bool) error {
	v := 0
	if checked {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE checklist_items SET checked=? WHERE id=?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteChecklistItem removes an item.
func (s *Service) DeleteChecklistItem(id string) error {
	res, err := s.db.Exec(`DELETE FROM checklist_items WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CheckIn marks a registration checked in. (#1)
func (s *Service) CheckIn(registrationID, method string) error {
	if method == "" {
		method = "manual"
	}
	res, err := s.db.Exec(
		`UPDATE registrations SET checked_in=1, checked_in_at=?, check_in_method=?, updated_at=? WHERE id=?`,
		now(), method, now(), registrationID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CheckInByToken redeems a player's QR/check-in token. Returns the registration id.
func (s *Service) CheckInByToken(eventID, token string) (string, error) {
	var regID string
	err := s.db.QueryRow(`SELECT id FROM registrations WHERE event_id=? AND check_in_token=?`, eventID, token).Scan(&regID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
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
	rows, err := s.db.Query(`SELECT r.id, p.full_name, p.phone
		FROM registrations r JOIN players p ON p.id=r.player_id
		WHERE r.event_id=?`, eventID)
	if err != nil {
		return "", "", err
	}
	var matchID, matchName string
	found := false
	for rows.Next() {
		var id, name, ph string
		if err := rows.Scan(&id, &name, &ph); err != nil {
			rows.Close()
			return "", "", err
		}
		have := digitsOnly(ph)
		if have == "" {
			continue
		}
		if have == want || strings.HasSuffix(have, want) || strings.HasSuffix(want, have) {
			matchID, matchName = id, name
			found = true
			break
		}
	}
	// Close the read cursor BEFORE the CheckIn write — holding an open cursor
	// during an UPDATE deadlocks the single file-backed SQLite connection.
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
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
	var roundNumber int
	var eventID string
	err := s.db.QueryRow(`SELECT round_number, event_id FROM rounds WHERE id=?`, roundID).Scan(&roundNumber, &eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(`UPDATE rounds SET status='active', started_at=?, updated_at=? WHERE id=?`, now(), now(), roundID); err != nil {
		return 0, err
	}
	// Mark every not-yet-played match in the round as in progress, so starting a
	// whole round behaves like starting each match individually.
	if _, err := s.db.Exec(
		`UPDATE matches SET status='in_progress', updated_at=? WHERE round_id=? AND status='scheduled'`,
		now(), roundID); err != nil {
		return 0, err
	}

	type mc struct{ id, court, eventID string }
	rows, err := s.db.Query(`SELECT m.id, COALESCE(c.label,'your court'), m.event_id
		FROM matches m LEFT JOIN courts c ON c.id=m.court_id WHERE m.round_id=?`, roundID)
	if err != nil {
		return 0, err
	}
	var matches []mc
	for rows.Next() {
		var x mc
		if err := rows.Scan(&x.id, &x.court, &x.eventID); err != nil {
			rows.Close()
			return 0, err
		}
		matches = append(matches, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, m := range matches {
		n, err := s.notifyMatchStart(m.id, m.eventID, m.court, roundNumber)
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
	var court, eventID string
	var roundID sql.NullString
	var roundNumber sql.NullInt64
	err := s.db.QueryRow(`SELECT COALESCE(c.label,'your court'), m.event_id, m.round_id, r.round_number
		FROM matches m
		LEFT JOIN courts c ON c.id=m.court_id
		LEFT JOIN rounds r ON r.id=m.round_id
		WHERE m.id=?`, matchID).Scan(&court, &eventID, &roundID, &roundNumber)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(
		`UPDATE matches SET status='in_progress', updated_at=? WHERE id=? AND status='scheduled'`,
		now(), matchID); err != nil {
		return 0, err
	}
	// Reflect that play has begun on the parent pool round (if any).
	if roundID.Valid {
		if _, err := s.db.Exec(
			`UPDATE rounds SET status='active', started_at=COALESCE(started_at,?), updated_at=? WHERE id=? AND status='pending'`,
			now(), now(), roundID.String); err != nil {
			return 0, err
		}
	}
	rn := 0
	if roundNumber.Valid {
		rn = int(roundNumber.Int64)
	}
	return s.notifyMatchStart(matchID, eventID, court, rn)
}

// notifyMatchStart texts every player in a match that they're up, recording each
// notification. Returns the count successfully sent.
func (s *Service) notifyMatchStart(matchID, eventID, court string, roundNumber int) (int, error) {
	prows, err := s.db.Query(`SELECT p.phone FROM match_participants mp JOIN players p ON p.id=mp.player_id WHERE mp.match_id=?`, matchID)
	if err != nil {
		return 0, err
	}
	var phones []string
	for prows.Next() {
		var ph sql.NullString
		if err := prows.Scan(&ph); err != nil {
			prows.Close()
			return 0, err
		}
		if ph.Valid && ph.String != "" {
			phones = append(phones, ph.String)
		}
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, phone := range phones {
		body := fmt.Sprintf("PlanMyPickle: You are up! Head to %s for round %d.", court, roundNumber)
		notifID := newID()
		if _, err := s.db.Exec(`INSERT INTO notifications (id,event_id,match_id,type,to_address,body) VALUES (?,?,?,?,?,?)`,
			notifID, eventID, matchID, "game_starting", phone, body); err != nil {
			return 0, err
		}
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
		if _, err := s.db.Exec(`UPDATE notifications SET status=?, provider_ref=?, sent_at=?, updated_at=? WHERE id=?`,
			st, ref, sentAt, now(), notifID); err != nil {
			return 0, err
		}
	}
	return sent, nil
}

func (s *Service) queueDuprSubmission(matchID, eventID string) error {
	var existing string
	err := s.db.QueryRow(`SELECT id FROM dupr_submissions WHERE match_id=?`, matchID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.Exec(`INSERT INTO dupr_submissions (id,event_id,match_id) VALUES (?,?,?)`, newID(), eventID, matchID)
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE dupr_submissions SET status='pending', error=NULL, updated_at=? WHERE id=?`, now(), existing)
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
	var duprEventID string
	_ = s.db.QueryRow(`SELECT COALESCE(dupr_event_id,'') FROM events WHERE id=?`, eventID).Scan(&duprEventID)

	rows, err := s.db.Query(`SELECT id, match_id FROM dupr_submissions WHERE event_id=? AND status='pending'`, eventID)
	if err != nil {
		return DuprImportSummary{}, err
	}
	type pend struct{ id, matchID string }
	var pendings []pend
	for rows.Next() {
		var p pend
		if err := rows.Scan(&p.id, &p.matchID); err != nil {
			rows.Close()
			return DuprImportSummary{}, err
		}
		pendings = append(pendings, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DuprImportSummary{}, err
	}

	var sum DuprImportSummary
	for _, p := range pendings {
		var t1s, t2s, wt sql.NullInt64
		if err := s.db.QueryRow(`SELECT team1_score, team2_score, winning_team FROM matches WHERE id=?`, p.matchID).
			Scan(&t1s, &t2s, &wt); err != nil {
			return sum, err
		}
		if !wt.Valid || !t1s.Valid || !t2s.Valid {
			s.markSubmission(p.id, "failed", "", "match not completed")
			sum.Failed++
			continue
		}
		prows, err := s.db.Query(
			`SELECT mp.team, COALESCE(p.dupr_id,''), p.full_name FROM match_participants mp JOIN players p ON p.id=mp.player_id WHERE mp.match_id=?`, p.matchID)
		if err != nil {
			return sum, err
		}
		var t1, t2 []string
		missing := ""
		for prows.Next() {
			var team int
			var did, name string
			if err := prows.Scan(&team, &did, &name); err != nil {
				prows.Close()
				return sum, err
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
		prows.Close()
		if err := prows.Err(); err != nil {
			return sum, err
		}
		if missing != "" {
			s.markSubmission(p.id, "failed", "", "Missing DUPR id for "+missing)
			sum.Failed++
			continue
		}
		res, err := s.Dupr.SubmitMatch(gateway.DuprPayload{
			EventID: eventID, DuprEventID: duprEventID,
			Team1DuprIDs: t1, Team2DuprIDs: t2,
			Team1Score: int(t1s.Int64), Team2Score: int(t2s.Int64),
		})
		if err != nil {
			return sum, err
		}
		if res.OK {
			s.markSubmission(p.id, "submitted", res.DuprMatchID, "")
			sum.Submitted++
		} else {
			s.markSubmission(p.id, "failed", "", res.Error)
			sum.Failed++
		}
	}
	return sum, nil
}

func (s *Service) markSubmission(id, status, ref, errMsg string) {
	var submittedAt, refv, errv any
	if status == "submitted" {
		submittedAt = now()
	}
	if ref != "" {
		refv = ref
	}
	if errMsg != "" {
		errv = errMsg
	}
	_, _ = s.db.Exec(
		`UPDATE dupr_submissions SET status=?, provider_ref=?, error=?, submitted_at=?, updated_at=? WHERE id=?`,
		status, refv, errv, submittedAt, now(), id)
}

// advanceTeam copies one side (by team number) of a finished match into its
// next match's slot — used to advance a winner (e.g. to the gold game) or drop
// a loser (e.g. to the bronze game). It first clears any players previously
// advanced into that exact (feed match, slot) so a re-scored match that flips
// the result does not leave both teams' players on one side.
func (s *Service) advanceTeam(matchID string, team int, feedsMatchID string, feedsSlot int) error {
	if _, err := s.db.Exec(
		`DELETE FROM match_participants WHERE match_id=? AND team=?`, feedsMatchID, feedsSlot); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT player_id FROM match_participants WHERE match_id=? AND team=?`, matchID, int(team))
	if err != nil {
		return err
	}
	var winners []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return err
		}
		winners = append(winners, pid)
	}
	// Close the read cursor BEFORE the INSERT writes — holding it open during a
	// write deadlocks the single file-backed SQLite connection.
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, pid := range winners {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO match_participants (id,match_id,player_id,team) VALUES (?,?,?,?)`,
			newID(), feedsMatchID, pid, feedsSlot); err != nil {
			return err
		}
	}
	return nil
}

// ----------------------------------------------------------- standings
func (s *Service) Standings(eventID, bracketID string, byWins bool) ([]model.Standing, error) {
	order := "points_for DESC, wins DESC, point_diff DESC"
	if byWins {
		order = "wins DESC, losses ASC, points_for DESC, point_diff DESC"
	}
	bracketClause := ""
	args := []any{eventID}
	if bracketID != "" {
		bracketClause = "AND m.bracket_id = ?"
		args = append(args, bracketID)
	}
	q := fmt.Sprintf(`
SELECT mp.player_id, pl.full_name, COUNT(*) AS games_played,
  SUM(CASE WHEN m.winning_team=mp.team THEN 1 ELSE 0 END) AS wins,
  SUM(CASE WHEN m.winning_team<>mp.team THEN 1 ELSE 0 END) AS losses,
  SUM(CASE mp.team WHEN 1 THEN m.team1_score ELSE m.team2_score END) AS points_for,
  SUM(CASE mp.team WHEN 1 THEN m.team2_score ELSE m.team1_score END) AS points_against,
  SUM(CASE mp.team WHEN 1 THEN m.team1_score ELSE m.team2_score END)
    - SUM(CASE mp.team WHEN 1 THEN m.team2_score ELSE m.team1_score END) AS point_diff
FROM match_participants mp
JOIN matches m ON m.id=mp.match_id
JOIN players pl ON pl.id=mp.player_id
WHERE m.event_id=? AND m.stage='pool' AND m.status='completed' AND m.winning_team IS NOT NULL %s
GROUP BY mp.player_id
ORDER BY %s`, bracketClause, order)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Standing
	for rows.Next() {
		var st model.Standing
		if err := rows.Scan(&st.PlayerID, &st.FullName, &st.GamesPlayed, &st.Wins, &st.Losses,
			&st.PointsFor, &st.PointsAgainst, &st.PointDiff); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ------------------------------------------------------ bracket dashboard
func (s *Service) BracketMatches(bracketID string) ([]model.Match, error) {
	rows, err := s.db.Query(`SELECT id,bracket_id,stage,bracket_round,bracket_slot,
		team1_score,team2_score,winning_team,status
		FROM matches WHERE bracket_id=? AND stage='bracket' ORDER BY bracket_round, bracket_slot`, bracketID)
	if err != nil {
		return nil, err
	}
	var out []model.Match
	for rows.Next() {
		m, err := scanMatch(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close() // close BEFORE the nested matchSides queries (single sqlite conn)
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		sides, err := s.matchSides(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Sides = sides
	}
	return out, nil
}

func (s *Service) Rounds(eventID string) ([]model.RoundView, error) {
	rows, err := s.db.Query(
		`SELECT id, bracket_id, round_number, status FROM rounds WHERE event_id=? ORDER BY bracket_id, round_number`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RoundView
	for rows.Next() {
		var rv model.RoundView
		var bid sql.NullString
		if err := rows.Scan(&rv.ID, &bid, &rv.RoundNumber, &rv.Status); err != nil {
			return nil, err
		}
		rv.BracketID = ns(bid)
		out = append(out, rv)
	}
	return out, rows.Err()
}

// MatchesForRound returns a pool round's matches with resolved sides + court #.
func (s *Service) MatchesForRound(roundID string) ([]model.Match, error) {
	rows, err := s.db.Query(`SELECT m.id,m.bracket_id,m.stage,m.bracket_round,m.bracket_slot,
		c.court_number,m.team1_score,m.team2_score,m.winning_team,m.status
		FROM matches m LEFT JOIN courts c ON c.id=m.court_id
		WHERE m.round_id=? ORDER BY c.court_number`, roundID)
	if err != nil {
		return nil, err
	}
	var out []model.Match
	for rows.Next() {
		var m model.Match
		var bid sql.NullString
		var br, bs, court, t1, t2, wt sql.NullInt64
		if err := rows.Scan(&m.ID, &bid, &m.Stage, &br, &bs, &court, &t1, &t2, &wt, &m.Status); err != nil {
			rows.Close()
			return nil, err
		}
		m.BracketID = ns(bid)
		m.BracketRound = ni(br)
		m.BracketSlot = ni(bs)
		m.CourtNumber = ni(court)
		m.Team1Score = ni(t1)
		m.Team2Score = ni(t2)
		m.WinningTeam = ni(wt)
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		sides, err := s.matchSides(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Sides = sides
	}
	return out, nil
}

// EventPoolMatches returns every pool match in the event with resolved sides,
// court number, and round context (id/number/status). The Game tab loads this
// as one stream so it can group + filter (search, status, division) in memory.
func (s *Service) EventPoolMatches(eventID string) ([]model.Match, error) {
	rows, err := s.db.Query(`SELECT m.id,m.bracket_id,m.stage,m.bracket_round,m.bracket_slot,
		c.court_number,m.team1_score,m.team2_score,m.winning_team,m.status,
		r.id,r.round_number,r.status
		FROM matches m
		LEFT JOIN courts c ON c.id=m.court_id
		JOIN rounds r ON r.id=m.round_id
		WHERE m.event_id=? AND m.stage='pool'
		ORDER BY r.round_number, m.bracket_id, c.court_number`, eventID)
	if err != nil {
		return nil, err
	}
	var out []model.Match
	for rows.Next() {
		var m model.Match
		var bid, rid, roundStatus sql.NullString
		var br, bs, court, t1, t2, wt, roundNum sql.NullInt64
		if err := rows.Scan(&m.ID, &bid, &m.Stage, &br, &bs, &court, &t1, &t2, &wt, &m.Status,
			&rid, &roundNum, &roundStatus); err != nil {
			rows.Close()
			return nil, err
		}
		m.BracketID = ns(bid)
		m.BracketRound = ni(br)
		m.BracketSlot = ni(bs)
		m.CourtNumber = ni(court)
		m.Team1Score = ni(t1)
		m.Team2Score = ni(t2)
		m.WinningTeam = ni(wt)
		m.RoundID = ns(rid)
		m.RoundNumber = ni(roundNum)
		m.RoundStatus = roundStatus.String
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		sides, err := s.matchSides(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Sides = sides
	}
	return out, nil
}

func (s *Service) matchSides(matchID string) ([]model.Side, error) {
	rows, err := s.db.Query(`SELECT mp.team, mp.player_id, p.full_name FROM match_participants mp
		JOIN players p ON p.id=mp.player_id WHERE mp.match_id=? ORDER BY mp.team`, matchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[int][]string{}
	ids := map[int][]string{}
	for rows.Next() {
		var team int
		var pid, name string
		if err := rows.Scan(&team, &pid, &name); err != nil {
			return nil, err
		}
		names[team] = append(names[team], name)
		ids[team] = append(ids[team], pid)
	}
	var sides []model.Side
	for _, t := range []int{1, 2} {
		if ns, ok := names[t]; ok {
			sides = append(sides, model.Side{Team: t, Players: ns, PlayerIDs: ids[t]})
		}
	}
	return sides, rows.Err()
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
	var pid string
	err := s.db.QueryRow(`SELECT id FROM players WHERE id=?`, inPlayerID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("replacement player not found")
	}
	if err != nil {
		return err
	}
	// The player being swapped out must currently be in the match.
	var team int
	err = s.db.QueryRow(`SELECT team FROM match_participants WHERE match_id=? AND player_id=?`, matchID, outPlayerID).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Don't swap in someone already playing in this match.
	var dup int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM match_participants WHERE match_id=? AND player_id=?`, matchID, inPlayerID).Scan(&dup); err != nil {
		return err
	}
	if dup > 0 {
		return errors.New("that player is already in this match")
	}
	res, err := s.db.Exec(`UPDATE match_participants SET player_id=? WHERE match_id=? AND player_id=?`, inPlayerID, matchID, outPlayerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
	rows, err := s.db.Query(
		`SELECT id, player_id, COALESCE(partner_id,'') FROM registrations WHERE event_id=? AND bracket_id=?`,
		eventID, bracketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reg
	for rows.Next() {
		var r reg
		if err := rows.Scan(&r.id, &r.playerID, &r.partnerID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) playerSkills() (map[string]float64, error) {
	rows, err := s.db.Query(`SELECT id, COALESCE(skill_level,0) FROM players`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]float64{}
	for rows.Next() {
		var id string
		var sk float64
		if err := rows.Scan(&id, &sk); err != nil {
			return nil, err
		}
		m[id] = sk
	}
	return m, rows.Err()
}

func (s *Service) courtIDsByNumber(eventID string) (map[int]string, error) {
	rows, err := s.db.Query(`SELECT court_number, id FROM courts WHERE event_id=?`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int]string{}
	for rows.Next() {
		var n int
		var id string
		if err := rows.Scan(&n, &id); err != nil {
			return nil, err
		}
		m[n] = id
	}
	return m, rows.Err()
}

func (s *Service) insertSide(matchID string, team int, side []string) error {
	if side == nil || engine.IsBye(side) {
		return nil
	}
	for _, pid := range side {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO match_participants (id,match_id,player_id,team) VALUES (?,?,?,?)`,
			newID(), matchID, pid, team); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) wipeAllMatches(eventID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM match_participants WHERE match_id IN (SELECT id FROM matches WHERE event_id=?)`, eventID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM matches WHERE event_id=?`, eventID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM rounds WHERE event_id=?`, eventID)
	return err
}

func (s *Service) wipeBracketStage(bracketID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM match_participants WHERE match_id IN (SELECT id FROM matches WHERE bracket_id=? AND stage='bracket')`,
		bracketID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM matches WHERE bracket_id=? AND stage='bracket'`, bracketID)
	return err
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

// scan helpers
type scanner interface{ Scan(dest ...any) error }

func scanEvent(sc scanner) (model.Event, error) {
	var e model.Event
	var dupr int
	var loc sql.NullString
	err := sc.Scan(&e.ID, &e.Name, &e.Format, &e.PartnerMode, &e.TournamentFormat, &e.ScoringMode,
		&e.NumCourts, &e.PointsToWin, &e.RegistrationFeeCents, &e.Currency, &loc, &dupr, &e.Status)
	e.Location = ns(loc)
	e.DuprSanctioned = dupr == 1
	return e, err
}

func scanMatch(sc scanner) (model.Match, error) {
	var m model.Match
	var bid sql.NullString
	var br, bs, t1, t2, wt sql.NullInt64
	err := sc.Scan(&m.ID, &bid, &m.Stage, &br, &bs, &t1, &t2, &wt, &m.Status)
	m.BracketID = ns(bid)
	m.BracketRound = ni(br)
	m.BracketSlot = ni(bs)
	m.Team1Score = ni(t1)
	m.Team2Score = ni(t2)
	m.WinningTeam = ni(wt)
	return m, err
}

// null helpers
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullF(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
func nullI(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}
func agePtr(v int) *int { return &v }
func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func ns(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}
func ni(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
func nf(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
