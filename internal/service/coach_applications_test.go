package service

import (
	"strings"
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// The alert only fires on a real public submission, which is exactly the path
// nobody exercises by hand — so pin the body and the recipients here.
func TestNotifyCoachApplication(t *testing.T) {
	mock := &gateway.MockEmail{}
	s := &Service{Email: mock}

	s.notifyCoachApplication(model.CoachApplication{
		Phone:          "619-555-0134",
		City:           "Chula Vista, CA",
		Certifications: "PPR Certified #40219",
		Experience:     "8 years, mostly 3.0-4.0 adults",
		HasInsurance:   true,
	}, "coach@example.com", "Ana Delgado")

	if len(mock.Sent) != len(coachApplicationReviewers) {
		t.Fatalf("sent %d emails, want one per reviewer (%d)",
			len(mock.Sent), len(coachApplicationReviewers))
	}
	// Every reviewer who can approve must be told; the whole point is that an
	// application can't sit unseen.
	for i, want := range coachApplicationReviewers {
		if mock.Sent[i].To != want {
			t.Fatalf("recipient %d = %q, want %q", i, mock.Sent[i].To, want)
		}
	}

	m := mock.Sent[0]
	if !strings.Contains(m.Subject, "Ana Delgado") {
		t.Fatalf("subject should name the applicant, got %q", m.Subject)
	}
	// The reviewer decides from the email, so the facts they judge on have to be
	// in it — not just a "you have a new application" nudge.
	for _, want := range []string{
		"Ana Delgado", "coach@example.com", "619-555-0134", "Chula Vista, CA",
		"PPR Certified #40219", "8 years, mostly 3.0-4.0 adults",
		"liability insurance",
		"Verified", // approving alone does not publish them
	} {
		if !strings.Contains(m.HTML, want) {
			t.Fatalf("html missing %q", want)
		}
		if !strings.Contains(m.Text, want) {
			t.Fatalf("text missing %q", want)
		}
	}
}

// Blank optional fields must not render empty label rows.
func TestNotifyCoachApplicationSkipsBlankFields(t *testing.T) {
	mock := &gateway.MockEmail{}
	s := &Service{Email: mock}
	s.notifyCoachApplication(model.CoachApplication{}, "c@example.com", "Sam")

	m := mock.Sent[0]
	for _, unwanted := range []string{"Phone:", "City:", "Certifications:"} {
		if strings.Contains(m.HTML, unwanted) {
			t.Fatalf("html should omit blank field row %q", unwanted)
		}
	}
	// Insurance is always stated — "not stated" is itself information a reviewer
	// wants, unlike a blank phone number.
	if !strings.Contains(m.HTML, "Not stated") {
		t.Fatal("insurance should always render")
	}
}

// A mail outage must never cost a coach their application.
func TestNotifyCoachApplicationNoGateway(t *testing.T) {
	(&Service{}).notifyCoachApplication(model.CoachApplication{}, "c@example.com", "Sam")
}
