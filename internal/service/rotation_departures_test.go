package service

import (
	"encoding/json"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/engine"
)

// settleDepartures is tested directly rather than through the fake: the fake
// ignores query filters, so every DB-level test is forced into a one-court,
// one-departure, one-waiting-player shape — the single configuration in which
// the old refill happened to be correct. The interesting cases are all the
// others.

func court(n int, a1, a2, b1, b2 string) engine.RotCourt {
	return engine.RotCourt{Court: n, TeamA: [2]string{a1, a2}, TeamB: [2]string{b1, b2}}
}

func seatsOf(courts []engine.RotCourt) []string {
	var out []string
	for _, c := range courts {
		out = append(out, c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1])
	}
	return out
}

func has(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// The ordinary case: someone leaves, someone is waiting, they swap.
func TestSettle_WaitingPlayerTakesTheSeat(t *testing.T) {
	courts := []engine.RotCourt{court(1, "a", "b", "c", "d")}
	got, bench := settleDepartures(courts, []string{"w"}, map[string]bool{"d": true})

	if has(seatsOf(got), "d") {
		t.Fatal("the player who left is still on court")
	}
	if !has(seatsOf(got), "w") {
		t.Fatal("the waiting player never took the free seat")
	}
	if len(bench) != 0 {
		t.Fatalf("bench should be empty, got %v", bench)
	}
}

// THE case that previously had no answer anywhere in the product: a full house,
// nobody waiting. The room really has got smaller, so it must shrink.
func TestSettle_FullHouseDropsACourtInsteadOfStranding(t *testing.T) {
	courts := []engine.RotCourt{
		court(1, "a", "b", "c", "d"),
		court(2, "e", "f", "g", "h"),
	}
	got, bench := settleDepartures(courts, nil, map[string]bool{"h": true})

	if len(got) != 1 {
		t.Fatalf("want 1 court after losing a player from a full house, got %d", len(got))
	}
	if has(seatsOf(got), "h") {
		t.Fatal("the player who left is still seated")
	}
	// The other three from the dropped court are FIRST back on — they were
	// mid-game, not resting.
	if len(bench) != 3 {
		t.Fatalf("want the 3 survivors waiting, got %v", bench)
	}
	for _, id := range []string{"e", "f", "g"} {
		if !has(bench, id) {
			t.Errorf("%s vanished when their court was dropped", id)
		}
	}
}

// More departures than waiting players is exactly where the old refill silently
// gave up and left the departed player seated.
func TestSettle_MoreDeparturesThanWaiting(t *testing.T) {
	courts := []engine.RotCourt{
		court(1, "a", "b", "c", "d"),
		court(2, "e", "f", "g", "h"),
	}
	got, bench := settleDepartures(courts, []string{"w"},
		map[string]bool{"d": true, "h": true})

	for _, id := range []string{"d", "h"} {
		if has(seatsOf(got), id) {
			t.Fatalf("%s left but is still on court", id)
		}
	}
	all := append(seatsOf(got), bench...)
	for _, id := range []string{"a", "b", "c", "e", "f", "g", "w"} {
		if !has(all, id) {
			t.Errorf("%s was lost entirely", id)
		}
	}
	for _, c := range got {
		for _, seat := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			if seat == "" {
				t.Fatalf("court %d has an empty seat", c.Court)
			}
		}
	}
}

// A BLANK seat is a free seat. Walking past it is how a null seat became a
// permanent freeze — the engine's guard rejects it and nobody ever moves again.
func TestSettle_FillsABlankSeat(t *testing.T) {
	courts := []engine.RotCourt{court(1, "a", "", "c", "d")}
	got, bench := settleDepartures(courts, []string{"w"}, map[string]bool{"x": true})

	if has(seatsOf(got), "") {
		t.Fatal("a blank seat survived — the engine guard would freeze the session")
	}
	if !has(seatsOf(got), "w") {
		t.Fatalf("the waiting player didn't fill the blank seat: %v %v", got, bench)
	}
}

// Below four players there is no layout to write. Hand everyone back rather than
// inventing a court.
func TestSettle_BelowOneCourtReturnsEveryoneRemaining(t *testing.T) {
	courts := []engine.RotCourt{court(1, "a", "b", "c", "d")}
	got, bench := settleDepartures(courts, nil, map[string]bool{"d": true})

	if len(got) != 0 {
		t.Fatalf("want no courts with three players left, got %d", len(got))
	}
	for _, id := range []string{"a", "b", "c"} {
		if !has(bench, id) {
			t.Errorf("%s was lost when the last court broke up", id)
		}
	}
	if has(bench, "d") {
		t.Error("the player who left is in the queue")
	}
}

