package service

import "testing"

// ---- primitive extractors -------------------------------------------------

func TestPrimitiveExtractors(t *testing.T) {
	m := map[string]any{
		"s":    "hello",
		"empty": "",
		"fnum": float64(42),
		"snum": "7",
		"b":    true,
		"f":    float64(3.5),
		"fs":   "2.25",
	}

	if asStr(m, "s") != "hello" || asStr(m, "missing") != "" {
		t.Error("asStr")
	}
	if p := asStrPtr(m, "s"); p == nil || *p != "hello" {
		t.Error("asStrPtr present")
	}
	if asStrPtr(m, "empty") != nil || asStrPtr(m, "missing") != nil {
		t.Error("asStrPtr empty/missing should be nil")
	}
	if asInt(m, "fnum") != 42 || asInt(m, "snum") != 7 || asInt(m, "missing") != 0 {
		t.Error("asInt")
	}
	if p := asIntPtr(m, "fnum"); p == nil || *p != 42 {
		t.Error("asIntPtr float")
	}
	if p := asIntPtr(m, "snum"); p == nil || *p != 7 {
		t.Error("asIntPtr string")
	}
	if asIntPtr(m, "missing") != nil {
		t.Error("asIntPtr missing")
	}
	if !asBool(m, "b") || asBool(m, "missing") {
		t.Error("asBool")
	}
	if p := asFloatPtr(m, "f"); p == nil || *p != 3.5 {
		t.Error("asFloatPtr float")
	}
	if p := asFloatPtr(m, "fs"); p == nil || *p != 2.25 {
		t.Error("asFloatPtr string")
	}
	if asFloatPtr(m, "missing") != nil {
		t.Error("asFloatPtr missing")
	}
}

func TestNullHelpers(t *testing.T) {
	if orNull("") != nil || orNull("x") != "x" {
		t.Error("orNull")
	}
	f := 1.5
	if fOrNull(nil) != nil {
		t.Error("fOrNull nil")
	}
	if fOrNull(&f) != 1.5 {
		t.Error("fOrNull val")
	}
	i := 3
	if iOrNull(nil) != nil {
		t.Error("iOrNull nil")
	}
	if iOrNull(&i) != 3 {
		t.Error("iOrNull val")
	}
	if intOr(nil, 99) != 99 || intOr(&i, 99) != 3 {
		t.Error("intOr")
	}
	if strOr(nil) != "" {
		t.Error("strOr nil")
	}
	s := "y"
	if strOr(&s) != "y" {
		t.Error("strOr val")
	}
}

func TestAsMap(t *testing.T) {
	obj := map[string]any{"k": "v"}
	if got := asMap(map[string]any{"e": obj}, "e"); got == nil || got["k"] != "v" {
		t.Error("asMap object")
	}
	// PostgREST sometimes returns a to-one embed as a one-element array.
	arr := map[string]any{"e": []any{obj}}
	if got := asMap(arr, "e"); got == nil || got["k"] != "v" {
		t.Error("asMap array")
	}
	if asMap(map[string]any{"e": []any{}}, "e") != nil {
		t.Error("asMap empty array")
	}
	if asMap(map[string]any{}, "e") != nil {
		t.Error("asMap missing")
	}
}

func TestAsGamesAndIntArray(t *testing.T) {
	row := map[string]any{
		"games": []any{
			map[string]any{"team1": float64(11), "team2": float64(7)},
			map[string]any{"team1": float64(9), "team2": float64(11)},
		},
		"day_end_minutes": []any{float64(1020), float64(1080), "bad"},
	}
	g := asGames(row, "games")
	if len(g) != 2 || g[0].Team1 != 11 || g[1].Team2 != 11 {
		t.Errorf("asGames = %+v", g)
	}
	if asGames(map[string]any{}, "games") != nil {
		t.Error("asGames missing should be nil")
	}
	ints := mapIntArray(row, "day_end_minutes")
	if len(ints) != 3 || ints[0] != 1020 || ints[2] != -1 {
		t.Errorf("mapIntArray = %v", ints)
	}
	if mapIntArray(map[string]any{}, "x") != nil {
		t.Error("mapIntArray missing")
	}
}

