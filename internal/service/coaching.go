package service

import (
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"sort"
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
			go s.sendCoachInvite(coachID, email, name)
		}
		if rawPhone != "" {
			go s.sendCoachInviteSMS(coachID, rawPhone)
		}
	}
	return mapCoachStudent(ins[0]), nil
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
func (s *Service) sendCoachInviteSMS(coachID, phone string) {
	if s.Sms == nil || !gateway.SmsReachable(phone) {
		return
	}
	coach := s.coachingName(coachID)
	if strings.TrimSpace(coach) == "" {
		coach = "Your coach"
	}
	body := fmt.Sprintf(
		"%s invited you to PlanMyPickle to share pickleball clips & feedback. Sign up with this number to see them: https://app.planmypickle.com",
		coach)
	if r, err := s.Sms.Send(phone, body); err != nil || !r.OK {
		log.Printf("coaching: invite SMS to %s failed: %v", phone, err)
	}
}

// sendCoachInvite emails a not-yet-registered student a link to join
// PlanMyPickle. They must sign up with the SAME email the coach used so the
// roster row auto-links (backfilled on their first coaching view). No-op if the
// email gateway isn't configured.
func (s *Service) sendCoachInvite(coachID, studentEmail, studentName string) {
	if s.Email == nil || !s.Email.Live() {
		return
	}
	coach := s.coachingName(coachID)
	if strings.TrimSpace(coach) == "" {
		coach = "Your coach"
	}
	joinURL := "https://app.planmypickle.com/?invite=coaching&email=" +
		url.QueryEscape(studentEmail)
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
	// Student match: by email, or by phone (the account's phone == the invited one).
	studentMatch := email != "" && strings.EqualFold(cs.StudentEmail, email)
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
	// Opening a thread marks it read for the viewer.
	s.markThreadRead(userID, threadID)

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
			AuthorName: nameOf(asStr(f, "author_id")),
			Body:       asStr(f, "body"),
			CreatedAt:  asStr(f, "created_at"),
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
			Feedback:       byVideo[asStr(v, "id")],
		}
		out.Videos = append(out.Videos, vid)
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
	s.notifyCoachingCounterpart(cs, role, userID, vid.UploaderName,
		vid.UploaderName+" shared a new coaching clip")
	return vid, nil
}

// AddVideoFeedback adds a comment to a clip, then notifies the counterpart.
func (s *Service) AddVideoFeedback(videoID, userID, email string, req model.CoachingFeedbackRequest) (model.CoachingFeedback, error) {
	if !s.coachingReady() {
		return model.CoachingFeedback{}, ErrCoachingUnavailable
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
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
	ins, err := s.sb.Insert("coaching_feedback", map[string]any{
		"coach_student_id": threadID,
		"video_id":         videoID,
		"author_id":        userID,
		"author_role":      role,
		"body":             body,
	})
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
	s.bumpThreadActivity(threadID)
	s.markThreadRead(userID, threadID)
	s.notifyCoachingCounterpart(cs, role, userID, name, name+": "+truncate(body, 120))
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

// DeleteCoachScheduleItem removes a schedule entry the coach owns.
func (s *Service) DeleteCoachScheduleItem(coachID, id string) error {
	if !s.scheduleReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_schedule",
		"id=eq."+store.Q(id)+"&select=coach_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	return s.sb.Delete("coaching_schedule", "id=eq."+store.Q(id))
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
	if _, role, err := s.threadMembership(threadID, coachID, email); err != nil {
		return model.CoachingSkillRating{}, err
	} else if role != "coach" {
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
	}
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

// ListCoachesNearby returns listed coaches ranked by distance from (lat,lng).
// Coaches without coordinates sort last; radiusKm<=0 means no radius cap.
func (s *Service) ListCoachesNearby(lat, lng, radiusKm float64) ([]model.CoachProfile, error) {
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
	photos := s.photosByUser(uids)
	for i := range out {
		out[i].PhotoURL = photos[out[i].UserID]
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
	ins, err := s.sb.Insert("coaching_classes", map[string]any{
		"coach_id":    coachID,
		"title":       title,
		"description": orNull(strings.TrimSpace(req.Description)),
		"starts_at":   req.StartsAt,
		"ends_at":     orNull(strings.TrimSpace(req.EndsAt)),
		"location":    orNull(strings.TrimSpace(req.Location)),
		"capacity":    capacity,
		"price_cents": price,
	})
	if err != nil {
		return model.CoachingClass{}, err
	}
	if len(ins) == 0 {
		return model.CoachingClass{}, errors.New("could not save that class")
	}
	return mapClass(ins[0]), nil
}

// DeleteClass removes a class the coach owns.
func (s *Service) DeleteClass(coachID, id string) error {
	if !s.classesReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(id)+"&select=coach_id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
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
		if viewerID != "" {
			r, _ := s.sb.SelectOne("coaching_enrollments",
				"class_id=eq."+store.Q(list[i].ID)+"&user_id=eq."+store.Q(viewerID)+
					"&status=eq.enrolled&select=id")
			list[i].Enrolled = r != nil
		}
	}
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
	if existing != nil && asStr(existing, "status") == "enrolled" {
		return mapEnrollment(existing), nil // idempotent
	}
	capacity := asInt(cls, "capacity")
	if capacity > 0 && s.classEnrolledCount(classID) >= capacity {
		return model.CoachingEnrollment{}, errors.New("this class is full")
	}
	price := asInt(cls, "price_cents")
	row := map[string]any{
		"class_id":     classID,
		"user_id":      userID,
		"name":         orNull(strings.TrimSpace(name)),
		"email":        orNull(strings.ToLower(strings.TrimSpace(email))),
		"status":       "enrolled",
		"amount_cents": price,
		"paid":         price == 0,
	}
	if price > 0 {
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
	_, err := s.sb.Update("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&user_id=eq."+store.Q(userID),
		map[string]any{"status": "canceled"})
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
		"class_id=eq."+store.Q(classID)+"&status=eq.enrolled&order=created_at.asc")
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
		"user_id=eq."+store.Q(userID)+"&status=eq.enrolled"+
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
	yearsReady := s.columnReady("coach_profiles", "years_experience")
	n := 0
	for _, d := range demoCoaches {
		row := map[string]any{
			"user_id":           newID(),
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
		if _, err := s.sb.Insert("coach_profiles", row); err == nil {
			n++
		}
	}
	return n, nil
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
	return len(rows), nil
}

// notifyCoachingCounterpart sends a bell + push to whichever party did NOT act.
// If the actor is the coach, the student is notified (resolving their id live if
// the roster row isn't linked yet); if the actor is the student, the coach is.
func (s *Service) notifyCoachingCounterpart(cs model.CoachStudent, actorRole, actorID, actorName, body string) {
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
	s.notifyUser(recipient, "coaching", actorID, actorName, body, "coaching:"+cs.ID)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