// Nobody left: don't touch anything.
func TestSettle_NoDeparturesIsAnExactNoOp(t *testing.T) {
	courts := []engine.RotCourt{court(1, "a", "b", "c", "d")}
	got, bench := settleDepartures(courts, []string{"w"}, map[string]bool{})

	if len(got) != 1 || got[0] != courts[0] {
		t.Fatalf("courts changed with nobody departing: %v", got)
	}
	if len(bench) != 1 || bench[0] != "w" {
		t.Fatalf("queue changed with nobody departing: %v", bench)
	}
}

// Whatever happens, nobody may be duplicated — the engine guard now rejects
// duplicates, so producing one would freeze the session.
func TestSettle_NeverDuplicatesAnyone(t *testing.T) {
	courts := []engine.RotCourt{
		court(1, "a", "b", "c", "d"),
		court(2, "e", "f", "g", "h"),
		court(3, "i", "j", "k", "l"),
	}
	for _, left := range []map[string]bool{
		{"a": true}, {"a": true, "e": true}, {"a": true, "e": true, "i": true},
		{"a": true, "b": true, "c": true, "d": true},
		{"i": true, "j": true, "k": true, "l": true},
	} {
		for _, bench := range [][]string{nil, {"w"}, {"w", "x"}, {"w", "x", "y", "z"}} {
			got, rest := settleDepartures(
				append([]engine.RotCourt(nil), courts...),
				append([]string(nil), bench...), left)

			seen := map[string]int{}
			for _, id := range append(seatsOf(got), rest...) {
				seen[id]++
			}
			for id, n := range seen {
				if n != 1 {
					t.Fatalf("left=%v bench=%v: %s appears %d times", left, bench, id, n)
				}
				if left[id] {
					t.Fatalf("left=%v: departed %s is still in play", left, id)
				}
			}
			for _, c := range got {
				for _, seat := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
					if seat == "" {
						t.Fatalf("left=%v bench=%v: empty seat on court %d", left, bench, c.Court)
					}
				}
			}
			for i, c := range got {
				if c.Court != i+1 {
					t.Fatalf("courts not contiguous after settling: %v", got)
				}
			}
		}
	}
}

// A player stepping out for ONE round used to destroy a court permanently:
// settleDepartures shrank the room and nothing anywhere grew it back, so eight
// players returned to a single court with four of them benched all night.
func TestGrow_ReopensACourtWhenThePlayersComeBack(t *testing.T) {
	courts := []engine.RotCourt{court(1, "a", "b", "c", "d")}
	got, bench := growCourts(courts, []string{"e", "f", "g", "h"}, 4)

	if len(got) != 2 {
		t.Fatalf("want the second court re-opened, got %d courts", len(got))
	}
	if len(bench) != 0 {
		t.Fatalf("want an empty queue, got %v", bench)
	}
	for i, c := range got {
		if c.Court != i+1 {
			t.Fatalf("courts not contiguous: %v", got)
		}
	}
}

// The venue cap still holds — three waiting players don't conjure a court.
func TestGrow_RespectsTheVenueCapAndNeedsFour(t *testing.T) {
	courts := []engine.RotCourt{court(1, "a", "b", "c", "d")}

	got, bench := growCourts(courts, []string{"e", "f", "g"}, 4)
	if len(got) != 1 || len(bench) != 3 {
		t.Fatalf("three waiting players opened a court: %v %v", got, bench)
	}

	got, bench = growCourts(courts, []string{"e", "f", "g", "h"}, 1)
	if len(got) != 1 || len(bench) != 4 {
		t.Fatalf("the venue cap of 1 was exceeded: %v %v", got, bench)
	}
}

// Shrink then grow must return the room to where it started, with nobody lost.
func TestSettleThenGrow_RoundTripsWithoutLosingAnyone(t *testing.T) {
	courts := []engine.RotCourt{
		court(1, "a", "b", "c", "d"),
		court(2, "e", "f", "g", "h"),
	}
	// h steps out...
	shrunk, bench := settleDepartures(courts, nil, map[string]bool{"h": true})
	if len(shrunk) != 1 {
		t.Fatalf("want the room to shrink to 1 court, got %d", len(shrunk))
	}
	// ...and comes straight back.
	grown, rest := growCourts(shrunk, append(bench, "h"), 2)
	if len(grown) != 2 {
		t.Fatalf("the court never came back: %d courts, queue %v", len(grown), rest)
	}
	seen := map[string]int{}
	for _, id := range append(seatsOf(grown), rest...) {
		seen[id]++
	}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if seen[id] != 1 {
			t.Errorf("%s appears %d times after a shrink/grow round trip", id, seen[id])
		}
	}
}

