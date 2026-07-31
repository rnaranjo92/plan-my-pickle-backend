package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// phoneOf returns the account's stored phone (raw), or "".
func (s *Service) phoneOf(userID string) string {
	if userID == "" {
		return ""
	}
	if row, _ := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=phone"); row != nil {
		if p := strings.TrimSpace(asStr(row, "phone")); p != "" {
			return p
		}
	}
	// Fall back to the phone captured at sign-up (auth user_metadata) and persist
	// it to the profile, so a student's number is tied to their data immediately —
	// before they've verified it — and a coach's text invite links right away.
	if u, err := s.sb.GetAuthUser(userID); err == nil && u != nil {
		if um, ok := u["user_metadata"].(map[string]any); ok {
			p := strings.TrimSpace(asStr(um, "phone"))
			if p != "" {
				_, _ = s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
					"user_id": userID,
					"phone":   p,
				})
				return p
			}
		}
	}
	return ""
}

// Instructor Mode — Phase 1: coach↔student video feedback.
//
// A coach keeps a roster (coach_students) and, per student, a thread of clips
// (coaching_videos) and text comments (coaching_feedback). The roster row's id IS
// the thread id. Either party may upload a clip or comment; the counterpart gets
// a bell + push notification. Students are addressed by email so a coach can add
// someone before their account id is known; the id is resolved best-effort at add
// time and backfilled when the student first opens their coaching view.
//
// All coaching tables are gated behind columnReady so the code ships safely
// before add_coaching.sql runs (the feature just stays dark).

var ErrCoachingUnavailable = errors.New("coaching isn't set up yet")

func (s *Service) coachingReady() bool {
	return s.columnReady("coach_students", "id")
}

// userIDByEmail resolves an account-linked player's auth user id by email, or ""
// if there's no account for that email yet. Mirrors playerIDByEmail but returns
// the user_id (the account), which is what notifications key off of.
func (s *Service) userIDByEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ""
	}
	row, err := s.sb.SelectOne("players",
		"email=eq."+store.Q(email)+"&user_id=not.is.null&select=user_id&limit=1")
	if err != nil || row == nil {
		return ""
	}
	return asStr(row, "user_id")
}

// coachingName resolves a display name for coaching, preferring the account's
// PROFILE name (pmp_profiles.full_name — what they set in "Basic info") over an
// old per-event registration name. Falls back to the shared resolver (auth
// metadata / players / email) when there's no profile name.
func (s *Service) coachingName(userID string) string {
	if userID == "" {
		return ""
	}
	if row, _ := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=full_name"); row != nil {
		if n := strings.TrimSpace(asStr(row, "full_name")); n != "" {
			return n
		}
	}
	return s.resolveDisplayName(userID, "")
}

// --- Instructor (coach) allowlist ---

// IsInstructor reports whether an account email is on the coach allowlist
// (instructors table). The two founding-owner emails are handled by the caller
// (they're always coaches); this only checks the DB table. Safe before the
// migration runs (returns false → owners-only).
func (s *Service) IsInstructor(userID, email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !s.columnReady("instructors", "id") {
		return false
	}
	row, err := s.sb.SelectOne("instructors",
		"email=eq."+store.Q(email)+"&select=id")
	return err == nil && row != nil
}

func mapInstructor(row map[string]any) model.Instructor {
	return model.Instructor{
		ID:        asStr(row, "id"),
		Email:     asStr(row, "email"),
		Name:      asStr(row, "name"),
		CreatedAt: asStr(row, "created_at"),
	}
}

// ListInstructors returns the coach allowlist, newest first (owner-only surface).
func (s *Service) ListInstructors() ([]model.Instructor, error) {
	if !s.columnReady("instructors", "id") {
		return []model.Instructor{}, nil
	}
	rows, err := s.sb.Select("instructors", "order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Instructor, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapInstructor(r))
	}
	return out, nil
}

// AddInstructor grants coach access to an email (idempotent on the email).
func (s *Service) AddInstructor(email, name string) (model.Instructor, error) {
	if !s.columnReady("instructors", "id") {
		return model.Instructor{}, ErrCoachingUnavailable
	}
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || !strings.Contains(email, "@") {
		return model.Instructor{}, errors.New("enter a valid email")
	}
	if existing, _ := s.sb.SelectOne("instructors",
		"email=eq."+store.Q(email)+"&select=id"); existing != nil {
		return mapInstructor(existing), nil // already a coach
	}
	row := map[string]any{"email": email, "name": orNull(name)}
	if uid := s.userIDByEmail(email); uid != "" {
		row["user_id"] = uid
	}
	ins, err := s.sb.Insert("instructors", row)
	if err != nil {
		return model.Instructor{}, err
	}
	if len(ins) == 0 {
		return model.Instructor{}, errors.New("could not add that coach")
	}
	return mapInstructor(ins[0]), nil
}

// RemoveInstructor revokes coach access.
func (s *Service) RemoveInstructor(id string) error {
	if !s.columnReady("instructors", "id") {
		return ErrCoachingUnavailable
	}
	return s.sb.Delete("instructors", "id=eq."+store.Q(id))
}

func mapCoachStudent(row map[string]any) model.CoachStudent {
	return model.CoachStudent{
		ID:             asStr(row, "id"),
		CoachID:        asStr(row, "coach_id"),
		StudentEmail:   asStr(row, "student_email"),
		StudentPhone:   asStr(row, "student_phone"),
		StudentName:    asStr(row, "student_name"),
		StudentID:      asStr(row, "student_id"),
		SkillLevel:     asStr(row, "skill_level"),
		CreatedAt:      asStr(row, "created_at"),
		LastActivityAt: asStr(row, "last_activity_at"),
		CoachNote:      asStr(row, "coach_note"),
		SharedNote:     asStr(row, "shared_note"),
	}
}

// readsReady gates the read-receipt/unread feature on add_coaching_reads.sql.
func (s *Service) readsReady() bool {
	return s.columnReady("coaching_reads", "id") &&
		s.columnReady("coach_students", "last_activity_at")
}

// markThreadRead records that a viewer has seen a thread up to now. Best-effort;
// unread is a nicety, never a correctness concern.
func (s *Service) markThreadRead(userID, threadID string) {
	if !s.readsReady() || userID == "" || threadID == "" {
		return
	}
	if _, err := s.sb.Upsert("coaching_reads", "user_id,coach_student_id", map[string]any{
		"user_id":          userID,
		"coach_student_id": threadID,
		"last_seen_at":     now(),
	}); err != nil {
		log.Printf("coaching: markThreadRead(%s): %v", threadID, err)
	}
}

// bumpThreadActivity stamps a thread's last_activity_at so the counterpart's list
// can flag it as unread.
func (s *Service) bumpThreadActivity(threadID string) {
	if !s.readsReady() || threadID == "" {
		return
	}
	if _, err := s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
		map[string]any{"last_activity_at": now()}); err != nil {
		log.Printf("coaching: bumpThreadActivity(%s): %v", threadID, err)
	}
}

// fetchReads returns userID's last_seen_at per thread (RFC3339 string), for the
// given thread ids. Empty map if the feature isn't migrated yet.
func (s *Service) fetchReads(userID string, threadIDs []string) map[string]string {
	out := map[string]string{}
	if !s.readsReady() || userID == "" || len(threadIDs) == 0 {
		return out
	}
	rows, err := s.sb.Select("coaching_reads",
		"user_id=eq."+store.Q(userID)+"&coach_student_id=in.("+
			strings.Join(threadIDs, ",")+")&select=coach_student_id,last_seen_at")
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[asStr(r, "coach_student_id")] = asStr(r, "last_seen_at")
	}
	return out
}

// applyUnread sets HasUnread on each row for viewer userID: a thread is unread
// when its last_activity_at is newer than the viewer's last_seen_at (or the
// viewer has never opened it). Threads with no activity timestamp are never
// unread.
func (s *Service) applyUnread(userID string, rows []model.CoachStudent) {
	if !s.readsReady() || len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	reads := s.fetchReads(userID, ids)
	for i := range rows {
		act := strings.TrimSpace(rows[i].LastActivityAt)
		if act == "" {
			continue
		}
		seen, ok := reads[rows[i].ID]
		rows[i].HasUnread = !ok || act > seen // ISO-8601 strings sort chronologically
	}
}

// AddCoachStudent adds a student to a coach's roster by email. Idempotent-ish: a
// duplicate (same coach + email) is rejected by the unique index, surfaced as a
// friendly error.
func (s *Service) AddCoachStudent(coachID, email, phone, name, level string) (model.CoachStudent, error) {
	if !s.coachingReady() {
		return model.CoachStudent{}, ErrCoachingUnavailable
	}
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	level = strings.TrimSpace(level)
	rawPhone := strings.TrimSpace(phone)
	np := normPhone(rawPhone)
	// Phone invites need add_coach_student_phone.sql (student_phone column +
	// nullable email). Until it runs, degrade to email-only so Add Student never
	// breaks.
	phoneReady := s.columnReady("coach_students", "student_phone")
	if !phoneReady {
		np = ""
		rawPhone = ""
	}
	if email != "" && !strings.Contains(email, "@") {
		return model.CoachStudent{}, errors.New("enter a valid student email")
	}
	if email == "" && (!phoneReady || len(np) < 10) {
		if !phoneReady {
			return model.CoachStudent{}, errors.New("enter a valid student email")
		}
		return model.CoachStudent{}, errors.New("enter the student's email or phone")
	}
	// Already on this coach's roster (by email or phone)?
	if email != "" {
		if existing, _ := s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_email=eq."+store.Q(email)+"&select=id"); existing != nil {
			return model.CoachStudent{}, errors.New("that student is already on your roster")
		}
	}
	if np != "" {
		if existing, _ := s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_phone=eq."+store.Q(np)+"&select=id"); existing != nil {
			return model.CoachStudent{}, errors.New("that student is already on your roster")
		}
	}
	row := map[string]any{
		"coach_id":      coachID,
		"student_email": orNull(email),
		"student_name":  orNull(name),
	}
	if phoneReady {
		row["student_phone"] = orNull(np)
	}
	if level != "" && s.columnReady("coach_students", "skill_level") {
		row["skill_level"] = level
	}
	// Resolve to an existing account (by email, else phone) so a registered
	// student links immediately and we skip the invite.
	resolved := s.userIDByEmail(email)
	if resolved == "" && np != "" {
		resolved = s.userIDByPhone(np)
	}
	if resolved != "" {
		row["student_id"] = resolved
	}
	// Not resolved to an account yet → mint an invite token that binds the
	// relationship on ANY claim (survives an email/phone mismatch at signup).
	inviteToken := ""
	if resolved == "" && s.columnReady("coach_students", "invite_token") {
		inviteToken = newID()
		row["invite_token"] = inviteToken
	}
	ins, err := s.sb.Insert("coach_students", row)
	if err != nil {
		return model.CoachStudent{}, err
	}
	if len(ins) == 0 {
		return model.CoachStudent{}, errors.New("could not add that student")
	}
	// Not on PlanMyPickle yet → invite via whatever channels were given (they'll
	// auto-link when they sign up with that email/phone). Best-effort, off-path.
	if resolved == "" {
		if email != "" {
			go s.sendCoachInvite(coachID, email, name, inviteToken)
		}
		if rawPhone != "" {
			go s.sendCoachInviteSMS(coachID, rawPhone, inviteToken)
		}
	}
	return mapCoachStudent(ins[0]), nil
}

// coachInviteURL builds the claim link. A token binds on claim regardless of the
// email/phone the invitee signs up with; email is a fallback prefill hint.
func coachInviteURL(token, email string) string {
	u := "https://app.planmypickle.com/?invite=coaching"
	if token != "" {
		u += "&ct=" + url.QueryEscape(token)
	}
	if email != "" {
		u += "&email=" + url.QueryEscape(email)
	}
	return u
}

// ClaimCoachInvite binds an invite token to the caller (sets student_id on the
// still-unlinked roster row), so the relationship links no matter which
// email/phone the student signed up with. Returns the thread id.
func (s *Service) ClaimCoachInvite(userID, token string) (string, error) {
	if !s.coachingReady() {
		return "", ErrCoachingUnavailable
	}
	token = strings.TrimSpace(token)
	if token == "" || userID == "" {
		return "", ErrForbidden
	}
	if !s.columnReady("coach_students", "invite_token") {
		return "", errors.New("invites aren't available yet")
	}
	row, err := s.sb.SelectOne("coach_students",
		"invite_token=eq."+store.Q(token)+"&select=id,student_id,coach_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	threadID := asStr(row, "id")
	if sid := asStr(row, "student_id"); sid != "" {
		if sid == userID {
			return threadID, nil // already claimed by this user — idempotent
		}
		return "", ErrForbidden // already claimed by someone else
	}
	if _, err := s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
		map[string]any{"student_id": userID, "invite_token": nil}); err != nil {
		return "", err
	}
	if coachID := asStr(row, "coach_id"); coachID != "" {
		who := s.coachingName(userID)
		if who == "" {
			who = "A student"
		}
		s.notifyUser(coachID, "coaching", userID, who,
			who+" joined your coaching", "coaching:"+threadID)
	}
	return threadID, nil
}

// userIDByPhone resolves an account-linked player by normalized phone (last-10),
// or "" if none. players.phone is compared on its last-10 digits.
func (s *Service) userIDByPhone(np string) string {
	if len(np) < 10 {
		return ""
	}
	// PostgREST can't normalize server-side, so fetch account-linked players whose
	// phone ends with these 10 digits (a like filter) and confirm in Go.
	rows, err := s.sb.Select("players",
		"phone=like.*"+store.Q(np)+"&user_id=not.is.null&select=user_id,phone&limit=20")
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if normPhone(asStr(r, "phone")) == np {
			return asStr(r, "user_id")
		}
	}
	return ""
}

// sendCoachInviteSMS texts a not-yet-registered student a link to join. They must
// sign up with this phone so the roster row auto-links. No-op if SMS isn't set up
// or the number isn't textable.
func (s *Service) sendCoachInviteSMS(coachID, phone, token string) {
	if s.Sms == nil || !gateway.SmsReachable(phone) {
		return
	}
	coach := s.coachingName(coachID)
	if strings.TrimSpace(coach) == "" {
		coach = "Your coach"
	}
	body := fmt.Sprintf(
		"%s invited you to PlanMyPickle to share pickleball clips & feedback. Join here: %s",
		coach, coachInviteURL(token, ""))
	if r, err := s.Sms.Send(phone, body); err != nil || !r.OK {
		log.Printf("coaching: invite SMS to %s failed: %v", phone, err)
	}
}

