package service

import "testing"

// Being staff at ONE branch is not permission to read another branch's
// numbers. The organization is what says these sites belong together and who
// may see across them, so the check is the caller's ORG role -- not their role
// at the club they happen to be looking at.
func TestSiblingClubsRefusesSomeoneWithNoOrgRole(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","org_id":"o1","name":"Otay Ranch"}]`).
		seed("organizations", `[{"id":"o1","owner_id":"owner1"}]`).
		seed("organization_members", `[]`)
	s := newFakeSvc(t, f)
	out, err := s.SiblingClubs("c1", "stranger")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a non-member of the org saw %d sibling clubs", len(out))
	}
}

// A club with no organization has no siblings, and that is not an error --
// it feeds a section that simply doesn't appear.
func TestSiblingClubsIsEmptyWithoutAnOrganization(t *testing.T) {
	f := newFake().seed("clubs", `[{"id":"c1","name":"Solo Club"}]`)
	s := newFakeSvc(t, f)
	out, err := s.SiblingClubs("c1", "u1")
	if err != nil {
		t.Fatalf("no organization should be empty, not an error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d siblings for a club in no org", len(out))
	}
}

// The happy path: an org viewer sees the other branches.
func TestSiblingClubsListsTheOtherBranches(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c2","org_id":"o1","name":"Del Mar","city":"Del Mar"}]`).
		seed("organizations", `[{"id":"o1","owner_id":"owner1"}]`).
		seed("organization_members", `[{"org_id":"o1","user_id":"viewer1","role":"viewer"}]`)
	s := newFakeSvc(t, f)
	out, err := s.SiblingClubs("c1", "viewer1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 || out[0].Name != "Del Mar" {
		t.Fatalf("want the sibling branch, got %+v", out)
	}
}
