package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// TestServiceReads drives the DB-backed read methods against the seeded fake
// Supabase. Business errors are tolerated (the point is to exercise the query +
// mapping code paths); a couple of seeded reads are asserted to confirm the
// harness wires through correctly. External-network methods (geocode, courts,
// stripe/paypal) are intentionally excluded.
func TestServiceReads(t *testing.T) {
	s := newFakeSvc(t, seededFake())

	// Asserted: the seeded event maps through.
	if ev, err := s.GetEvent("e1"); err != nil || ev.ID != "e1" || ev.Name != "Slam" {
		t.Fatalf("GetEvent = %+v, err=%v", ev, err)
	}
	if evs, err := s.ListEvents("o1"); err != nil || len(evs) != 1 {
		t.Fatalf("ListEvents = %v, err=%v", evs, err)
	}
	if regs, err := s.Registrations("e1"); err != nil || len(regs) != 1 || regs[0].FullName != "Al" {
		t.Fatalf("Registrations = %+v, err=%v", regs, err)
	}

	// Smoke: exercise the rest; tolerate business errors.
	_, _ = s.MyEvents("u1", "a@b.com")
	_, _ = s.PublicEvents(10)
	_, _ = s.GetBrackets("e1")
	_, _ = s.Rounds("e1")
	_, _ = s.MatchesForRound("rd1")
	_, _ = s.EventPoolMatches("e1")
	_, _ = s.BracketMatches("b1")
	_, _ = s.Roster("e1")
	_, _ = s.Standings("e1", "b1", true)
	_, _ = s.Standings("e1", "", false)
	_, _ = s.Checklist("e1")
	_, _ = s.FinanceEntries("e1")
	_, _ = s.ListLeagues("o1")
	_, _ = s.MyLeagues("u1", "a@b.com")
	_, _ = s.GetLeague("l1")
	_, _ = s.LeagueStandings("l1")
	_, _ = s.ListLadder("lb1")
	_, _ = s.LadderHistory("lb1")
	_, _ = s.ListTeamStandings("lb1")
	_, _ = s.TeamFixtures("lb1")
	_, _ = s.ListFlexMatchups("lb1")
	_, _ = s.ListFlexStandings("lb1")
	_, _ = s.MyClubs("u1")
	_, _ = s.GetClub("cl1", "u1")
	_, _ = s.ClubMembers("cl1")
	_, _ = s.ClubEvents("cl1")
	_, _ = s.Followers("u1")
	_, _ = s.Following("u1")
	_, _ = s.SearchUsers("u1", "al")
	_, _ = s.MyFeed("u1")
	_, _ = s.ListFeed("e1", "u1")
	_, _ = s.ListComments("fi1", "u1")
	_, _ = s.PlayerProfile("p1")
	_, _ = s.BusyCourts("e1")
	_, _ = s.DuprConnection("u1")
	_, _ = s.DuprSubmissionStatuses("e1")
	_, _ = s.RosterCSV("e1")
	_, _ = s.ResultsCSV("e1")
	_, _ = s.MyNextMatch("e1", "u1", "a@b.com")
	_, _ = s.PlayoffSeedTeams("b1", "wins")

	// Non-error returns.
	_ = s.MyProfile("u1", "a@b.com")
	_ = s.AccountExists("a@b.com")
	_ = s.IsPremium("u1")
	_ = s.GetPremiumStatus("u1")
	_, _ = s.DuprSsoURL()

	// Owner / authz resolution helpers.
	_, _ = s.OwnerOf("event", "e1")
	_, _ = s.EventIDOfMatch("m1")
	_, _ = s.LeagueIDOfDivision("lb1")
	_, _ = s.LadderOwner("lb1")
	_, _ = s.TeamOwner("lb1")
	_, _ = s.FlexOwner("lb1")
	_, _ = s.LadderOwnerOfEntrant("en1")
	_, _ = s.TeamOwnerOfTeam("t1")
	_, _ = s.FlexOwnerOfMatchup("fm1")
	_ = s.OwnsClub("cl1", "u1")
	_, _ = s.IsLeagueParticipant("l1", "u1", "a@b.com")
	_, _ = s.VerifyAdminPasscode("e1", "1234")
	_, _ = s.AuthorizeRegistrationAction("r1", "tok", "u1")
}

