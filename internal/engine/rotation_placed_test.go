package engine

import (
	"fmt"
	"testing"
)

// A court number the organizer typed has to survive as a DESTINATION. The
// tempting implementation — sort the roster by court and chunk it into fours —
// only ranks players: place two on court 3, leave the rest unplaced, and those
// two sort to the front and open the top court. Every test here exists to keep
// that from coming back.

// courtOf reports which court a player ended up on, or 0 if they're on the bench.
func courtOf(courts []RotCourt, bench []string, id string) int {
	for _, c := range courts {
		for _, seat := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
			if seat == id {
				return c.Court
			}
		}
	}
	for _, b := range bench {
		if b == id {
			return 0
		}
	}
	return -1 // lost entirely
}

// seatsIn builds n unplaced players, p0 strongest.
func unplaced(n int) []Seat {
	out := make([]Seat, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Seat{ID: fmt.Sprintf("p%d", i)})
	}
	return out
}

// THE bug this function exists for. Two players placed on court 3, ten unplaced:
// a sort-and-chunk implementation puts the placed pair on court 1 — the exact
// opposite of what was typed.
func TestPlaced_PlacementIsADestinationNotARanking(t *testing.T) {
	players := unplaced(12)
	players[10].Court = 3 // p10 and p11 are the two weakest AND hand-placed low
	players[11].Court = 3

	courts, bench := SeedPlacedCourts(players, 0)

	if got := courtOf(courts, bench, "p10"); got != 3 {
		t.Errorf("p10 was placed on court 3 but started on court %d", got)
	}
	if got := courtOf(courts, bench, "p11"); got != 3 {
		t.Errorf("p11 was placed on court 3 but started on court %d", got)
	}
}

// Skipped numbers must not compact. Placing on courts 1 and 3 has to leave a gap
// at court 2 for the unplaced, not slide the court-3 group up to court 2.
func TestPlaced_SkippedCourtNumbersDontCompact(t *testing.T) {
	players := unplaced(12)
	for i := 0; i < 4; i++ {
		players[i].Court = 1
	}
	for i := 4; i < 8; i++ {
		players[i].Court = 3
	}

	courts, bench := SeedPlacedCourts(players, 0)

	for i := 4; i < 8; i++ {
		id := fmt.Sprintf("p%d", i)
		if got := courtOf(courts, bench, id); got != 3 {
			t.Errorf("%s was placed on court 3 but started on court %d", id, got)
		}
	}
	// The unplaced four fill the gap at court 2.
	for i := 8; i < 12; i++ {
		id := fmt.Sprintf("p%d", i)
		if got := courtOf(courts, bench, id); got != 2 {
			t.Errorf("unplaced %s should fill the court-2 gap, got court %d", id, got)
		}
	}
}

// Five players typed onto one court is a miscount, not an instruction to shift
// everyone below. The court keeps four; the extra returns to the pool.
func TestPlaced_OversubscribedCourtDoesntSpillDownward(t *testing.T) {
	players := unplaced(12)
	for i := 0; i < 5; i++ {
		players[i].Court = 1
	}

	courts, bench := SeedPlacedCourts(players, 0)

	// Count real seats. The previous version of this assertion set a literal 4
	// and could never fail — it proved nothing.
	on1 := 0
	for i := 0; i < 12; i++ {
		if courtOf(courts, bench, fmt.Sprintf("p%d", i)) == 1 {
			on1++
		}
	}
	if on1 != 4 {
		t.Fatalf("court 1 holds %d players, want 4", on1)
	}
	for i := 0; i < 4; i++ {
		if got := courtOf(courts, bench, fmt.Sprintf("p%d", i)); got != 1 {
			t.Errorf("p%d should hold court 1 (top four by rating), got %d", i, got)
		}
	}
	// The fifth is placed by the fallback, not lost — and must not be PROMOTED.
	got := courtOf(courts, bench, "p4")
	if got < 0 {
		t.Fatal("the fifth player placed on court 1 vanished entirely")
	}
	if got == 1 {
		t.Fatal("five players on a court of four")
	}
}

// An evicted player must not leapfrog UPWARD past the court they were typed on.
func TestPlaced_EvictedPlayerIsNotPromoted(t *testing.T) {
	players := unplaced(12)
	for i := 0; i < 5; i++ {
		players[i].Court = 2 // p0 (strongest)..p4; court 2 keeps four
	}
	courts, bench := SeedPlacedCourts(players, 0)

	if got := courtOf(courts, bench, "p4"); got == 1 {
		t.Fatal("an evicted player opened court 1 — a court the organizer never typed")
	}
}

// A court that isn't running tonight can't be honoured, but the player must not
// be rewarded with the TOP court for it.
func TestPlaced_OutOfRangePlacementNeverOpensTheTopCourt(t *testing.T) {
	players := unplaced(12) // 3 courts run
	for i := 0; i < 4; i++ {
		players[i].Court = 9 // the four STRONGEST, on a court that won't run
	}
	courts, bench := SeedPlacedCourts(players, 0)

	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("p%d", i)
		if got := courtOf(courts, bench, id); got == 1 {
			t.Fatalf("%s was placed on court 9 (not running) and opened court 1", id)
		}
	}
}

