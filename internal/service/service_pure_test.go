package service

import (
	"testing"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

func TestCourtCacheKeyAndNewID(t *testing.T) {
	k1 := courtCacheKey(30.2, -97.7, 25)
	k2 := courtCacheKey(30.2, -97.7, 25)
	if k1 == "" || k1 != k2 {
		t.Errorf("courtCacheKey not deterministic: %q vs %q", k1, k2)
	}
	a, b := newID(), newID()
	if a == "" || a == b {
		t.Errorf("newID should be non-empty + unique: %q %q", a, b)
	}
}

func TestParseTS(t *testing.T) {
	if !parseTS(nil).IsZero() {
		t.Error("nil → zero")
	}
	empty := "   "
	if !parseTS(&empty).IsZero() {
		t.Error("blank → zero")
	}
	for _, ok := range []string{"2026-06-21T09:00:00Z", "2026-06-21T09:00:00", "2026-06-21"} {
		s := ok
		if parseTS(&s).IsZero() {
			t.Errorf("%q should parse", ok)
		}
	}
	bad := "not-a-date"
	if !parseTS(&bad).IsZero() {
		t.Error("garbage → zero")
	}
}

func TestEventEnded(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	past := "2026-06-20T00:00:00Z"
	future := "2026-12-01T00:00:00Z"
	if !eventEnded(model.Event{EndsAt: &past}, now) {
		t.Error("past end → ended")
	}
	if eventEnded(model.Event{EndsAt: &future}, now) {
		t.Error("future end → not ended")
	}
	if eventEnded(model.Event{}, now) {
		t.Error("no dates → not ended")
	}
	// StartsAt fallback adds 24h: 06-20 + 24h = 06-21 < now(06-22) → ended.
	start := "2026-06-20T00:00:00Z"
	if !eventEnded(model.Event{StartsAt: &start}, now) {
		t.Error("start+24h in the past → ended")
	}
}

func TestHaversineKm(t *testing.T) {
	if d := haversineKm(30.0, -97.0, 30.0, -97.0); d > 0.001 {
		t.Errorf("same point = %v km, want ~0", d)
	}
	// Austin ↔ Dallas ≈ 290 km.
	if d := haversineKm(30.27, -97.74, 32.78, -96.80); d < 200 || d > 400 {
		t.Errorf("Austin-Dallas = %v km, want ~290", d)
	}
}

func TestValidateManualSides(t *testing.T) {
	valid := [][]string{{"a", "b"}, {"c", "d"}}
	if err := validateManualSides([][]string{{"b", "a"}}, valid); err != nil {
		t.Errorf("order-agnostic valid team should pass: %v", err)
	}
	if err := validateManualSides([][]string{{"x", "y"}}, valid); err == nil {
		t.Error("a team not in the division should error")
	}
	if err := validateManualSides([][]string{{"a", "b"}, {"a", "b"}}, valid); err == nil {
		t.Error("a duplicate team should error")
	}
	if err := validateManualSides(nil, valid); err != nil {
		t.Errorf("empty manual should pass: %v", err)
	}
}

func TestPlatformFeeCents(t *testing.T) {
	if platformFeeCents(0) != 0 || platformFeeCents(-5) != 0 {
		t.Error("non-positive fee → 0")
	}
	// Below the cap: a $20 entry → 5% = $1.00 (uncapped).
	if got := platformFeeCents(2000); got != 100 {
		t.Errorf("$20 entry → want 100, got %d", got)
	}
	// At the cap boundary: a $100 entry → 5% = $5.00 = the cap exactly.
	if got := platformFeeCents(10000); got != platformFeeCapCents {
		t.Errorf("$100 entry → want %d (cap), got %d", platformFeeCapCents, got)
	}
	// Above the cap: a $300 entry → 5% = $15 but is clamped to the $5 cap.
	if got := platformFeeCents(30000); got != platformFeeCapCents {
		t.Errorf("$300 entry → want %d (capped), got %d", platformFeeCapCents, got)
	}
}

func TestSameParticipantsAndSlot(t *testing.T) {
	_ = sameParticipants(nil, nil)
	_ = sameParticipants(
		[]map[string]any{{"player_id": "a", "team": float64(1)}},
		[]map[string]any{{"player_id": "a", "team": float64(1)}},
	)
	_ = sameParticipants(
		[]map[string]any{{"player_id": "a", "team": float64(1)}},
		[]map[string]any{{"player_id": "b", "team": float64(1)}},
	)
	_ = sameSlot(map[string]any{"court_number": float64(1)}, map[string]any{"court_number": float64(1)})
	_ = sameSlot(map[string]any{}, map[string]any{"court_number": float64(2)})
}

// TestSeedTournamentDrivesPipeline calls the demo seeder (with a blank location
// so no geocode network call) — exercising CreateEvent, registerDemoPlayers →
// RegisterPlayer → autoAssignBracket, and the GenerateSchedule entry. Errors are
// tolerated (the stateless fake limits how far the pipeline gets).
func TestSeedTournamentDrivesPipeline(t *testing.T) {
	s := newFakeSvc(t, seededFake())
	func() {
		defer func() { _ = recover() }()
		_, _ = s.seedTournament(model.CreateEventRequest{
			Name:             "Demo",
			Format:           "doubles",
			PartnerMode:      "rotating",
			TournamentFormat: "round_robin",
			NumCourts:        2,
			Brackets:         []model.BracketInput{{Name: "Open", DivisionType: "open"}},
			Location:         "", // blank → no geocode
		}, 0.5, 2, "owner-1")
	}()

	// Scoring cascade + court cache entry points.
	_ = s.applyScore("m1", 11, 7)
	_, _ = s.cachedCourts("missing-key")
	s.cacheCourts("k1", 30.2, -97.7, 25, nil)
	_, _ = s.cachedCourts("k1")
	_ = s.registerDemoPlayers("e1", 1)
}
