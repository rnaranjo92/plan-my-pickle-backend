package service

import (
	"errors"
	"testing"
)

// A few service methods enforce ownership THEMSELVES, below the API layer's
// ownerOnly gate. They were invisible to the super-user grant, so a support
// account passed the route check and then got a 403 from inside the service —
// which is what stopped one from adding members to another organizer's league.
//
// These pin the boundary in BOTH directions: staff may manage, a random signed-in
// stranger still may not, and the default (no hook wired) stays strict.

func leagueFake() *fakeSupabase {
	return newFake().
		seed("leagues", `[{"id":"l1","owner_id":"owner-1","name":"Fall"}]`).
		seed("league_members", `[{"id":"lm1","league_id":"l1"}]`)
}

func TestAddLeagueMember_StaffMayManageAnotherOrgsLeague(t *testing.T) {
	s := newFakeSvc(t, leagueFake())
	s.IsStaffEmail = func(e string) bool { return e == "staff@pmp.test" }

	// Not the owner, but staff → must get past the ownership check. Any later
	// business error is fine; ErrForbidden is not.
	_, err := s.AddLeagueMember("l1", "someone-else", "staff@pmp.test",
		"Jenn Esh", "", "6192478482")
	if errors.Is(err, ErrForbidden) {
		t.Fatal("staff was refused on another organizer's league")
	}
}

func TestAddLeagueMember_NonStaffStrangerStillForbidden(t *testing.T) {
	s := newFakeSvc(t, leagueFake())
	s.IsStaffEmail = func(e string) bool { return e == "staff@pmp.test" }

	_, err := s.AddLeagueMember("l1", "someone-else", "randouser@example.com",
		"Jenn Esh", "", "6192478482")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger got err=%v, want ErrForbidden", err)
	}
}

func TestRemoveLeagueMember_StaffAllowedStrangerNot(t *testing.T) {
	s := newFakeSvc(t, leagueFake())
	s.IsStaffEmail = func(e string) bool { return e == "staff@pmp.test" }

	if err := s.RemoveLeagueMember("l1", "lm1", "someone-else",
		"staff@pmp.test"); errors.Is(err, ErrForbidden) {
		t.Fatal("staff was refused a member removal (reversible support action)")
	}
	if err := s.RemoveLeagueMember("l1", "lm1", "someone-else",
		"rando@example.com"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger removal err=%v, want ErrForbidden", err)
	}
}

// With no hook wired (tests, or any caller that doesn't set it) the service must
// fall back to strict ownership rather than letting everyone through.
func TestStaffDefaultsToStrictOwnership(t *testing.T) {
	s := newFakeSvc(t, leagueFake())
	if s.staff("krizhia_roxas29@yahoo.com") {
		t.Fatal("staff() true with no IsStaffEmail hook — must default closed")
	}
	if _, err := s.AddLeagueMember("l1", "someone-else", "krizhia_roxas29@yahoo.com",
		"Jenn Esh", "", "6192478482"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unwired service err=%v, want ErrForbidden", err)
	}
}

// The owner must keep working exactly as before.
func TestOwnerUnaffected(t *testing.T) {
	s := newFakeSvc(t, leagueFake())
	s.IsStaffEmail = func(string) bool { return false }

	if _, err := s.AddLeagueMember("l1", "owner-1", "owner@example.com",
		"Jenn Esh", "", "6192478482"); errors.Is(err, ErrForbidden) {
		t.Fatal("the league OWNER was refused")
	}
}