func TestMapBreaks(t *testing.T) {
	row := map[string]any{
		"schedule_breaks": []any{
			map[string]any{"startMin": float64(720), "endMin": float64(780), "label": "Lunch"},
			"not-an-object",
		},
	}
	b := mapBreaks(row)
	if len(b) != 1 || b[0].StartMin != 720 || b[0].Label != "Lunch" {
		t.Errorf("mapBreaks = %+v", b)
	}
	if mapBreaks(map[string]any{}) != nil {
		t.Error("mapBreaks missing")
	}
}

// ---- row -> model mappers -------------------------------------------------

func TestMapEvent(t *testing.T) {
	row := map[string]any{
		"id": "e1", "name": "Slam", "format": "doubles",
		"num_courts": float64(4), "points_to_win": float64(11), "win_by": float64(2),
		"best_of": float64(3), "registration_fee_cents": float64(2500),
		"currency": "usd", "location": "Austin",
		"dupr_sanctioned": true, "cash_prize": true,
		"venue_lat": float64(30.2), "venue_lng": float64(-97.7),
		"sponsor_watermark_opacity": float64(0.2),
		"schedule_breaks": []any{
			map[string]any{"startMin": float64(720), "endMin": float64(780), "label": "Lunch"},
		},
		"status": "live",
	}
	e := mapEvent(row)
	if e.ID != "e1" || e.Name != "Slam" || e.Format != "doubles" {
		t.Errorf("event basics = %+v", e)
	}
	if e.NumCourts != 4 || e.PointsToWin != 11 || e.BestOf != 3 {
		t.Error("event ints")
	}
	if !e.DuprSanctioned || !e.CashPrize {
		t.Error("event bools")
	}
	if e.Location == nil || *e.Location != "Austin" {
		t.Error("event location ptr")
	}
	if e.VenueLat == nil || *e.VenueLat != 30.2 {
		t.Error("event venue lat")
	}
	if len(e.ScheduleBreaks) != 1 || e.ScheduleBreaks[0].Label != "Lunch" {
		t.Error("event breaks")
	}
	// Defaulted sponsor scale (key absent) should use the fallback, not 0.
	if e.SponsorWatermarkScale != 0.5 {
		t.Errorf("sponsor scale default = %v, want 0.5", e.SponsorWatermarkScale)
	}
	if e.SponsorWatermarkOpacity != 0.2 {
		t.Errorf("sponsor opacity = %v", e.SponsorWatermarkOpacity)
	}
}

func TestMapLeagueAndBrackets(t *testing.T) {
	lg := mapLeague(map[string]any{
		"id": "l1", "owner_id": "o", "name": "Fall", "league_type": "ladder",
		"day_type": "single", "sanctioned": true,
	})
	if lg.ID != "l1" || lg.LeagueType != "ladder" || !lg.Sanctioned {
		t.Errorf("league = %+v", lg)
	}

	b := mapBracket(map[string]any{
		"id": "b1", "event_id": "e1", "name": "3.5+", "sort_order": float64(2),
		"division_type": "mixed_doubles", "min_rating": float64(3.5),
	})
	if b.ID != "b1" || b.SortOrder != 2 || b.DivisionType != "mixed_doubles" {
		t.Errorf("bracket = %+v", b)
	}
	if b.MinRating == nil || *b.MinRating != 3.5 {
		t.Error("bracket min rating")
	}

	lb := mapLeagueBracket(map[string]any{
		"id": "lb1", "league_id": "l1", "name": "Open", "division_type": "open",
		"sort_order": float64(1),
	})
	if lb.ID != "lb1" || lb.LeagueID != "l1" || lb.SortOrder != 1 {
		t.Errorf("league bracket = %+v", lb)
	}
}

