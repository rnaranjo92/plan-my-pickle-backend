package service

import (
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