// sendCoachInvite emails a not-yet-registered student a link to join
// PlanMyPickle. They must sign up with the SAME email the coach used so the
// roster row auto-links (backfilled on their first coaching view). No-op if the
// email gateway isn't configured.
func (s *Service) sendCoachInvite(coachID, studentEmail, studentName, token string) {
	if s.Email == nil || !s.Email.Live() {
		return
	}
	coach := s.coachingName(coachID)
	if strings.TrimSpace(coach) == "" {
		coach = "Your coach"
	}
	joinURL := coachInviteURL(token, studentEmail)
	esc := html.EscapeString
	hi := ""
	if strings.TrimSpace(studentName) != "" {
		hi = " " + esc(strings.TrimSpace(studentName))
	}
	subject := coach + " wants to coach you on PlanMyPickle"
	htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">COACHING INVITE</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">%s wants to coach you</h1>
    </div>
    <div style="padding:24px 26px">
      <p style="margin:0 0 10px;color:#16203a;font-size:15px">Hi%s — <b>%s</b> wants to share video clips and personalized feedback with you on PlanMyPickle.</p>
      <p style="margin:0 0 4px;color:#16203a;font-size:15px">Join with <b>%s</b> so your clips and feedback link up automatically.</p>
      <a href="%s" style="display:block;margin:22px 0 4px;background:#f5c518;color:#16203a;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">Join PlanMyPickle</a>
      <p style="margin:10px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">Free to join. Then open <b>You &rsaquo; My Coaching</b> to see your feedback.</p>
    </div>
  </div>
  <p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a></p>
</div>`, esc(coach), hi, esc(coach), esc(studentEmail), joinURL)
	text := fmt.Sprintf("%s wants to coach you on PlanMyPickle.\n\nJoin with %s so your clips and feedback link up automatically:\n%s\n\nThen open You > My Coaching to see your feedback.",
		coach, studentEmail, joinURL)
	if err := s.Email.SendEmail(studentEmail, subject, htmlBody, text); err != nil {
		log.Printf("coaching: invite email to %s failed: %v", studentEmail, err)
	}
}

// ListCoachStudents returns a coach's roster, newest first, each with its clip count.
func (s *Service) ListCoachStudents(coachID string) ([]model.CoachStudent, error) {
	if !s.coachingReady() {
		return []model.CoachStudent{}, nil
	}
	// Newest-activity-first when the activity column exists (from add_coaching_reads),
	// otherwise newest-added-first.
	order := "created_at.desc"
	if s.readsReady() {
		order = "last_activity_at.desc"
	}
	rows, err := s.sb.Select("coach_students",
		"coach_id=eq."+store.Q(coachID)+"&order="+order)
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachStudent, 0, len(rows))
	for _, r := range rows {
		cs := mapCoachStudent(r)
		cs.VideoCount = s.threadVideoCount(cs.ID)
		out = append(out, cs)
	}
	s.applyUnread(coachID, out)
	return out, nil
}

// RemoveCoachStudent deletes a roster row (and its clips/feedback via cascade).
func (s *Service) RemoveCoachStudent(coachID, id string) error {
	if !s.coachingReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coach_students", "id=eq."+store.Q(id)+"&select=coach_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	return s.sb.Delete("coach_students", "id=eq."+store.Q(id))
}

// ListStudentThreads returns the threads addressed to this student's email (their
// view of "My Coaching"). Also backfills student_id on any roster row that hasn't
// been linked yet, so future coach→student notifications reach them.
func (s *Service) ListStudentThreads(studentID, email string) ([]model.CoachStudent, error) {
	if !s.coachingReady() {
		return []model.CoachStudent{}, nil
	}
	email = strings.ToLower(strings.TrimSpace(email))
	np := normPhone(s.phoneOf(studentID))
	if email == "" && np == "" {
		return []model.CoachStudent{}, nil
	}
	// Threads addressed to this student by email OR phone. Two queries + dedup
	// avoids fiddly PostgREST or() escaping.
	byID := map[string]map[string]any{}
	if email != "" {
		if rows, e := s.sb.Select("coach_students",
			"student_email=eq."+store.Q(email)+"&order=created_at.desc"); e == nil {
			for _, r := range rows {
				byID[asStr(r, "id")] = r
			}
		}
	}
	if np != "" {
		if rows, e := s.sb.Select("coach_students",
			"student_phone=eq."+store.Q(np)+"&order=created_at.desc"); e == nil {
			for _, r := range rows {
				byID[asStr(r, "id")] = r
			}
		}
	}
	rows := make([]map[string]any, 0, len(byID))
	for _, r := range byID {
		rows = append(rows, r)
	}
	out := make([]model.CoachStudent, 0, len(rows))
	for _, r := range rows {
		cs := mapCoachStudent(r)
		// Backfill the account link the first time we see this student logged in.
		if cs.StudentID == "" && studentID != "" {
			if _, uerr := s.sb.Update("coach_students", "id=eq."+store.Q(cs.ID),
				map[string]any{"student_id": studentID}); uerr == nil {
				cs.StudentID = studentID
			}
		}
		cs.CoachName = s.coachingName(cs.CoachID)
		cs.VideoCount = s.threadVideoCount(cs.ID)
		cs.CoachNote = "" // the coach's private note about the student is never sent to them
		cs.SkillLevel = "" // coach's assessment — coach-only
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	s.applyUnread(studentID, out)
	return out, nil
}

func (s *Service) threadVideoCount(threadID string) int {
	rows, err := s.sb.Select("coaching_videos",
		"coach_student_id=eq."+store.Q(threadID)+"&select=id")
	if err != nil {
		return 0
	}
	return len(rows)
}

// threadMembership loads a thread and the caller's role in it ("coach"|"student"),
// or ErrForbidden if the caller is neither. email is the caller's email (for the
// student-side match).
func (s *Service) threadMembership(threadID, userID, email string) (model.CoachStudent, string, error) {
	row, err := s.sb.SelectOne("coach_students", "id=eq."+store.Q(threadID))
	if err != nil {
		return model.CoachStudent{}, "", err
	}
	if row == nil {
		return model.CoachStudent{}, "", ErrNotFound
	}
	cs := mapCoachStudent(row)
	email = strings.ToLower(strings.TrimSpace(email))
	if userID != "" && cs.CoachID == userID {
		return cs, "coach", nil
	}
	// Student match: by linked account ID (survives email drift once the
	// account is connected), then by email, then by phone.
	studentMatch := userID != "" && cs.StudentID == userID
	if !studentMatch {
		studentMatch = email != "" && strings.EqualFold(cs.StudentEmail, email)
	}
	if !studentMatch && cs.StudentPhone != "" && userID != "" {
		studentMatch = normPhone(s.phoneOf(userID)) == cs.StudentPhone
	}
	if studentMatch {
		// Backfill the account link opportunistically.
		if cs.StudentID == "" && userID != "" {
			if _, uerr := s.sb.Update("coach_students", "id=eq."+store.Q(cs.ID),
				map[string]any{"student_id": userID}); uerr == nil {
				cs.StudentID = userID
			}
		}
		return cs, "student", nil
	}
	return model.CoachStudent{}, "", ErrForbidden
}

// coachingVideoPath extracts the object path within the coaching-videos bucket
// from a stored value — whether it's a bare path (new uploads) or a legacy public
// URL (…/object/public/coaching-videos/<path>?…). Lets us sign both uniformly.
func coachingVideoPath(stored string) string {
	stored = strings.TrimSpace(stored)
	const marker = "/coaching-videos/"
	if i := strings.Index(stored, marker); i >= 0 {
		p := stored[i+len(marker):]
		if q := strings.IndexByte(p, '?'); q >= 0 {
			p = p[:q]
		}
		return p
	}
	return stored
}

// signCoachingVideos rewrites each clip's VideoURL to a short-lived SIGNED URL
// (the bucket is private). Best-effort: on any failure the stored value is left
// as-is, so a signing blip degrades gracefully rather than hiding clips.
func (s *Service) signCoachingVideos(vids []model.CoachingVideo) {
	if len(vids) == 0 {
		return
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(vids))
	for _, v := range vids {
		p := coachingVideoPath(v.VideoURL)
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	signed, err := s.sb.SignedURLs("coaching-videos", paths, 6*60*60) // 6h
	if err != nil || len(signed) == 0 {
		return
	}
	for i := range vids {
		if u, ok := signed[coachingVideoPath(vids[i].VideoURL)]; ok {
			vids[i].VideoURL = u
		}
	}
}

func (s *Service) pbVisionReady() bool {
	return s.columnReady("coaching_pbvision", "coach_student_id")
}

// GetPBVision returns the PB Vision analytics for a thread (member-gated). When
// the column/feature isn't live or no report is synced, Connected is false so
// the UI shows the "not connected yet" state instead of erroring.
func (s *Service) GetPBVision(threadID, userID, email string) (model.PBVisionStats, error) {
	if !s.coachingReady() {
		return model.PBVisionStats{}, ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return model.PBVisionStats{}, err
	}
	if !s.pbVisionReady() {
		return model.PBVisionStats{Connected: false}, nil
	}
	row, err := s.sb.SelectOne("coaching_pbvision",
		"coach_student_id=eq."+store.Q(threadID))
	if err != nil {
		return model.PBVisionStats{}, err
	}
	if row == nil {
		return model.PBVisionStats{Connected: false}, nil
	}
	out := model.PBVisionStats{
		Connected:    true,
		Rating:       asFloatPtr(row, "rating"),
		LastSyncedAt: asStr(row, "last_synced_at"),
	}
	if m, ok := row["stats"].(map[string]any); ok {
		out.Stats = m
	}
	return out, nil
}

// GetThreadPBVisionAnalysis returns the detected players from the analysis
// relevant to this thread — one INITIATED here, or one where this thread's
// student was ASSIGNED a player by the coach (so a single analysis covers up to
// 4 students). A coach viewer also gets their roster + current assignments so
// they can distribute the players; a student gets only their own tagged player.
func (s *Service) GetThreadPBVisionAnalysis(threadID, userID, email string) (model.PBVisionAnalysis, error) {
	if !s.coachingReady() {
		return model.PBVisionAnalysis{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.PBVisionAnalysis{}, err
	}
	if !s.pbvisionJobsReady() {
		return model.PBVisionAnalysis{Ready: false}, nil
	}
	hasAssign := s.columnReady("coaching_pbvision_assignments", "id")
	sel := func(id string) string {
		q := "status=eq.ready&limit=1&select=id,report_url,insights"
		if s.columnReady("coaching_pbvision_jobs", "tagged_avatar_id") {
			q += ",tagged_avatar_id"
		}
		if s.columnReady("coaching_pbvision_jobs", "source_video_id") {
			q += ",source_video_id"
		}
		return id + "&" + q
	}
	// Prefer a job initiated in this thread; else one assigned to this thread.
	row, _ := s.sb.SelectOne("coaching_pbvision_jobs",
		sel("coach_student_id=eq."+store.Q(threadID)+"&order=updated_at.desc"))
	if row == nil && hasAssign {
		if a, _ := s.sb.SelectOne("coaching_pbvision_assignments",
			"coach_student_id=eq."+store.Q(threadID)+
				"&order=created_at.desc&limit=1&select=job_id"); a != nil {
			row, _ = s.sb.SelectOne("coaching_pbvision_jobs",
				sel("id=eq."+store.Q(asStr(a, "job_id"))))
		}
	}
	if row == nil {
		return model.PBVisionAnalysis{Ready: false}, nil
	}
	jobID := asStr(row, "id")
	out := model.PBVisionAnalysis{
		Ready:      true,
		JobID:      jobID,
		ReportURL:  asStr(row, "report_url"),
		Players:    parsePBVisionPlayers(row["insights"]),
		Highlights: parsePBVisionHighlights(row["insights"]),
		ViewerRole: role,
	}
	// This thread's tagged player: an explicit assignment wins; else the legacy
	// single tagged_avatar_id (only meaningful for a job initiated here).
	if hasAssign {
		if a, _ := s.sb.SelectOne("coaching_pbvision_assignments",
			"job_id=eq."+store.Q(jobID)+"&coach_student_id=eq."+store.Q(threadID)+
				"&select=avatar_id"); a != nil {
			out.TaggedID = asIntPtr(a, "avatar_id")
		}
	}
	if out.TaggedID == nil {
		out.TaggedID = asIntPtr(row, "tagged_avatar_id")
	}
	// Coach view: roster (to assign) + this analysis's current assignments.
	if role == "coach" {
		nameByThread := map[string]string{}
		if rosterRows, rerr := s.sb.Select("coach_students",
			"coach_id=eq."+store.Q(cs.CoachID)+
				"&select=id,student_name,student_email&order=created_at.desc"); rerr == nil {
			for _, r := range rosterRows {
				name := asStr(r, "student_name")
				if name == "" {
					name = asStr(r, "student_email")
				}
				tid := asStr(r, "id")
				nameByThread[tid] = name
				out.Roster = append(out.Roster,
					model.CoachStudentBrief{ThreadID: tid, Name: name})
			}
		}
		if hasAssign {
			if asgRows, aerr := s.sb.Select("coaching_pbvision_assignments",
				"job_id=eq."+store.Q(jobID)+"&select=avatar_id,coach_student_id"); aerr == nil {
				for _, r := range asgRows {
					tid := asStr(r, "coach_student_id")
					out.Assignments = append(out.Assignments, model.PBVisionAssignment{
						AvatarID:        asInt(r, "avatar_id"),
						StudentThreadID: tid,
						StudentName:     nameByThread[tid],
					})
				}
			}
		}
	}
	// Sign the source clip so the highlights (time-ranges into it) can play.
	if len(out.Highlights) > 0 {
		if svid := asStr(row, "source_video_id"); svid != "" {
			if vrow, _ := s.sb.SelectOne("coaching_videos",
				"id=eq."+store.Q(svid)+"&select=video_url"); vrow != nil {
				path := coachingVideoPath(asStr(vrow, "video_url"))
				if signed, serr := s.sb.SignedURLs("coaching-videos",
					[]string{path}, 6*60*60); serr == nil {
					out.SourceVideoURL = signed[path]
				}
			}
		}
	}
	return out, nil
}

// SetPBVisionAssignment maps a detected player (avatarID) to a student for one
// analysis. A student may only assign THEMSELVES; a coach may assign any of
// their students. avatarID < 0 clears the target's assignment. Both unique
// constraints (one player↔one student) are honored by clearing conflicts first.
func (s *Service) SetPBVisionAssignment(threadID, userID, email, jobID string, avatarID int, targetThreadID string) error {
	if !s.coachingReady() || !s.pbvisionJobsReady() {
		return ErrCoachingUnavailable
	}
	if !s.columnReady("coaching_pbvision_assignments", "id") {
		return errors.New("player assignment isn't available yet")
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return err
	}
	job, err := s.sb.SelectOne("coaching_pbvision_jobs",
		"id=eq."+store.Q(jobID)+"&select=id,coach_student_id")
	if err != nil {
		return err
	}
	if job == nil {
		return ErrNotFound
	}
	// OBJECT-LEVEL AUTH: the job must belong to the caller, or anyone could bind
	// a foreign job to their thread and read its analysis. A student may only
	// touch a job initiated in their OWN thread; a coach must own the job's
	// thread (be its coach).
	jobThread := asStr(job, "coach_student_id")
	if role == "student" {
		if jobThread != threadID {
			return ErrForbidden
		}
	} else {
		jt, _ := s.sb.SelectOne("coach_students",
			"id=eq."+store.Q(jobThread)+"&select=coach_id")
		if jt == nil || asStr(jt, "coach_id") != cs.CoachID {
			return ErrForbidden
		}
	}
	targetThreadID = strings.TrimSpace(targetThreadID)
	if role == "student" {
		targetThreadID = threadID // a student can only tag themselves
	}
	var tgt map[string]any
	if role != "student" && targetThreadID != "" {
		tgt, _ = s.sb.SelectOne("coach_students", "id=eq."+store.Q(targetThreadID)+
			"&select=coach_id,student_id,student_email,student_phone")
		if tgt == nil || asStr(tgt, "coach_id") != cs.CoachID {
			return ErrForbidden // coaches can only assign their own students
		}
	}
	jobF := "job_id=eq." + store.Q(jobID)
	// Clear a PLAYER's assignment (dropdown set to "none"): avatar given, no target.
	if avatarID >= 0 && targetThreadID == "" {
		return s.sb.Delete("coaching_pbvision_assignments",
			jobF+"&avatar_id=eq."+strconv.Itoa(avatarID))
	}
	// Clear a STUDENT's assignment: negative avatar with a target.
	if avatarID < 0 {
		if targetThreadID == "" {
			return errors.New("nothing to unassign")
		}
		return s.sb.Delete("coaching_pbvision_assignments",
			jobF+"&coach_student_id=eq."+store.Q(targetThreadID))
	}
	// Bind avatar -> student: free both sides of a prior mapping, then insert.
	_ = s.sb.Delete("coaching_pbvision_assignments",
		jobF+"&avatar_id=eq."+strconv.Itoa(avatarID))
	_ = s.sb.Delete("coaching_pbvision_assignments",
		jobF+"&coach_student_id=eq."+store.Q(targetThreadID))
	if _, err = s.sb.Insert("coaching_pbvision_assignments", map[string]any{
		"job_id": jobID, "avatar_id": avatarID, "coach_student_id": targetThreadID,
	}); err != nil {
		return err
	}
	// A coach assigning a student → notify that student their stats are ready.
	if tgt != nil {
		recipient := asStr(tgt, "student_id")
		if recipient == "" {
			if e := asStr(tgt, "student_email"); e != "" {
				recipient = s.userIDByEmail(e)
			}
		}
		if recipient == "" {
			if p := asStr(tgt, "student_phone"); p != "" {
				recipient = s.userIDByPhone(p)
			}
		}
		if recipient != "" && recipient != userID {
			s.notifyUser(recipient, "coaching", userID, s.coachingName(cs.CoachID),
				"Your coach shared a match analysis — see your PB Vision stats",
				"coaching:"+targetThreadID+"?tab=pbvision")
		}
	}
	return nil
}

// parsePBVisionHighlights pulls the flagged moments out of insights.highlights:
// time-ranges (s..e seconds) into the source clip with a label.
func parsePBVisionHighlights(insights any) []model.PBVisionHighlight {
	m, ok := insights.(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := m["highlights"].([]any)
	if !ok {
		return nil
	}
	out := make([]model.PBVisionHighlight, 0, len(arr))
	for _, item := range arr {
		h, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title := strings.TrimSpace(fmt.Sprint(orDefault(h["short_description"], "")))
		if title == "" {
			title = strings.TrimSpace(fmt.Sprint(orDefault(h["kind"], "Highlight")))
		}
		out = append(out, model.PBVisionHighlight{
			StartSeconds: pbNum(h["s"]),
			EndSeconds:   pbNum(h["e"]),
			Kind:         fmt.Sprint(orDefault(h["kind"], "")),
			Title:        title,
		})
	}
	return out
}

// orDefault returns v if non-nil, else def.
func orDefault(v, def any) any {
	if v == nil {
		return def
	}
	return v
}

// TagPBVisionPlayer is a student self-tag on the thread's latest ready analysis
// (kept for the existing "which player are you?" flow) — it assigns that player
// to the caller's own thread.
func (s *Service) TagPBVisionPlayer(threadID, userID, email string, avatarID int) error {
	if !s.coachingReady() || !s.pbvisionJobsReady() {
		return ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return err
	}
	row, err := s.sb.SelectOne("coaching_pbvision_jobs",
		"coach_student_id=eq."+store.Q(threadID)+
			"&status=eq.ready&order=updated_at.desc&limit=1&select=id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	return s.SetPBVisionAssignment(threadID, userID, email, asStr(row, "id"),
		avatarID, threadID)
}

// parsePBVisionPlayers pulls the detected players out of a raw insights blob:
// each player's avatar_id (per-video 0..3 index), team, and per-player stats.
// PB Vision gives no name/photo, so a label is derived from team + court side.
func parsePBVisionPlayers(insights any) []model.PBVisionPlayer {
	m, ok := insights.(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := m["player_data"].([]any)
	if !ok {
		return nil
	}
	// These are flat 0-100 numbers in the payload; copied straight through.
	scalarKeys := []string{"shot_count", "left_side_percentage",
		"total_team_shot_percentage"}
	players := make([]model.PBVisionPlayer, 0, len(arr))
	for _, item := range arr {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		stats := map[string]any{}
		for _, k := range scalarKeys {
			if v, ok := p[k]; ok {
				stats[k] = v
			}
		}
		// kitchen_arrival_percentage is a nested numerator/denominator breakdown
		// (serving/receiving × oneself/partner), NOT a scalar — derive the %.
		// Omitted when the denominator is 0 (no data, e.g. a very short clip),
		// so the UI can show "—" instead of a misleading 0%.
		if num, den := pbSumNumDen(p["kitchen_arrival_percentage"]); den > 0 {
			stats["kitchen_arrival_pct"] = num / den * 100
		}
		// court_coverage is a heat map (coordinates), not a %, so it's not shown.
		players = append(players, model.PBVisionPlayer{
			AvatarID: int(pbNum(p["avatar_id"])),
			Team:     int(pbNum(p["team"])),
			Stats:    stats,
		})
	}
	labelPBVisionPlayers(players)
	return players
}

// labelPBVisionPlayers labels each detected player "Team A/B · Player N", where
// N = avatar_id+1 — matching BOTH our colored badge and PB Vision's own
// "Player 1–4" report labels, so cross-referencing is trivial. (An earlier
// left/right-side label derived from left_side_percentage was too noisy on short
// clips to be trustworthy.)
func labelPBVisionPlayers(players []model.PBVisionPlayer) {
	teamName := func(t int) string {
		switch t {
		case 0:
			return "Team A"
		case 1:
			return "Team B"
		default:
			return fmt.Sprintf("Team %d", t+1)
		}
	}
	for i := range players {
		players[i].Label = fmt.Sprintf("%s · Player %d",
			teamName(players[i].Team), players[i].AvatarID+1)
	}
}

// pbSumNumDen recursively sums every {numerator, denominator} leaf in a nested
// PB Vision breakdown object (e.g. kitchen_arrival_percentage's serving/receiving
// × oneself/partner tree) so it can be collapsed to a single ratio.
func pbSumNumDen(v any) (num, den float64) {
	switch t := v.(type) {
	case map[string]any:
		if n, ok := t["numerator"]; ok {
			num += pbNum(n)
		}
		if d, ok := t["denominator"]; ok {
			den += pbNum(d)
		}
		for k, child := range t {
			if k == "numerator" || k == "denominator" {
				continue
			}
			cn, cd := pbSumNumDen(child)
			num += cn
			den += cd
		}
	case []any:
		for _, child := range t {
			cn, cd := pbSumNumDen(child)
			num += cn
			den += cd
		}
	}
	return num, den
}

// pbNum coerces a JSON value (float64/int/string) to a float64; 0 on failure.
func pbNum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func (s *Service) pbVisionHistoryReady() bool {
	return s.columnReady("coaching_pbvision_reports", "id")
}

// ListPBVisionHistory returns a thread's PB Vision report snapshots, newest first.
func (s *Service) ListPBVisionHistory(threadID, userID, email string) ([]model.PBVisionReport, error) {
	if !s.coachingReady() {
		return nil, ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return nil, err
	}
	if !s.pbVisionHistoryReady() {
		return []model.PBVisionReport{}, nil
	}
	rows, err := s.sb.Select("coaching_pbvision_reports",
		"coach_student_id=eq."+store.Q(threadID)+"&order=synced_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.PBVisionReport, 0, len(rows))
	for _, r := range rows {
		rep := model.PBVisionReport{
			ID:       asStr(r, "id"),
			Rating:   asFloatPtr(r, "rating"),
			SyncedAt: asStr(r, "synced_at"),
		}
		if m, ok := r["stats"].(map[string]any); ok {
			rep.Stats = m
		}
		out = append(out, rep)
	}
	return out, nil
}

// seedPBVisionReport appends a historical snapshot (best-effort, guarded).
func (s *Service) seedPBVisionReport(threadID string, rating float64, syncedAt string, stats map[string]any) {
	if !s.pbVisionHistoryReady() || threadID == "" {
		return
	}
	_, _ = s.sb.Insert("coaching_pbvision_reports", map[string]any{
		"coach_student_id": threadID,
		"rating":           rating,
		"synced_at":        syncedAt,
		"stats":            stats,
	})
}

// --- Multi-week training programs ---

func (s *Service) programsReady() bool {
	return s.columnReady("coaching_programs", "id")
}

func mapProgram(row map[string]any) model.CoachingProgram {
	p := model.CoachingProgram{
		ID:        asStr(row, "id"),
		Title:     asStr(row, "title"),
		CreatedAt: asStr(row, "created_at"),
		Weeks:     []model.CoachingProgramWeek{},
	}
	if arr, ok := row["weeks"].([]any); ok {
		for _, w := range arr {
			if m, ok := w.(map[string]any); ok {
				p.Weeks = append(p.Weeks, model.CoachingProgramWeek{
					Focus: asStr(m, "focus"),
					Done:  asBool(m, "done"),
				})
			}
		}
	}
	return p
}

// GetThreadProgram returns the thread's most recent active program (or empty).
func (s *Service) GetThreadProgram(threadID, userID, email string) (model.CoachingProgram, error) {
	if !s.coachingReady() {
		return model.CoachingProgram{}, ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return model.CoachingProgram{}, err
	}
	if !s.programsReady() {
		return model.CoachingProgram{}, nil
	}
	row, err := s.sb.SelectOne("coaching_programs",
		"coach_student_id=eq."+store.Q(threadID)+
			"&active=is.true&order=created_at.desc&limit=1")
	if err != nil || row == nil {
		return model.CoachingProgram{}, err
	}
	return mapProgram(row), nil
}

// CreateProgram assigns a new multi-week program to a student (coach only).
// Supersedes any prior active program on the thread.
func (s *Service) CreateProgram(threadID, userID, email string, req model.CoachingProgramRequest) (model.CoachingProgram, error) {
	if !s.programsReady() {
		return model.CoachingProgram{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingProgram{}, err
	}
	if role != "coach" {
		return model.CoachingProgram{}, ErrForbidden
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return model.CoachingProgram{}, errors.New("give the program a title")
	}
	weeks := make([]map[string]any, 0, len(req.Weeks))
	for _, f := range req.Weeks {
		if f = strings.TrimSpace(f); f != "" {
			weeks = append(weeks, map[string]any{"focus": f, "done": false})
		}
	}
	if len(weeks) == 0 {
		return model.CoachingProgram{}, errors.New("add at least one week")
	}
	// Retire prior active programs on this thread.
	_, _ = s.sb.Update("coaching_programs",
		"coach_student_id=eq."+store.Q(threadID)+"&active=is.true",
		map[string]any{"active": false})
	ins, err := s.sb.Insert("coaching_programs", map[string]any{
		"coach_student_id": threadID,
		"title":            title,
		"weeks":            weeks,
	})
	if err != nil || len(ins) == 0 {
		if err == nil {
			err = errors.New("could not save the program")
		}
		return model.CoachingProgram{}, err
	}
	s.bumpThreadActivity(threadID)
	s.notifyCoachingCounterpart(cs, "coach", userID, s.coachingName(cs.CoachID),
		"Your coach assigned a new training program: "+title)
	return mapProgram(ins[0]), nil
}

// ToggleProgramWeek flips a week's done flag (coach or student).
func (s *Service) ToggleProgramWeek(programID string, index int, userID, email string) (model.CoachingProgram, error) {
	if !s.programsReady() {
		return model.CoachingProgram{}, ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_programs", "id=eq."+store.Q(programID))
	if err != nil || row == nil {
		if err == nil {
			err = ErrNotFound
		}
		return model.CoachingProgram{}, err
	}
	threadID := asStr(row, "coach_student_id")
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingProgram{}, err
	}
	weeks, _ := row["weeks"].([]any)
	if index < 0 || index >= len(weeks) {
		return model.CoachingProgram{}, errors.New("no such week")
	}
	nowDone := false
	focus := ""
	if m, ok := weeks[index].(map[string]any); ok {
		nowDone = !asBool(m, "done")
		focus = asStr(m, "focus")
		m["done"] = nowDone
		weeks[index] = m
	}
	out, err := s.sb.Update("coaching_programs", "id=eq."+store.Q(programID),
		map[string]any{"weeks": weeks, "updated_at": now()})
	if err != nil {
		return model.CoachingProgram{}, err
	}
	s.bumpThreadActivity(threadID)
	// Student completing a program week → tell the coach (parity with drills).
	if role == "student" && nowDone {
		who := s.coachingName(userID)
		if who == "" {
			who = "Your student"
		}
		body := who + " completed a program week"
		if focus != "" {
			body = who + " completed a week: " + focus
		}
		s.notifyUser(cs.CoachID, "coaching", userID, who, body,
			"coaching:"+threadID)
	}
	if len(out) > 0 {
		return mapProgram(out[0]), nil
	}
	return mapProgram(row), nil
}

// DeleteProgram removes a program (coach who owns the thread).
func (s *Service) DeleteProgram(programID, userID, email string) error {
	if !s.programsReady() {
		return ErrCoachingUnavailable
	}
	row, _ := s.sb.SelectOne("coaching_programs",
		"id=eq."+store.Q(programID)+"&select=coach_student_id")
	if row == nil {
		return ErrNotFound
	}
	if _, role, err := s.threadMembership(asStr(row, "coach_student_id"), userID, email); err != nil || role != "coach" {
		return ErrForbidden
	}
	return s.sb.Delete("coaching_programs", "id=eq."+store.Q(programID))
}

// --- PB Vision auto-import (highlights + stats into the coaching thread) ---

func (s *Service) pbvisionJobsReady() bool {
	return s.columnReady("coaching_pbvision_jobs", "id")
}

// AnalyzeThreadVideo submits a match video to PB Vision for a student's thread
// (coach only). The student's email is passed so PB Vision attributes/shares the
// report to them. On completion the webhook ingests highlights + stats. Returns
// the PB Vision video id.
func (s *Service) AnalyzeThreadVideo(threadID, userID, email, videoURL, videoID string) (string, error) {
	if !s.coachingReady() || !s.pbvisionJobsReady() {
		return "", ErrCoachingUnavailable
	}
	if s.PBV == nil || !s.PBV.Configured() {
		return "", errors.New("PB Vision isn't configured yet")
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return "", err
	}
	// Player-initiated: only the student may kick off their own PB Vision
	// analysis, so the report attributes to their account.
	if role != "student" {
		return "", ErrForbidden
	}
	videoURL = strings.TrimSpace(videoURL)
	if videoURL == "" {
		return "", errors.New("a video URL is required")
	}
	// Guard against an accidental double-submit of the SAME clip: if an analysis
	// for this clip is already in flight, don't spend another PB Vision run.
	if videoID != "" && s.columnReady("coaching_pbvision_jobs", "source_video_id") {
		if prev, _ := s.sb.SelectOne("coaching_pbvision_jobs",
			"source_video_id=eq."+store.Q(videoID)+
				"&status=eq.processing&select=id&limit=1"); prev != nil {
			return "", errors.New("this clip is already being analyzed on PB Vision")
		}
	}
	emails := []string{}
	if cs.StudentEmail != "" {
		emails = append(emails, cs.StudentEmail)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vid, err := s.PBV.AddVideoByURL(ctx, videoURL, gateway.PBVideoMeta{
		Name:       "Coaching · " + cs.StudentName,
		UserEmails: emails,
	})
	if err != nil {
		return "", err
	}
	job := map[string]any{
		"vid":              vid,
		"coach_student_id": threadID,
		"status":           "processing",
		"updated_at":       now(),
	}
	if videoID != "" && s.columnReady("coaching_pbvision_jobs", "source_video_id") {
		job["source_video_id"] = videoID
	}
	_, err = s.sb.Upsert("coaching_pbvision_jobs", "vid", job)
	if err != nil {
		return "", err
	}
	s.bumpThreadActivity(threadID)
	return vid, nil
}

// handleCoachingPBVisionCallback ingests a PB Vision completion for a coaching
// job — invoked from HandlePBVisionWebhook. Best-effort; a no-op when the vid
// isn't one of ours. Stores the raw payloads now; the highlight/stats PARSER is
// wired once the insights/stats field shapes are confirmed from a real payload.
func (s *Service) handleCoachingPBVisionCallback(vid, webpage string, insights, stats json.RawMessage, errReason string) {
	if !s.pbvisionJobsReady() {
		return
	}
	job, _ := s.sb.SelectOne("coaching_pbvision_jobs",
		"vid=eq."+store.Q(vid)+"&select=id,coach_student_id")
	if job == nil {
		return
	}
	threadID := asStr(job, "coach_student_id")
	upd := map[string]any{"updated_at": now()}
	if strings.TrimSpace(errReason) != "" {
		upd["status"] = "failed"
		upd["error"] = errReason
	} else {
		upd["status"] = "ready"
		if webpage != "" {
			upd["report_url"] = webpage
		}
		if len(insights) > 0 {
			upd["insights"] = insights
		}
		if len(stats) > 0 {
			upd["stats"] = stats
		}
	}
	_, _ = s.sb.Update("coaching_pbvision_jobs",
		"id=eq."+store.Q(asStr(job, "id")), upd)
	failed := strings.TrimSpace(errReason) != ""
	if !failed {
		// TODO(pbvision-parser): with a real insights/stats payload, parse here —
		//   stats            -> coaching_pbvision (+ coaching_pbvision_reports)
		//   insights.highlights -> coaching_videos (source='pbvision', external_ref,
		//                          video_url=clip, title="Long Rally #1" etc.)
		//   insights.players -> map to a student (coach_students.pbvision_player_id)
		s.ingestPBVisionHighlights(threadID, insights)
	}
	s.bumpThreadActivity(threadID)
	// Push + bell to BOTH the coach and the student, on success or failure.
	coachBody, studentBody := "PB Vision analysis ready — review highlights",
		"Your PB Vision highlights are ready"
	if failed {
		coachBody, studentBody = "PB Vision analysis failed — try a longer match clip",
			"PB Vision couldn't analyze that clip — try a longer match video"
	}
	if cs, _ := s.sb.SelectOne("coach_students", "id=eq."+store.Q(threadID)); cs != nil {
		coachID := asStr(cs, "coach_id")
		pbLink := "coaching:" + threadID + "?tab=pbvision"
		s.notifyUser(coachID, "coaching", "", "", coachBody, pbLink)
		if sid := asStr(cs, "student_id"); sid != "" && sid != coachID {
			s.notifyUser(sid, "coaching", "", "", studentBody, pbLink)
		}
	}
}

// ingestPBVisionHighlights turns PB Vision highlight clips into coaching_videos
// rows (source='pbvision', deduped by external_ref). STUB until the insights
// field shape is confirmed — deliberately parses defensively and no-ops on an
// unrecognized shape so a live callback never errors before the parser lands.
func (s *Service) ingestPBVisionHighlights(threadID string, insights json.RawMessage) {
	if len(insights) == 0 || threadID == "" {
		return
	}
	// Defensive best-guess shape; refined once we have a real payload:
	//   {"highlights":[{"id","label","url","startSec","endSec","type"}]}
	var p struct {
		Highlights []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Type  string `json:"type"`
			URL   string `json:"url"`
		} `json:"highlights"`
	}
	if err := json.Unmarshal(insights, &p); err != nil || len(p.Highlights) == 0 {
		return // shape not yet known — leave raw payload on the job for reprocessing
	}
	for _, h := range p.Highlights {
		if strings.TrimSpace(h.URL) == "" {
			continue
		}
		title := strings.TrimSpace(h.Label)
		if title == "" {
			title = strings.TrimSpace(h.Type)
		}
		// Dedupe by external_ref so a re-delivered callback can't double-import.
		if h.ID != "" {
			if exist, _ := s.sb.SelectOne("coaching_videos",
				"coach_student_id=eq."+store.Q(threadID)+"&external_ref=eq."+store.Q(h.ID)+"&select=id"); exist != nil {
				continue
			}
		}
		_, _ = s.sb.Insert("coaching_videos", map[string]any{
			"coach_student_id": threadID,
			"uploaded_by":      "00000000-0000-0000-0000-000000000000",
			"uploader_role":    "coach",
			"video_url":        h.URL,
			"title":            orNull(title),
			"source":           "pbvision",
			"external_ref":     orNull(h.ID),
		})
	}
}

// GetThread returns a thread's clips (each with nested feedback), for a member.
func (s *Service) GetThread(threadID, userID, email string) (model.CoachingThread, error) {
	if !s.coachingReady() {
		return model.CoachingThread{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingThread{}, err
	}
	cs.CoachName = s.coachingName(cs.CoachID)
	if role != "coach" {
		cs.CoachNote = "" // students never see the coach's private note about them
		cs.SkillLevel = "" // nor the coach's skill assessment
	}
	// Opening a thread marks it read for the viewer — both the thread's unread
	// dot and the bell rows that deep-link into it.
	s.markThreadRead(userID, threadID)
	s.markThreadNotificationsRead(userID, threadID)

	vids, err := s.sb.Select("coaching_videos",
		"coach_student_id=eq."+store.Q(threadID)+"&order=created_at.desc")
	if err != nil {
		return model.CoachingThread{}, err
	}
	fbs, err := s.sb.Select("coaching_feedback",
		"coach_student_id=eq."+store.Q(threadID)+"&order=created_at.asc")
	if err != nil {
		return model.CoachingThread{}, err
	}
	// Bucket feedback by video id.
	byVideo := map[string][]model.CoachingFeedback{}
	nameCache := map[string]string{}
	nameOf := func(uid string) string {
		if n, ok := nameCache[uid]; ok {
			return n
		}
		n := s.coachingName(uid)
		nameCache[uid] = n
		return n
	}
	for _, f := range fbs {
		fb := model.CoachingFeedback{
			ID:         asStr(f, "id"),
			VideoID:    asStr(f, "video_id"),
			AuthorID:   asStr(f, "author_id"),
			AuthorRole: asStr(f, "author_role"),
			AuthorName:       nameOf(asStr(f, "author_id")),
			Body:             asStr(f, "body"),
			CreatedAt:        asStr(f, "created_at"),
			TimestampSeconds: asFloatPtr(f, "timestamp_seconds"),
			Annotation:       f["annotation"],
		}
		byVideo[fb.VideoID] = append(byVideo[fb.VideoID], fb)
	}
	out := model.CoachingThread{Student: cs, Videos: make([]model.CoachingVideo, 0, len(vids))}
	for _, v := range vids {
		vid := model.CoachingVideo{
			ID:             asStr(v, "id"),
			CoachStudentID: asStr(v, "coach_student_id"),
			UploadedBy:     asStr(v, "uploaded_by"),
			UploaderRole:   asStr(v, "uploader_role"),
			UploaderName:   nameOf(asStr(v, "uploaded_by")),
			VideoURL:       asStr(v, "video_url"),
			Title:          asStr(v, "title"),
			CreatedAt:      asStr(v, "created_at"),
			Source:         asStr(v, "source"),
			Feedback:       byVideo[asStr(v, "id")],
		}
		out.Videos = append(out.Videos, vid)
	}
	// Attach PB Vision analysis status per source clip (processing/ready/failed).
	if s.pbvisionJobsReady() && s.columnReady("coaching_pbvision_jobs", "source_video_id") {
		jobs, _ := s.sb.Select("coaching_pbvision_jobs",
			"coach_student_id=eq."+store.Q(threadID)+
				"&source_video_id=not.is.null&select=source_video_id,status&order=updated_at.desc")
		statusByVideo := map[string]string{}
		for _, j := range jobs {
			svid := asStr(j, "source_video_id")
			if svid != "" && statusByVideo[svid] == "" { // first = most recent
				statusByVideo[svid] = asStr(j, "status")
			}
		}
		for i := range out.Videos {
			out.Videos[i].PBVisionStatus = statusByVideo[out.Videos[i].ID]
		}
	}
	// Private coach notes — attached ONLY for the coach, never sent to a student.
	if role == "coach" && len(out.Videos) > 0 {
		ids := make([]string, len(out.Videos))
		for i, v := range out.Videos {
			ids[i] = v.ID
		}
		notes := s.clipNotes(ids)
		for i := range out.Videos {
			out.Videos[i].CoachNote = notes[out.Videos[i].ID]
		}
	}
	// Private bucket → hand out short-lived signed playback URLs.
	s.signCoachingVideos(out.Videos)
	return out, nil
}

// AddThreadVideo records a clip a member uploaded, then notifies the counterpart.
func (s *Service) AddThreadVideo(threadID, userID, email string, req model.CoachingVideoRequest) (model.CoachingVideo, error) {
	if !s.coachingReady() {
		return model.CoachingVideo{}, ErrCoachingUnavailable
	}
	url := strings.TrimSpace(req.VideoURL)
	if url == "" {
		return model.CoachingVideo{}, errors.New("upload a clip first")
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingVideo{}, err
	}
	ins, err := s.sb.Insert("coaching_videos", map[string]any{
		"coach_student_id": threadID,
		"uploaded_by":      userID,
		"uploader_role":    role,
		"video_url":        url,
		"title":            orNull(strings.TrimSpace(req.Title)),
	})
	if err != nil {
		return model.CoachingVideo{}, err
	}
	if len(ins) == 0 {
		return model.CoachingVideo{}, errors.New("could not save the clip")
	}
	vid := model.CoachingVideo{
		ID:             asStr(ins[0], "id"),
		CoachStudentID: threadID,
		UploadedBy:     userID,
		UploaderRole:   role,
		UploaderName:   s.coachingName(userID),
		VideoURL:       url,
		Title:          asStr(ins[0], "title"),
		CreatedAt:      asStr(ins[0], "created_at"),
	}
	if m, err := s.sb.SignedURLs("coaching-videos",
		[]string{coachingVideoPath(url)}, 6*60*60); err == nil {
		if u, ok := m[coachingVideoPath(url)]; ok {
			vid.VideoURL = u
		}
	}
	// Stamp thread activity + mark the uploader themselves read (so their own
	// upload never shows as unread to them; the counterpart's list flags it).
	s.bumpThreadActivity(threadID)
	s.markThreadRead(userID, threadID)
	s.notifyCoachingCounterpartLink(cs, role, userID, vid.UploaderName,
		vid.UploaderName+" shared a new coaching clip",
		"coaching:"+cs.ID+"?tab=videos")
	return vid, nil
}

// AddVideoFeedback adds a comment to a clip, then notifies the counterpart.
func (s *Service) AddVideoFeedback(videoID, userID, email string, req model.CoachingFeedbackRequest) (model.CoachingFeedback, error) {
	if !s.coachingReady() {
		return model.CoachingFeedback{}, ErrCoachingUnavailable
	}
	body := strings.TrimSpace(req.Body)
	if body == "" && req.Annotation == nil {
		return model.CoachingFeedback{}, errors.New("write some feedback first")
	}
	vrow, err := s.sb.SelectOne("coaching_videos",
		"id=eq."+store.Q(videoID)+"&select=id,coach_student_id")
	if err != nil {
		return model.CoachingFeedback{}, err
	}
	if vrow == nil {
		return model.CoachingFeedback{}, ErrNotFound
	}
	threadID := asStr(vrow, "coach_student_id")
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingFeedback{}, err
	}
	fbRow := map[string]any{
		"coach_student_id": threadID,
		"video_id":         videoID,
		"author_id":        userID,
		"author_role":      role,
		"body":             body,
	}
	if req.TimestampSeconds != nil && *req.TimestampSeconds >= 0 &&
		s.columnReady("coaching_feedback", "timestamp_seconds") {
		fbRow["timestamp_seconds"] = *req.TimestampSeconds
	}
	if req.Annotation != nil && s.columnReady("coaching_feedback", "annotation") {
		fbRow["annotation"] = req.Annotation
	}
	ins, err := s.sb.Insert("coaching_feedback", fbRow)
	if err != nil {
		return model.CoachingFeedback{}, err
	}
	if len(ins) == 0 {
		return model.CoachingFeedback{}, errors.New("could not save your feedback")
	}
	name := s.coachingName(userID)
	fb := model.CoachingFeedback{
		ID:             asStr(ins[0], "id"),
		CoachStudentID: threadID,
		VideoID:        videoID,
		AuthorID:       userID,
		AuthorRole:     role,
		AuthorName:     name,
		Body:           body,
		CreatedAt:      asStr(ins[0], "created_at"),
	}
	fb.TimestampSeconds = asFloatPtr(ins[0], "timestamp_seconds")
	fb.Annotation = ins[0]["annotation"]
	s.bumpThreadActivity(threadID)
	s.markThreadRead(userID, threadID)
	notifyBody := name + ": " + truncate(body, 120)
	if body == "" {
		notifyBody = name + " drew an annotation on a clip"
	}
	s.notifyCoachingCounterpartLink(cs, role, userID, name,
		notifyBody, "coaching:"+cs.ID+"?tab=videos")
	return fb, nil
}

// DeleteCoachingVideo removes a clip (and its feedback via cascade). Allowed for
// the uploader or the thread's coach.
func (s *Service) DeleteCoachingVideo(videoID, userID, email string) error {
	if !s.coachingReady() {
		return ErrCoachingUnavailable
	}
	vrow, err := s.sb.SelectOne("coaching_videos",
		"id=eq."+store.Q(videoID)+"&select=id,coach_student_id,uploaded_by")
	if err != nil {
		return err
	}
	if vrow == nil {
		return ErrNotFound
	}
	_, role, err := s.threadMembership(asStr(vrow, "coach_student_id"), userID, email)
	if err != nil {
		return err
	}
	if role != "coach" && asStr(vrow, "uploaded_by") != userID {
		return ErrForbidden
	}
	return s.sb.Delete("coaching_videos", "id=eq."+store.Q(videoID))
}

// DeleteCoachingFeedback removes a comment. Allowed for the author or the coach.
func (s *Service) DeleteCoachingFeedback(feedbackID, userID, email string) error {
	if !s.coachingReady() {
		return ErrCoachingUnavailable
	}
	frow, err := s.sb.SelectOne("coaching_feedback",
		"id=eq."+store.Q(feedbackID)+"&select=id,coach_student_id,author_id")
	if err != nil {
		return err
	}
	if frow == nil {
		return ErrNotFound
	}
	_, role, err := s.threadMembership(asStr(frow, "coach_student_id"), userID, email)
	if err != nil {
		return err
	}
	if role != "coach" && asStr(frow, "author_id") != userID {
		return ErrForbidden
	}
	return s.sb.Delete("coaching_feedback", "id=eq."+store.Q(feedbackID))
}

// notesReady gates the coach private-notes feature on add_coaching_notes.sql.
func (s *Service) notesReady() bool {
	return s.columnReady("coaching_notes", "id")
}

// clipNotes returns a video_id → note body map for the given clips (empty if the
// feature isn't migrated). Coach-only data — callers must check role first.
func (s *Service) clipNotes(videoIDs []string) map[string]string {
	out := map[string]string{}
	if !s.notesReady() || len(videoIDs) == 0 {
		return out
	}
	rows, err := s.sb.Select("coaching_notes",
		"video_id=in.("+strings.Join(videoIDs, ",")+")&select=video_id,body")
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[asStr(r, "video_id")] = asStr(r, "body")
	}
	return out
}

// SetClipNote upserts (or clears, when body is empty) the coach's private note on
// a clip. Only the thread's coach may set it.
func (s *Service) SetClipNote(videoID, userID, email, body string) error {
	if !s.notesReady() {
		return ErrCoachingUnavailable
	}
	vrow, err := s.sb.SelectOne("coaching_videos",
		"id=eq."+store.Q(videoID)+"&select=id,coach_student_id")
	if err != nil {
		return err
	}
	if vrow == nil {
		return ErrNotFound
	}
	threadID := asStr(vrow, "coach_student_id")
	_, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return err
	}
	if role != "coach" {
		return ErrForbidden
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return s.sb.Delete("coaching_notes", "video_id=eq."+store.Q(videoID))
	}
	_, err = s.sb.Upsert("coaching_notes", "video_id", map[string]any{
		"coach_student_id": threadID,
		"video_id":         videoID,
		"body":             body,
		"updated_at":       now(),
	})
	return err
}

// SetStudentNote sets (or clears) the coach's private running note about a
// student. Only the thread's coach may set it.
func (s *Service) SetStudentNote(threadID, coachID, body string) error {
	if !s.columnReady("coach_students", "coach_note") {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coach_students",
		"id=eq."+store.Q(threadID)+"&select=coach_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	_, err = s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
		map[string]any{"coach_note": orNull(strings.TrimSpace(body))})
	return err
}

// SetSharedNote sets (or clears) the SHARED note — visible to the student. Only
// the thread's coach may set it; the student is pinged when it's added.
func (s *Service) SetSharedNote(threadID, coachID, body string) error {
	if !s.columnReady("coach_students", "shared_note") {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coach_students", "id=eq."+store.Q(threadID))
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	cs := mapCoachStudent(row)
	if cs.CoachID != coachID {
		return ErrForbidden
	}
	body = strings.TrimSpace(body)
	if _, err = s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
		map[string]any{"shared_note": orNull(body)}); err != nil {
		return err
	}
	s.bumpThreadActivity(threadID)
	if body != "" {
		s.notifyCoachingCounterpart(cs, "coach", coachID, s.coachingName(coachID),
			"Your coach shared a note")
	}
	return nil
}

// --- Shared notes list (titled, dated; editable within 24h) ---

func (s *Service) sharedNotesReady() bool {
	return s.columnReady("coaching_shared_notes", "id")
}

// editableWithin24h reports whether a note posted at createdAt can still be
// edited/deleted (within 24h). Fails open (editable) on an unparseable stamp.
func editableWithin24h(createdAt string) bool {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return true
		}
	}
	return time.Since(t) < 24*time.Hour
}

func mapSharedNote(row map[string]any) model.CoachingSharedNote {
	created := asStr(row, "created_at")
	return model.CoachingSharedNote{
		ID:        asStr(row, "id"),
		Title:     asStr(row, "title"),
		Body:      asStr(row, "body"),
		CreatedAt: created,
		UpdatedAt: asStr(row, "updated_at"),
		Editable:  editableWithin24h(created),
	}
}

// ListSharedNotes returns a thread's shared notes (newest first), for a member.
func (s *Service) ListSharedNotes(threadID, userID, email string) ([]model.CoachingSharedNote, error) {
	if !s.sharedNotesReady() {
		return []model.CoachingSharedNote{}, nil
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return nil, err
	}
	rows, err := s.sb.Select("coaching_shared_notes",
		"coach_student_id=eq."+store.Q(threadID)+"&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingSharedNote, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapSharedNote(r))
	}
	return out, nil
}

// AddSharedNote posts a new shared note (coach-only); pings the student.
func (s *Service) AddSharedNote(threadID, coachID, email, title, body string) (model.CoachingSharedNote, error) {
	if !s.sharedNotesReady() {
		return model.CoachingSharedNote{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, coachID, email)
	if err != nil {
		return model.CoachingSharedNote{}, err
	}
	if role != "coach" {
		return model.CoachingSharedNote{}, ErrForbidden
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return model.CoachingSharedNote{}, errors.New("write something in the note")
	}
	ins, err := s.sb.Insert("coaching_shared_notes", map[string]any{
		"coach_student_id": threadID,
		"title":            orNull(strings.TrimSpace(title)),
		"body":             body,
		"created_at":       now(),
		"updated_at":       now(),
	})
	if err != nil {
		return model.CoachingSharedNote{}, err
	}
	if len(ins) == 0 {
		return model.CoachingSharedNote{}, errors.New("could not save that note")
	}
	s.bumpThreadActivity(threadID)
	s.notifyCoachingCounterpart(cs, "coach", coachID, s.coachingName(coachID),
		"Your coach shared a note")
	return mapSharedNote(ins[0]), nil
}

// sharedNoteCoachGuard loads a shared note + its thread and verifies the caller
// is the coach and the note is still within its 24h edit window.
func (s *Service) sharedNoteCoachGuard(noteID, coachID, email string) (map[string]any, error) {
	row, err := s.sb.SelectOne("coaching_shared_notes", "id=eq."+store.Q(noteID))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	_, role, err := s.threadMembership(asStr(row, "coach_student_id"), coachID, email)
	if err != nil {
		return nil, err
	}
	if role != "coach" {
		return nil, ErrForbidden
	}
	if !editableWithin24h(asStr(row, "created_at")) {
		return nil, ErrForbidden
	}
	return row, nil
}

// EditSharedNote edits a shared note (coach-only, within 24h of posting).
func (s *Service) EditSharedNote(noteID, coachID, email, title, body string) (model.CoachingSharedNote, error) {
	if !s.sharedNotesReady() {
		return model.CoachingSharedNote{}, ErrCoachingUnavailable
	}
	row, err := s.sharedNoteCoachGuard(noteID, coachID, email)
	if err != nil {
		return model.CoachingSharedNote{}, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return model.CoachingSharedNote{}, errors.New("write something in the note")
	}
	out, err := s.sb.Update("coaching_shared_notes", "id=eq."+store.Q(noteID),
		map[string]any{
			"title":      orNull(strings.TrimSpace(title)),
			"body":       body,
			"updated_at": now(),
		})
	if err != nil {
		return model.CoachingSharedNote{}, err
	}
	s.bumpThreadActivity(asStr(row, "coach_student_id"))
	if len(out) > 0 {
		return mapSharedNote(out[0]), nil
	}
	return mapSharedNote(row), nil
}

// DeleteSharedNote removes a shared note (coach-only, within 24h of posting).
func (s *Service) DeleteSharedNote(noteID, coachID, email string) error {
	if !s.sharedNotesReady() {
		return ErrCoachingUnavailable
	}
	if _, err := s.sharedNoteCoachGuard(noteID, coachID, email); err != nil {
		return err
	}
	return s.sb.Delete("coaching_shared_notes", "id=eq."+store.Q(noteID))
}

// SetStudentLevel sets (or clears) the coach's skill-level assessment of a
// student. Only the thread's coach may set it.
func (s *Service) SetStudentLevel(threadID, coachID, level string) error {
	if !s.columnReady("coach_students", "skill_level") {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coach_students",
		"id=eq."+store.Q(threadID)+"&select=coach_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	_, err = s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
		map[string]any{"skill_level": orNull(strings.TrimSpace(level))})
	return err
}

// seedVideoURLs are stable public sample clips used for demo data. They live
// OUTSIDE the coaching-videos bucket, so signCoachingVideos can't sign them and
// falls back to the URL as-is — which plays fine in the player.
var seedVideoURLs = []string{
	"https://flutter.github.io/assets-for-api-docs/assets/videos/butterfly.mp4",
	"https://flutter.github.io/assets-for-api-docs/assets/videos/bee.mp4",
	"https://flutter.github.io/assets-for-api-docs/assets/videos/butterfly.mp4",
}

const seedEmailDomain = "@coachdemo.test"
const seedStudentAuthor = "00000000-0000-0000-0000-000000000000"

// SeedCoachingTestData populates a coach's Instructor pages with realistic dummy
// data — a few students (some with clips + coach/student feedback + private
// notes, one text-invited & pending) — so the pages can be demoed/tested. It
// first clears any prior seed for this coach (students whose email is @coachdemo.test),
// so the button is safely re-runnable. Returns the number of students created.
func (s *Service) SeedCoachingTestData(coachID string) (int, error) {
	if !s.coachingReady() {
		return 0, ErrCoachingUnavailable
	}
	// Clear prior seed (cascade removes their clips/feedback/notes).
	_ = s.sb.Delete("coach_students",
		"coach_id=eq."+store.Q(coachID)+"&student_email=like.*"+store.Q(seedEmailDomain))

	notesOK := s.notesReady()
	studentNoteOK := s.columnReady("coach_students", "coach_note")
	levelOK := s.columnReady("coach_students", "skill_level")

	addStudent := func(name, email, phone, note, level string) string {
		row := map[string]any{
			"coach_id":      coachID,
			"student_email": email,
			"student_name":  name,
			"student_phone": orNull(normPhone(phone)),
		}
		if studentNoteOK && note != "" {
			row["coach_note"] = note
		}
		if levelOK && level != "" {
			row["skill_level"] = level
		}
		ins, err := s.sb.Insert("coach_students", row)
		if err != nil || len(ins) == 0 {
			return ""
		}
		return asStr(ins[0], "id")
	}
	addClip := func(threadID, url, title string) string {
		ins, err := s.sb.Insert("coaching_videos", map[string]any{
			"coach_student_id": threadID,
			"uploaded_by":      coachID,
			"uploader_role":    "coach",
			"video_url":        url,
			"title":            title,
		})
		if err != nil || len(ins) == 0 {
			return ""
		}
		return asStr(ins[0], "id")
	}
	addFeedback := func(threadID, videoID, role, body string) {
		author := coachID
		if role == "student" {
			author = seedStudentAuthor
		}
		_, _ = s.sb.Insert("coaching_feedback", map[string]any{
			"coach_student_id": threadID,
			"video_id":         videoID,
			"author_id":        author,
			"author_role":      role,
			"body":             body,
		})
	}
	addClipNote := func(threadID, videoID, body string) {
		if !notesOK || body == "" {
			return
		}
		_, _ = s.sb.Upsert("coaching_notes", "video_id", map[string]any{
			"coach_student_id": threadID,
			"video_id":         videoID,
			"body":             body,
		})
	}

	count := 0
	var alexID, taylorID string

	// 1) Alex — two clips, coach + student feedback, a per-clip note.
	if t := addStudent("Alex Cruz", "alex.cruz"+seedEmailDomain, "",
		"Working on his third-shot drop — improving, but still pops it up under pressure. Try the drop-and-freeze drill next session.", "3.5"); t != "" {
		count++
		alexID = t
		v1 := addClip(t, seedVideoURLs[0], "Third-shot drop reps")
		addFeedback(t, v1, "coach", "Good contact point, but you're swinging up too hard — soften the paddle face and let it float.")
		addFeedback(t, v1, "student", "Got it — should I still step in after the drop?")
		addClipNote(t, v1, "Remind him about the freeze after the drop — he's rushing to the kitchen.")
		v2 := addClip(t, seedVideoURLs[1], "Dinking cross-court")
		addFeedback(t, v2, "coach", "Much better patience here. Keep the paddle up between dinks.")
	}

	// 2) Jordan — one clip, one comment.
	if t := addStudent("Jordan Lee", "jordan.lee"+seedEmailDomain, "", "", "3.0"); t != "" {
		count++
		v := addClip(t, seedVideoURLs[2], "Serve mechanics")
		addFeedback(t, v, "coach", "Toss is a little low — get more lift and you'll add depth.")
	}

	// 3) Sam — text-invited, still pending, no clips yet.
	if addStudent("Sam Rivera", "sam.rivera"+seedEmailDomain, "6265550142", "", "") != "" {
		count++
	}

	// 4) Taylor — one clip + a per-student note.
	if t := addStudent("Taylor Kim", "taylor.kim"+seedEmailDomain, "",
		"Great hands at the net. Push her on footwork and resets.", "4.0"); t != "" {
		count++
		taylorID = t
		v := addClip(t, seedVideoURLs[0], "Hands battle at the net")
		addFeedback(t, v, "coach", "Love the quick hands. Reset when it's above the net — don't counter everything.")
	}

	// Real coach↔student link: connect the running coach (krizhia when she runs
	// "Generate demo data") to rolando's live account so BOTH real accounts see a
	// fully-loaded coaching thread. Skipped if the runner IS rolando (no self-coach).
	const rolandoEmail = "rolando.naranjo0420@gmail.com"
	if rolID := s.userIDByEmail(rolandoEmail); rolID != coachID {
		// Idempotent: clear any prior seeded link (cascade removes its data).
		_ = s.sb.Delete("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_email=eq."+store.Q(rolandoEmail))
		row := map[string]any{
			"coach_id":      coachID,
			"student_email": rolandoEmail,
			"student_name":  "Rolando Naranjo",
			"student_id":    orNull(rolID),
		}
		if studentNoteOK {
			row["coach_note"] = "Solid hands at the net — push footwork and third-shot consistency."
		}
		if levelOK {
			row["skill_level"] = "3.5"
		}
		if ins, _ := s.sb.Insert("coach_students", row); len(ins) > 0 {
			rid := asStr(ins[0], "id")
			count++
			v1 := addClip(rid, seedVideoURLs[0], "Third-shot drop reps")
			addFeedback(rid, v1, "coach", "Nice soft hands — let the drop float and reset before you move in.")
			addFeedback(rid, v1, "student", "Got it coach — I'll freeze after the drop.")
			addClipNote(rid, v1, "He rushes the kitchen after the drop — cue the freeze.")
			v2 := addClip(rid, seedVideoURLs[1], "Dinking cross-court")
			addFeedback(rid, v2, "coach", "Good patience here. Keep the paddle up between dinks.")

			if s.columnReady("coaching_skill_ratings", "id") {
				ratings := map[string]float64{
					"serve": 3.5, "return": 3.0, "dinks": 3.5,
					"drops": 2.5, "volleys": 3.5, "strategy": 3.0,
				}
				for _, sk := range coachingSkills {
					r := ratings[sk]
					_, _ = s.sb.Upsert("coaching_skill_ratings", "coach_student_id,skill",
						map[string]any{
							"coach_student_id": rid, "skill": sk,
							"rating": r, "first_rating": r - 0.5,
						})
				}
			}
			if s.columnReady("coaching_shared_notes", "id") {
				_, _ = s.sb.Insert("coaching_shared_notes", map[string]any{
					"coach_student_id": rid,
					"title":            "This week's focus",
					"body":             "Third-shot drops under pressure, then get to the kitchen line. 20 min of drops before each session.",
				})
			}
			if s.columnReady("coaching_assignments", "id") {
				_, _ = s.sb.Insert("coaching_assignments", map[string]any{
					"coach_student_id": rid,
					"title":            "Drop-and-freeze drill",
					"skill_category":   "drops",
					"goal":             "10 in a row landing in the kitchen, then freeze before advancing.",
					"status":           "assigned",
					"assigned_by":      coachID,
				})
			}
			if s.programsReady() {
				_, _ = s.sb.Insert("coaching_programs", map[string]any{
					"coach_student_id": rid,
					"title":            "4-week third-shot mastery",
					"weeks": []map[string]any{
						{"focus": "Week 1 · Drop mechanics — soft hands, contact point", "done": true},
						{"focus": "Week 2 · Drop under light pressure + freeze", "done": true},
						{"focus": "Week 3 · Transition zone footwork to the kitchen", "done": false},
						{"focus": "Week 4 · Live points: drop → advance → reset", "done": false},
					},
				})
			}
			if s.pbVisionReady() {
				rolStats := func(sq, third int, unforced float64) map[string]any {
					return map[string]any{
						"matchesAnalyzed": 6, "shotQuality": sq,
						"serveInPct": 92, "returnInPct": 85, "kitchenArrivalPct": 58,
						"thirdDropPct": third, "dinkErrorPct": 14, "speedupWinPct": 52,
						"winnersPerGame": 5.4, "unforcedPerGame": unforced,
						"avgShotSpeedMph": 29, "topShotSpeedMph": 54, "avgRallyLength": 6.8,
						"shotMix": map[string]any{
							"Dinks": 31, "Volleys": 16, "Serves": 15,
							"Returns": 14, "Drives": 13, "Drops": 11,
						},
						"strengths": []string{"Hands at the net", "Serve consistency"},
						"improve":   []string{"3rd-shot drop under pressure", "Cut unforced errors"},
					}
				}
				nowTS := time.Now().UTC()
				latest := rolStats(69, 44, 9.1)
				_, _ = s.sb.Insert("coaching_pbvision", map[string]any{
					"coach_student_id": rid,
					"rating":           3.35,
					"last_synced_at":   nowTS.Format(time.RFC3339),
					"stats":            latest,
				})
				// History showing progress over the last ~3 weeks.
				s.seedPBVisionReport(rid, 3.05, nowTS.AddDate(0, 0, -21).Format(time.RFC3339), rolStats(61, 52, 12.0))
				s.seedPBVisionReport(rid, 3.20, nowTS.AddDate(0, 0, -10).Format(time.RFC3339), rolStats(65, 48, 10.4))
				s.seedPBVisionReport(rid, 3.35, nowTS.Format(time.RFC3339), latest)
			}
		}
	}

	// Sample PB Vision report for Taylor so the PB Vision tab has example stats.
	// (Fresh thread row each run → no prior pbvision row to clear.)
	if s.pbVisionReady() && taylorID != "" {
		tayStats := func(sq, third int, unforced float64) map[string]any {
			return map[string]any{
				"matchesAnalyzed": 8, "shotQuality": sq,
				"serveInPct": 94, "returnInPct": 88, "kitchenArrivalPct": 61,
				"thirdDropPct": third, "dinkErrorPct": 12, "speedupWinPct": 55,
				"winnersPerGame": 6.2, "unforcedPerGame": unforced,
				"avgShotSpeedMph": 31, "topShotSpeedMph": 58, "avgRallyLength": 7.3,
				"shotMix": map[string]any{
					"Dinks": 34, "Volleys": 15, "Serves": 14,
					"Returns": 13, "Drives": 12, "Drops": 12,
				},
				"strengths": []string{"Hands at the net", "Dink consistency", "Kitchen coverage"},
				"improve":   []string{"3rd-shot drop under pressure", "Return depth", "Cut unforced errors"},
			}
		}
		nowTS := time.Now().UTC()
		latest := tayStats(72, 47, 8.4)
		_, _ = s.sb.Insert("coaching_pbvision", map[string]any{
			"coach_student_id": taylorID,
			"rating":           3.42,
			"last_synced_at":   nowTS.Format(time.RFC3339),
			"stats":            latest,
		})
		s.seedPBVisionReport(taylorID, 3.18, nowTS.AddDate(0, 0, -24).Format(time.RFC3339), tayStats(64, 55, 10.6))
		s.seedPBVisionReport(taylorID, 3.30, nowTS.AddDate(0, 0, -12).Format(time.RFC3339), tayStats(68, 51, 9.3))
		s.seedPBVisionReport(taylorID, 3.42, nowTS.Format(time.RFC3339), latest)
	}

	// Sample schedule entries (booked sessions + open + a day off). Re-runnable:
	// clears prior demo entries first (tagged with a "Demo:" note prefix).
	if s.scheduleReady() {
		_ = s.sb.Delete("coaching_schedule",
			"coach_id=eq."+store.Q(coachID)+"&notes=like."+store.Q("Demo:")+"*")
		now := time.Now().UTC()
		day := func(offset, hour int) time.Time {
			d := now.AddDate(0, 0, offset)
			return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, time.UTC)
		}
		addSched := func(kind, studentID, label string, start time.Time, durMins int, allDay bool, end time.Time, location, note string) {
			row := map[string]any{
				"coach_id":  coachID,
				"kind":      kind,
				"starts_at": start.Format(time.RFC3339),
				"all_day":   allDay,
				"location":  orNull(location),
				"notes":     "Demo: " + note,
			}
			if studentID != "" {
				row["coach_student_id"] = studentID
				row["student_label"] = label
			}
			if kind == "session" {
				row["ends_at"] = start.Add(time.Duration(durMins) * time.Minute).Format(time.RFC3339)
			} else if !end.IsZero() {
				row["ends_at"] = end.Format(time.RFC3339)
			}
			_, _ = s.sb.Insert("coaching_schedule", row)
		}
		addSched("session", alexID, "Alex Cruz", day(1, 22), 60, false, time.Time{}, "Community courts", "3rd-shot drop work")
		addSched("session", taylorID, "Taylor Kim", day(3, 23), 60, false, time.Time{}, "Community courts", "net game + resets")
		addSched("open", "", "", day(2, 16), 0, false, day(2, 19), "", "open for lessons")
		addSched("blocked", "", "", day(5, 0), 0, true, time.Time{}, "", "day off")
	}

	return count, nil
}

// --- Coach schedule: booked sessions, open availability, blocked time ---

func (s *Service) scheduleReady() bool {
	return s.columnReady("coaching_schedule", "id")
}

func mapScheduleItem(row map[string]any) model.CoachingScheduleItem {
	return model.CoachingScheduleItem{
		ID:             asStr(row, "id"),
		Kind:           asStr(row, "kind"),
		CoachStudentID: asStr(row, "coach_student_id"),
		StudentLabel:   asStr(row, "student_label"),
		StartsAt:       asStr(row, "starts_at"),
		EndsAt:         asStr(row, "ends_at"),
		AllDay:         asBool(row, "all_day"),
		Location:       asStr(row, "location"),
		Notes:          asStr(row, "notes"),
		Status:         asStr(row, "status"),
	}
}

// ListCoachSchedule returns the coach's schedule, earliest first.
func (s *Service) ListCoachSchedule(coachID string) ([]model.CoachingScheduleItem, error) {
	if !s.scheduleReady() {
		return []model.CoachingScheduleItem{}, nil
	}
	rows, err := s.sb.Select("coaching_schedule",
		"coach_id=eq."+store.Q(coachID)+"&order=starts_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingScheduleItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapScheduleItem(r))
	}
	return out, nil
}

// AddCoachScheduleItem books a session / opens availability / blocks time.
func (s *Service) AddCoachScheduleItem(coachID string, req model.CoachingScheduleRequest) (model.CoachingScheduleItem, error) {
	if !s.scheduleReady() {
		return model.CoachingScheduleItem{}, ErrCoachingUnavailable
	}
	kind := strings.TrimSpace(req.Kind)
	if kind != "session" && kind != "open" && kind != "blocked" {
		return model.CoachingScheduleItem{}, errors.New("invalid schedule kind")
	}
	if strings.TrimSpace(req.StartsAt) == "" {
		return model.CoachingScheduleItem{}, errors.New("pick a date and time")
	}
	label := strings.TrimSpace(req.StudentLabel)
	row := map[string]any{
		"coach_id":  coachID,
		"kind":      kind,
		"starts_at": req.StartsAt,
		"all_day":   req.AllDay,
		"ends_at":   orNull(strings.TrimSpace(req.EndsAt)),
		"location":  orNull(strings.TrimSpace(req.Location)),
		"notes":     orNull(strings.TrimSpace(req.Notes)),
	}
	if kind == "session" && strings.TrimSpace(req.CoachStudentID) != "" {
		row["coach_student_id"] = req.CoachStudentID
		if label == "" {
			if r, _ := s.sb.SelectOne("coach_students",
				"id=eq."+store.Q(req.CoachStudentID)+"&select=student_name,student_email"); r != nil {
				label = asStr(r, "student_name")
				if label == "" {
					label = asStr(r, "student_email")
				}
			}
		}
	}
	if label != "" {
		row["student_label"] = label
	}
	ins, err := s.sb.Insert("coaching_schedule", row)
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}
	if len(ins) == 0 {
		return model.CoachingScheduleItem{}, errors.New("could not save that")
	}
	return mapScheduleItem(ins[0]), nil
}

// markThreadNotificationsRead clears the viewer's unread bell rows that deep-link
// into this thread (so opening the thread stops it showing as unread in the bell).
func (s *Service) markThreadNotificationsRead(userID, threadID string) {
	if userID == "" || threadID == "" ||
		!s.columnReady("user_notifications", "read") {
		return
	}
	_, _ = s.sb.Update("user_notifications",
		"recipient_id=eq."+store.Q(userID)+"&read=is.false"+
			"&link=like.coaching:"+threadID+"*",
		map[string]any{"read": true})
}

// notifyStudentOfThread resolves a thread's student (linked id, else email, else
// phone) and pushes them a message. Best-effort no-op if unresolved.
func (s *Service) notifyStudentOfThread(threadID, actorID, actorName, body, link string) {
	if threadID == "" {
		return
	}
	row, _ := s.sb.SelectOne("coach_students", "id=eq."+store.Q(threadID)+
		"&select=student_id,student_email,student_phone")
	if row == nil {
		return
	}
	recipient := asStr(row, "student_id")
	if recipient == "" {
		if e := asStr(row, "student_email"); e != "" {
			recipient = s.userIDByEmail(e)
		}
	}
	if recipient == "" {
		if p := asStr(row, "student_phone"); p != "" {
			recipient = s.userIDByPhone(p)
		}
	}
	if recipient != "" && recipient != actorID {
		s.notifyUser(recipient, "coaching", actorID, actorName, body, link)
	}
}

// DeleteCoachScheduleItem removes a schedule entry the coach owns.
func (s *Service) DeleteCoachScheduleItem(coachID, id string) error {
	if !s.scheduleReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_schedule",
		"id=eq."+store.Q(id)+"&select=coach_id,kind,coach_student_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	if err := s.sb.Delete("coaching_schedule", "id=eq."+store.Q(id)); err != nil {
		return err
	}
	// A booked session the coach dropped → tell the student.
	if asStr(row, "kind") == "session" {
		s.notifyStudentOfThread(asStr(row, "coach_student_id"), coachID,
			s.coachingName(coachID), "Your coach cancelled your 1:1 session",
			"coaching:"+asStr(row, "coach_student_id"))
	}
	return nil
}

// SetSessionAttendance marks a booked session attended / no_show (coach only).
// "" clears it. No-op until the status column runs.
func (s *Service) SetSessionAttendance(coachID, sessionID, status string) error {
	if !s.scheduleReady() || !s.columnReady("coaching_schedule", "status") {
		return ErrCoachingUnavailable
	}
	status = strings.TrimSpace(status)
	if status != "" && status != "attended" && status != "no_show" {
		return errors.New("invalid status")
	}
	row, _ := s.sb.SelectOne("coaching_schedule",
		"id=eq."+store.Q(sessionID)+"&select=coach_id,kind")
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	if asStr(row, "kind") != "session" {
		return errors.New("only a booked session has attendance")
	}
	_, err := s.sb.Update("coaching_schedule", "id=eq."+store.Q(sessionID),
		map[string]any{"status": orNull(status)})
	return err
}

// UpdateCoachScheduleItem edits a schedule entry the coach owns (its kind is
// fixed; time/location/notes/student are editable).
func (s *Service) UpdateCoachScheduleItem(coachID, id string, req model.CoachingScheduleRequest) (model.CoachingScheduleItem, error) {
	if !s.scheduleReady() {
		return model.CoachingScheduleItem{}, ErrCoachingUnavailable
	}
	cur, err := s.sb.SelectOne("coaching_schedule",
		"id=eq."+store.Q(id)+"&select=coach_id,kind,coach_student_id")
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}
	if cur == nil {
		return model.CoachingScheduleItem{}, ErrNotFound
	}
	if asStr(cur, "coach_id") != coachID {
		return model.CoachingScheduleItem{}, ErrForbidden
	}
	if strings.TrimSpace(req.StartsAt) == "" {
		return model.CoachingScheduleItem{}, errors.New("pick a date and time")
	}
	kind := asStr(cur, "kind")
	upd := map[string]any{
		"starts_at": req.StartsAt,
		"all_day":   req.AllDay,
		"ends_at":   orNull(strings.TrimSpace(req.EndsAt)),
		"location":  orNull(strings.TrimSpace(req.Location)),
		"notes":     orNull(strings.TrimSpace(req.Notes)),
	}
	if kind == "session" {
		label := strings.TrimSpace(req.StudentLabel)
		if id := strings.TrimSpace(req.CoachStudentID); id != "" {
			upd["coach_student_id"] = id
			if label == "" {
				if r, _ := s.sb.SelectOne("coach_students",
					"id=eq."+store.Q(id)+"&select=student_name,student_email"); r != nil {
					label = asStr(r, "student_name")
					if label == "" {
						label = asStr(r, "student_email")
					}
				}
			}
		}
		if label != "" {
			upd["student_label"] = label
		}
	}
	out, err := s.sb.Update("coaching_schedule", "id=eq."+store.Q(id), upd)
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}
	// Booked session's time/location changed → tell the student.
	if kind == "session" {
		threadID := strings.TrimSpace(req.CoachStudentID)
		if threadID == "" {
			threadID = asStr(cur, "coach_student_id")
		}
		s.notifyStudentOfThread(threadID, coachID, s.coachingName(coachID),
			"Your coach updated your 1:1 session time", "coaching:"+threadID)
	}
	if len(out) > 0 {
		return mapScheduleItem(out[0]), nil
	}
	return model.CoachingScheduleItem{}, errors.New("could not update that item")
}

// --- Drill library + assignments (a student's game plan) ---

func (s *Service) drillsReady() bool {
	return s.columnReady("coaching_drills", "id")
}

func (s *Service) assignmentsReady() bool {
	return s.columnReady("coaching_assignments", "id")
}

func mapDrill(row map[string]any) model.CoachingDrill {
	return model.CoachingDrill{
		ID:            asStr(row, "id"),
		CoachID:       asStr(row, "coach_id"),
		Title:         asStr(row, "title"),
		SkillCategory: asStr(row, "skill_category"),
		LevelBand:     asStr(row, "level_band"),
		Format:        asStr(row, "format"),
		Goal:          asStr(row, "goal"),
		Description:   asStr(row, "description"),
		VideoURL:      asStr(row, "video_url"),
		IsStarter:     asBool(row, "is_starter"),
		CreatedAt:     asStr(row, "created_at"),
	}
}

func mapAssignment(row map[string]any) model.CoachingAssignment {
	return model.CoachingAssignment{
		ID:             asStr(row, "id"),
		CoachStudentID: asStr(row, "coach_student_id"),
		DrillID:        asStr(row, "drill_id"),
		Title:          asStr(row, "title"),
		SkillCategory:  asStr(row, "skill_category"),
		Goal:           asStr(row, "goal"),
		Status:         asStr(row, "status"),
		DueAt:          asStr(row, "due_at"),
		CompletedAt:    asStr(row, "completed_at"),
		CompletedBy:    asStr(row, "completed_by"),
		CreatedAt:      asStr(row, "created_at"),
	}
}

// ListDrills returns the shared starter drills plus the coach's own, own first.
func (s *Service) ListDrills(coachID string) ([]model.CoachingDrill, error) {
	if !s.drillsReady() {
		return []model.CoachingDrill{}, nil
	}
	rows, err := s.sb.Select("coaching_drills",
		"or=(coach_id.eq."+coachID+",is_starter.is.true)&order=is_starter.asc,created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingDrill, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapDrill(r))
	}
	return out, nil
}

// CreateDrill adds a coach's own custom drill to their library.
func (s *Service) CreateDrill(coachID string, req model.CoachingDrillRequest) (model.CoachingDrill, error) {
	if !s.drillsReady() {
		return model.CoachingDrill{}, ErrCoachingUnavailable
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return model.CoachingDrill{}, errors.New("give the drill a title")
	}
	row := map[string]any{
		"coach_id":       coachID,
		"title":          title,
		"skill_category": orNull(strings.TrimSpace(req.SkillCategory)),
		"level_band":     orNull(strings.TrimSpace(req.LevelBand)),
		"format":         orNull(strings.TrimSpace(req.Format)),
		"goal":           orNull(strings.TrimSpace(req.Goal)),
		"description":    orNull(strings.TrimSpace(req.Description)),
		"video_url":      orNull(strings.TrimSpace(req.VideoURL)),
		"is_starter":     false,
	}
	ins, err := s.sb.Insert("coaching_drills", row)
	if err != nil {
		return model.CoachingDrill{}, err
	}
	if len(ins) == 0 {
		return model.CoachingDrill{}, errors.New("could not save that drill")
	}
	return mapDrill(ins[0]), nil
}

// DeleteDrill removes one of the coach's OWN drills (never a shared starter).
func (s *Service) DeleteDrill(coachID, id string) error {
	if !s.drillsReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_drills",
		"id=eq."+store.Q(id)+"&select=coach_id,is_starter")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asBool(row, "is_starter") || asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	return s.sb.Delete("coaching_drills", "id=eq."+store.Q(id))
}

// ListAssignments returns a thread's assignments (goals), newest first, for a member.
func (s *Service) ListAssignments(threadID, userID, email string) ([]model.CoachingAssignment, error) {
	if !s.assignmentsReady() {
		return []model.CoachingAssignment{}, nil
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return nil, err
	}
	rows, err := s.sb.Select("coaching_assignments",
		"coach_student_id=eq."+store.Q(threadID)+"&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingAssignment, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapAssignment(r))
	}
	return out, nil
}

// AssignDrill assigns a drill (by id, snapshotting its fields) or an ad-hoc goal
// to a roster student. Coach-only; pings the student.
func (s *Service) AssignDrill(threadID, coachID, email string, req model.AssignDrillRequest) (model.CoachingAssignment, error) {
	if !s.assignmentsReady() {
		return model.CoachingAssignment{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, coachID, email)
	if err != nil {
		return model.CoachingAssignment{}, err
	}
	if role != "coach" {
		return model.CoachingAssignment{}, ErrForbidden
	}
	title := strings.TrimSpace(req.Title)
	skill := strings.TrimSpace(req.SkillCategory)
	goal := strings.TrimSpace(req.Goal)
	drillID := strings.TrimSpace(req.DrillID)
	if drillID != "" && s.drillsReady() {
		if d, _ := s.sb.SelectOne("coaching_drills", "id=eq."+store.Q(drillID)); d != nil {
			if title == "" {
				title = asStr(d, "title")
			}
			if skill == "" {
				skill = asStr(d, "skill_category")
			}
			if goal == "" {
				goal = asStr(d, "goal")
			}
		}
	}
	if title == "" {
		return model.CoachingAssignment{}, errors.New("pick a drill or name a goal")
	}
	row := map[string]any{
		"coach_student_id": threadID,
		"drill_id":         orNull(drillID),
		"title":            title,
		"skill_category":   orNull(skill),
		"goal":             orNull(goal),
		"status":           "assigned",
		"due_at":           orNull(strings.TrimSpace(req.DueAt)),
		"assigned_by":      coachID,
	}
	ins, err := s.sb.Insert("coaching_assignments", row)
	if err != nil {
		return model.CoachingAssignment{}, err
	}
	if len(ins) == 0 {
		return model.CoachingAssignment{}, errors.New("could not save that")
	}
	s.bumpThreadActivity(threadID)
	s.notifyCoachingCounterpart(cs, "coach", coachID, s.coachingName(coachID), "New drill assigned: "+title)
	return mapAssignment(ins[0]), nil
}

// SetAssignmentDone marks an assignment done (or reopens it). Either the coach or
// the addressed student may do it (self-check); records who. Pings the counterpart.
func (s *Service) SetAssignmentDone(assignmentID, userID, email string, done bool) (model.CoachingAssignment, error) {
	if !s.assignmentsReady() {
		return model.CoachingAssignment{}, ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_assignments", "id=eq."+store.Q(assignmentID))
	if err != nil {
		return model.CoachingAssignment{}, err
	}
	if row == nil {
		return model.CoachingAssignment{}, ErrNotFound
	}
	threadID := asStr(row, "coach_student_id")
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingAssignment{}, err
	}
	upd := map[string]any{}
	if done {
		upd["status"] = "done"
		upd["completed_at"] = now()
		upd["completed_by"] = role
	} else {
		upd["status"] = "assigned"
		upd["completed_at"] = nil
		upd["completed_by"] = nil
	}
	out, err := s.sb.Update("coaching_assignments", "id=eq."+store.Q(assignmentID), upd)
	if err != nil {
		return model.CoachingAssignment{}, err
	}
	s.bumpThreadActivity(threadID)
	if done {
		s.notifyCoachingCounterpart(cs, role, userID, s.coachingName(userID), "Completed: "+asStr(row, "title"))
	}
	if len(out) == 0 {
		return mapAssignment(row), nil
	}
	return mapAssignment(out[0]), nil
}

// DeleteAssignment removes an assignment from a student's plan (coach-only).
func (s *Service) DeleteAssignment(assignmentID, userID, email string) error {
	if !s.assignmentsReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_assignments",
		"id=eq."+store.Q(assignmentID)+"&select=coach_student_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	_, role, merr := s.threadMembership(asStr(row, "coach_student_id"), userID, email)
	if merr != nil {
		return merr
	}
	if role != "coach" {
		return ErrForbidden
	}
	return s.sb.Delete("coaching_assignments", "id=eq."+store.Q(assignmentID))
}

// --- Per-skill ratings (the coach's rubric assessment) ---

// coachingSkills is the canonical 6-skill rubric (USAP matrix + PB Vision axes).
var coachingSkills = []string{"serve", "return", "dinks", "drops", "volleys", "strategy"}

func validSkill(skill string) bool {
	for _, s := range coachingSkills {
		if s == skill {
			return true
		}
	}
	return false
}

func (s *Service) skillsReady() bool {
	return s.columnReady("coaching_skill_ratings", "id")
}

func mapSkillRating(row map[string]any) model.CoachingSkillRating {
	return model.CoachingSkillRating{
		Skill:       asStr(row, "skill"),
		Rating:      asFloatOr(row, "rating", 0),
		FirstRating: asFloatOr(row, "first_rating", 0),
		UpdatedAt:   asStr(row, "updated_at"),
	}
}

// ListSkillRatings returns a thread's per-skill ratings, for a member.
func (s *Service) ListSkillRatings(threadID, userID, email string) ([]model.CoachingSkillRating, error) {
	if !s.skillsReady() {
		return []model.CoachingSkillRating{}, nil
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return nil, err
	}
	rows, err := s.sb.Select("coaching_skill_ratings",
		"coach_student_id=eq."+store.Q(threadID))
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingSkillRating, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapSkillRating(r))
	}
	return out, nil
}

// SetSkillRating sets one skill's 1-5 rating for a student (coach-only). Captures
// first_rating on the first set so Progress can show "since you started".
func (s *Service) SetSkillRating(threadID, coachID, email, skill string, rating float64) (model.CoachingSkillRating, error) {
	if !s.skillsReady() {
		return model.CoachingSkillRating{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, coachID, email)
	if err != nil {
		return model.CoachingSkillRating{}, err
	}
	if role != "coach" {
		return model.CoachingSkillRating{}, ErrForbidden
	}
	skill = strings.ToLower(strings.TrimSpace(skill))
	if !validSkill(skill) {
		return model.CoachingSkillRating{}, errors.New("unknown skill")
	}
	if rating < 0 || rating > 5 {
		return model.CoachingSkillRating{}, errors.New("rating must be 0-5")
	}
	filter := "coach_student_id=eq." + store.Q(threadID) + "&skill=eq." + store.Q(skill)
	existing, _ := s.sb.SelectOne("coaching_skill_ratings", filter)
	if existing != nil {
		out, err := s.sb.Update("coaching_skill_ratings",
			"id=eq."+store.Q(asStr(existing, "id")),
			map[string]any{"rating": rating, "updated_at": now()})
		if err != nil {
			return model.CoachingSkillRating{}, err
		}
		s.bumpThreadActivity(threadID)
		s.notifyCoachingCounterpart(cs, "coach", coachID, s.coachingName(cs.CoachID),
			"Your coach updated your skill assessment")
		if len(out) > 0 {
			return mapSkillRating(out[0]), nil
		}
		return mapSkillRating(existing), nil
	}
	ins, err := s.sb.Insert("coaching_skill_ratings", map[string]any{
		"coach_student_id": threadID,
		"skill":            skill,
		"rating":           rating,
		"first_rating":     rating,
		"updated_at":       now(),
	})
	if err != nil {
		return model.CoachingSkillRating{}, err
	}
	if len(ins) == 0 {
		return model.CoachingSkillRating{}, errors.New("could not save that rating")
	}
	s.bumpThreadActivity(threadID)
	s.notifyCoachingCounterpart(cs, "coach", coachID, s.coachingName(cs.CoachID),
		"Your coach rated your progress")
	return mapSkillRating(ins[0]), nil
}

// --- Coaching marketplace: public coach profiles + nearby discovery ---

func (s *Service) coachProfilesReady() bool {
	return s.columnReady("coach_profiles", "user_id")
}

func mapCoachProfile(row map[string]any) model.CoachProfile {
	return model.CoachProfile{
		UserID:          asStr(row, "user_id"),
		Name:            asStr(row, "name"),
		Listed:          asBool(row, "listed"),
		Bio:             asStr(row, "bio"),
		YearsExperience: asIntPtr(row, "years_experience"),
		City:            asStr(row, "city"),
		Lat:             asFloatPtr(row, "lat"),
		Lng:             asFloatPtr(row, "lng"),
		HourlyRateCents: asIntPtr(row, "hourly_rate_cents"),
		Skills:          asStr(row, "skills"),
		PhotoURL:        asStr(row, "photo_url"),
		HasIntroVideo:   strings.TrimSpace(asStr(row, "intro_video_url")) != "",
		CancelPolicy:    asStr(row, "cancel_policy"),
		Verified:        asBool(row, "verified"),
		Certifications:  asStr(row, "certifications"),
	}
}

// SetCoachVerified grants/revokes a coach's verified badge (owner-gated in API).
func (s *Service) SetCoachVerified(coachUserID string, verified bool) error {
	if !s.coachProfilesReady() || !s.columnReady("coach_profiles", "verified") {
		return ErrCoachingUnavailable
	}
	_, err := s.sb.Upsert("coach_profiles", "user_id", map[string]any{
		"user_id":  coachUserID,
		"name":     s.coachingName(coachUserID),
		"verified": verified,
	})
	return err
}

// cancelCutoffHours is how long before start a booking locks (no cancel).
func cancelCutoffHours(policy string) int {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "strict":
		return 72
	case "moderate":
		return 24
	default:
		return 0 // flexible — cancel anytime
	}
}

// enforceCancelCutoff returns an error if it's too close to startsAt to cancel
// under the coach's policy.
func (s *Service) enforceCancelCutoff(coachUserID, startsAt string) error {
	if coachUserID == "" || !s.coachProfilesReady() ||
		!s.columnReady("coach_profiles", "cancel_policy") {
		return nil
	}
	row, _ := s.sb.SelectOne("coach_profiles",
		"user_id=eq."+store.Q(coachUserID)+"&select=cancel_policy")
	hrs := cancelCutoffHours(asStr(row, "cancel_policy"))
	if hrs <= 0 {
		return nil
	}
	start, ok := parseSchedTime(startsAt)
	if !ok {
		return nil
	}
	if time.Until(start) < time.Duration(hrs)*time.Hour {
		return fmt.Errorf("this coach's cancellation policy locks bookings %d hours before start", hrs)
	}
	return nil
}

func (s *Service) introVideoReady() bool {
	return s.columnReady("coach_profiles", "intro_video_url")
}

// SetMyCoachIntroVideo saves the coach's intro-clip object path (already uploaded
// to the coaching-videos bucket) on their profile.
func (s *Service) SetMyCoachIntroVideo(userID, path string) error {
	if userID == "" {
		return errors.New("not signed in")
	}
	if !s.coachProfilesReady() || !s.introVideoReady() {
		return ErrCoachingUnavailable
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("no video uploaded")
	}
	_, err := s.sb.Upsert("coach_profiles", "user_id", map[string]any{
		"user_id":         userID,
		"name":            s.coachingName(userID),
		"intro_video_url": path,
		"updated_at":      now(),
	})
	return err
}

// ClearMyCoachIntroVideo removes the coach's intro clip reference.
func (s *Service) ClearMyCoachIntroVideo(userID string) error {
	if !s.coachProfilesReady() || !s.introVideoReady() {
		return ErrCoachingUnavailable
	}
	_, err := s.sb.Upsert("coach_profiles", "user_id", map[string]any{
		"user_id":         userID,
		"intro_video_url": nil,
	})
	return err
}

// CoachIntroVideoURL returns a short-lived SIGNED playback URL for a coach's
// intro clip (any signed-in viewer). Empty string when none is set.
func (s *Service) CoachIntroVideoURL(coachUserID string) (string, error) {
	if !s.coachProfilesReady() || !s.introVideoReady() {
		return "", nil
	}
	row, err := s.sb.SelectOne("coach_profiles",
		"user_id=eq."+store.Q(coachUserID)+"&select=intro_video_url")
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(asStr(row, "intro_video_url"))
	if path == "" {
		return "", nil
	}
	m, err := s.sb.SignedURLs("coaching-videos",
		[]string{coachingVideoPath(path)}, 6*60*60)
	if err != nil {
		return "", err
	}
	return m[coachingVideoPath(path)], nil
}

// GetMyCoachProfile returns the signed-in coach's discovery profile (a default,
// name-filled one if they haven't set it up yet).
func (s *Service) GetMyCoachProfile(userID string) (model.CoachProfile, error) {
	if !s.coachProfilesReady() {
		return model.CoachProfile{Name: s.coachingName(userID)}, nil
	}
	row, err := s.sb.SelectOne("coach_profiles", "user_id=eq."+store.Q(userID))
	if err != nil {
		return model.CoachProfile{}, err
	}
	if row == nil {
		return model.CoachProfile{Name: s.coachingName(userID)}, nil
	}
	p := mapCoachProfile(row)
	if p.Name == "" {
		p.Name = s.coachingName(userID)
	}
	// No dedicated instructor photo yet → fall back to the account avatar so the
	// editor preview + card aren't empty.
	if p.PhotoURL == "" {
		if ph := s.photosByUser([]string{userID}); ph[userID] != "" {
			p.PhotoURL = ph[userID]
		}
	}
	return p, nil
}

// UpsertCoachProfile creates/updates the coach's discovery profile. City is
// geocoded to lat/lng so players can rank coaches by distance.
func (s *Service) UpsertCoachProfile(userID string, req model.CoachProfileRequest) (model.CoachProfile, error) {
	if !s.coachProfilesReady() {
		return model.CoachProfile{}, ErrCoachingUnavailable
	}
	yearsReady := s.columnReady("coach_profiles", "years_experience")
	// Listing publicly requires a bio + years of experience so players see a
	// real, vetted profile. (Years enforced once its column exists.)
	if req.Listed {
		if strings.TrimSpace(req.Bio) == "" {
			return model.CoachProfile{}, errors.New("add a short bio before listing your profile")
		}
		if yearsReady && (req.YearsExperience == nil || *req.YearsExperience < 0) {
			return model.CoachProfile{}, errors.New("add your years of experience before listing your profile")
		}
	}
	row := map[string]any{
		"user_id":           userID,
		"name":              s.coachingName(userID),
		"listed":            req.Listed,
		"bio":               orNull(strings.TrimSpace(req.Bio)),
		"city":              orNull(strings.TrimSpace(req.City)),
		"skills":            orNull(strings.TrimSpace(req.Skills)),
		"hourly_rate_cents": req.HourlyRateCents,
		"updated_at":        now(),
	}
	if yearsReady {
		row["years_experience"] = req.YearsExperience
	}
	if s.columnReady("coach_profiles", "cancel_policy") {
		pol := strings.ToLower(strings.TrimSpace(req.CancelPolicy))
		if pol != "moderate" && pol != "strict" {
			pol = "flexible"
		}
		row["cancel_policy"] = pol
	}
	if s.columnReady("coach_profiles", "certifications") {
		row["certifications"] = orNull(strings.TrimSpace(req.Certifications))
	}
	if city := strings.TrimSpace(req.City); city != "" {
		if lat, lng := bestEffortGeocode(city); lat != nil && lng != nil {
			row["lat"] = *lat
			row["lng"] = *lng
		}
	}
	out, err := s.sb.Upsert("coach_profiles", "user_id", row)
	if err != nil {
		return model.CoachProfile{}, err
	}
	if len(out) > 0 {
		p := mapCoachProfile(out[0])
		if p.Name == "" {
			p.Name = s.coachingName(userID)
		}
		return p, nil
	}
	return s.GetMyCoachProfile(userID)
}

// --- Saved / favorite coaches ---

func (s *Service) favoritesReady() bool {
	return s.columnReady("coach_favorites", "id")
}

// ToggleFavoriteCoach adds/removes a saved coach; returns the new favorited state.
func (s *Service) ToggleFavoriteCoach(userID, coachUserID string) (bool, error) {
	if !s.favoritesReady() || userID == "" || coachUserID == "" {
		return false, ErrCoachingUnavailable
	}
	existing, _ := s.sb.SelectOne("coach_favorites",
		"user_id=eq."+store.Q(userID)+"&coach_user_id=eq."+store.Q(coachUserID)+"&select=id")
	if existing != nil {
		return false, s.sb.Delete("coach_favorites",
			"user_id=eq."+store.Q(userID)+"&coach_user_id=eq."+store.Q(coachUserID))
	}
	_, err := s.sb.Insert("coach_favorites", map[string]any{
		"user_id": userID, "coach_user_id": coachUserID,
	})
	return err == nil, err
}

// favoritesSet returns which of coachIDs the viewer has saved.
func (s *Service) favoritesSet(userID string, coachIDs []string) map[string]bool {
	out := map[string]bool{}
	if !s.favoritesReady() || userID == "" || len(coachIDs) == 0 {
		return out
	}
	rows, err := s.sb.Select("coach_favorites",
		"user_id=eq."+store.Q(userID)+"&coach_user_id="+store.In(coachIDs)+
			"&select=coach_user_id")
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[asStr(r, "coach_user_id")] = true
	}
	return out
}

// ListFavoriteCoaches returns the viewer's saved coaches (profile + rating +
// photo), newest-saved first.
func (s *Service) ListFavoriteCoaches(userID string) ([]model.CoachProfile, error) {
	if !s.favoritesReady() || !s.coachProfilesReady() || userID == "" {
		return []model.CoachProfile{}, nil
	}
	favs, err := s.sb.Select("coach_favorites",
		"user_id=eq."+store.Q(userID)+"&order=created_at.desc&select=coach_user_id")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(favs))
	for _, f := range favs {
		if id := asStr(f, "coach_user_id"); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []model.CoachProfile{}, nil
	}
	rows, err := s.sb.Select("coach_profiles", "user_id="+store.In(ids))
	if err != nil {
		return nil, err
	}
	byID := map[string]model.CoachProfile{}
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		p := mapCoachProfile(r)
		if p.Name == "" {
			p.Name = s.coachingName(p.UserID)
		}
		p.Favorited = true
		byID[p.UserID] = p
		uids = append(uids, p.UserID)
	}
	photos := s.photosByUser(uids)
	agg := s.reviewsAggregate(uids)
	out := make([]model.CoachProfile, 0, len(ids))
	for _, id := range ids { // preserve saved order
		p, ok := byID[id]
		if !ok {
			continue
		}
		if p.PhotoURL == "" {
			p.PhotoURL = photos[id]
		}
		if a, ok := agg[id]; ok {
			avg := a.avg
			p.RatingAvg = &avg
			p.RatingCount = a.count
		}
		out = append(out, p)
	}
	return out, nil
}

// ListCoachesNearby returns listed coaches ranked by distance from (lat,lng).
// Coaches without coordinates sort last; radiusKm<=0 means no radius cap.
func (s *Service) ListCoachesNearby(lat, lng, radiusKm float64, viewerID string) ([]model.CoachProfile, error) {
	if !s.coachProfilesReady() {
		return []model.CoachProfile{}, nil
	}
	rows, err := s.sb.Select("coach_profiles", "listed=is.true")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachProfile, 0, len(rows))
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		p := mapCoachProfile(r)
		if p.Name == "" {
			p.Name = s.coachingName(p.UserID)
		}
		if p.Lat != nil && p.Lng != nil {
			d := haversineKm(lat, lng, *p.Lat, *p.Lng)
			if radiusKm > 0 && d > radiusKm {
				continue
			}
			p.DistanceKm = &d
		}
		out = append(out, p)
		uids = append(uids, p.UserID)
	}
	// Prefer the coach's dedicated instructor photo (already on the row); fall
	// back to their account avatar only when they haven't set one.
	photos := s.photosByUser(uids)
	agg := s.reviewsAggregate(uids)
	favs := s.favoritesSet(viewerID, uids)
	for i := range out {
		if out[i].PhotoURL == "" {
			out[i].PhotoURL = photos[out[i].UserID]
		}
		if a, ok := agg[out[i].UserID]; ok {
			avg := a.avg
			out[i].RatingAvg = &avg
			out[i].RatingCount = a.count
		}
		out[i].Favorited = favs[out[i].UserID]
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].DistanceKm, out[j].DistanceKm
		if di == nil && dj == nil {
			return out[i].Name < out[j].Name
		}
		if di == nil {
			return false
		}
		if dj == nil {
			return true
		}
		return *di < *dj
	})
	return out, nil
}

// --- Coach reviews (marketplace social proof) ---

func (s *Service) coachReviewsReady() bool {
	return s.columnReady("coach_reviews", "id")
}

func mapCoachReview(row map[string]any) model.CoachReview {
	return model.CoachReview{
		ID:          asStr(row, "id"),
		CoachUserID: asStr(row, "coach_user_id"),
		AuthorID:    asStr(row, "author_id"),
		AuthorName:  asStr(row, "author_name"),
		Rating:      asInt(row, "rating"),
		Body:        asStr(row, "body"),
		CreatedAt:   asStr(row, "created_at"),
	}
}

type reviewAgg struct {
	avg   float64
	count int
}

// reviewsAggregate returns avg rating + count per coach user id (batched).
func (s *Service) reviewsAggregate(coachUserIDs []string) map[string]reviewAgg {
	out := map[string]reviewAgg{}
	if !s.coachReviewsReady() || len(coachUserIDs) == 0 {
		return out
	}
	rows, err := s.sb.Select("coach_reviews",
		"coach_user_id="+store.In(coachUserIDs)+"&select=coach_user_id,rating")
	if err != nil {
		return out
	}
	sums := map[string]int{}
	counts := map[string]int{}
	for _, r := range rows {
		id := asStr(r, "coach_user_id")
		sums[id] += asInt(r, "rating")
		counts[id]++
	}
	for id, c := range counts {
		if c > 0 {
			out[id] = reviewAgg{avg: float64(sums[id]) / float64(c), count: c}
		}
	}
	return out
}

// canReviewCoach reports whether authorID has trained with coachUserID — i.e.
// they're on the coach's roster (direct add OR auto-linked on class enrollment).
func (s *Service) canReviewCoach(authorID, coachUserID string) bool {
	if authorID == "" || coachUserID == "" || authorID == coachUserID {
		return false
	}
	if !s.coachingReady() {
		return false
	}
	row, _ := s.sb.SelectOne("coach_students",
		"coach_id=eq."+store.Q(coachUserID)+"&student_id=eq."+store.Q(authorID)+"&select=id&limit=1")
	return row != nil
}

// ListCoachReviews returns a coach's reviews + aggregate, and whether the viewer
// may leave one (plus their existing review, if any).
func (s *Service) ListCoachReviews(coachUserID, viewerID string) (model.CoachReviewsResponse, error) {
	resp := model.CoachReviewsResponse{Reviews: []model.CoachReview{}}
	if !s.coachReviewsReady() {
		return resp, nil
	}
	rows, err := s.sb.Select("coach_reviews",
		"coach_user_id=eq."+store.Q(coachUserID)+"&order=created_at.desc")
	if err != nil {
		return resp, err
	}
	sum := 0
	for _, r := range rows {
		rv := mapCoachReview(r)
		resp.Reviews = append(resp.Reviews, rv)
		sum += rv.Rating
		if viewerID != "" && rv.AuthorID == viewerID {
			mine := rv
			resp.MyReview = &mine
		}
	}
	resp.RatingCount = len(resp.Reviews)
	if resp.RatingCount > 0 {
		avg := float64(sum) / float64(resp.RatingCount)
		resp.RatingAvg = &avg
	}
	resp.CanReview = s.canReviewCoach(viewerID, coachUserID)
	return resp, nil
}

// SubmitCoachReview creates/updates the caller's review of a coach (eligibility
// enforced). One review per (coach, author).
func (s *Service) SubmitCoachReview(authorID, authorName, coachUserID string, req model.CoachReviewRequest) error {
	if !s.coachReviewsReady() {
		return ErrCoachingUnavailable
	}
	if req.Rating < 1 || req.Rating > 5 {
		return errors.New("rating must be 1 to 5 stars")
	}
	if !s.canReviewCoach(authorID, coachUserID) {
		return errors.New("only players who've trained with this coach can review them")
	}
	authorName = strings.TrimSpace(authorName)
	if authorName == "" {
		authorName = s.coachingName(authorID)
	}
	body := strings.TrimSpace(req.Body)
	if r := []rune(body); len(r) > 1000 {
		body = string(r[:1000])
	}
	_, err := s.sb.Upsert("coach_reviews", "coach_user_id,author_id", map[string]any{
		"coach_user_id": coachUserID,
		"author_id":     authorID,
		"author_name":   orNull(strings.TrimSpace(authorName)),
		"rating":        req.Rating,
		"body":          orNull(body),
		"updated_at":    now(),
	})
	return err
}

// DeleteCoachReview removes the caller's own review.
func (s *Service) DeleteCoachReview(authorID, reviewID string) error {
	if !s.coachReviewsReady() {
		return ErrCoachingUnavailable
	}
	return s.sb.Delete("coach_reviews",
		"id=eq."+store.Q(reviewID)+"&author_id=eq."+store.Q(authorID))
}

// --- Thread chat (free-form messaging, distinct from clip feedback) ---

func (s *Service) messagesReady() bool {
	return s.columnReady("coaching_messages", "id")
}

func mapCoachingMessage(row map[string]any) model.CoachingMessage {
	return model.CoachingMessage{
		ID:         asStr(row, "id"),
		SenderID:   asStr(row, "sender_id"),
		SenderRole: asStr(row, "sender_role"),
		Body:       asStr(row, "body"),
		CreatedAt:  asStr(row, "created_at"),
	}
}

// ListThreadMessages returns a thread's chat history (member-gated) and marks it
// read for the viewer.
func (s *Service) ListThreadMessages(threadID, userID, email string) ([]model.CoachingMessage, error) {
	if !s.coachingReady() {
		return nil, ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return nil, err
	}
	if !s.messagesReady() {
		return []model.CoachingMessage{}, nil
	}
	rows, err := s.sb.Select("coaching_messages",
		"coach_student_id=eq."+store.Q(threadID)+"&order=created_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapCoachingMessage(r))
	}
	s.markThreadRead(userID, threadID)
	return out, nil
}

// SendThreadMessage posts a chat message on a thread and notifies the other party.
func (s *Service) SendThreadMessage(threadID, userID, email, name string, req model.CoachingMessageRequest) (model.CoachingMessage, error) {
	if !s.coachingReady() {
		return model.CoachingMessage{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingMessage{}, err
	}
	if !s.messagesReady() {
		return model.CoachingMessage{}, ErrCoachingUnavailable
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return model.CoachingMessage{}, errors.New("message is empty")
	}
	if r := []rune(body); len(r) > 4000 {
		body = string(r[:4000])
	}
	ins, err := s.sb.Insert("coaching_messages", map[string]any{
		"coach_student_id": threadID,
		"sender_id":        userID,
		"sender_role":      role,
		"body":             body,
	})
	if err != nil || len(ins) == 0 {
		if err == nil {
			err = errors.New("could not send message")
		}
		return model.CoachingMessage{}, err
	}
	s.bumpThreadActivity(threadID)
	// Chat-specific notification: "<name> messaged you", deep-linked to the Chat tab.
	sender := strings.TrimSpace(name)
	if sender == "" {
		sender = s.coachingName(userID)
	}
	if sender == "" {
		sender = "Someone"
	}
	s.notifyCoachingCounterpartLink(cs, role, userID, name,
		sender+" messaged you", "coaching:"+threadID+"?tab=chat")
	return mapCoachingMessage(ins[0]), nil
}

// BroadcastToStudents sends the same chat message into each selected student
// thread (a group announcement). Reuses SendThreadMessage, which validates the
// coach owns each thread — non-owned threads are skipped. Returns how many sent.
func (s *Service) BroadcastToStudents(coachID, email, name string, threadIDs []string, body string) (int, error) {
	if !s.coachingReady() || !s.messagesReady() {
		return 0, ErrCoachingUnavailable
	}
	if strings.TrimSpace(body) == "" {
		return 0, errors.New("message is empty")
	}
	if len(threadIDs) == 0 {
		return 0, errors.New("pick at least one student")
	}
	sent := 0
	for _, tid := range threadIDs {
		if tid = strings.TrimSpace(tid); tid == "" {
			continue
		}
		if _, err := s.SendThreadMessage(tid, coachID, email, name,
			model.CoachingMessageRequest{Body: body}); err == nil {
			sent++
		}
	}
	return sent, nil
}

// BulkAssignDrill assigns the same drill/goal to each selected student thread.
// Reuses AssignDrill (which validates coach ownership). Returns how many.
func (s *Service) BulkAssignDrill(coachID, email string, threadIDs []string, req model.AssignDrillRequest) (int, error) {
	if !s.assignmentsReady() {
		return 0, ErrCoachingUnavailable
	}
	if len(threadIDs) == 0 {
		return 0, errors.New("pick at least one student")
	}
	n := 0
	for _, tid := range threadIDs {
		if tid = strings.TrimSpace(tid); tid == "" {
			continue
		}
		if _, err := s.AssignDrill(tid, coachID, email, req); err == nil {
			n++
		}
	}
	return n, nil
}

// --- 1:1 session booking (player books a coach's open availability) ---

func parseSchedTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// ListCoachAvailability returns a coach's upcoming OPEN availability windows
// (the slots a player can book a 1:1 into).
func (s *Service) ListCoachAvailability(coachUserID string) ([]model.CoachingScheduleItem, error) {
	if !s.scheduleReady() {
		return []model.CoachingScheduleItem{}, nil
	}
	rows, err := s.sb.Select("coaching_schedule",
		"coach_id=eq."+store.Q(coachUserID)+"&kind=eq.open&order=starts_at.asc")
	if err != nil {
		return nil, err
	}
	nowT := time.Now().UTC()
	out := make([]model.CoachingScheduleItem, 0, len(rows))
	for _, r := range rows {
		it := mapScheduleItem(r)
		end, ok := parseSchedTime(it.EndsAt)
		if !ok {
			end, _ = parseSchedTime(it.StartsAt)
		}
		if !end.IsZero() && end.Before(nowT) {
			continue // window already passed
		}
		out = append(out, it)
	}
	return out, nil
}

// BookCoachSession books a player's 1:1 session inside one of the coach's open
// windows. Validates the slot fits a window and doesn't collide with an existing
// session, links the player to the coach's roster, and notifies the coach.
func (s *Service) BookCoachSession(playerID, playerEmail, playerName, coachUserID string, req model.CoachBookingRequest) (model.CoachingScheduleItem, error) {
	if !s.scheduleReady() || !s.coachingReady() {
		return model.CoachingScheduleItem{}, ErrCoachingUnavailable
	}
	if playerID == "" || coachUserID == "" || playerID == coachUserID {
		return model.CoachingScheduleItem{}, ErrForbidden
	}
	start, ok := parseSchedTime(req.StartsAt)
	if !ok {
		return model.CoachingScheduleItem{}, errors.New("pick a valid start time")
	}
	dur := req.DurationMins
	if dur <= 0 {
		dur = 60
	}
	if dur < 15 {
		dur = 15
	}
	if dur > 240 {
		dur = 240
	}
	end := start.Add(time.Duration(dur) * time.Minute)
	if start.Before(time.Now().UTC()) {
		return model.CoachingScheduleItem{}, errors.New("that time has already passed")
	}

	// Load the coach's schedule once: confirm the slot fits an open window and
	// doesn't overlap an existing session.
	rows, err := s.sb.Select("coaching_schedule",
		"coach_id=eq."+store.Q(coachUserID)+"&select=kind,starts_at,ends_at")
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}
	inWindow := false
	for _, r := range rows {
		kind := asStr(r, "kind")
		ws, ok1 := parseSchedTime(asStr(r, "starts_at"))
		we, ok2 := parseSchedTime(asStr(r, "ends_at"))
		switch kind {
		case "open":
			if ok1 && ok2 && !start.Before(ws) && !end.After(we) {
				inWindow = true
			}
		case "session":
			// Overlap if start < existing end AND end > existing start.
			se := we
			if !ok2 {
				se = ws.Add(time.Hour)
			}
			if ok1 && start.Before(se) && end.After(ws) {
				return model.CoachingScheduleItem{}, errors.New("that time is already booked — pick another slot")
			}
		}
	}
	if !inWindow {
		return model.CoachingScheduleItem{}, errors.New("that time isn't within the coach's open availability")
	}

	// Link the player to the coach's roster and get the thread id.
	s.ensureCoachStudentLink(coachUserID, playerID, playerName, playerEmail)
	link, _ := s.sb.SelectOne("coach_students",
		"coach_id=eq."+store.Q(coachUserID)+"&or=(student_id.eq."+store.Q(playerID)+
			",student_email.eq."+store.Q(strings.ToLower(strings.TrimSpace(playerEmail)))+")&select=id&limit=1")
	threadID := asStr(link, "id")

	label := strings.TrimSpace(playerName)
	if label == "" {
		label = playerEmail
	}
	row := map[string]any{
		"coach_id":  coachUserID,
		"kind":      "session",
		"starts_at": start.Format(time.RFC3339),
		"ends_at":   end.Format(time.RFC3339),
		"all_day":   false,
		"location":  orNull(strings.TrimSpace(req.Location)),
		"notes":     "Booked by player",
	}
	if threadID != "" {
		row["coach_student_id"] = threadID
		row["student_label"] = label
	}
	ins, err := s.sb.Insert("coaching_schedule", row)
	if err != nil || len(ins) == 0 {
		if err == nil {
			err = errors.New("could not book the session")
		}
		return model.CoachingScheduleItem{}, err
	}

	// Notify the coach.
	s.notifyUser(coachUserID, "coaching", playerID, label,
		label+" booked a 1:1 session", "coaching:"+threadID)

	return mapScheduleItem(ins[0]), nil
}

// ListMySessions returns a player's upcoming booked 1:1 sessions (with coach name).
func (s *Service) ListMySessions(playerID, email string) ([]model.CoachingScheduleItem, error) {
	if !s.scheduleReady() || !s.coachingReady() {
		return []model.CoachingScheduleItem{}, nil
	}
	email = strings.ToLower(strings.TrimSpace(email))
	// The player's thread ids (roster rows linked to them).
	filter := "select=id,coach_id&"
	if playerID != "" && email != "" {
		filter += "or=(student_id.eq." + store.Q(playerID) + ",student_email.eq." + store.Q(email) + ")"
	} else if playerID != "" {
		filter += "student_id=eq." + store.Q(playerID)
	} else if email != "" {
		filter += "student_email=eq." + store.Q(email)
	} else {
		return []model.CoachingScheduleItem{}, nil
	}
	links, err := s.sb.Select("coach_students", filter)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return []model.CoachingScheduleItem{}, nil
	}
	ids := make([]string, 0, len(links))
	coachOf := map[string]string{}
	for _, l := range links {
		id := asStr(l, "id")
		ids = append(ids, id)
		coachOf[id] = asStr(l, "coach_id")
	}
	rows, err := s.sb.Select("coaching_schedule",
		"coach_student_id="+store.In(ids)+"&kind=eq.session&order=starts_at.asc")
	if err != nil {
		return nil, err
	}
	nowT := time.Now().UTC()
	nameCache := map[string]string{}
	out := make([]model.CoachingScheduleItem, 0, len(rows))
	for _, r := range rows {
		it := mapScheduleItem(r)
		end, ok := parseSchedTime(it.EndsAt)
		if !ok {
			end, _ = parseSchedTime(it.StartsAt)
		}
		if !end.IsZero() && end.Before(nowT) {
			continue
		}
		coachID := coachOf[it.CoachStudentID]
		if coachID != "" {
			n, seen := nameCache[coachID]
			if !seen {
				n = s.coachingName(coachID)
				nameCache[coachID] = n
			}
			it.CoachName = n
		}
		out = append(out, it)
	}
	return out, nil
}

// CancelMySession lets a player cancel a session they booked (their thread only).
func (s *Service) CancelMySession(playerID, email, sessionID string) error {
	if !s.scheduleReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_schedule",
		"id=eq."+store.Q(sessionID)+"&select=coach_student_id,starts_at")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	threadID := asStr(row, "coach_student_id")
	if threadID == "" {
		return ErrForbidden
	}
	// Verify the thread belongs to this player.
	cs, role, err := s.threadMembership(threadID, playerID, email)
	if err != nil || role != "student" {
		return ErrForbidden
	}
	// Honor the coach's cancellation cutoff.
	if err := s.enforceCancelCutoff(cs.CoachID, asStr(row, "starts_at")); err != nil {
		return err
	}
	if err := s.sb.Delete("coaching_schedule", "id=eq."+store.Q(sessionID)); err != nil {
		return err
	}
	who := s.coachingName(playerID)
	if who == "" {
		who = "Your student"
	}
	s.notifyUser(cs.CoachID, "coaching", playerID, who,
		who+" cancelled their 1:1 session", "coaching:"+threadID)
	return nil
}

// --- Class packs (buy N credits, spend one per paid enrollment) ---

func (s *Service) packsReady() bool {
	return s.columnReady("coach_packs", "id")
}

func (s *Service) creditsReady() bool {
	return s.columnReady("coaching_credits", "id")
}

func mapPack(row map[string]any) model.CoachPack {
	return model.CoachPack{
		ID:         asStr(row, "id"),
		CoachID:    asStr(row, "coach_id"),
		Title:      asStr(row, "title"),
		Credits:    asInt(row, "credits"),
		PriceCents: asInt(row, "price_cents"),
		Active:     asBool(row, "active"),
		CreatedAt:  asStr(row, "created_at"),
	}
}

// ListCoachPacks returns a coach's active packs (public / player-facing + coach).
func (s *Service) ListCoachPacks(coachUserID string) ([]model.CoachPack, error) {
	if !s.packsReady() {
		return []model.CoachPack{}, nil
	}
	rows, err := s.sb.Select("coach_packs",
		"coach_id=eq."+store.Q(coachUserID)+"&active=is.true&order=price_cents.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachPack, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapPack(r))
	}
	return out, nil
}

// CreatePack adds a pack the coach sells.
func (s *Service) CreatePack(coachID string, req model.CoachPackRequest) (model.CoachPack, error) {
	if !s.packsReady() {
		return model.CoachPack{}, ErrCoachingUnavailable
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return model.CoachPack{}, errors.New("give the pack a title")
	}
	if req.Credits <= 0 {
		return model.CoachPack{}, errors.New("credits must be at least 1")
	}
	price := req.PriceCents
	if price < 0 {
		price = 0
	}
	ins, err := s.sb.Insert("coach_packs", map[string]any{
		"coach_id":    coachID,
		"title":       title,
		"credits":     req.Credits,
		"price_cents": price,
	})
	if err != nil || len(ins) == 0 {
		if err == nil {
			err = errors.New("could not save that pack")
		}
		return model.CoachPack{}, err
	}
	return mapPack(ins[0]), nil
}

// DeletePack soft-inactivates a pack the coach owns (keeps history).
func (s *Service) DeletePack(coachID, id string) error {
	if !s.packsReady() {
		return ErrCoachingUnavailable
	}
	_, err := s.sb.Update("coach_packs",
		"id=eq."+store.Q(id)+"&coach_id=eq."+store.Q(coachID),
		map[string]any{"active": false})
	return err
}

// MyCoachCredits returns the caller's remaining credit balance with a coach.
func (s *Service) MyCoachCredits(coachUserID, userID string) (int, error) {
	if !s.creditsReady() || userID == "" {
		return 0, nil
	}
	row, err := s.sb.SelectOne("coaching_credits",
		"coach_id=eq."+store.Q(coachUserID)+"&user_id=eq."+store.Q(userID)+
			"&select=credits_remaining")
	if err != nil || row == nil {
		return 0, err
	}
	return asInt(row, "credits_remaining"), nil
}

// BuyPack starts a hosted checkout for a class pack. On success the webhook
// grants the credits (metaKey pack_purchase = "coachId:userId:credits").
func (s *Service) BuyPack(packID, userID, email, successURL, cancelURL string) (string, error) {
	if !s.packsReady() {
		return "", ErrCoachingUnavailable
	}
	pack, err := s.sb.SelectOne("coach_packs", "id=eq."+store.Q(packID))
	if err != nil {
		return "", err
	}
	if pack == nil || !asBool(pack, "active") {
		return "", ErrNotFound
	}
	coachID := asStr(pack, "coach_id")
	credits := asInt(pack, "credits")
	price := asInt(pack, "price_cents")
	meta := coachID + ":" + userID + ":" + strconv.Itoa(credits)
	// Free pack → grant immediately, no checkout.
	if price <= 0 {
		return "", s.grantPackCredits(meta)
	}
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	return gw.CreatePlatformCheckout(meta, "pack_purchase", price, "usd",
		"PlanMyPickle — "+asStr(pack, "title"), email, successURL, cancelURL)
}

// grantPackCredits credits a player's balance from a "coachId:userId:credits"
// metadata string (idempotency is not enforced — Stripe delivers once normally).
func (s *Service) grantPackCredits(meta string) error {
	if !s.creditsReady() {
		return nil
	}
	parts := strings.Split(meta, ":")
	if len(parts) != 3 {
		return nil
	}
	coachID, userID := parts[0], parts[1]
	credits, _ := strconv.Atoi(parts[2])
	if coachID == "" || userID == "" || credits <= 0 {
		return nil
	}
	cur, _ := s.MyCoachCredits(coachID, userID)
	_, err := s.sb.Upsert("coaching_credits", "coach_id,user_id", map[string]any{
		"coach_id":          coachID,
		"user_id":           userID,
		"credits_remaining": cur + credits,
		"updated_at":        now(),
	})
	return err
}

// consumeCredit spends one credit if the player has any (read-then-write; fine at
// this scale). Returns true when a credit was spent.
func (s *Service) consumeCredit(coachID, userID string) bool {
	if !s.creditsReady() || coachID == "" || userID == "" {
		return false
	}
	cur, _ := s.MyCoachCredits(coachID, userID)
	if cur <= 0 {
		return false
	}
	_, err := s.sb.Update("coaching_credits",
		"coach_id=eq."+store.Q(coachID)+"&user_id=eq."+store.Q(userID),
		map[string]any{"credits_remaining": cur - 1, "updated_at": now()})
	return err == nil
}

// --- Coaching classes (marketplace Phase B) ---

func (s *Service) classesReady() bool {
	return s.columnReady("coaching_classes", "id")
}

func mapClass(row map[string]any) model.CoachingClass {
	return model.CoachingClass{
		ID:          asStr(row, "id"),
		CoachID:     asStr(row, "coach_id"),
		Title:       asStr(row, "title"),
		Description: asStr(row, "description"),
		StartsAt:    asStr(row, "starts_at"),
		EndsAt:      asStr(row, "ends_at"),
		Location:    asStr(row, "location"),
		Capacity:    asInt(row, "capacity"),
		PriceCents:  asInt(row, "price_cents"),
		IsIntro:     asBool(row, "is_intro"),
		CreatedAt:   asStr(row, "created_at"),
	}
}

// ListMyClasses returns the signed-in coach's own UPCOMING classes.
func (s *Service) ListMyClasses(coachID string) ([]model.CoachingClass, error) {
	if !s.classesReady() {
		return []model.CoachingClass{}, nil
	}
	rows, err := s.sb.Select("coaching_classes",
		"coach_id=eq."+store.Q(coachID)+"&starts_at=gte."+store.Q(now())+
			"&order=starts_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingClass, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapClass(r))
	}
	s.enrichClasses(out, "")
	return out, nil
}

// ListCoachClassesPublic returns a coach's UPCOMING classes (player view), with
// enrolled counts and whether the viewer is enrolled.
func (s *Service) ListCoachClassesPublic(coachUserID, viewerID string) ([]model.CoachingClass, error) {
	if !s.classesReady() {
		return []model.CoachingClass{}, nil
	}
	rows, err := s.sb.Select("coaching_classes",
		"coach_id=eq."+store.Q(coachUserID)+"&starts_at=gte."+store.Q(now())+
			"&order=starts_at.asc")
	if err != nil {
		return nil, err
	}
	name := s.coachingName(coachUserID)
	out := make([]model.CoachingClass, 0, len(rows))
	for _, r := range rows {
		c := mapClass(r)
		c.CoachName = name
		out = append(out, c)
	}
	s.enrichClasses(out, viewerID)
	return out, nil
}

// CreateClass adds a class for the signed-in coach.
func (s *Service) CreateClass(coachID string, req model.CoachingClassRequest) (model.CoachingClass, error) {
	if !s.classesReady() {
		return model.CoachingClass{}, ErrCoachingUnavailable
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return model.CoachingClass{}, errors.New("give the class a title")
	}
	if strings.TrimSpace(req.StartsAt) == "" {
		return model.CoachingClass{}, errors.New("pick a date and time")
	}
	capacity := req.Capacity
	if capacity < 0 {
		capacity = 0
	}
	price := req.PriceCents
	if price < 0 {
		price = 0
	}
	classRow := map[string]any{
		"coach_id":    coachID,
		"title":       title,
		"description": orNull(strings.TrimSpace(req.Description)),
		"starts_at":   req.StartsAt,
		"ends_at":     orNull(strings.TrimSpace(req.EndsAt)),
		"location":    orNull(strings.TrimSpace(req.Location)),
		"capacity":    capacity,
		"price_cents": price,
	}
	if s.columnReady("coaching_classes", "is_intro") {
		classRow["is_intro"] = req.IsIntro
	}
	ins, err := s.sb.Insert("coaching_classes", classRow)
	if err != nil {
		return model.CoachingClass{}, err
	}
	if len(ins) == 0 {
		return model.CoachingClass{}, errors.New("could not save that class")
	}
	return mapClass(ins[0]), nil
}

// UpdateClass edits a class the coach owns.
func (s *Service) UpdateClass(coachID, id string, req model.CoachingClassRequest) (model.CoachingClass, error) {
	if !s.classesReady() {
		return model.CoachingClass{}, ErrCoachingUnavailable
	}
	cur, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(id)+"&select=coach_id,starts_at,location")
	if cur == nil {
		return model.CoachingClass{}, ErrNotFound
	}
	if asStr(cur, "coach_id") != coachID {
		return model.CoachingClass{}, ErrForbidden
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return model.CoachingClass{}, errors.New("give the class a title")
	}
	if strings.TrimSpace(req.StartsAt) == "" {
		return model.CoachingClass{}, errors.New("pick a date and time")
	}
	capacity := req.Capacity
	if capacity < 0 {
		capacity = 0
	}
	price := req.PriceCents
	if price < 0 {
		price = 0
	}
	upd := map[string]any{
		"title":       title,
		"description": orNull(strings.TrimSpace(req.Description)),
		"starts_at":   req.StartsAt,
		"ends_at":     orNull(strings.TrimSpace(req.EndsAt)),
		"location":    orNull(strings.TrimSpace(req.Location)),
		"capacity":    capacity,
		"price_cents": price,
	}
	if s.columnReady("coaching_classes", "is_intro") {
		upd["is_intro"] = req.IsIntro
	}
	out, err := s.sb.Update("coaching_classes", "id=eq."+store.Q(id), upd)
	if err != nil {
		return model.CoachingClass{}, err
	}
	// Notify enrollees only when the time or location actually changed.
	if req.StartsAt != asStr(cur, "starts_at") ||
		strings.TrimSpace(req.Location) != asStr(cur, "location") {
		s.notifyClassEnrollees(id, coachID, s.coachingName(coachID),
			"Class updated: \""+title+"\" — check the new time/location")
	}
	if len(out) > 0 {
		return mapClass(out[0]), nil
	}
	return model.CoachingClass{}, errors.New("could not update that class")
}

// notifyClassEnrollees pushes a message to everyone enrolled/waitlisted in a
// class (used when the coach changes or cancels it). Best-effort; no deep link
// (classes live in "My classes", not a thread).
func (s *Service) notifyClassEnrollees(classID, actorID, actorName, body string) {
	if !s.enrollmentsReady() {
		return
	}
	rows, _ := s.sb.SelectAll("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&select=user_id")
	seen := map[string]bool{}
	for _, r := range rows {
		uid := asStr(r, "user_id")
		if uid == "" || uid == actorID || seen[uid] {
			continue
		}
		seen[uid] = true
		s.notifyUser(uid, "coaching", actorID, actorName, body, "")
	}
}

// DeleteClass removes a class the coach owns.
func (s *Service) DeleteClass(coachID, id string) error {
	if !s.classesReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(id)+"&select=coach_id,title")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	title := asStr(row, "title")
	if title == "" {
		title = "a class"
	}
	// Notify enrollees BEFORE the delete (its FK cascade wipes the enrollments).
	s.notifyClassEnrollees(id, coachID, s.coachingName(coachID),
		"Class cancelled: \""+title+"\"")
	return s.sb.Delete("coaching_classes", "id=eq."+store.Q(id))
}

// --- Class enrollments (marketplace Phase C/D) ---

func (s *Service) enrollmentsReady() bool {
	return s.columnReady("coaching_enrollments", "id")
}

func mapEnrollment(row map[string]any) model.CoachingEnrollment {
	return model.CoachingEnrollment{
		ID:          asStr(row, "id"),
		ClassID:     asStr(row, "class_id"),
		UserID:      asStr(row, "user_id"),
		Name:        asStr(row, "name"),
		Email:       asStr(row, "email"),
		Status:      asStr(row, "status"),
		AmountCents: asInt(row, "amount_cents"),
		Paid:        asBool(row, "paid"),
		ChargeAt:    asStr(row, "charge_at"),
		CreatedAt:   asStr(row, "created_at"),
	}
}

func (s *Service) classEnrolledCount(classID string) int {
	rows, err := s.sb.Select("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&status=eq.enrolled&select=id")
	if err != nil {
		return 0
	}
	return len(rows)
}

// enrichClasses fills EnrolledCount (and, when viewerID is set, Enrolled).
func (s *Service) enrichClasses(list []model.CoachingClass, viewerID string) {
	if !s.enrollmentsReady() {
		return
	}
	for i := range list {
		list[i].EnrolledCount = s.classEnrolledCount(list[i].ID)
		list[i].WaitlistCount = s.classStatusCount(list[i].ID, "waitlisted")
		if viewerID != "" {
			r, _ := s.sb.SelectOne("coaching_enrollments",
				"class_id=eq."+store.Q(list[i].ID)+"&user_id=eq."+store.Q(viewerID)+
					"&status=in.(enrolled,waitlisted)&select=status")
			list[i].Enrolled = asStr(r, "status") == "enrolled"
			list[i].Waitlisted = asStr(r, "status") == "waitlisted"
		}
	}
}

func (s *Service) classStatusCount(classID, status string) int {
	rows, err := s.sb.Select("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&status=eq."+status+"&select=id")
	if err != nil {
		return 0
	}
	return len(rows)
}

// CoachPromoteEnrollment moves a waitlisted player into an open seat.
func (s *Service) CoachPromoteEnrollment(coachID, enrollmentID string) error {
	if !s.enrollmentsReady() {
		return ErrCoachingUnavailable
	}
	if !s.coachOwnsEnrollment(coachID, enrollmentID) {
		return ErrForbidden
	}
	row, _ := s.sb.SelectOne("coaching_enrollments",
		"id=eq."+store.Q(enrollmentID)+"&select=class_id,user_id")
	if row == nil {
		return ErrNotFound
	}
	classID := asStr(row, "class_id")
	cls, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(classID)+"&select=capacity,starts_at,price_cents,title")
	capacity := asInt(cls, "capacity")
	if capacity > 0 && s.classEnrolledCount(classID) >= capacity {
		return errors.New("no open seats — remove someone first")
	}
	upd := map[string]any{"status": "enrolled"}
	if asInt(cls, "price_cents") > 0 {
		if t, ok := parseTime(asStr(cls, "starts_at")); ok {
			upd["charge_at"] = t.Add(-time.Hour).UTC().Format(time.RFC3339)
		}
	}
	if _, err := s.sb.Update("coaching_enrollments",
		"id=eq."+store.Q(enrollmentID), upd); err != nil {
		return err
	}
	s.notifyUser(asStr(row, "user_id"), "coaching", coachID, s.coachingName(coachID),
		"A seat opened up — you're in "+asStr(cls, "title"), "")
	return nil
}

// promoteNextWaitlisted auto-fills a freed seat with the oldest waitlisted player.
func (s *Service) promoteNextWaitlisted(classID string) {
	if !s.enrollmentsReady() || classID == "" {
		return
	}
	cls, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(classID)+"&select=capacity,starts_at,price_cents,title,coach_id")
	if cls == nil {
		return
	}
	capacity := asInt(cls, "capacity")
	if capacity > 0 && s.classEnrolledCount(classID) >= capacity {
		return // no free seat
	}
	next, _ := s.sb.SelectOne("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&status=eq.waitlisted&order=created_at.asc&limit=1&select=id,user_id")
	if next == nil {
		return
	}
	upd := map[string]any{"status": "enrolled"}
	if asInt(cls, "price_cents") > 0 {
		if t, ok := parseTime(asStr(cls, "starts_at")); ok {
			upd["charge_at"] = t.Add(-time.Hour).UTC().Format(time.RFC3339)
		}
	}
	if _, err := s.sb.Update("coaching_enrollments",
		"id=eq."+store.Q(asStr(next, "id")), upd); err != nil {
		return
	}
	s.notifyUser(asStr(next, "user_id"), "coaching", asStr(cls, "coach_id"),
		s.coachingName(asStr(cls, "coach_id")),
		"A seat opened up — you're now enrolled in "+asStr(cls, "title"), "")
}

// ensureCoachStudentLink adds a roster link (coach↔player) if one doesn't exist,
// so an enrolled player shows up in the coach's Students list.
func (s *Service) ensureCoachStudentLink(coachID, studentID, name, email string) {
	if coachID == "" || !s.coachingReady() {
		return
	}
	email = strings.ToLower(strings.TrimSpace(email))
	var found map[string]any
	if studentID != "" {
		found, _ = s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_id=eq."+store.Q(studentID)+"&select=id")
	}
	if found == nil && email != "" {
		found, _ = s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_email=eq."+store.Q(email)+"&select=id")
	}
	if found != nil {
		return
	}
	_, _ = s.sb.Insert("coach_students", map[string]any{
		"coach_id":      coachID,
		"student_id":    orNull(studentID),
		"student_name":  orNull(strings.TrimSpace(name)),
		"student_email": orNull(email),
	})
}

// Enroll books a player's seat in a class. Free classes are paid immediately;
// paid classes record charge_at = starts_at − 1h for a later off-session charge.
func (s *Service) Enroll(classID, userID, name, email string) (model.CoachingEnrollment, error) {
	if !s.classesReady() || !s.enrollmentsReady() {
		return model.CoachingEnrollment{}, ErrCoachingUnavailable
	}
	cls, err := s.sb.SelectOne("coaching_classes", "id=eq."+store.Q(classID))
	if err != nil {
		return model.CoachingEnrollment{}, err
	}
	if cls == nil {
		return model.CoachingEnrollment{}, ErrNotFound
	}
	if strings.TrimSpace(name) == "" {
		name = s.coachingName(userID)
	}
	existing, _ := s.sb.SelectOne("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&user_id=eq."+store.Q(userID))
	if existing != nil {
		st := asStr(existing, "status")
		if st == "enrolled" || st == "waitlisted" {
			return mapEnrollment(existing), nil // idempotent
		}
	}
	capacity := asInt(cls, "capacity")
	full := capacity > 0 && s.classEnrolledCount(classID) >= capacity
	price := asInt(cls, "price_cents")

	// Full class → join the waitlist (no charge, no credit spent until promoted).
	if full {
		row := map[string]any{
			"class_id":     classID,
			"user_id":      userID,
			"name":         orNull(strings.TrimSpace(name)),
			"email":        orNull(strings.ToLower(strings.TrimSpace(email))),
			"status":       "waitlisted",
			"amount_cents": price,
			"paid":         false,
		}
		var out []map[string]any
		if existing != nil {
			out, err = s.sb.Update("coaching_enrollments",
				"id=eq."+store.Q(asStr(existing, "id")), row)
		} else {
			out, err = s.sb.Insert("coaching_enrollments", row)
		}
		if err != nil {
			return model.CoachingEnrollment{}, err
		}
		if len(out) > 0 {
			return mapEnrollment(out[0]), nil
		}
		return mapEnrollment(row), nil
	}

	// A paid class: if the player holds a class-credit with this coach, spend one
	// and the seat is paid instantly — no per-class charge.
	usedCredit := false
	if price > 0 && s.consumeCredit(asStr(cls, "coach_id"), userID) {
		usedCredit = true
	}
	row := map[string]any{
		"class_id":     classID,
		"user_id":      userID,
		"name":         orNull(strings.TrimSpace(name)),
		"email":        orNull(strings.ToLower(strings.TrimSpace(email))),
		"status":       "enrolled",
		"amount_cents": price,
		"paid":         price == 0 || usedCredit,
	}
	if usedCredit {
		row["payment_ref"] = "credit"
	} else if price > 0 {
		if t, ok := parseTime(asStr(cls, "starts_at")); ok {
			row["charge_at"] = t.Add(-time.Hour).UTC().Format(time.RFC3339)
		}
	}
	var out []map[string]any
	if existing != nil {
		out, err = s.sb.Update("coaching_enrollments",
			"id=eq."+store.Q(asStr(existing, "id")), row)
	} else {
		out, err = s.sb.Insert("coaching_enrollments", row)
	}
	if err != nil {
		return model.CoachingEnrollment{}, err
	}
	s.ensureCoachStudentLink(asStr(cls, "coach_id"), userID, name, email)
	if len(out) > 0 {
		return mapEnrollment(out[0]), nil
	}
	return mapEnrollment(row), nil
}

// CancelClassEnrollment drops the player's seat (kept as a canceled row).
func (s *Service) CancelClassEnrollment(classID, userID string) error {
	if !s.enrollmentsReady() {
		return ErrCoachingUnavailable
	}
	// Honor the coach's cancellation cutoff.
	if cls, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(classID)+"&select=coach_id,starts_at"); cls != nil {
		if err := s.enforceCancelCutoff(asStr(cls, "coach_id"),
			asStr(cls, "starts_at")); err != nil {
			return err
		}
	}
	_, err := s.sb.Update("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&user_id=eq."+store.Q(userID),
		map[string]any{"status": "canceled"})
	if err == nil {
		// If that freed a seat, pull in the next waitlisted player (no-op when
		// the canceler was themselves waitlisted — the class is still full).
		s.promoteNextWaitlisted(classID)
	}
	return err
}

// coachOwnsEnrollment verifies an enrollment belongs to a class the coach owns.
func (s *Service) coachOwnsEnrollment(coachID, enrollmentID string) bool {
	row, _ := s.sb.SelectOne("coaching_enrollments",
		"id=eq."+store.Q(enrollmentID)+"&select=class_id")
	if row == nil {
		return false
	}
	cls, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(asStr(row, "class_id"))+"&select=coach_id")
	return cls != nil && asStr(cls, "coach_id") == coachID
}

// CoachMarkEnrollmentPaid lets the coach mark a seat paid (e.g. paid in cash).
func (s *Service) CoachMarkEnrollmentPaid(coachID, enrollmentID string) error {
	if !s.enrollmentsReady() {
		return ErrCoachingUnavailable
	}
	if !s.coachOwnsEnrollment(coachID, enrollmentID) {
		return ErrForbidden
	}
	_, err := s.sb.Update("coaching_enrollments", "id=eq."+store.Q(enrollmentID),
		map[string]any{"paid": true, "payment_ref": "manual"})
	return err
}

// CoachRemoveEnrollment lets the coach drop a player from a class.
func (s *Service) CoachRemoveEnrollment(coachID, enrollmentID string) error {
	if !s.enrollmentsReady() {
		return ErrCoachingUnavailable
	}
	if !s.coachOwnsEnrollment(coachID, enrollmentID) {
		return ErrForbidden
	}
	row, _ := s.sb.SelectOne("coaching_enrollments",
		"id=eq."+store.Q(enrollmentID)+"&select=class_id,status")
	wasEnrolled := asStr(row, "status") == "enrolled"
	_, err := s.sb.Update("coaching_enrollments", "id=eq."+store.Q(enrollmentID),
		map[string]any{"status": "canceled"})
	if err == nil && wasEnrolled {
		s.promoteNextWaitlisted(asStr(row, "class_id"))
	}
	return err
}

// ListClassEnrollments returns a class's enrolled players (coach-only).
func (s *Service) ListClassEnrollments(classID, coachID string) ([]model.CoachingEnrollment, error) {
	if !s.enrollmentsReady() {
		return []model.CoachingEnrollment{}, nil
	}
	cls, err := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(classID)+"&select=coach_id")
	if err != nil {
		return nil, err
	}
	if cls == nil {
		return nil, ErrNotFound
	}
	if asStr(cls, "coach_id") != coachID {
		return nil, ErrForbidden
	}
	rows, err := s.sb.Select("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&status=in.(enrolled,waitlisted)&order=status.asc,created_at.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingEnrollment, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapEnrollment(r))
	}
	return out, nil
}

// MyEnrolledClasses returns the player's upcoming enrolled classes.
func (s *Service) MyEnrolledClasses(userID string) ([]model.CoachingEnrollment, error) {
	if !s.enrollmentsReady() {
		return []model.CoachingEnrollment{}, nil
	}
	rows, err := s.sb.Select("coaching_enrollments",
		"user_id=eq."+store.Q(userID)+"&status=in.(enrolled,waitlisted)"+
			"&select=*,class:coaching_classes(title,starts_at,location,coach_id,price_cents)")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingEnrollment, 0, len(rows))
	for _, r := range rows {
		e := mapEnrollment(r)
		if c := asMap(r, "class"); c != nil {
			starts := asStr(c, "starts_at")
			if t, ok := parseTime(starts); ok && t.Before(time.Now()) {
				continue // past class
			}
			e.ClassTitle = asStr(c, "title")
			e.StartsAt = starts
			e.Location = asStr(c, "location")
			e.CoachName = s.coachingName(asStr(c, "coach_id"))
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt < out[j].StartsAt })
	return out, nil
}

// markEnrollmentPaid flips a paid class enrollment to paid (webhook path).
func (s *Service) markEnrollmentPaid(enrollmentID string) error {
	if !s.enrollmentsReady() {
		return nil
	}
	_, err := s.sb.Update("coaching_enrollments", "id=eq."+store.Q(enrollmentID),
		map[string]any{"paid": true})
	return err
}

// PayForEnrollment returns a hosted-checkout URL for an unpaid paid enrollment.
// Only the enrollee may pay; free/already-paid returns "" (nothing to do).
func (s *Service) PayForEnrollment(enrollmentID, userID, email, successURL, cancelURL string) (string, error) {
	if !s.enrollmentsReady() {
		return "", ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_enrollments", "id=eq."+store.Q(enrollmentID))
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	if asStr(row, "user_id") != userID {
		return "", ErrForbidden
	}
	if asBool(row, "paid") || asInt(row, "amount_cents") <= 0 {
		return "", nil // nothing to charge
	}
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	title := "Class"
	if c, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(asStr(row, "class_id"))+"&select=title"); c != nil {
		if t := asStr(c, "title"); t != "" {
			title = t
		}
	}
	return gw.CreatePlatformCheckout(enrollmentID, "enrollment_id",
		asInt(row, "amount_cents"), "usd", "PlanMyPickle — "+title, email,
		successURL, cancelURL)
}

// RemindDueClassPayments notifies players ~1h before a paid class to pay & confirm
// their seat (reconciler tick). Reminds once per enrollment (payment_ref sentinel).
// RemindUpcomingSessions notifies students of a booked 1:1 session starting
// within the next 24h (once — reminded_at guards re-sends). Cron sweep.
func (s *Service) RemindUpcomingSessions() error {
	if !s.scheduleReady() || !s.columnReady("coaching_schedule", "reminded_at") {
		return nil
	}
	nowT := time.Now().UTC()
	horizon := nowT.Add(24 * time.Hour)
	rows, err := s.sb.Select("coaching_schedule",
		"kind=eq.session&reminded_at=is.null"+
			"&starts_at=gte."+store.Q(nowT.Format(time.RFC3339))+
			"&starts_at=lte."+store.Q(horizon.Format(time.RFC3339))+
			"&select=id,coach_id,coach_student_id&limit=300")
	if err != nil {
		return err
	}
	for _, r := range rows {
		threadID := asStr(r, "coach_student_id")
		coachID := asStr(r, "coach_id")
		if threadID != "" {
			s.notifyStudentOfThread(threadID, coachID, s.coachingName(coachID),
				"Reminder: your 1:1 coaching session is coming up soon",
				"coaching:"+threadID)
		}
		_, _ = s.sb.Update("coaching_schedule", "id=eq."+store.Q(asStr(r, "id")),
			map[string]any{"reminded_at": nowT.Format(time.RFC3339)})
	}
	return nil
}

// SweepInactiveStudents nudges a linked student (and their coach) when a thread
// has had no activity in 14 days, at most once per 14 days (nudged_at guard).
func (s *Service) SweepInactiveStudents() error {
	if !s.coachingReady() ||
		!s.columnReady("coach_students", "nudged_at") ||
		!s.columnReady("coach_students", "last_activity_at") {
		return nil
	}
	nowT := time.Now().UTC()
	cutoff := nowT.Add(-14 * 24 * time.Hour).Format(time.RFC3339)
	rows, err := s.sb.Select("coach_students",
		"last_activity_at=lt."+store.Q(cutoff)+"&student_id=not.is.null"+
			"&or=(nudged_at.is.null,nudged_at.lt."+store.Q(cutoff)+")"+
			"&select=id,coach_id,student_id&limit=200")
	if err != nil {
		return err
	}
	for _, r := range rows {
		threadID := asStr(r, "id")
		coachID := asStr(r, "coach_id")
		sid := asStr(r, "student_id")
		if sid != "" {
			s.notifyUser(sid, "coaching", coachID, s.coachingName(coachID),
				"Your coach is ready when you are — upload a clip for feedback",
				"coaching:"+threadID)
		}
		who := s.coachingName(sid)
		if who == "" {
			who = "A student"
		}
		s.notifyUser(coachID, "coaching", sid, who,
			who+" has been quiet for 2 weeks — check in?", "coaching:"+threadID)
		_, _ = s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
			map[string]any{"nudged_at": nowT.Format(time.RFC3339)})
	}
	return nil
}

func (s *Service) RemindDueClassPayments() error {
	if !s.enrollmentsReady() {
		return nil
	}
	rows, err := s.sb.Select("coaching_enrollments",
		"paid=is.false&status=eq.enrolled&charge_at=lte."+store.Q(now())+
			"&payment_ref=is.null&select=id,user_id,class_id")
	if err != nil {
		return err
	}
	for _, r := range rows {
		uid := asStr(r, "user_id")
		title := "your class"
		if c, _ := s.sb.SelectOne("coaching_classes",
			"id=eq."+store.Q(asStr(r, "class_id"))+"&select=title"); c != nil {
			if t := asStr(c, "title"); t != "" {
				title = t
			}
		}
		if uid != "" {
			s.notifyUser(uid, "coaching", "", "PlanMyPickle",
				"“"+title+"” is coming up — open My classes to pay and confirm your seat.",
				"myclasses")
		}
		_, _ = s.sb.Update("coaching_enrollments", "id=eq."+store.Q(asStr(r, "id")),
			map[string]any{"payment_ref": "reminded"})
	}
	return nil
}

// parseTime parses a PostgREST timestamptz (RFC3339 / with nanos).
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// --- Demo helpers (owner-only): seed/remove dummy coaches + clear dummy students ---

const demoCoachBaseLat = 32.6401
const demoCoachBaseLng = -117.0842

var demoCoaches = []struct {
	Name, City, Bio, Skills string
	DLat, DLng              float64
	Years, RateCents        int
}{
	{"Ana Reyes", "Chula Vista, CA", "Former college player; I love getting rec players comfortable at the kitchen line.", "Dinks, resets, shot selection", 0.012, 0.010, 8, 5000},
	{"Marcus Lee", "Bonita, CA", "Patient, drill-focused coaching. We'll rebuild your third-shot drop from the ground up.", "Third-shot drops, transition game", 0.030, -0.022, 12, 6500},
	{"Priya Nair", "Eastlake, CA", "New to pickleball? I specialize in fundamentals and building confidence.", "Fundamentals, serve & return", -0.020, 0.028, 5, 4000},
	{"Diego Santos", "National City, CA", "Tournament-tested. I coach strategy, stacking, and high-level shot patterns.", "Advanced strategy, tournament prep", 0.050, 0.018, 15, 8000},
	{"Kayla Brooks", "Otay Ranch, CA", "Footwork and consistency are everything — let's make your game repeatable.", "Footwork, consistency, dinking", -0.038, -0.030, 6, 5500},
}

// SeedDemoCoaches inserts a handful of listed dummy coaches near San Diego so the
// "Find a coach" search has data to show. Idempotent (clears prior demo first).
func (s *Service) SeedDemoCoaches() (int, error) {
	if !s.coachProfilesReady() {
		return 0, ErrCoachingUnavailable
	}
	_ = s.RemoveDemoCoaches()
	if s.coachReviewsReady() {
		_ = s.sb.Delete("coach_reviews", "author_name="+store.In(demoReviewAuthors))
	}
	yearsReady := s.columnReady("coach_profiles", "years_experience")
	n := 0
	for _, d := range demoCoaches {
		uid := newID()
		row := map[string]any{
			"user_id":           uid,
			"name":              d.Name,
			"listed":            true,
			"bio":               d.Bio,
			"city":              d.City,
			"lat":               demoCoachBaseLat + d.DLat,
			"lng":               demoCoachBaseLng + d.DLng,
			"hourly_rate_cents": d.RateCents,
			"skills":            d.Skills,
			"updated_at":        now(),
		}
		if yearsReady {
			row["years_experience"] = d.Years
		}
		if s.columnReady("coach_profiles", "verified") {
			row["verified"] = true
			row["certifications"] = "PPR Certified · IPTPA Level 2"
		}
		if _, err := s.sb.Insert("coach_profiles", row); err == nil {
			n++
			s.seedDemoReviews(uid)
		}
	}
	return n, nil
}

var demoReviewAuthors = []string{"Jamie P.", "Chris M.", "Dana R."}

// seedDemoReviews attaches a few sample reviews to a demo coach so the
// marketplace shows star ratings + counts. No-op until reviews ship.
func (s *Service) seedDemoReviews(coachUID string) {
	if !s.coachReviewsReady() {
		return
	}
	revs := []struct {
		name   string
		rating int
		body   string
	}{
		{"Jamie P.", 5, "Huge help with my third-shot drop — saw results in two sessions."},
		{"Chris M.", 5, "Patient, clear, and genuinely fun. My dinking is way more consistent now."},
		{"Dana R.", 4, "Great tactical eye and easy to work with. Wish we'd had more court time."},
	}
	for _, r := range revs {
		_, _ = s.sb.Insert("coach_reviews", map[string]any{
			"coach_user_id": coachUID,
			"author_id":     newID(),
			"author_name":   r.name,
			"rating":        r.rating,
			"body":          r.body,
		})
	}
}

// RemoveDemoCoaches deletes the seeded dummy coaches (matched by name).
func (s *Service) RemoveDemoCoaches() error {
	if !s.coachProfilesReady() {
		return nil
	}
	names := make([]string, len(demoCoaches))
	for i, d := range demoCoaches {
		names[i] = d.Name
	}
	return s.sb.Delete("coach_profiles", "name="+store.In(names))
}

// RemoveDemoStudents clears the calling coach's seeded dummy students (the
// @coachdemo.test roster) — the counterpart to SeedCoachingTestData.
func (s *Service) RemoveDemoStudents(coachID string) (int, error) {
	if !s.coachingReady() {
		return 0, nil
	}
	filter := "coach_id=eq." + store.Q(coachID) +
		"&student_email=like.*" + store.Q(seedEmailDomain)
	rows, _ := s.sb.Select("coach_students", filter+"&select=id")
	if err := s.sb.Delete("coach_students", filter); err != nil {
		return 0, err
	}
	// Also clear the seeded real krizhia↔rolando link.
	_ = s.sb.Delete("coach_students",
		"coach_id=eq."+store.Q(coachID)+
			"&student_email=eq."+store.Q("rolando.naranjo0420@gmail.com"))
	return len(rows), nil
}

// notifyCoachingCounterpart sends a bell + push to whichever party did NOT act.
// If the actor is the coach, the student is notified (resolving their id live if
// the roster row isn't linked yet); if the actor is the student, the coach is.
func (s *Service) notifyCoachingCounterpart(cs model.CoachStudent, actorRole, actorID, actorName, body string) {
	s.notifyCoachingCounterpartLink(cs, actorRole, actorID, actorName, body, "coaching:"+cs.ID)
}

// notifyCoachingCounterpartLink is like notifyCoachingCounterpart but with a
// custom deep link (e.g. open the Chat tab for a message).
func (s *Service) notifyCoachingCounterpartLink(cs model.CoachStudent, actorRole, actorID, actorName, body, link string) {
	var recipient string
	if actorRole == "coach" {
		recipient = cs.StudentID
		if recipient == "" && cs.StudentEmail != "" {
			recipient = s.userIDByEmail(cs.StudentEmail)
		}
		if recipient == "" && cs.StudentPhone != "" {
			recipient = s.userIDByPhone(cs.StudentPhone)
		}
	} else {
		recipient = cs.CoachID
	}
	if recipient == "" || recipient == actorID {
		return
	}
	s.notifyUser(recipient, "coaching", actorID, actorName, body, link)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
