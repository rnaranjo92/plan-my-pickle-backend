package service

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func fp(f float64) *float64 { return &f }

func (s *Service) poolMatchIDs(t *testing.T, eventID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM matches WHERE event_id=? AND stage='pool'`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func TestRoundRobinFlowAndStandings(t *testing.T) {
	s := newSvc(t)
	eid, err := s.CreateEvent(model.CreateEventRequest{Name: "Friday", Format: "singles", NumCourts: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.RegisterPlayer(eid, model.RegisterRequest{FullName: fmt.Sprintf("P%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.GenerateSchedule(eid)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 { // C(4,2)
		t.Fatalf("want 6 matches, got %d", n)
	}

	ids := s.poolMatchIDs(t, eid)
	if len(ids) != 6 {
		t.Fatalf("want 6 pool matches, got %d", len(ids))
	}
	for _, id := range ids {
		if err := s.RecordScore(id, 11, 5); err != nil {
			t.Fatal(err)
		}
	}

	st, err := s.Standings(eid, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 4 {
		t.Fatalf("want 4 ranked players, got %d", len(st))
	}
	totalWins := 0
	for _, r := range st {
		totalWins += r.Wins
	}
	if totalWins != 6 {
		t.Fatalf("total wins should equal matches (6), got %d", totalWins)
	}
	// leaderboard must be sorted by wins desc
	for i := 1; i < len(st); i++ {
		if st[i-1].Wins < st[i].Wins {
			t.Fatalf("standings not sorted by wins desc")
		}
	}
}

func TestSingleElimAdvancement(t *testing.T) {
	s := newSvc(t)
	eid, err := s.CreateEvent(model.CreateEventRequest{
		Name: "Cup", Format: "singles", TournamentFormat: "single_elim", NumCourts: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.RegisterPlayer(eid, model.RegisterRequest{
			FullName: fmt.Sprintf("P%d", i), SkillLevel: fp(4.0 - float64(i)*0.1)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.GenerateSchedule(eid)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // 2 semis + final
		t.Fatalf("want 3 bracket matches, got %d", n)
	}

	bks, _ := s.GetBrackets(eid)
	matches, _ := s.BracketMatches(bks[0].ID)

	var finalID string
	var semis []string
	for _, m := range matches {
		if m.BracketRound != nil && *m.BracketRound == 1 {
			semis = append(semis, m.ID)
		} else {
			finalID = m.ID
		}
	}
	if len(semis) != 2 || finalID == "" {
		t.Fatalf("expected 2 semis + a final, got semis=%d final=%q", len(semis), finalID)
	}
	for _, id := range semis {
		if err := s.RecordScore(id, 11, 4); err != nil {
			t.Fatal(err)
		}
	}

	matches, _ = s.BracketMatches(bks[0].ID)
	for _, m := range matches {
		if m.ID == finalID {
			if len(m.Sides) != 2 {
				t.Fatalf("final should have 2 advanced sides, got %d", len(m.Sides))
			}
		}
	}
}

func TestRescoreFlipDoesNotCorruptFeed(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{
		Name: "Cup", Format: "singles", TournamentFormat: "single_elim", NumCourts: 1})
	for i := 0; i < 4; i++ {
		s.RegisterPlayer(eid, model.RegisterRequest{FullName: fmt.Sprintf("P%d", i), SkillLevel: fp(4.0 - float64(i)*0.1)})
	}
	s.GenerateSchedule(eid)
	bks, _ := s.GetBrackets(eid)
	matches, _ := s.BracketMatches(bks[0].ID)
	var semi, finalID string
	for _, m := range matches {
		if m.BracketRound != nil && *m.BracketRound == 1 {
			if semi == "" {
				semi = m.ID
			}
		} else {
			finalID = m.ID
		}
	}
	// score, then re-score with the winner flipped
	s.RecordScore(semi, 11, 3)
	s.RecordScore(semi, 3, 11)

	// the final must still have exactly one player on the fed slot, not two
	var feedCount int
	s.db.QueryRow(`SELECT count(*) FROM match_participants mp
		JOIN matches m ON m.id=? WHERE mp.match_id=?`, finalID, finalID).Scan(&feedCount)
	if feedCount > 1 {
		t.Fatalf("re-score left %d players on the feed slot (stale advancement bug)", feedCount)
	}
}

func TestSeedPlayoffDemo(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedPlayoffDemo()
	if err != nil {
		t.Fatalf("SeedPlayoffDemo: %v", err)
	}
	if eid == "" {
		t.Fatal("expected a non-empty event id")
	}

	ev, err := s.GetEvent(eid)
	if err != nil {
		t.Fatal(err)
	}
	if ev.TournamentFormat != "pools_playoff" {
		t.Fatalf("tournament_format = %q, want pools_playoff", ev.TournamentFormat)
	}

	scalar := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return n
	}

	// Players only at kickoff: 24 registered, but no schedule, matches, or bracket.
	if regs := scalar(`SELECT COUNT(*) FROM registrations WHERE event_id=?`, eid); regs != 24 {
		t.Fatalf("expected 24 registrations, got %d", regs)
	}
	if rounds := scalar(`SELECT COUNT(*) FROM rounds WHERE event_id=?`, eid); rounds != 0 {
		t.Fatalf("expected no rounds before Generate schedule, got %d", rounds)
	}
	if matches := scalar(`SELECT COUNT(*) FROM matches WHERE event_id=?`, eid); matches != 0 {
		t.Fatalf("expected no matches before Generate schedule, got %d", matches)
	}
}

func TestStartMatch(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedPlayoffDemo()
	if err != nil {
		t.Fatalf("SeedPlayoffDemo: %v", err)
	}
	if _, err := s.GenerateSchedule(eid); err != nil {
		t.Fatalf("GenerateSchedule: %v", err)
	}

	// Grab one pool match (and its round) to start.
	var matchID, roundID string
	if err := s.db.QueryRow(
		`SELECT id, round_id FROM matches WHERE event_id=? AND stage='pool' ORDER BY rowid LIMIT 1`, eid).
		Scan(&matchID, &roundID); err != nil {
		t.Fatalf("pick match: %v", err)
	}

	if _, err := s.StartMatch(matchID); err != nil {
		t.Fatalf("StartMatch: %v", err)
	}

	// The started match flips to in_progress; its sibling in the round does not.
	var status string
	if err := s.db.QueryRow(`SELECT status FROM matches WHERE id=?`, matchID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "in_progress" {
		t.Fatalf("started match status = %q, want in_progress", status)
	}
	var others int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM matches WHERE round_id=? AND id!=? AND status!='scheduled'`, roundID, matchID).
		Scan(&others); err != nil {
		t.Fatal(err)
	}
	if others != 0 {
		t.Fatalf("expected sibling matches untouched, %d changed", others)
	}

	// The parent round is now active (play has begun).
	var roundStatus string
	if err := s.db.QueryRow(`SELECT status FROM rounds WHERE id=?`, roundID).Scan(&roundStatus); err != nil {
		t.Fatal(err)
	}
	if roundStatus != "active" {
		t.Fatalf("round status = %q, want active", roundStatus)
	}

	// Starting an unknown match is a not-found error.
	if _, err := s.StartMatch("does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("StartMatch(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestPlayoffRequiresCompletedPools(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedPlayoffDemo()
	if err != nil {
		t.Fatalf("SeedPlayoffDemo: %v", err)
	}
	bks, err := s.GetBrackets(eid)
	if err != nil || len(bks) == 0 {
		t.Fatalf("GetBrackets: %v", err)
	}
	b := bks[0]

	bracketMatchCount := func() int {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM matches WHERE bracket_id=? AND stage='bracket'`, b.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Before any schedule exists, building the playoff must fail.
	if _, err := s.GeneratePlayoffBracket(b.ID, 0); err == nil {
		t.Fatal("expected error building playoff before the pool schedule exists")
	}

	// Generating the schedule lays down the empty medal skeleton (4 matches),
	// but a manual seeded build before pools finish must still fail, and must
	// not seed the skeleton's semifinals.
	if _, err := s.GenerateSchedule(eid); err != nil {
		t.Fatalf("GenerateSchedule: %v", err)
	}
	if n := bracketMatchCount(); n != 4 {
		t.Fatalf("expected a 4-match skeleton after schedule generation, got %d", n)
	}
	if _, err := s.GeneratePlayoffBracket(b.ID, 0); err == nil {
		t.Fatal("expected error building playoff before pools are completed")
	}
	var seeded int
	s.db.QueryRow(`SELECT COUNT(*) FROM match_participants mp
		JOIN matches m ON m.id=mp.match_id
		WHERE m.bracket_id=? AND m.stage='bracket'`, b.ID).Scan(&seeded)
	if seeded != 0 {
		t.Fatalf("skeleton should be unseeded before pools complete, got %d participants", seeded)
	}

	// Complete every pool match in this division, then it must succeed.
	rows, err := s.db.Query(`SELECT id FROM matches WHERE bracket_id=? AND stage='pool'`, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		if err := s.RecordScore(id, 11, 4+i%6); err != nil {
			t.Fatalf("RecordScore: %v", err)
		}
	}
	n, err := s.GeneratePlayoffBracket(b.ID, 0)
	if err != nil {
		t.Fatalf("build playoff after pools completed: %v", err)
	}
	if n == 0 || bracketMatchCount() == 0 {
		t.Fatal("expected bracket matches to be created after pools completed")
	}
}

func TestSeedDemoRoundStatusReconciled(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedDemo()
	if err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	// No round may read 'pending' while every one of its matches is completed.
	var mismatched int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM rounds r
		WHERE r.event_id=? AND r.status='pending'
		  AND EXISTS (SELECT 1 FROM matches m WHERE m.round_id=r.id)
		  AND NOT EXISTS (SELECT 1 FROM matches m WHERE m.round_id=r.id AND m.status!='completed')`,
		eid).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("found %d fully-scored rounds still marked pending", mismatched)
	}

	// The round-robin demo scores ~60%, so some rounds should be completed.
	var completed int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM rounds WHERE event_id=? AND status='completed'`, eid).
		Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed == 0 {
		t.Fatal("expected at least one completed round in the round-robin demo")
	}
}

func TestSpreadCourtsUsesAllCourts(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedPlayoffDemo()
	if err != nil {
		t.Fatalf("SeedPlayoffDemo: %v", err)
	}
	if _, err := s.GenerateSchedule(eid); err != nil {
		t.Fatalf("GenerateSchedule: %v", err)
	}

	// The demo has 3 courts and two divisions each running 2 concurrent matches,
	// so every court should be utilized across the pool matches.
	rows, err := s.db.Query(`
		SELECT DISTINCT c.court_number
		FROM matches m JOIN courts c ON c.id=m.court_id
		WHERE m.event_id=? AND m.stage='pool' ORDER BY c.court_number`, eid)
	if err != nil {
		t.Fatal(err)
	}
	var used []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		used = append(used, n)
	}
	rows.Close()
	if len(used) != 3 {
		t.Fatalf("expected all 3 courts used, got %v", used)
	}

	// Within round 1, concurrent matches must land on distinct courts (the first
	// three across the two divisions should be on courts 1, 2, 3).
	rows2, err := s.db.Query(`
		SELECT c.court_number
		FROM matches m JOIN rounds r ON r.id=m.round_id JOIN courts c ON c.id=m.court_id
		WHERE m.event_id=? AND m.stage='pool' AND r.round_number=1
		ORDER BY m.bracket_id, m.rowid`, eid)
	if err != nil {
		t.Fatal(err)
	}
	var round1 []int
	for rows2.Next() {
		var n int
		if err := rows2.Scan(&n); err != nil {
			rows2.Close()
			t.Fatal(err)
		}
		round1 = append(round1, n)
	}
	rows2.Close()
	if len(round1) < 3 {
		t.Fatalf("expected >=3 matches in round 1, got %d", len(round1))
	}
	distinct := map[int]bool{round1[0]: true, round1[1]: true, round1[2]: true}
	if len(distinct) != 3 {
		t.Fatalf("expected first 3 round-1 matches on distinct courts, got %v", round1)
	}
}

func TestFinanceEntries(t *testing.T) {
	s := newSvc(t)
	eid, err := s.CreateEvent(model.CreateEventRequest{Name: "Money Cup", NumCourts: 1})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}

	inc, err := s.AddFinanceEntry(eid, model.FinanceEntryRequest{
		Kind: "income", Category: "Registration fees", AmountCents: 12000, Note: "20 players",
	})
	if err != nil {
		t.Fatalf("add income: %v", err)
	}
	if _, err := s.AddFinanceEntry(eid, model.FinanceEntryRequest{
		Kind: "expense", Category: "Balls", AmountCents: 4500,
	}); err != nil {
		t.Fatalf("add expense: %v", err)
	}

	// validation
	if _, err := s.AddFinanceEntry(eid, model.FinanceEntryRequest{Kind: "donation", Category: "x", AmountCents: 1}); err == nil {
		t.Fatal("expected error for bad kind")
	}
	if _, err := s.AddFinanceEntry(eid, model.FinanceEntryRequest{Kind: "income", Category: "", AmountCents: 1}); err == nil {
		t.Fatal("expected error for empty category")
	}
	if _, err := s.AddFinanceEntry(eid, model.FinanceEntryRequest{Kind: "income", Category: "x", AmountCents: 0}); err == nil {
		t.Fatal("expected error for non-positive amount")
	}

	entries, err := s.FinanceEntries(eid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if err := s.DeleteFinanceEntry(inc.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	entries, _ = s.FinanceEntries(eid)
	if len(entries) != 1 || entries[0].Kind != "expense" {
		t.Fatalf("expected only the expense to remain, got %+v", entries)
	}
	if err := s.DeleteFinanceEntry("nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound deleting missing entry, got %v", err)
	}
}

func TestCheckInByPhone(t *testing.T) {
	s := newSvc(t)
	eid, err := s.CreateEvent(model.CreateEventRequest{Name: "Phone Cup", NumCourts: 1})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	if _, err := s.RegisterPlayer(eid, model.RegisterRequest{FullName: "Ana Rivera", Phone: "+1 (555) 100-0000"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// digits-only + country-code-tolerant suffix match
	regID, name, err := s.CheckInByPhone(eid, "555-100-0000")
	if err != nil {
		t.Fatalf("checkin by phone: %v", err)
	}
	if name != "Ana Rivera" || regID == "" {
		t.Fatalf("unexpected result: id=%q name=%q", regID, name)
	}

	regs, _ := s.Registrations(eid)
	if len(regs) != 1 || !regs[0].CheckedIn {
		t.Fatalf("player should be checked in: %+v", regs)
	}

	if _, _, err := s.CheckInByPhone(eid, "5559999999"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for unknown number, got %v", err)
	}
	if _, _, err := s.CheckInByPhone(eid, "123"); err == nil {
		t.Fatal("expected error for too-short number")
	}
}

func TestMedalBracket(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedPlayoffDemo()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	bks, _ := s.GetBrackets(eid)
	if len(bks) == 0 {
		t.Fatal("no brackets")
	}
	b := bks[0]
	if _, err := s.GenerateSchedule(eid); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// Complete every pool match in this division.
	collect := func(q string, args ...any) []string {
		rows, err := s.db.Query(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	for i, id := range collect(`SELECT id FROM matches WHERE bracket_id=? AND stage='pool'`, b.ID) {
		if err := s.RecordScore(id, 11, 4+i%6); err != nil {
			t.Fatalf("score pool: %v", err)
		}
	}

	n, err := s.GeneratePlayoffBracket(b.ID, 0)
	if err != nil {
		t.Fatalf("build playoff: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected 4 medal-bracket matches, got %d", n)
	}

	count := func(q string, args ...any) int {
		var c int
		if err := s.db.QueryRow(q, args...).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if r1 := count(`SELECT COUNT(*) FROM matches WHERE bracket_id=? AND stage='bracket' AND bracket_round=1`, b.ID); r1 != 2 {
		t.Fatalf("expected 2 semifinals, got %d", r1)
	}
	if r2 := count(`SELECT COUNT(*) FROM matches WHERE bracket_id=? AND stage='bracket' AND bracket_round=2`, b.ID); r2 != 2 {
		t.Fatalf("expected 2 round-2 games (gold+bronze), got %d", r2)
	}

	// Score both semifinals; winners should land in gold, losers in bronze.
	for _, id := range collect(`SELECT id FROM matches WHERE bracket_id=? AND stage='bracket' AND bracket_round=1`, b.ID) {
		if err := s.RecordScore(id, 11, 6); err != nil {
			t.Fatalf("score semi: %v", err)
		}
	}
	goldTeams := count(`SELECT COUNT(DISTINCT team) FROM match_participants WHERE match_id=
		(SELECT id FROM matches WHERE bracket_id=? AND stage='bracket' AND bracket_round=2 AND bracket_slot=0)`, b.ID)
	bronzeTeams := count(`SELECT COUNT(DISTINCT team) FROM match_participants WHERE match_id=
		(SELECT id FROM matches WHERE bracket_id=? AND stage='bracket' AND bracket_round=2 AND bracket_slot=1)`, b.ID)
	if goldTeams != 2 {
		t.Fatalf("gold game should have both finalists, got %d teams", goldTeams)
	}
	if bronzeTeams != 2 {
		t.Fatalf("bronze game should have both semifinal losers, got %d teams", bronzeTeams)
	}
}

func TestSwapMatchPlayer(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedPlayoffDemo()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GenerateSchedule(eid); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	// a registered player not necessarily in this match — register a fresh sub
	sub, err := s.RegisterPlayer(eid, model.RegisterRequest{FullName: "Sub Player"})
	if err != nil {
		t.Fatalf("register sub: %v", err)
	}

	// pick a pool match and one of its players
	var matchID string
	if err := s.db.QueryRow(`SELECT id FROM matches WHERE event_id=? AND stage='pool' LIMIT 1`, eid).Scan(&matchID); err != nil {
		t.Fatal(err)
	}
	var outID string
	if err := s.db.QueryRow(`SELECT player_id FROM match_participants WHERE match_id=? LIMIT 1`, matchID).Scan(&outID); err != nil {
		t.Fatal(err)
	}

	if err := s.SwapMatchPlayer(matchID, outID, sub.PlayerID); err != nil {
		t.Fatalf("swap: %v", err)
	}
	// the sub is now in the match, the original is not
	var hasSub, hasOut int
	s.db.QueryRow(`SELECT COUNT(*) FROM match_participants WHERE match_id=? AND player_id=?`, matchID, sub.PlayerID).Scan(&hasSub)
	s.db.QueryRow(`SELECT COUNT(*) FROM match_participants WHERE match_id=? AND player_id=?`, matchID, outID).Scan(&hasOut)
	if hasSub != 1 || hasOut != 0 {
		t.Fatalf("swap didn't take: sub=%d out=%d", hasSub, hasOut)
	}

	// swapping in someone already in the match is rejected
	var otherID string
	s.db.QueryRow(`SELECT player_id FROM match_participants WHERE match_id=? AND player_id<>? LIMIT 1`, matchID, sub.PlayerID).Scan(&otherID)
	if err := s.SwapMatchPlayer(matchID, sub.PlayerID, otherID); err == nil {
		t.Fatal("expected error swapping in a player already in the match")
	}
	// swapping out someone not in the match -> not found
	if err := s.SwapMatchPlayer(matchID, "ghost", sub.PlayerID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPlayoffSkeletonAutoSeeds(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedPlayoffDemo()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	bks, _ := s.GetBrackets(eid)
	b := bks[0]
	if _, err := s.GenerateSchedule(eid); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	count := func(q string, args ...any) int {
		var c int
		if err := s.db.QueryRow(q, args...).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	// Skeleton exists right after schedule generation, unseeded.
	if n := count(`SELECT COUNT(*) FROM matches WHERE bracket_id=? AND stage='bracket'`, b.ID); n != 4 {
		t.Fatalf("expected a 4-match skeleton, got %d", n)
	}
	seedCount := `SELECT COUNT(*) FROM match_participants mp JOIN matches m ON m.id=mp.match_id
		WHERE m.bracket_id=? AND m.stage='bracket' AND m.bracket_round=1`
	if p := count(seedCount, b.ID); p != 0 {
		t.Fatalf("semis should be unseeded before pools, got %d participants", p)
	}

	// Complete every pool match in this division — the last one auto-seeds.
	rows, _ := s.db.Query(`SELECT id FROM matches WHERE bracket_id=? AND stage='pool'`, b.ID)
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		if err := s.RecordScore(id, 11, 4+i%6); err != nil {
			t.Fatalf("score pool: %v", err)
		}
	}

	// Semifinals are now seeded with 2 teams each (4 players in doubles).
	if teams := count(`SELECT COUNT(DISTINCT m.id||'-'||mp.team) FROM match_participants mp
		JOIN matches m ON m.id=mp.match_id
		WHERE m.bracket_id=? AND m.stage='bracket' AND m.bracket_round=1`, b.ID); teams != 4 {
		t.Fatalf("expected both semifinals seeded (4 team-slots), got %d", teams)
	}
}