// TestServiceWrites drives the create/record/set/delete methods. Writes echo a
// representation row (with an id) from the fake, so happy paths run; business
// errors are tolerated.
func TestServiceWrites(t *testing.T) {
	s := newFakeSvc(t, seededFake())

	// Creates.
	_, _ = s.CreateLeague("o1", model.CreateLeagueRequest{Name: "New League"})
	_, _ = s.CreateClub("o1", model.CreateClubRequest{Name: "New Club"})

	// Adds.
	_, _ = s.AddChecklistItem("e1", "Bring nets")
	_, _ = s.AddFinanceEntry("e1", model.FinanceEntryRequest{})
	_, _ = s.AddTeam("lb1", model.AddTeamRequest{Name: "Smashers"})
	_, _ = s.AddFlexTeam("lb1", model.AddTeamRequest{Name: "Dinkers"})
	_, _ = s.AddLadderEntrant("lb1", model.AddLadderEntrantRequest{DisplayName: "New"})
	_, _ = s.PostAnnouncement("e1", "Heads up", "Organizer")
	s.AddFeedItem("e1", "match_result", "text", "ref")

	// Records.
	_, _ = s.RecordFixture("lb1", model.RecordFixtureRequest{})
	_, _ = s.RecordFlexResult("lb1", "fm1", model.RecordFlexResultRequest{})
	_, _ = s.RecordLadderResult("lb1", model.RecordLadderResultRequest{})
	_ = s.RecordScore("m1", 11, 7)
	_ = s.RecordSeries("m1", []model.GameScore{{Team1: 11, Team2: 7}})
	_ = s.ForfeitMatch("m1", 1, "forfeit", nil, nil)

	// Sets.
	_ = s.SetChecklistChecked("c1", true)
	_ = s.SetDayCap("e1", 1080)
	_ = s.SetDayEnds("e1", []int{1020})
	_ = s.SetDivisionOrder("e1", []string{"b1"})
	_ = s.SetEventBreaks("e1", []model.ScheduleBreak{{StartMin: 720, EndMin: 780, Label: "Lunch"}})
	_ = s.SetEventPoster("e1", "https://p")
	_, _ = s.SetGameDuration("e1", 30)
	_ = s.SetLeaguePoster("l1", "https://p")
	_ = s.SetMatchDay("m1", 1)
	_, _ = s.SetMatchDuration("m1", 30)
	_ = s.SetStartTime("e1", "2026-06-21T09:00:00Z")
	_ = s.SetSponsorWatermarkSettings("e1", "https://w", 0.1, 0.5, "br")
	_, _ = s.SetPartner("r1", "", "Sam")
	_ = s.ClearSponsorWatermark("e1")

	// Social mutations.
	_ = s.Follow("u1", "u2")
	_ = s.Unfollow("u1", "u2")
	_ = s.JoinClub("cl1", "u1")
	_ = s.LeaveClub("cl1", "u1")
	_, _ = s.ToggleReaction("fi1", "u1", "like")
	_, _ = s.AddComment("fi1", "u1", "a@b.com", "nice")

	// Check-in flow.
	_, _ = s.CheckIn("r1", "manual")
	_ = s.UncheckIn("r1")
	_, _ = s.CheckInByToken("e1", "tok")
	_, _, _ = s.CheckInByPhone("e1", "5551234")

	// Match lifecycle.
	_, _ = s.StartRound("rd1")
	_, _ = s.StartMatch("m1")
	_ = s.UnstartMatch("m1")
	_ = s.SwapMatchPlayer("m1", "p1", "p2")
	_, _ = s.SwapPlayersAcrossMatches("m1", "p1", "m2", "p2")
	_ = s.UpdateRegistrationDetails("r1", "Al New", nil)

	// Deletes.
	_ = s.DeleteChecklistItem("c1")
	_ = s.DeleteFinanceEntry("fe1")
	_ = s.DeleteRound("rd1")
	_ = s.RemoveTeam("t1")
	_ = s.RemoveFlexTeam("t1")
	_ = s.RemoveLadderEntrant("en1")
	_ = s.DeleteComment("fc1", "u1")
	_ = s.DeleteFeedItem("fi1")
	_ = s.DeleteRegistration("r1")
	_ = s.AddEventToLeague("l1", "e1")
	_ = s.RemoveEventFromLeague("l1", "e1")
	_ = s.DeleteEvent("e1")
}
