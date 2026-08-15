package service

import (
	"errors"
	"fmt"
	"html"
	"log"
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
	row := map[string]any{
		"email":          email,
		"name":           name,
		"phone":          orNull(strings.TrimSpace(a.Phone)),
		"city":           orNull(strings.TrimSpace(a.City)),
		"certifications": orNull(strings.TrimSpace(a.Certifications)),
		"experience":     orNull(strings.TrimSpace(a.Experience)),
		"has_insurance":  a.HasInsurance,
		"note":           orNull(strings.TrimSpace(a.Note)),
		"status":         "pending",
	}
	if _, err := s.sb.Insert("coach_applications", row); err != nil {
		return err
	}
	// Tell the reviewers. Applications sit in `pending` until a human acts, and
	// nothing else in the system surfaces them — a coach would get "we'll be in
	// touch" and the request would rot in a table nobody thought to open.
	//
	// After the insert and never fatal: the coach's application is already safely
	// recorded, and a mail outage must not turn into "please apply again".
	go s.notifyCoachApplication(a, email, name)
	return nil
}

// coachApplicationReviewers are the accounts that can act on an application.
// Deliberately the same two the ownerEmailOnly gate allows — alerting anyone who
// can't actually approve is noise, and missing someone who can is the bug this
// exists to prevent.
var coachApplicationReviewers = []string{
	"rolando.naranjo0420@gmail.com",
	"krizhia_roxas29@yahoo.com",
}

// notifyCoachApplication emails the reviewers that someone applied to coach.
func (s *Service) notifyCoachApplication(a model.CoachApplication, email, name string) {
	if s.Email == nil {
		return
	}
	line := func(label, v string) string {
		if strings.TrimSpace(v) == "" {
			return ""
		}
		return fmt.Sprintf(
			`<p style="margin:0 0 8px;color:#16203a;font-size:14.5px">`+
				`<b style="color:#5b6b80">%s:</b> %s</p>`, html.EscapeString(label), html.EscapeString(v))
	}
	insurance := "Not stated"
	if a.HasInsurance {
		insurance = "Says they carry liability insurance"
	}
	subject := "New coach application — " + name

	htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:560px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">COACH APPLICATION</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">%s wants to coach</h1>
    </div>
    <div style="padding:24px 26px">
      %s%s%s%s%s%s
      <p style="margin:14px 0 0;color:#5b6b80;font-size:13px">Review in the app under
      <b>Profile &rsaquo; Manage coaches</b>. Approving grants coach access; you
      still need to mark them <b>Verified</b> for them to appear on the public
      coaches page.</p>
    </div>
  </div>
  <p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a></p>
</div>`,
		html.EscapeString(name),
		line("Email", email),
		line("Phone", a.Phone),
		line("City", a.City),
		line("Certifications", a.Certifications),
		line("Experience", a.Experience),
		line("Insurance", insurance))

	text := fmt.Sprintf(
		"New coach application\n\nName: %s\nEmail: %s\nPhone: %s\nCity: %s\n"+
			"Certifications: %s\nExperience: %s\nInsurance: %s\n\n"+
			"Review in Profile > Manage coaches. Approving grants coach access; "+
			"mark them Verified to publish them on the coaches page.",
		name, email, a.Phone, a.City, a.Certifications, a.Experience, insurance)

	for _, to := range coachApplicationReviewers {
		if err := s.Email.SendEmail(to, subject, htmlBody, text); err != nil {
			log.Printf("coach application: alert to %s failed: %v", to, err)
		}
	}
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
