package service

import "testing"

// An INVITED league member has no players row until they're placed in a bracket
// or registered for a session. leagueIDsForUser used to resolve membership only
// through player rows and bailed early when there were none — so an invite-only
// league was readable by link but INVISIBLE in MyLeagues ("I was invited but I
// don't see it in my leagues"). These pin the league_members path, which runs
// before that early return.
//
// The fake ignores PostgREST filters and replays whatever a table was seeded
// with, so these assert REACHABILITY (does the members lookup happen at all,
// with no player rows present), not the user_id/email predicate itself.

func TestLeagueIDsForUser_IncludesInviteOnlyMembership(t *testing.T) {
	f := newFake().
		seed("league_members", `[{"id":"lm1","league_id":"l1","user_id":"u9","email":"inv@b.com"}]`)
	s := newFakeSvc(t, f)

	// No players / registrations / entrants seeded => no player rows at all.
	ids, err := s.leagueIDsForUser("u9", "inv@b.com")
	if err != nil {
		t.Fatalf("leagueIDsForUser err = %v", err)
	}
	if !ids["l1"] {
		t.Fatalf("invited member did not see the league: got %v, want l1 present "+
			"(regression: the no-player-rows early return skipped league_members)", ids)
	}
}

// The list and the read gate must agree: if MyLeagues shows a league the viewer
// gate would refuse, the user taps it and gets a 403.
func TestIsLeagueParticipant_MatchesInviteOnlyMembership(t *testing.T) {
	f := newFake().
		seed("league_members", `[{"id":"lm1","league_id":"l1","user_id":"u9","email":"inv@b.com"}]`)
	s := newFakeSvc(t, f)

	ok, err := s.IsLeagueParticipant("l1", "u9", "inv@b.com")
	if err != nil {
		t.Fatalf("IsLeagueParticipant err = %v", err)
	}
	if !ok {
		t.Fatal("invited member is not a participant — the league would list but 403 on open")
	}
}

// A caller with no membership and no player rows must still resolve to nothing,
// so the new lookup can't hand out leagues it shouldn't.
func TestLeagueIDsForUser_EmptyWithoutMembership(t *testing.T) {
	s := newFakeSvc(t, newFake())

	ids, err := s.leagueIDsForUser("u9", "inv@b.com")
	if err != nil {
		t.Fatalf("leagueIDsForUser err = %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("non-member resolved to %v, want empty", ids)
	}
}
