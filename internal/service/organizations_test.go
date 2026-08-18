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

// Everything is inert until the migration runs, so this can ship long before
// anyone creates an organization — and a single-club organizer never pays a
// query for a layer they don't have.
func TestOrgsInertWithoutTheMigration(t *testing.T) {
	s := &Service{}
	// A nil store would panic if these actually queried, which is the point:
	// orgsReady() must be checked before any lookup.
	if s.OrgRoleFor("org", "user") != "" {
		t.Error("a role was returned with no organizations table")
	}
	if s.clubOrgAdmin("club", "user") {
		t.Error("org inheritance fired with no organizations table")
	}
	orgs, err := s.MyOrganizations("user")
	if err != nil || len(orgs) != 0 {
		t.Errorf("expected no orgs and no error, got %v / %v", orgs, err)
	}
	// An empty user can't be anything, regardless of the table.
	if s.OrgRoleFor("org", "") != "" {
		t.Error("an empty user id got a role")
	}
}