func TestMapLeagueSubFormats(t *testing.T) {
	le := mapLadderEntrant(map[string]any{
		"id": "en1", "league_bracket_id": "b", "display_name": "Sam",
		"player_id": "p1", "is_team": true, "position": float64(3),
	})
	if le.DisplayName != "Sam" || !le.IsTeam || le.Position != 3 {
		t.Errorf("ladder entrant = %+v", le)
	}
	if le.PlayerID == nil || *le.PlayerID != "p1" {
		t.Error("ladder entrant player id")
	}

	lm := mapLadderMatch(map[string]any{
		"id": "m", "league_bracket_id": "b", "entrant_a_id": "a", "entrant_b_id": "c",
		"winner_entrant_id": "a", "score": "11-7", "played_at": "2026-06-21",
	})
	if lm.WinnerEntrantID != "a" || lm.Score != "11-7" {
		t.Errorf("ladder match = %+v", lm)
	}

	tm := mapTeam(map[string]any{"id": "t", "league_bracket_id": "b", "name": "Aces"})
	if tm.Name != "Aces" {
		t.Errorf("team = %+v", tm)
	}

	tf := mapTeamFixture(map[string]any{
		"id": "f", "league_bracket_id": "b", "team_a_id": "a", "team_b_id": "c",
		"winner_team_id": "c", "score": "3-1",
	})
	if tf.WinnerTeamID != "c" || tf.Score != "3-1" {
		t.Errorf("team fixture = %+v", tf)
	}

	fm := mapFlexMatchup(map[string]any{
		"id": "fm", "league_bracket_id": "b", "team_a_id": "a", "team_b_id": "c",
		"status": "completed", "winner_team_id": "a",
	})
	if fm.Status != "completed" || fm.WinnerTeamID != "a" {
		t.Errorf("flex matchup = %+v", fm)
	}
}

func TestMapMiscRows(t *testing.T) {
	fe := mapFinanceEntry(map[string]any{
		"id": "f", "event_id": "e", "kind": "income", "category": "fees",
		"amount_cents": float64(1000), "note": "x", "created_at": "t",
	})
	if fe.Kind != "income" || fe.AmountCents != 1000 {
		t.Errorf("finance = %+v", fe)
	}

	ci := mapChecklistItem(map[string]any{
		"id": "c", "event_id": "e", "label": "Nets", "checked": true, "sort_order": float64(1),
	})
	if ci.Label != "Nets" || !ci.Checked {
		t.Errorf("checklist = %+v", ci)
	}

	st := mapStanding(map[string]any{
		"player_id": "p", "full_name": "Pat", "wins": float64(4), "losses": float64(1),
		"points_for": float64(44), "points_against": float64(30), "point_diff": float64(14),
	})
	if st.Wins != 4 || st.PointDiff != 14 || st.FullName != "Pat" {
		t.Errorf("standing = %+v", st)
	}

	rv := mapRoundView(map[string]any{
		"id": "r", "bracket_id": "b", "round_number": float64(2), "status": "live",
	})
	if rv.RoundNumber != 2 || rv.Status != "live" {
		t.Errorf("round view = %+v", rv)
	}

	fi := mapFeedItem(map[string]any{
		"id": "fi", "event_id": "e", "type": "announcement", "text": "Hi", "created_at": "t",
	})
	if fi.Type != "announcement" || fi.Text != "Hi" {
		t.Errorf("feed item = %+v", fi)
	}

	fc := mapFeedComment(map[string]any{
		"id": "fc", "feed_item_id": "fi", "author_name": "Lee", "text": "nice",
	})
	if fc.AuthorName != "Lee" || fc.Text != "nice" {
		t.Errorf("feed comment = %+v", fc)
	}
}

