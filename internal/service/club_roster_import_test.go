package service

import "testing"

// A club's spreadsheet is whatever the club already has. These cover the shapes
// people actually paste, because a parser that only accepts our own export is a
// parser nobody can use.
func TestParseRosterCSVReadsNamedColumnsInAnyOrder(t *testing.T) {
	rows, err := parseRosterCSV("Phone,Full Name,Email\n(858) 519-2065,Dave A,DAVE@x.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Name != "Dave A" {
		t.Fatalf("name: %q", rows[0].Name)
	}
	// Lower-cased, or the same person fails to match their own account.
	if rows[0].Email != "dave@x.com" {
		t.Fatalf("email: %q", rows[0].Email)
	}
	if rows[0].Phone != "8585192065" {
		t.Fatalf("phone: %q", rows[0].Phone)
	}
}

// The commonest paste of all: a bare column of addresses, no header.
func TestParseRosterCSVHandlesAHeaderlessColumnOfEmails(t *testing.T) {
	rows, err := parseRosterCSV("a@x.com\nb@x.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 || rows[0].Email != "a@x.com" || rows[1].Email != "b@x.com" {
		t.Fatalf("got %+v", rows)
	}
}

// A first row holding an address is DATA. Treating it as a header silently
// drops a member, which is the worst kind of import bug: it looks like it worked.
func TestParseRosterCSVDoesNotEatAFirstRowThatIsData(t *testing.T) {
	rows, _ := parseRosterCSV("dave@x.com,Dave\nsam@x.com,Sam\n")
	if len(rows) != 2 {
		t.Fatalf("first row is data, not a header; got %d rows", len(rows))
	}
}

// Ragged lines are normal in exported sheets. One short row must not reject the
// other 299.
func TestParseRosterCSVToleratesRaggedRows(t *testing.T) {
	rows, err := parseRosterCSV("Name,Email,Phone\nDave,dave@x.com\nSam,sam@x.com,8585192065\n\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
}

func TestNormalizePhoneMatchesTheSameNumberWrittenAnyWay(t *testing.T) {
	want := "8585192065"
	for _, in := range []string{"(858) 519-2065", "858.519.2065", "+1 858 519 2065", "1-858-519-2065"} {
		if got := normalizePhone(in); got != want {
			t.Fatalf("%q normalized to %q, want %q", in, got, want)
		}
	}
	// Too short to identify anybody — better unmatched than wrong.
	if normalizePhone("2065") != "" {
		t.Fatalf("a 4-digit fragment must not be treated as a phone")
	}
}

// THE DEMOTION TRAP. A club's roster sheet lists its co-owners too. An upsert
// with role "member" would strip their access, and the owner would find out
// when a co-owner couldn't run the Tuesday night.
func TestImportClubRosterNeverDemotesAnExistingMember(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","owner_id":"owner1"}]`).
		seed("club_members", `[{"club_id":"c1","user_id":"u1","role":"co_owner"}]`).
		seed("players", `[{"user_id":"u1","email":"co@x.com","phone":null}]`)
	s := newFakeSvc(t, f)
	out, err := s.ImportClubRoster("c1", "owner1", "Email\nco@x.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Added != 0 {
		t.Fatalf("an existing member must not be re-added; added=%d", out.Added)
	}
	if out.AlreadyIn != 1 {
		t.Fatalf("want alreadyIn=1, got %d", out.AlreadyIn)
	}
	for _, w := range f.written("club_members") {
		if w["user_id"] == "u1" {
			t.Fatalf("wrote to an existing member's row: %+v", w)
		}
	}
}

// Someone with no account can't be added, and must be REPORTED rather than
// silently dropped — the owner needs to know who to send a join link to.
func TestImportClubRosterReportsPeopleItCannotIdentify(t *testing.T) {
	f := newFake().
		seed("clubs", `[{"id":"c1","owner_id":"owner1"}]`).
		seed("club_members", `[]`).
		seed("players", `[]`)
	s := newFakeSvc(t, f)
	out, err := s.ImportClubRoster("c1", "owner1", "Email\nnobody@x.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Added != 0 || len(out.Unmatched) != 1 {
		t.Fatalf("want 0 added / 1 unmatched, got %d / %d", out.Added, len(out.Unmatched))
	}
	if out.Unmatched[0].Email != "nobody@x.com" {
		t.Fatalf("unmatched row lost its email: %+v", out.Unmatched[0])
	}
}

// Importing is a management action. A plain member pasting a roster would be
// adding people to somebody else's club.
func TestImportClubRosterRefusesANonAdmin(t *testing.T) {
	f := newFake().seed("clubs", `[{"id":"c1","owner_id":"owner1"}]`)
	s := newFakeSvc(t, f)
	if _, err := s.ImportClubRoster("c1", "", "Email\na@x.com\n"); err != ErrForbidden {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}
