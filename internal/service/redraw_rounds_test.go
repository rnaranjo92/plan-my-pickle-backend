package service

import (
	"fmt"
	"testing"
	"time"
)

// A redraw DELETES rounds, so "which rounds are safely replaceable?" must be
// exact. These lock the three rules that keep it safe.
func rdSvc(t *testing.T, rounds, matches, regs string) *Service {
	f := newFake().
		seed("events", `[{"id":"e1","starts_at":"2026-01-01T18:00:00Z","perpetual":true,`+
			`"tournament_format":"round_robin","format":"doubles"}]`).
		seed("rounds", rounds).
		seed("matches", matches).
		seed("brackets", `[{"id":"b1","name":"Open"}]`).
		seed("registrations", regs)
	return newFakeSvc(t, f)
}

// Four checked-in players so the division is never the thing blocking a redraw.
const rdFullRoster = `[{"id":"r1","player_id":"p1"},{"id":"r2","player_id":"p2"},
	{"id":"r3","player_id":"p3"},{"id":"r4","player_id":"p4"}]`

func rdRound(id string, n int, when time.Time) string {
	return fmt.Sprintf(
		`{"id":%q,"bracket_id":"b1","round_number":%d,"created_at":%q}`,
		id, n, when.UTC().Format(time.RFC3339))
}

// Round 1 played, rounds 2-3 untouched → only 2 and 3 may be redrawn.
func TestRedrawTakesOnlyRoundsAfterTheLastPlayed(t *testing.T) {
	now := time.Now()
	s := rdSvc(t,
		"["+rdRound("rA", 1, now)+","+rdRound("rB", 2, now)+","+rdRound("rC", 3, now)+"]",
		`[{"round_id":"rA","status":"completed","team1_score":11},
		  {"round_id":"rB","status":"scheduled","team1_score":null},
		  {"round_id":"rC","status":"scheduled","team1_score":null}]`,
		rdFullRoster)
	ids, plan, err := s.redrawTargets("e1")
	if err != nil {
		t.Fatalf("redrawTargets: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if got["rA"] {
		t.Error("a scored round must never be redrawn")
	}
	if !got["rB"] || !got["rC"] {
		t.Errorf("rounds after the last played one should be redrawable; got %v", ids)
	}
	if plan.NothingPlayedYet {
		t.Error("something WAS played — the paper-scoring warning should not fire")
	}
}

// An unplayed round BEFORE a played one isn't "upcoming" — a stray untouched
// round 1 must not be redrawn once round 2 has been played.
func TestRedrawIgnoresGapsBeforeThePlayedHighWater(t *testing.T) {
	now := time.Now()
	s := rdSvc(t,
		"["+rdRound("rA", 1, now)+","+rdRound("rB", 2, now)+","+rdRound("rC", 3, now)+"]",
		`[{"round_id":"rA","status":"scheduled","team1_score":null},
		  {"round_id":"rB","status":"completed","team1_score":11},
		  {"round_id":"rC","status":"scheduled","team1_score":null}]`,
		rdFullRoster)
	ids, _, err := s.redrawTargets("e1")
	if err != nil {
		t.Fatalf("redrawTargets: %v", err)
	}
	for _, id := range ids {
		if id == "rA" {
			t.Error("round 1 sits before a played round 2 — it is not upcoming")
		}
	}
}

// A PRIOR session is history: it shows in History and may still be awaiting
// paper scores. It must never be redrawn, however untouched it looks.
func TestRedrawNeverTouchesAPriorSession(t *testing.T) {
	now := time.Now()
	lastWeek := now.AddDate(0, 0, -7)
	s := rdSvc(t,
		"["+rdRound("old", 1, lastWeek)+","+rdRound("new", 2, now)+"]",
		`[{"round_id":"old","status":"scheduled","team1_score":null},
		  {"round_id":"new","status":"scheduled","team1_score":null}]`,
		rdFullRoster)
	ids, _, err := s.redrawTargets("e1")
	if err != nil {
		t.Fatalf("redrawTargets: %v", err)
	}
	for _, id := range ids {
		if id == "old" {
			t.Fatal("last week's session must never be redrawn")
		}
	}
}

// Nothing marked played is ambiguous: it may have been played on paper. The
// rounds stay eligible, but the plan flags it so the app can warn first.
func TestRedrawFlagsWhenNothingIsMarkedPlayed(t *testing.T) {
	now := time.Now()
	s := rdSvc(t,
		"["+rdRound("rA", 1, now)+"]",
		`[{"round_id":"rA","status":"scheduled","team1_score":null}]`,
		rdFullRoster)
	_, plan, err := s.redrawTargets("e1")
	if err != nil {
		t.Fatalf("redrawTargets: %v", err)
	}
	if !plan.NothingPlayedYet {
		t.Error("nothing is marked played — the paper-scoring warning must fire")
	}
}

// A division that can't field a game KEEPS its rounds: deleting a draw and then
// failing to rebuild it is the one outcome that loses work for nothing.
func TestRedrawLeavesAShortDivisionAlone(t *testing.T) {
	now := time.Now()
	s := rdSvc(t,
		"["+rdRound("rA", 1, now)+"]",
		`[{"round_id":"rA","status":"scheduled","team1_score":null}]`,
		`[{"id":"r1","player_id":"p1"}]`) // 1 checked in, doubles needs 4
	ids, plan, err := s.redrawTargets("e1")
	if err != nil {
		t.Fatalf("redrawTargets: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("a division that can't rebuild must keep its rounds; got %v", ids)
	}
	if len(plan.Blocked) != 1 {
		t.Errorf("the blocked division should be reported; got %v", plan.Blocked)
	}
}