// A court number beyond the last real court can't be honoured — but the player
// must not disappear.
func TestPlaced_CourtBeyondTheRoomFallsBackNotAway(t *testing.T) {
	players := unplaced(8) // only 2 courts exist
	players[0].Court = 9

	courts, bench := SeedPlacedCourts(players, 0)

	if got := courtOf(courts, bench, "p0"); got < 0 {
		t.Fatal("a player placed on a court that doesn't exist was lost")
	}
}

// Nobody may be lost or duplicated, whatever the placements say.
func TestPlaced_PopulationIsAlwaysPreserved(t *testing.T) {
	// Every combination of roster size, cap, and a scattering of placements.
	for n := 4; n <= 17; n++ {
		for cap := 0; cap <= 4; cap++ {
			for spread := 1; spread <= 5; spread++ {
				players := unplaced(n)
				for i := range players {
					if i%spread == 0 {
						players[i].Court = (i % 5) + 1
					}
				}
				courts, bench := SeedPlacedCourts(players, cap)

				seen := map[string]int{}
				for _, c := range courts {
					if c.TeamA[0] == "" || c.TeamA[1] == "" ||
						c.TeamB[0] == "" || c.TeamB[1] == "" {
						t.Fatalf("n=%d cap=%d spread=%d: court %d has an empty seat",
							n, cap, spread, c.Court)
					}
					for _, s := range []string{c.TeamA[0], c.TeamA[1], c.TeamB[0], c.TeamB[1]} {
						seen[s]++
					}
				}
				for _, b := range bench {
					seen[b]++
				}
				if len(seen) != n {
					t.Fatalf("n=%d cap=%d spread=%d: %d distinct players out, want %d",
						n, cap, spread, len(seen), n)
				}
				for id, times := range seen {
					if times != 1 {
						t.Fatalf("n=%d cap=%d spread=%d: %s appears %d times",
							n, cap, spread, id, times)
					}
				}
				// Courts must stay contiguous from 1 — NextRound erases players
				// otherwise.
				for i, c := range courts {
					if c.Court != i+1 {
						t.Fatalf("n=%d cap=%d spread=%d: court %d at index %d",
							n, cap, spread, c.Court, i)
					}
				}
			}
		}
	}
}

// With nobody placed this must be exactly the old rating seed — otherwise every
// existing session changes behaviour the moment this ships.
func TestPlaced_NoPlacementsMatchesTheRatingSeed(t *testing.T) {
	for n := 4; n <= 17; n++ {
		for cap := 0; cap <= 4; cap++ {
			ids := make([]string, 0, n)
			for i := 0; i < n; i++ {
				ids = append(ids, fmt.Sprintf("p%d", i))
			}
			wantCourts, wantBench := SeedCourts(ids, cap)
			gotCourts, gotBench := SeedPlacedCourts(unplaced(n), cap)

			if len(gotCourts) != len(wantCourts) {
				t.Fatalf("n=%d cap=%d: %d courts, want %d",
					n, cap, len(gotCourts), len(wantCourts))
			}
			for i := range wantCourts {
				if gotCourts[i] != wantCourts[i] {
					t.Fatalf("n=%d cap=%d: court %d is %v, want %v",
						n, cap, i+1, gotCourts[i], wantCourts[i])
				}
			}
			if len(gotBench) != len(wantBench) {
				t.Fatalf("n=%d cap=%d: bench %v, want %v", n, cap, gotBench, wantBench)
			}
			for i := range wantBench {
				if gotBench[i] != wantBench[i] {
					t.Fatalf("n=%d cap=%d: bench %v, want %v", n, cap, gotBench, wantBench)
				}
			}
		}
	}
}

// A fully placed roster must come out exactly as typed.
func TestPlaced_FullyPlacedRosterIsHonouredExactly(t *testing.T) {
	players := unplaced(12)
	// Deliberately place the STRONGEST on the bottom court — if placement is
	// really honoured, rating must not pull them back up.
	for i := 0; i < 4; i++ {
		players[i].Court = 3
	}
	for i := 4; i < 8; i++ {
		players[i].Court = 2
	}
	for i := 8; i < 12; i++ {
		players[i].Court = 1
	}

	courts, bench := SeedPlacedCourts(players, 0)

	for i := 0; i < 12; i++ {
		want := players[i].Court
		if got := courtOf(courts, bench, fmt.Sprintf("p%d", i)); got != want {
			t.Errorf("p%d placed on court %d, started on %d", i, want, got)
		}
	}
}

// The output feeds straight into NextRound, so it must satisfy that function's
// preconditions for every shape — including the malformed-input guard.
func TestPlaced_OutputIsSafeForNextRound(t *testing.T) {
	for n := 4; n <= 17; n++ {
		players := unplaced(n)
		for i := range players {
			players[i].Court = (i % 4) + 1
		}
		courts, bench := SeedPlacedCourts(players, 0)
		if len(courts) == 0 {
			continue
		}
		results := make([]RotResult, 0, len(courts))
		for _, c := range courts {
			results = append(results, RotResult{Court: c.Court, Winner: "a"})
		}
		next, nextBench := NextRound(courts, results, bench, LosersDown)

		total := len(next)*4 + len(nextBench)
		if total != n {
			t.Fatalf("n=%d: %d players after a round, want %d — the guard tripped "+
				"or a player was lost", n, total, n)
		}
	}
}
