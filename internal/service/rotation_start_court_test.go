package service

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Round 1 of a rotation ladder is seeded strongest-first by self-rating. That is
// the wrong answer for a social night where nobody has rated themselves, and for
// an organizer who knows the room and wants to place people by hand. start_court
// covers both: placed players seed onto their court, everyone else falls to the
// rating-ordered tail, and "shuffle" just fills the placements in at random.

// idOf pulls the player id out of a PostgREST filter like "id=eq.p3".
func idOf(filter string) string {
	for _, part := range strings.Split(filter, "&") {
		if rest, ok := strings.CutPrefix(part, "id=eq."); ok {
			return rest
		}
	}
	return ""
}

// rosterOf seeds n active players p0..p(n-1) in a session that hasn't started.
func rosterOf(n int) *fakeSupabase {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"p%d","session_id":"s1","active":true}`, i))
	}
	return newFake().
		seed("rotation_sessions", `[{"id":"s1","status":"setup"}]`).
		seed("rotation_players", "["+strings.Join(rows, ",")+"]")
}

// placementsFrom maps player id -> assigned start court from the captured writes.
func placementsFrom(t *testing.T, f *fakeSupabase) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, w := range f.targetedWrites("rotation_players") {
		id := idOf(w.filter)
		if id == "" {
			t.Fatalf("a roster update carried no player id: %q", w.filter)
		}
		court, ok := w.row["start_court"].(float64)
		if !ok {
			t.Fatalf("player %s got a non-numeric start_court: %#v", id, w.row["start_court"])
		}
		out[id] = int(court)
	}
	return out
}

// Every player must come out placed exactly once, four to a court. Anything else
// and the shuffle has either dropped someone or double-booked a seat.
func TestShuffle_PlacesEveryoneFourToACourt(t *testing.T) {
	f := rosterOf(12)
	s := newFakeSvc(t, f)

	n, err := s.ShuffleRotationStartCourts("s1")
	if err != nil {
		t.Fatalf("shuffle failed: %v", err)
	}
	if n != 12 {
		t.Fatalf("reported %d placed, roster was 12", n)
	}

	got := placementsFrom(t, f)
	if len(got) != 12 {
		t.Fatalf("placed %d distinct players, roster was 12", len(got))
	}
	perCourt := map[int]int{}
	for _, court := range got {
		perCourt[court]++
	}
	for court := 1; court <= 3; court++ {
		if perCourt[court] != 4 {
			t.Errorf("court %d got %d players, want 4 (all: %v)", court, perCourt[court], perCourt)
		}
	}
	if len(perCourt) != 3 {
		t.Errorf("12 players spread over %d courts, want 3: %v", len(perCourt), perCourt)
	}
}

// A roster that doesn't divide by four still places everyone: the remainder is
// numbered past the last full court, which is how it reaches the bench at seed
// time. Leaving them NULL instead would quietly fall back to a rating nobody set.
func TestShuffle_RemainderIsPlacedPastTheLastFullCourt(t *testing.T) {
	f := rosterOf(10)
	s := newFakeSvc(t, f)

	if _, err := s.ShuffleRotationStartCourts("s1"); err != nil {
		t.Fatalf("shuffle failed: %v", err)
	}

	got := placementsFrom(t, f)
	if len(got) != 10 {
		t.Fatalf("placed %d of 10 players", len(got))
	}
	perCourt := map[int]int{}
	for _, court := range got {
		perCourt[court]++
	}
	if perCourt[1] != 4 || perCourt[2] != 4 || perCourt[3] != 2 {
		t.Fatalf("want 4/4/2 across courts 1..3, got %v", perCourt)
	}
}

// The point of the button is that it varies. A "shuffle" that returns the same
// arrangement every time is just the insertion order with extra steps.
func TestShuffle_ActuallyVaries(t *testing.T) {
	seen := map[string]bool{}
	for attempt := 0; attempt < 40; attempt++ {
		f := rosterOf(12)
		s := newFakeSvc(t, f)
		if _, err := s.ShuffleRotationStartCourts("s1"); err != nil {
			t.Fatalf("shuffle failed: %v", err)
		}
		got := placementsFrom(t, f)
		keys := make([]string, 0, len(got))
		for id, court := range got {
			keys = append(keys, fmt.Sprintf("%s:%d", id, court))
		}
		sort.Strings(keys)
		seen[strings.Join(keys, ",")] = true
	}
	// 12 players over 3 courts has ~370k distinct arrangements; 40 identical
	// draws would mean the shuffle isn't shuffling.
	if len(seen) < 2 {
		t.Fatal("40 shuffles produced a single arrangement — the roster order was never randomised")
	}
}

// Seeding happens once, at start. Re-drawing the courts underneath a live
// session would move players who are already mid-game.
func TestShuffle_RefusedOnceLive(t *testing.T) {
	f := rosterOf(8).seed("rotation_sessions", `[{"id":"s1","status":"live"}]`)
	s := newFakeSvc(t, f)

	if _, err := s.ShuffleRotationStartCourts("s1"); err == nil {
		t.Fatal("a live session let its starting courts be reshuffled")
	}
	if len(f.written("rotation_players")) != 0 {
		t.Fatal("a refused shuffle still wrote to the roster")
	}
}

func TestSetStartCourt_RejectsCourtZeroAndBelow(t *testing.T) {
	for _, court := range []int{0, -1} {
		f := rosterOf(8)
		s := newFakeSvc(t, f)
		if err := s.SetRotationPlayerStartCourt("p0", &court); err == nil {
			t.Errorf("court %d was accepted; courts are numbered from 1", court)
		}
		if len(f.written("rotation_players")) != 0 {
			t.Errorf("court %d was rejected but still written", court)
		}
	}
}

// Un-placing must write a real NULL, not drop the field: a PATCH that omits the
// column leaves the old court in place, so "clear" would silently do nothing.
func TestSetStartCourt_NilUnplacesThePlayer(t *testing.T) {
	f := rosterOf(8)
	s := newFakeSvc(t, f)

	if err := s.SetRotationPlayerStartCourt("p0", nil); err != nil {
		t.Fatalf("un-placing failed: %v", err)
	}
	writes := f.written("rotation_players")
	if len(writes) != 1 {
		t.Fatalf("want exactly one write, got %d", len(writes))
	}
	val, present := writes[0]["start_court"]
	if !present {
		t.Fatal("the update omitted start_court entirely — the old court would survive")
	}
	if val != nil {
		t.Fatalf("want an explicit null, got %#v", val)
	}
}

func TestSetStartCourt_RefusedOnceLive(t *testing.T) {
	f := rosterOf(8).seed("rotation_sessions", `[{"id":"s1","status":"live"}]`)
	s := newFakeSvc(t, f)

	court := 2
	if err := s.SetRotationPlayerStartCourt("p0", &court); err == nil {
		t.Fatal("a player was re-placed in a live session")
	}
}
