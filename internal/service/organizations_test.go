package service

import "testing"

// The line between "can change a season" and "can look at the numbers" is the
// whole reason a viewer role exists. A regional manager who needs the report
// must not be handed the ability to delete one.
func TestOrgRolePermissions(t *testing.T) {
	manage := map[string]bool{
		OrgRoleOwner: true, OrgRoleAdmin: true, OrgRoleViewer: false,
		"": false, "regional": false,
	}
	for role, want := range manage {
		if got := orgCanManage(role); got != want {
			t.Errorf("orgCanManage(%q) = %v, want %v", role, got, want)
		}
	}
	read := map[string]bool{
		OrgRoleOwner: true, OrgRoleAdmin: true, OrgRoleViewer: true,
		"": false, "nobody": false,
	}
	for role, want := range read {
		if got := orgCanRead(role); got != want {
			t.Errorf("orgCanRead(%q) = %v, want %v", role, got, want)
		}
	}
	// Case and whitespace come from whatever wrote the row; a role that only
	// works when typed exactly is an access check that fails silently.
	if !orgCanManage("  ADMIN  ") {
		t.Error("role matching is case/space sensitive")
	}
}

// An empty user id is answered WITHOUT touching the database.
//
// The first version of this test asserted much more — that a nil-store Service
// stayed inert — which was wrong about the code rather than about the intent:
// orgsReady() is itself a query, so it panicked before it could be inert. The
// real inertness (no organizations table ⇒ no organizations) needs a store to
// demonstrate and is covered by columnReady's own behaviour.
func TestEmptyUserNeedsNoDatabase(t *testing.T) {
	s := &Service{} // nil store: anything that queries will panic
	if s.OrgRoleFor("org", "") != "" {
		t.Error("an empty user id got a role")
	}
	if s.OrgRoleFor("org", "   ") != "" {
		t.Error("whitespace was treated as a user id")
	}
	if s.clubOrgAdmin("club", "") {
		t.Error("an empty user id inherited club admin")
	}
	orgs, err := s.MyOrganizations("")
	if err != nil || len(orgs) != 0 {
		t.Errorf("expected no orgs and no error, got %v / %v", orgs, err)
	}
}