// Whoever has waited LONGEST re-enters lowest, which is the engine's stated
// rule ("you re-enter onto the bottom court"). With vacancies on more than one
// court the order of the scan decides who gets which, and filling top-down
// handed the longest-waiting player a seat on court 1.
func TestSettle_LongestWaitingPlayerReEntersLowest(t *testing.T) {
	courts := []engine.RotCourt{
		court(1, "a", "b", "c", "d"),
		court(2, "e", "f", "g", "h"),
	}
	// A vacancy on each court, and exactly enough people waiting to fill both —
	// so no court is dropped and the only question is who goes where.
	got, bench := settleDepartures(courts, []string{"first", "second"},
		map[string]bool{"a": true, "e": true})

	if len(got) != 2 {
		t.Fatalf("both vacancies were fillable; the room should not shrink: %v", got)
	}
	if len(bench) != 0 {
		t.Fatalf("queue should be empty, got %v", bench)
	}
	seatOn := func(n int) []string {
		for _, c := range got {
			if c.Court == n {
				return []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]}
			}
		}
		return nil
	}
	if !has(seatOn(2), "first") {
		t.Errorf("the longest-waiting player should re-enter on the BOTTOM court; "+
			"court 2 = %v", seatOn(2))
	}
	if !has(seatOn(1), "second") {
		t.Errorf("court 1's vacancy should go to the next in line; court 1 = %v",
			seatOn(1))
	}
}

// A player who leaves, returns, and leaves again resolves across rounds onto the
// id that took over from them the first time — putting that person in the queue
// while they are already on court. The engine guard rejects a duplicate, so the
// session freezes for good. Simulation hit this in 15,091 of 40,000 nights once
// the upstream refusal was bypassed, which is too thin a defence for a dead
// night, so the advance dedupes the queue against the seats.
func TestAdvance_QueueNeverHoldsSomeoneAlreadySeated(t *testing.T) {
	// pGone is WAITING (not seated) and was substituted out earlier; the player
	// who took over from them, pOnCourt, is on court. Resolving the queue entry
	// therefore points at somebody already playing.
	f := oneCourtSession(11, 11, 9, 9).
		seed("rotation_sessions",
			`[{"id":"s1","status":"live","current_round":1,"round_minutes":12,
			   "bench":["pGone"]}]`).
		seed("rotation_round_courts", `[{"session_id":"s1","round":1,"court":1,
			"team_a_p1":"pOnCourt","team_a_p2":"pA2",
			"team_b_p1":"pB1","team_b_p2":"pB2","winner":"a"}]`).
		seed("rotation_substitutions",
			`[{"session_id":"s1","round":1,"out_player":"pGone","in_player":"pOnCourt"}]`).
		seed("rotation_players", `[
			{"id":"pOnCourt","session_id":"s1","active":true},
			{"id":"pA2","session_id":"s1","active":true},
			{"id":"pB1","session_id":"s1","active":true},
			{"id":"pB2","session_id":"s1","active":true},
			{"id":"pGone","session_id":"s1","active":false}]`)
	s := newFakeSvc(t, f)

	if err := s.AdvanceRotationSession("s1", 1); err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	sent := string(f.rpcBodies("advance_rotation_session")[0])

	// pB1 is on court; the queue resolved pA1 -> pB1, which would duplicate them.
	var payload struct {
		Bench  []string `json:"p_bench"`
		Courts []struct {
			A []string `json:"a"`
			B []string `json:"b"`
		} `json:"p_courts"`
	}
	if err := json.Unmarshal([]byte(sent), &payload); err != nil {
		t.Fatalf("unparseable advance payload: %v", err)
	}
	seated := map[string]bool{}
	for _, c := range payload.Courts {
		for _, id := range append(append([]string{}, c.A...), c.B...) {
			if seated[id] {
				t.Fatalf("%s is seated twice — the session would freeze: %s", id, sent)
			}
			seated[id] = true
		}
	}
	for _, id := range payload.Bench {
		if seated[id] {
			t.Fatalf("%s is both on court and in the queue: %s", id, sent)
		}
	}
}
