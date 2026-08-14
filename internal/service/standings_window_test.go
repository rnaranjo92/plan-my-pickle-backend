package service

import "testing"

// The whole "today's standings" feature rests on which DAY a game belongs to.
// Get this wrong by one hour and a Wednesday-evening league reads as Thursday —
// the board opens empty while people are on the court, and last night's results
// reappear the next morning, which is exactly what this feature exists to stop.
func TestLocalDateOf_UsesTheCallersDayNotUTC(t *testing.T) {
	const pdt = -7 * 60 // US west coast, summer

	cases := []struct {
		name, ts string
		offset   int
		want     string
	}{
		// 7pm PDT Wednesday is 02:00 UTC Thursday. The session is Wednesday's.
		{"evening session stays on its own day", "2026-08-12T02:00:00Z", pdt, "2026-08-11"},
		// Ten past midnight PDT — a late finish still belongs to that night's
		// date, not to the day that just started.
		{"just after local midnight", "2026-08-12T07:10:00Z", pdt, "2026-08-12"},
		// One minute BEFORE local midnight.
		{"just before local midnight", "2026-08-12T06:59:00Z", pdt, "2026-08-11"},
		// UTC caller gets UTC days.
		{"utc caller", "2026-08-12T02:00:00Z", 0, "2026-08-12"},
		// East of UTC: Manila is +8, so 6pm local is 10:00 UTC the same day.
		{"positive offset", "2026-08-12T10:00:00Z", 8 * 60, "2026-08-12"},
		// Manila again, but late enough that UTC is still the previous day.
		{"positive offset crosses back", "2026-08-11T17:00:00Z", 8 * 60, "2026-08-12"},
		// Postgres returns fractional seconds on some rows and not others.
		{"fractional seconds parse", "2026-08-12T02:00:00.123456Z", pdt, "2026-08-11"},
		{"offset-carrying timestamp", "2026-08-11T19:00:00-07:00", pdt, "2026-08-11"},
	}
	for _, c := range cases {
		if got := localDateOf(c.ts, c.offset); got != c.want {
			t.Errorf("%s: localDateOf(%q, %d) = %q, want %q",
				c.name, c.ts, c.offset, got, c.want)
		}
	}
}

func TestLocalDateOf_UnparseableIsDropped(t *testing.T) {
	for _, ts := range []string{"", "   ", "not a time", "2026-13-45"} {
		if got := localDateOf(ts, 0); got != "" {
			t.Errorf("localDateOf(%q) = %q, want empty so the row is skipped", ts, got)
		}
	}
}

// Days are grouped in the caller's timezone and returned newest first — the
// most recent session is what anyone opens this list for.
func TestSessionDays_GroupsAndOrders(t *testing.T) {
	f := newFake().seed("matches", `[
		{"completed_at":"2026-08-12T02:00:00Z"},
		{"completed_at":"2026-08-12T02:30:00Z"},
		{"completed_at":"2026-08-05T03:00:00Z"},
		{"completed_at":""}
	]`)
	s := newFakeSvc(t, f)

	days, err := s.SessionDays("e1", "", -7*60)
	if err != nil {
		t.Fatalf("SessionDays failed: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2 (the blank timestamp is dropped): %+v", len(days), days)
	}
	if days[0].Date != "2026-08-11" || days[0].Games != 2 {
		t.Errorf("newest day is %+v, want 2026-08-11 with 2 games", days[0])
	}
	if days[1].Date != "2026-08-04" || days[1].Games != 1 {
		t.Errorf("older day is %+v, want 2026-08-04 with 1 game", days[1])
	}
}

// A windowed board must rank by the same rules as the all-time board — the same
// leaderboard filtered, not a second one that breaks ties its own way.
func TestStandingsBetween_RanksLikeTheAllTimeBoard(t *testing.T) {
	seed := `[{"team1_score":11,"team2_score":5,"winning_team":1,
		"participants":[
			{"team":1,"player":{"id":"p1","full_name":"Ann"}},
			{"team":2,"player":{"id":"p2","full_name":"Bea"}}]}]`
	f := newFake().seed("matches", seed)
	s := newFakeSvc(t, f)

	got, err := s.StandingsBetween("e1", "", true, "2026-08-11T07:00:00Z", "2026-08-12T07:00:00Z")
	if err != nil {
		t.Fatalf("StandingsBetween failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	// The winner ranks first, and the box score is the usual convention.
	if got[0].PlayerID != "p1" || got[0].Wins != 1 || got[0].PointDiff != 6 {
		t.Errorf("winner row is %+v, want p1 with 1 win and +6", got[0])
	}
	if got[1].PlayerID != "p2" || got[1].Losses != 1 || got[1].PointDiff != -6 {
		t.Errorf("loser row is %+v, want p2 with 1 loss and -6", got[1])
	}
}

// A bye (completed, no scores) and a forfeit (counts_for_diff false) decide a
// match but must never inflate games played or differential — the same rule the
// all-time board follows.
func TestStandingsBetween_SkipsByesAndForfeits(t *testing.T) {
	f := newFake().seed("matches", `[
		{"team1_score":null,"team2_score":null,"winning_team":1,
		 "participants":[{"team":1,"player":{"id":"p1","full_name":"Ann"}}]},
		{"team1_score":11,"team2_score":0,"winning_team":1,"counts_for_diff":false,
		 "participants":[{"team":1,"player":{"id":"p1","full_name":"Ann"}}]}
	]`)
	s := newFakeSvc(t, f)

	got, err := s.StandingsBetween("e1", "", true, "", "")
	if err != nil {
		t.Fatalf("StandingsBetween failed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a bye and a forfeit produced a box score: %+v", got)
	}
}
