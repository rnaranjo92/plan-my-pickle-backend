package service

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/courts"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// --------------------------------------------------------------- auto-geocode
// Listed events surface in the public "Nearby" feed only when they carry
// venue_lat/venue_lng (see NearbyEvents). When an organizer TYPES a text
// `location` but doesn't pick a venue on the map, the coordinates are nil and
// the event silently never appears. These helpers fill that gap best-effort:
// resolve the free-text location to a point via OpenStreetMap Nominatim (reused
// from courts.Geocode) and stamp the coords onto the event. A geocode failure
// NEVER fails the create/update — it just leaves the event without coords, the
// pre-existing behaviour (mirrors the syncEventStatus best-effort pattern).

// geocodeMu + geocodeLast serialize outbound geocode calls and enforce
// Nominatim's ~1 req/sec usage policy. Every call to bestEffortGeocode waits its
// turn and then sleeps off any remaining gap since the previous request, so even
// concurrent creates/updates (or a backfill loop) stay under the rate limit.
var (
	geocodeMu   sync.Mutex
	geocodeLast time.Time
)

const geocodeMinInterval = 1100 * time.Millisecond // ~1 req/sec, a touch of slack

// bestEffortGeocode resolves loc to a (lat,lng) point. It returns (nil,nil) on a
// blank query, no result, or ANY error/timeout — callers treat "no coords" as a
// non-event and proceed. It is rate-limited to honour Nominatim's policy.
func bestEffortGeocode(loc string) (lat, lng *float64) {
	q := strings.TrimSpace(loc)
	if q == "" {
		return nil, nil
	}

	geocodeMu.Lock()
	if wait := geocodeMinInterval - time.Since(geocodeLast); wait > 0 {
		time.Sleep(wait)
	}
	res, err := courts.Geocode(q)
	geocodeLast = time.Now()
	geocodeMu.Unlock()

	if err != nil {
		log.Printf("geocode: lookup for %q failed (continuing without coords): %v", q, err)
		return nil, nil
	}
	if res == nil {
		// No match — common for vague free text; not an error.
		return nil, nil
	}
	la, ln := res.Lat, res.Lng
	return &la, &ln
}

// BackfillEventCoords geocodes existing LISTED events that have a text location
// but no coords, so they start appearing in Nearby without the organizer having
// to re-edit. It is NOT exposed as an HTTP endpoint: a backfill touches every
// organizer's events, and the API has no admin-secret auth (only requireAuth /
// ownerOnly), so wiring it to a route would either let any signed-in user mass-
// trigger external API calls or require a brand-new auth layer. Instead the lead
// runs this from a one-off command / console.
//
// It is rate-limited (~1 req/sec via bestEffortGeocode) and capped by `limit`
// (limit <= 0 → DefaultBackfillCap) so a single run can't hammer Nominatim or
// run unbounded. Per-event geocode failures are skipped, not fatal. Returns the
// number of events that were successfully stamped with coords.
func (s *Service) BackfillEventCoords(limit int) (int, error) {
	if limit <= 0 {
		limit = DefaultBackfillCap
	}
	rows, err := s.sb.Select("events",
		"listed=eq.true&venue_lat=is.null&venue_lng=is.null&location=not.is.null"+
			"&select=id,location&limit="+strconv.Itoa(limit))
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, r := range rows {
		id := asStr(r, "id")
		loc := asStr(r, "location")
		if id == "" || strings.TrimSpace(loc) == "" {
			continue
		}
		lat, lng := bestEffortGeocode(loc) // rate-limited; nil on failure/no-match
		if lat == nil || lng == nil {
			continue
		}
		if _, err := s.sb.Update("events", "id=eq."+store.Q(id), map[string]any{
			"venue_lat": *lat,
			"venue_lng": *lng,
		}); err != nil {
			log.Printf("BackfillEventCoords: update %s failed (continuing): %v", id, err)
			continue
		}
		updated++
	}
	log.Printf("BackfillEventCoords: stamped coords on %d/%d listed events", updated, len(rows))
	return updated, nil
}

// BackfillEventCounties stamps county+state on LISTED events that already have
// coords but no county (e.g. created before the county feature). Bounded by
// limit; per-event failures are skipped, never fatal. Reverse-geocoding is
// cached, so re-runs are cheap. Run once after deploy; safe to re-run.
func (s *Service) BackfillEventCounties(limit int) (int, error) {
	if limit <= 0 {
		limit = DefaultBackfillCap
	}
	rows, err := s.sb.Select("events",
		"listed=eq.true&venue_lat=not.is.null&venue_lng=not.is.null&county=is.null"+
			"&select=id,venue_lat,venue_lng&limit="+strconv.Itoa(limit))
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, r := range rows {
		id := asStr(r, "id")
		lat, lng := asFloatPtr(r, "venue_lat"), asFloatPtr(r, "venue_lng")
		if id == "" || lat == nil || lng == nil {
			continue
		}
		county, state := courts.ReverseCounty(*lat, *lng)
		if strings.TrimSpace(county) == "" {
			continue // geocoder unset / no match / transient — skip, retry later
		}
		if _, err := s.sb.Update("events", "id=eq."+store.Q(id), map[string]any{
			"county": county,
			"state":  state,
		}); err != nil {
			log.Printf("BackfillEventCounties: update %s failed (continuing): %v", id, err)
			continue
		}
		updated++
	}
	log.Printf("BackfillEventCounties: stamped county on %d/%d events", updated, len(rows))
	return updated, nil
}

// DefaultBackfillCap bounds a single BackfillEventCoords run when no explicit
// limit is given — keeps the ~1 req/sec loop to a few minutes at most.
const DefaultBackfillCap = 200
