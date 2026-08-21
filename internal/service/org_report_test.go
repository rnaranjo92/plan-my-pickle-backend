package service

import (
	"strings"
	"testing"
)

// The viewer role is defined as "reporting only". If the report refuses them,
// the role does nothing at all — this is the test that keeps that honest.
func TestOrgReportCSVIsReadableByAViewer(t *testing.T) {
	f := newFake().
		seed("organizations", `[{"id":"o1","name":"Life Time","owner_id":"owner1"}]`).
		seed("organization_members", `[{"org_id":"o1","user_id":"viewer1","role":"viewer"}]`).
		seed("clubs", `[{"id":"c1","name":"Otay Ranch","city":"Chula Vista","org_id":"o1"}]`).
		seed("club_members", `[{"club_id":"c1","user_id":"m1"}]`).
		seed("events", `[]`)
	s := newFakeSvc(t, f)
	out, err := s.OrgReportCSV("o1", "viewer1")
	if err != nil {
		t.Fatalf("a viewer must be able to read the report: %v", err)
	}
	body := string(out)
	if !strings.Contains(body, "Life Time") {
		t.Fatalf("report doesn't name the organization:\n%s", body)
	}
	if !strings.Contains(body, "Otay Ranch") {
		t.Fatalf("report doesn't list the site:\n%s", body)
	}
	// The window has to be stated, or "Sessions" reads as all-time.
	if !strings.Contains(body, "last 90 days") {
		t.Fatalf("report doesn't say what the session count covers:\n%s", body)
	}
}

// A site that has never run anything is the most actionable row in the file.
// An empty cell reads as missing data; the word is a finding.
func TestOrgReportCSVSaysNeverRatherThanLeavingABlank(t *testing.T) {
	if got := lastPlayedCell(""); got != "Never" {
		t.Fatalf(`lastPlayedCell("") = %q, want "Never"`, got)
	}
	if got := lastPlayedCell("2026-08-17T19:00:00Z"); got != "2026-08-17" {
		t.Fatalf("lastPlayedCell trimmed to %q", got)
	}
}

// Someone with no role in the organization gets nothing — the report carries
// site-by-site numbers for a paying customer.
func TestOrgReportCSVRefusesAStranger(t *testing.T) {
	f := newFake().
		seed("organizations", `[{"id":"o1","name":"Life Time","owner_id":"owner1"}]`).
		seed("organization_members", `[]`)
	s := newFakeSvc(t, f)
	if _, err := s.OrgReportCSV("o1", "stranger"); err != ErrForbidden {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}
