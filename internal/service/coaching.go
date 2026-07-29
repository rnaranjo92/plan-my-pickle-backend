package service

import (
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// phoneOf returns the account's stored phone (raw), or "".
func (s *Service) phoneOf(userID string) string {
	if userID == "" {
		return ""
	}
	row, _ := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=phone")
	if row == nil {
		return ""
	}
	return asStr(row, "phone")
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
		CreatedAt:      asStr(row, "created_at"),
		LastActivityAt: asStr(row, "last_activity_at"),
		CoachNote:      asStr(row, "coach_note"),
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
func (s *Service) AddCoachStudent(coachID, email, phone, name string) (model.CoachStudent, error) {
	if !s.coachingReady() {
		return model.CoachStudent{}, ErrCoachingUnavailable
	}
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
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
	coach := s.resolveDisplayName(coachID, "")
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
	coach := s.resolveDisplayName(coachID, "")
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
	rows, err := s.sb.Select("coach_students",
		"coach_id=eq."+store.Q(coachID)+"&order=created_at.desc")
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
		cs.CoachName = s.resolveDisplayName(cs.CoachID, "")
		cs.VideoCount = s.threadVideoCount(cs.ID)
		cs.CoachNote = "" // the coach's private note about the student is never sent to them
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
	cs.CoachName = s.resolveDisplayName(cs.CoachID, "")
	if role != "coach" {
		cs.CoachNote = "" // students never see the coach's private note about them
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
		n := s.resolveDisplayName(uid, "")
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
		UploaderName:   s.resolveDisplayName(userID, email),
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
	name := s.resolveDisplayName(userID, email)
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