func TestMapSides(t *testing.T) {
	row := map[string]any{
		"participants": []any{
			map[string]any{"team": float64(1), "player_id": "a",
				"player": map[string]any{"full_name": "Alice"}},
			map[string]any{"team": float64(2), "player_id": "b",
				"player": map[string]any{"full_name": "Bob"}},
		},
	}
	sides := mapSides(row)
	if len(sides) != 2 {
		t.Fatalf("sides len = %d", len(sides))
	}
	if sides[0].Team != 1 || sides[0].Players[0] != "Alice" || sides[0].PlayerIDs[0] != "a" {
		t.Errorf("side 1 = %+v", sides[0])
	}
	if sides[1].Team != 2 || sides[1].Players[0] != "Bob" {
		t.Errorf("side 2 = %+v", sides[1])
	}
}

func TestMapMatch(t *testing.T) {
	row := map[string]any{
		"id": "m1", "bracket_id": "b", "stage": "bracket", "bracket_round": float64(2),
		"team1_score": float64(11), "team2_score": float64(7), "winning_team": float64(1),
		"status": "completed", "play_order": float64(1.5),
		"games": []any{map[string]any{"team1": float64(11), "team2": float64(7)}},
		"court": map[string]any{"court_number": float64(3)},
		"round": map[string]any{"id": "r1", "round_number": float64(2), "status": "done", "started_at": "t"},
		"participants": []any{
			map[string]any{"team": float64(1), "player_id": "a",
				"player": map[string]any{"full_name": "Alice"}},
		},
	}
	mt := mapMatch(row)
	if mt.ID != "m1" || mt.Stage != "bracket" {
		t.Errorf("match basics = %+v", mt)
	}
	if mt.Team1Score == nil || *mt.Team1Score != 11 {
		t.Error("match score")
	}
	if mt.CourtNumber == nil || *mt.CourtNumber != 3 {
		t.Error("match court embed")
	}
	if mt.RoundID == nil || *mt.RoundID != "r1" || mt.RoundNumber == nil || *mt.RoundNumber != 2 {
		t.Error("match round embed")
	}
	if len(mt.Games) != 1 || mt.Games[0].Team1 != 11 {
		t.Error("match games")
	}
	if len(mt.Sides) != 1 || mt.Sides[0].Players[0] != "Alice" {
		t.Error("match sides")
	}
}

func TestMapRegistration(t *testing.T) {
	// In-band rating: no OutsideRating flag.
	in := mapRegistration(map[string]any{
		"id": "r1", "event_id": "e", "player_id": "p", "payment_status": "paid",
		"player": map[string]any{"full_name": "Al", "phone": "555", "dupr_rating": float64(3.6)},
		"bracket": map[string]any{"min_rating": float64(3.0), "max_rating": float64(4.0)},
	})
	if in.FullName != "Al" || in.Phone != "555" {
		t.Errorf("reg basics = %+v", in)
	}
	if in.OutsideRating {
		t.Error("3.6 within 3.0-4.0 should not be outside")
	}

	// Out-of-band rating sets the flag.
	out := mapRegistration(map[string]any{
		"id": "r2", "event_id": "e", "player_id": "p",
		"player":  map[string]any{"full_name": "Hi", "dupr_rating": float64(4.5)},
		"bracket": map[string]any{"min_rating": float64(3.0), "max_rating": float64(4.0)},
	})
	if !out.OutsideRating {
		t.Error("4.5 above max 4.0 should be outside")
	}

	// Skill-level fallback when no DUPR rating.
	skill := mapRegistration(map[string]any{
		"id": "r3", "event_id": "e", "player_id": "p",
		"player":  map[string]any{"full_name": "Lo", "skill_level": float64(2.0)},
		"bracket": map[string]any{"min_rating": float64(3.0)},
	})
	if !skill.OutsideRating {
		t.Error("skill 2.0 below min 3.0 should be outside")
	}

	// Resolved partner name via the partner embed.
	withPartner := mapRegistration(map[string]any{
		"id": "r4", "event_id": "e", "player_id": "p",
		"player":  map[string]any{"full_name": "X"},
		"partner": map[string]any{"full_name": "Partner Pat"},
	})
	if withPartner.PartnerName == nil || *withPartner.PartnerName != "Partner Pat" {
		t.Error("resolved partner name")
	}
}
