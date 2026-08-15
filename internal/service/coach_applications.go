package service

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Coach applications: the way IN to the coach allowlist.
//
// Coach access has always come from the `instructors` allowlist, which only the
// owner can write — so nobody has ever been able to list themselves. What was
// missing was the front door: a coach had to know to email someone, and no
// record survived the conversation.
//
// Approving an application writes the instructors row. The allowlist stays the
// single source of coach access; this only decides who gets proposed for it.

func (s *Service) coachApplicationsReady() bool {
	return s.columnReady("coach_applications", "id")
}

// SubmitCoachApplication records a public application. UNAUTHENTICATED — a coach
// applies before they have an account, which is the point.
func (s *Service) SubmitCoachApplication(a model.CoachApplication) error {
	if !s.coachApplicationsReady() {
		return fmt.Errorf("%w: run add_coach_applications.sql", ErrCoachingUnavailable)
	}
	email := strings.ToLower(strings.TrimSpace(a.Email))
	name := strings.TrimSpace(a.Name)
	if email == "" || !strings.Contains(email, "@") {
		return errors.New("enter a valid email")
	}
	if name == "" {
		return errors.New("enter your name")
	}
	// Already a coach → say so rather than queuing a review that ends in "they
	// already have it". Cheap, and it saves the applicant a wait.
	if existing, _ := s.sb.SelectOne("instructors",
		"email=eq."+store.Q(email)+"&select=id"); existing != nil {
		return errors.New(
			"that email already has coach access — sign in and open the Coach tab")
	}
	// A pending application already exists (the unique index enforces this too,
	// but a duplicate-key error is not a message anyone should have to read).
	if dup, _ := s.sb.SelectOne("coach_applications",
		"email=eq."+store.Q(email)+"&status=eq.pending&select=id"); dup != nil {
		return errors.New(
			"we already have your application — we'll be in touch shortly")
	}
	_, err := s.sb.Insert("coach_applications", map[string]any{
		"email":          email,
		"name":           name,
		"phone":          orNull(strings.TrimSpace(a.Phone)),
		"city":           orNull(strings.TrimSpace(a.City)),
		"certifications": orNull(strings.TrimSpace(a.Certifications)),
		"experience":     orNull(strings.TrimSpace(a.Experience)),
		"has_insurance":  a.HasInsurance,
		"note":           orNull(strings.TrimSpace(a.Note)),
		"status":         "pending",
	})
	return err
}

// ListCoachApplications returns applications by status ("" = pending).
func (s *Service) ListCoachApplications(status string) ([]model.CoachApplication, error) {
	if !s.coachApplicationsReady() {
		return nil, fmt.Errorf("%w: run add_coach_applications.sql", ErrCoachingUnavailable)
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = "pending"
	}
	rows, err := s.sb.Select("coach_applications",
		"status=eq."+store.Q(status)+"&order=created_at.desc&limit=200")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachApplication, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapCoachApplication(r))
	}
	return out, nil
}

// DecideCoachApplication approves or rejects one. Approving ALSO writes the
// instructors allowlist row — that write is the thing that actually grants coach
// access, and doing it here means an approved application can't sit around
// looking approved while the coach still has no access.
func (s *Service) DecideCoachApplication(
	id, decision, note, decidedBy string) error {
	if !s.coachApplicationsReady() {
		return fmt.Errorf("%w: run add_coach_applications.sql", ErrCoachingUnavailable)
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != "approved" && decision != "rejected" {
		return errors.New("decision must be approved or rejected")
	}
	row, err := s.sb.SelectOne("coach_applications", "id=eq."+store.Q(id))
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "status") != "pending" {
		return errors.New("that application has already been decided")
	}

	if decision == "approved" {
		// Grant access FIRST. If this fails the application stays pending, which
		// is recoverable; marking it approved first and then failing would leave
		// a coach told yes with no access and nothing in the queue to fix it.
		if _, aerr := s.AddInstructor(
			asStr(row, "email"), asStr(row, "name")); aerr != nil {
			return aerr
		}
	}
	_, err = s.sb.Update("coach_applications",
		"id=eq."+store.Q(id)+"&status=eq.pending",
		map[string]any{
			"status":        decision,
			"decision_note": orNull(strings.TrimSpace(note)),
			"decided_at":    now(),
			"decided_by":    orNull(strings.TrimSpace(decidedBy)),
		})
	return err
}

func mapCoachApplication(r map[string]any) model.CoachApplication {
	return model.CoachApplication{
		ID:             asStr(r, "id"),
		Email:          asStr(r, "email"),
		Name:           asStr(r, "name"),
		Phone:          asStr(r, "phone"),
		City:           asStr(r, "city"),
		Certifications: asStr(r, "certifications"),
		Experience:     asStr(r, "experience"),
		HasInsurance:   asBool(r, "has_insurance"),
		Note:           asStr(r, "note"),
		Status:         asStr(r, "status"),
		DecisionNote:   asStr(r, "decision_note"),
		CreatedAt:      asStr(r, "created_at"),
	}
}
