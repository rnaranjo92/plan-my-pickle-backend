package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// emailOf returns a user's account email (lowercased), or "" if unknown.
func (s *Service) emailOf(userID string) string {
	if userID == "" {
		return ""
	}
	if u, err := s.sb.GetAuthUser(userID); err == nil && u != nil {
		return strings.ToLower(strings.TrimSpace(asStr(u, "email")))
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

// coachLabel is the student-facing name for a coach in notifications, e.g.
// "Coach Kay". Falls back to "Your coach" when no name is on file.
func (s *Service) coachLabel(coachID string) string {
	return coachLabelFrom(s.coachingName(coachID))
}

// coachLabelFrom derives the "Coach <FirstName>" label from an already-resolved
// display name, so callers holding the name (or in a loop) don't re-query the
// profile. Uses the first name token that STARTS with a letter, so a name led by
// an emoji/punctuation ("🎾 Kay") still yields "Coach Kay" and never injects a
// native glyph into student copy. Falls back to "Your coach" when empty.
func coachLabelFrom(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Your coach"
	}
	for _, f := range strings.Fields(name) {
		r := []rune(f)
		if len(r) > 0 && unicode.IsLetter(r[0]) {
			return "Coach " + f
		}
	}
	return name // no alphabetic token — better than "Coach 🎾"
}

// coachPhotosByID returns coachID -> avatar URL for a batch of coaches, used to
// give the student's coaching cards a real face. A coach's dedicated profile
// photo (coach_profiles.photo_url) wins; otherwise it falls back to their
// account avatar (pmp_profiles). Missing/blank entries are simply absent.
func (s *Service) coachPhotosByID(coachIDs []string) map[string]string {
	out := s.photosByUser(coachIDs) // account avatars first
	if !s.coachProfilesReady() {
		return out
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(coachIDs))
	for _, c := range coachIDs {
		if c != "" && !seen[c] {
			seen[c] = true
			uniq = append(uniq, c)
		}
	}
	if len(uniq) == 0 {
		return out
	}
	if rows, err := s.sb.Select("coach_profiles",
		"user_id="+store.In(uniq)+"&select=user_id,photo_url"); err == nil {
		for _, r := range rows {
			if u := strings.TrimSpace(asStr(r, "photo_url")); u != "" {
				out[asStr(r, "user_id")] = u // dedicated coach photo overrides
			}
		}
	}
	return out
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
// fromLeague marks the row as league-created (auto-enroll) vs a manual add, so a
// later league-leave only ever removes rows it created — never a coach's manual
// (or manually-claimed) student, whose clip history would cascade away with it.
func (s *Service) AddCoachStudent(coachID, email, phone, name, level string, fromLeague bool) (model.CoachStudent, error) {
	if !s.coachingReady() {
		return model.CoachStudent{}, ErrCoachingUnavailable
	}
	fromLeagueReady := s.columnReady("coach_students", "from_league")
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
	// Resolve to an existing account (by email, else phone) up front so we can
	// also de-dupe by the ACCOUNT, not just the exact contact string.
	resolved := s.userIDByEmail(email)
	if resolved == "" && np != "" {
		resolved = s.userIDByPhone(np)
	}
	// Already on this coach's roster? Match by email or phone — AND by resolved
	// account id, so a student who's already this coach's student under one
	// contact (e.g. the original coach↔player thread) doesn't get a SECOND row
	// when they later enroll via a coach-led league using a different
	// email/phone. Same account → one row. A row the student LEFT (left_at set)
	// is HIDDEN from the roster (see activeStudentFilter), so a plain "already
	// added" error would strand the coach with an invisible duplicate they can
	// never re-add. Reactivate that left row instead; only an ACTIVE match is a
	// genuine duplicate.
	leftReady := s.columnReady("coach_students", "left_at")
	dupSel := "id,student_id,invite_token,student_email,student_phone,student_name"
	if leftReady {
		dupSel += ",left_at"
	}
	if fromLeagueReady {
		dupSel += ",from_league"
	}
	var dup map[string]any
	if email != "" {
		dup, _ = s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_email=eq."+store.Q(email)+"&select="+dupSel)
	}
	if dup == nil && np != "" {
		dup, _ = s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_phone=eq."+store.Q(np)+"&select="+dupSel)
	}
	if dup == nil && resolved != "" {
		dup, _ = s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_id=eq."+store.Q(resolved)+"&select="+dupSel)
	}
	if dup != nil {
		if !leftReady || asStr(dup, "left_at") == "" {
			// Active duplicate. A MANUAL add "claims" the student — make sure the
			// row is protected from league auto-removal (it may have originated as
			// a league auto-enroll) before reporting the duplicate.
			if !fromLeague && fromLeagueReady && asBool(dup, "from_league") {
				_, _ = s.sb.Update("coach_students", "id=eq."+store.Q(asStr(dup, "id")),
					map[string]any{"from_league": false})
			}
			return model.CoachStudent{}, errors.New("that student is already on your roster")
		}
		return s.reactivateLeftStudent(coachID, dup, email, np, rawPhone, name, level, phoneReady, fromLeague)
	}
	row := map[string]any{
		"coach_id":      coachID,
		"student_email": orNull(email),
		"student_name":  orNull(name),
	}
	if phoneReady {
		row["student_phone"] = orNull(np)
	}
	if fromLeagueReady {
		row["from_league"] = fromLeague
	}
	if level != "" && s.columnReady("coach_students", "skill_level") {
		row["skill_level"] = level
	}
	// resolved (computed above) links a registered student immediately so we can
	// skip the invite.
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
	} else {
		// Already a PlanMyPickle account → no invite email fires, so tell them
		// directly that the coaching relationship started (mirrors the invite).
		s.notifyUser(resolved, "coaching", coachID, s.coachingName(coachID),
			s.coachingName(coachID)+" added you as a student",
			"coaching:"+asStr(ins[0], "id"))
	}
	return mapCoachStudent(ins[0]), nil
}

// ResendCoachInvite re-sends the join invite (SMS and/or email) to a student who
// hasn't registered yet. Coach-only (the caller must own the roster row). Errors
// if the student already has a PlanMyPickle account or has no contact on file.
func (s *Service) ResendCoachInvite(coachID, id string) error {
	if !s.coachingReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coach_students", "id=eq."+store.Q(id)+
		"&select=coach_id,student_id,student_email,student_phone,student_name,invite_token")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	if asStr(row, "student_id") != "" {
		return errors.New("this student is already on PlanMyPickle")
	}
	email := asStr(row, "student_email")
	phone := asStr(row, "student_phone")
	if email == "" && phone == "" {
		return errors.New("no email or phone on file to send an invite to")
	}
	// Older rows may lack a token — mint one so the claim still binds.
	token := asStr(row, "invite_token")
	if token == "" && s.columnReady("coach_students", "invite_token") {
		token = newID()
		_, _ = s.sb.Update("coach_students", "id=eq."+store.Q(id),
			map[string]any{"invite_token": token})
	}
	name := asStr(row, "student_name")
	if email != "" {
		go s.sendCoachInvite(coachID, email, name, token)
	}
	if phone != "" {
		go s.sendCoachInviteSMS(coachID, phone, token)
	}
	return nil
}

// reactivateLeftStudent revives a coach_students row the student previously LEFT
// (left_at set → hidden from the roster) instead of erroring "already added". It
// clears left_at, refreshes name/level/contact, re-links to an account if one
// exists now, and re-fires the invite so the student is pinged again.
func (s *Service) reactivateLeftStudent(coachID string, row map[string]any,
	email, np, rawPhone, name, level string, phoneReady, fromLeague bool) (model.CoachStudent, error) {
	id := asStr(row, "id")
	patch := map[string]any{"left_at": nil}
	// A MANUAL re-add always protects the thread (from_league=false). A league
	// re-enroll must NOT downgrade a row that was already manual/protected — only
	// leave a genuinely league-originated row as league-owned.
	if s.columnReady("coach_students", "from_league") && !fromLeague {
		patch["from_league"] = false
	}
	if name != "" {
		patch["student_name"] = name
	}
	if email != "" {
		patch["student_email"] = email
	}
	if phoneReady && np != "" {
		patch["student_phone"] = np
	}
	if level != "" && s.columnReady("coach_students", "skill_level") {
		patch["skill_level"] = level
	}
	// Re-link if the contact now maps to an account.
	resolved := asStr(row, "student_id")
	if resolved == "" {
		resolved = s.userIDByEmail(email)
		if resolved == "" && np != "" {
			resolved = s.userIDByPhone(np)
		}
		if resolved != "" {
			patch["student_id"] = resolved
		}
	}
	inviteToken := asStr(row, "invite_token")
	if resolved == "" && inviteToken == "" && s.columnReady("coach_students", "invite_token") {
		inviteToken = newID()
		patch["invite_token"] = inviteToken
	}
	if _, err := s.sb.Update("coach_students", "id=eq."+store.Q(id), patch); err != nil {
		return model.CoachStudent{}, err
	}
	// Re-fire the invite on whatever channels we have (same as a fresh add).
	if resolved == "" {
		if email != "" {
			go s.sendCoachInvite(coachID, email, name, inviteToken)
		}
		if rawPhone != "" {
			go s.sendCoachInviteSMS(coachID, rawPhone, inviteToken)
		}
	} else {
		s.notifyUser(resolved, "coaching", coachID, s.coachingName(coachID),
			s.coachingName(coachID)+" added you as a student", "coaching:"+id)
	}
	fresh, err := s.sb.SelectOne("coach_students", "id=eq."+store.Q(id)+"&select=*")
	if err != nil || fresh == nil {
		return model.CoachStudent{}, errors.New("could not reactivate that student")
	}
	return mapCoachStudent(fresh), nil
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
	// Short & punchy, with a shortened link so the whole text stays in one SMS
	// segment (and doesn't look like a random token).
	body := fmt.Sprintf(
		"%s added you as a student on PlanMyPickle — clips, feedback & drills in one place. Join for free: %s",
		coach, s.ShortLink(coachInviteURL(token, "")))
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
		"coach_id=eq."+store.Q(coachID)+s.activeStudentFilter()+"&order="+order)
	if err != nil {
		return nil, err
	}
	// Collapse duplicate rows for the same person (same linked account, or same
	// email when unlinked) — e.g. one added by email and another created when
	// they booked a session. Keep the richer row (more clips); tie → the first,
	// which is the most-recently-active given the ordering above.
	out := make([]model.CoachStudent, 0, len(rows))
	seen := map[string]int{}
	for _, r := range rows {
		cs := mapCoachStudent(r)
		cs.VideoCount = s.threadVideoCount(cs.ID)
		key := ""
		if cs.StudentID != "" {
			key = "id:" + cs.StudentID
		} else if e := strings.ToLower(strings.TrimSpace(cs.StudentEmail)); e != "" {
			key = "em:" + e
		}
		if key != "" {
			if idx, ok := seen[key]; ok {
				// Only collapse a duplicate that carries NO clips of its own — an
				// empty booking/seed row. Never hide a thread that has data (its
				// clips, program, PB Vision, and ratings would become unreachable).
				if cs.VideoCount == 0 {
					continue // safe to drop this empty duplicate
				}
				if out[idx].VideoCount == 0 {
					out[idx] = cs // the survivor was empty; prefer the one with clips
					continue
				}
				// Both rows have clips: keep both rather than lose data.
			} else {
				seen[key] = len(out)
			}
		}
		out = append(out, cs)
	}
	s.applyUnread(coachID, out)
	s.applyRosterAggregates(out)
	return out, nil
}

// applyRosterAggregates fills each student's RubricAvg (mean skill rating) and
// OpenGoals (drills not done) using two batched queries over all their threads,
// so the coach roster can flag who needs attention without an N+1.
func (s *Service) applyRosterAggregates(students []model.CoachStudent) {
	if len(students) == 0 {
		return
	}
	ids := make([]string, 0, len(students))
	idx := make(map[string]int, len(students))
	for i, st := range students {
		ids = append(ids, st.ID)
		idx[st.ID] = i
	}
	// Skill-rating average per thread.
	if s.columnReady("coaching_skill_ratings", "id") {
		sum := map[string]float64{}
		cnt := map[string]int{}
		if rows, err := s.sb.Select("coaching_skill_ratings",
			"coach_student_id="+store.In(ids)+"&select=coach_student_id,rating"); err == nil {
			for _, r := range rows {
				tid := asStr(r, "coach_student_id")
				if v := asFloatPtr(r, "rating"); v != nil {
					sum[tid] += *v
					cnt[tid]++
				}
			}
		}
		for tid, c := range cnt {
			if c > 0 {
				avg := sum[tid] / float64(c)
				students[idx[tid]].RubricAvg = &avg
			}
		}
	}
	// Open (not-done) assigned drills per thread.
	if s.assignmentsReady() {
		if rows, err := s.sb.Select("coaching_assignments",
			"coach_student_id="+store.In(ids)+"&status=neq.done&select=coach_student_id"); err == nil {
			for _, r := range rows {
				if i, ok := idx[asStr(r, "coach_student_id")]; ok {
					students[i].OpenGoals++
				}
			}
		}
	}
	// Clips the student uploaded that have no coach comment yet — the coach's
	// review backlog. Two batched queries: the student's clips, then which of
	// those a coach has answered.
	if vids, err := s.sb.Select("coaching_videos",
		"coach_student_id="+store.In(ids)+
			"&uploader_role=eq.student&select=id,coach_student_id"); err == nil && len(vids) > 0 {
		vidThread := make(map[string]string, len(vids))
		vidIDs := make([]string, 0, len(vids))
		for _, v := range vids {
			id := asStr(v, "id")
			if id != "" {
				vidThread[id] = asStr(v, "coach_student_id")
				vidIDs = append(vidIDs, id)
			}
		}
		answered := map[string]bool{}
		if fb, ferr := s.sb.Select("coaching_feedback",
			"video_id="+store.In(vidIDs)+
				"&author_role=eq.coach&select=video_id"); ferr == nil {
			for _, f := range fb {
				answered[asStr(f, "video_id")] = true
			}
		}
		for vid, tid := range vidThread {
			if !answered[vid] {
				if i, ok := idx[tid]; ok {
					students[i].AwaitingFeedback++
				}
			}
		}
	}
}

// RemoveCoachStudent deletes a roster row (and its clips/feedback via cascade).
// It also tears down cleanly: cancels the student's future 1:1 sessions, tells
// the student the coaching ended, and prunes both parties' now-dead bell links.
func (s *Service) RemoveCoachStudent(coachID, id string) error {
	if !s.coachingReady() {
		return ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coach_students", "id=eq."+store.Q(id)+
		"&select=coach_id,student_id,student_email,student_phone,student_name")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	cs := mapCoachStudent(row)
	coachName := s.coachingName(coachID)

	// Cancel future 1:1 sessions on this thread first — the schedule FK is ON
	// DELETE SET NULL, so leaving them would strand a stale session on the coach's
	// calendar and vanish it from the student's My Sessions with no notice.
	if s.scheduleReady() {
		nowT := time.Now().UTC().Format(time.RFC3339)
		sess, _ := s.sb.SelectAll("coaching_schedule",
			"coach_student_id=eq."+store.Q(id)+"&kind=eq.session"+
				"&starts_at=gte."+store.Q(nowT)+"&select=id")
		if len(sess) > 0 {
			s.notifyCoachingCounterpartLink(cs, "coach", coachID, coachName,
				"Your upcoming 1:1 session was canceled", "")
			for _, r := range sess {
				_ = s.sb.Delete("coaching_schedule", "id=eq."+store.Q(asStr(r, "id")))
			}
		}
	}

	if derr := s.sb.Delete("coach_students", "id=eq."+store.Q(id)); derr != nil {
		return derr
	}
	// Tell the student the coaching ended — this is otherwise the only silent
	// destructive coaching action. The thread is gone, so use an EMPTY link (a
	// coaching:<id> link would dead-end on "thread not found").
	s.notifyCoachingCounterpartLink(cs, "coach", coachID, coachName,
		s.coachLabel(coachID)+" ended your coaching and removed your clip history", "")
	// Prune both parties' stale bell rows that deep-link into the now-gone thread
	// (they would otherwise route to a permanent "Could not load this thread").
	if s.columnReady("user_notifications", "link") {
		_ = s.sb.Delete("user_notifications", "link=like.coaching:"+id+"*")
	}
	return nil
}

// LeaveCoach lets a STUDENT end a coaching relationship (soft-archive so their
// clip history is preserved but the thread hides from both rosters). Coaches use
// RemoveCoachStudent instead. Inert until the left_at column exists.
func (s *Service) LeaveCoach(threadID, userID, email string) error {
	if !s.coachingReady() {
		return ErrCoachingUnavailable
	}
	if !s.columnReady("coach_students", "left_at") {
		return ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return err
	}
	if role != "student" {
		return ErrForbidden // the coach severs via RemoveCoachStudent
	}
	if _, err := s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
		map[string]any{"left_at": now()}); err != nil {
		return err
	}
	who := s.coachingName(userID)
	if strings.TrimSpace(who) == "" {
		who = "Your student"
	}
	s.notifyCoachingCounterpartLink(cs, "student", userID, who,
		who+" ended their coaching with you", "")
	return nil
}

// CoachStudentCredits lists each student who holds prepaid class credits with this
// coach — the coach-facing view of how many sessions they owe.
func (s *Service) CoachStudentCredits(coachID string) ([]model.CoachCreditOwed, error) {
	if !s.creditsReady() || coachID == "" {
		return []model.CoachCreditOwed{}, nil
	}
	rows, err := s.sb.Select("coaching_credits",
		"coach_id=eq."+store.Q(coachID)+"&credits_remaining=gt.0"+
			"&order=credits_remaining.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachCreditOwed, 0, len(rows))
	for _, r := range rows {
		uid := asStr(r, "user_id")
		name := s.coachingName(uid)
		if strings.TrimSpace(name) == "" {
			name = "A player"
		}
		out = append(out, model.CoachCreditOwed{
			StudentID:        uid,
			StudentName:      name,
			CreditsRemaining: asInt(r, "credits_remaining"),
		})
	}
	return out, nil
}

// activeStudentFilter appends a left_at IS NULL predicate once the soft-leave
// column exists, so a student who left a coach drops off both rosters.
func (s *Service) activeStudentFilter() string {
	if s.columnReady("coach_students", "left_at") {
		return "&left_at=is.null"
	}
	return ""
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
	if email == "" && np == "" && studentID == "" {
		return []model.CoachStudent{}, nil
	}
	// Threads addressed to this student by account id, email, OR phone. Separate
	// queries + dedup avoids fiddly PostgREST or() escaping. The student_id branch
	// is load-bearing: a phone-first signup (roster row has student_id but null
	// email/phone) or an invite claimed from a different email/phone than invited
	// would otherwise never surface here, even though thread ACCESS keys on
	// student_id — matching the id-based discovery ListMySessions already uses.
	byID := map[string]map[string]any{}
	af := s.activeStudentFilter()
	if studentID != "" {
		if rows, e := s.sb.Select("coach_students",
			"student_id=eq."+store.Q(studentID)+af+"&order=created_at.desc"); e == nil {
			for _, r := range rows {
				byID[asStr(r, "id")] = r
			}
		}
	}
	if email != "" {
		if rows, e := s.sb.Select("coach_students",
			"student_email=eq."+store.Q(email)+af+"&order=created_at.desc"); e == nil {
			for _, r := range rows {
				byID[asStr(r, "id")] = r
			}
		}
	}
	// A phone match only counts when the account's phone is VERIFIED. The profile
	// phone is self-set with no OTP (SetMyBasicInfo), so without this anyone could
	// type a stranger's number and pick up their coaching threads — threadMembership
	// already requires PhoneVerified for exactly this reason; mirror it here.
	phoneMatched := map[string]bool{}
	if np != "" && s.PhoneVerified(studentID) {
		if rows, e := s.sb.Select("coach_students",
			"student_phone=eq."+store.Q(np)+af+"&order=created_at.desc"); e == nil {
			for _, r := range rows {
				id := asStr(r, "id")
				if _, already := byID[id]; !already {
					phoneMatched[id] = true // matched ONLY by phone
				}
				byID[id] = r
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
		// Backfill the account link the first time we see this student logged in —
		// but NEVER off a phone-only match (even a verified one is a weaker claim
		// than an account/email match; a phone-invited student links via their
		// invite token in ClaimCoachInvite). Writing student_id is permanent: it
		// grants full thread membership from then on, bypassing later checks.
		if cs.StudentID == "" && studentID != "" && !phoneMatched[cs.ID] {
			if _, uerr := s.sb.Update("coach_students", "id=eq."+store.Q(cs.ID),
				map[string]any{"student_id": studentID}); uerr == nil {
				cs.StudentID = studentID
			}
		}
		cs.CoachName = s.coachingName(cs.CoachID)
		cs.VideoCount = s.threadVideoCount(cs.ID)
		cs.LastActivity = cs.LastActivityAt // ISO → frontend renders "3 days ago"
		cs.CoachNote = ""                   // the coach's private note about the student is never sent to them
		cs.SkillLevel = ""                  // coach's assessment — coach-only
		out = append(out, cs)
	}
	// Coach avatars (one batched lookup) so each card shows a real face.
	coachIDs := make([]string, 0, len(out))
	for _, cs := range out {
		coachIDs = append(coachIDs, cs.CoachID)
	}
	photos := s.coachPhotosByID(coachIDs)
	for i := range out {
		out[i].CoachPhotoURL = photos[out[i].CoachID]
	}
	// Open drills the student still owes (a batched aggregate) makes the card
	// actionable. The other roster aggregates are the COACH's private view
	// (skill assessment, their review backlog) — strip them from the student.
	s.applyRosterAggregates(out)
	for i := range out {
		out[i].RubricAvg = nil
		out[i].AwaitingFeedback = 0
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
	// Phone match authorizes thread access only when the caller's phone is
	// VERIFIED — the sign-up user_metadata phone is attacker-controllable and
	// unverified, so matching on it would let anyone who knows a phone-invited
	// student's number take over their thread. Unverified phone-invited students
	// link securely via the coach's invite token (ClaimCoachInvite) instead.
	phoneOnlyMatch := false
	if !studentMatch && cs.StudentPhone != "" && userID != "" && s.PhoneVerified(userID) {
		studentMatch = normPhone(s.phoneOf(userID)) == cs.StudentPhone
		phoneOnlyMatch = studentMatch
	}
	if studentMatch {
		// Backfill the account link opportunistically — but NEVER off a phone-only
		// match. Writing student_id is permanent and grants membership from then
		// on with no further checks; a recycled/reassigned number would inherit a
		// stranger's thread forever. Phone-invited students bind via their invite
		// token (ClaimCoachInvite) instead.
		if cs.StudentID == "" && userID != "" && !phoneOnlyMatch {
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
	// If this thread has REAL analyzed jobs, override the summary with live
	// truth: the actual match count + when the latest analysis was generated.
	// The detailed per-shot metrics PB Vision doesn't return per player (serve %,
	// mph, shot mix, an overall rating) are sample-only, so we drop them and flag
	// the summary "live" — the real per-player numbers live in the analysis card.
	if cnt, lastAt, ok := s.realPBVisionSummary(threadID); ok {
		out.Rating = nil
		out.LastSyncedAt = lastAt
		if out.Stats == nil {
			out.Stats = map[string]any{}
		} else {
			// Copy so we don't mutate the cached demo blob.
			cp := make(map[string]any, len(out.Stats)+2)
			for k, v := range out.Stats {
				cp[k] = v
			}
			out.Stats = cp
		}
		out.Stats["live"] = true
		out.Stats["matchesAnalyzed"] = cnt
	}
	return out, nil
}

// realPBVisionSummary returns a live rollup for a thread from its actual ready
// PB Vision jobs (ones initiated in this thread or assigned to it): how many
// matches were analyzed and when the latest one was generated. ok is false when
// there are no completed analyses yet, so GetPBVision keeps any seeded summary.
func (s *Service) realPBVisionSummary(threadID string) (count int, lastAt string, ok bool) {
	if !s.pbvisionJobsReady() {
		return 0, "", false
	}
	seen := map[string]bool{}
	consider := func(updated string) {
		count++
		if updated > lastAt {
			lastAt = updated
		}
	}
	// Jobs initiated in this thread.
	rows, _ := s.sb.Select("coaching_pbvision_jobs",
		"coach_student_id=eq."+store.Q(threadID)+
			"&status=eq.ready&select=id,updated_at")
	for _, r := range rows {
		id := asStr(r, "id")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		consider(asStr(r, "updated_at"))
	}
	// Jobs where this thread's student was assigned a detected player (so one
	// analysis can cover up to 4 students).
	if s.columnReady("coaching_pbvision_assignments", "id") {
		arows, _ := s.sb.Select("coaching_pbvision_assignments",
			"coach_student_id=eq."+store.Q(threadID)+"&select=job_id")
		ids := []string{}
		for _, a := range arows {
			jid := asStr(a, "job_id")
			if jid != "" && !seen[jid] {
				ids = append(ids, jid)
			}
		}
		if len(ids) > 0 {
			jrows, _ := s.sb.Select("coaching_pbvision_jobs",
				"id="+store.In(ids)+"&status=eq.ready&select=id,updated_at")
			for _, r := range jrows {
				id := asStr(r, "id")
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				consider(asStr(r, "updated_at"))
			}
		}
	}
	return count, lastAt, count > 0
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
		q := "status=eq.ready&limit=1&select=id,report_url,insights,stats,updated_at"
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
		ThreadID:   threadID,
		BuyerName:  cs.StudentName,
		CreatedAt:  asStr(row, "updated_at"),
		MatchStats: parsePBVisionMatchStats(row["stats"]),
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
			out.SourceVideoID = svid
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

// PBVisionRawJSON returns the raw PB Vision insights + stats payload for the
// thread's latest ready analysis — a member-gated inspector so we can map new
// stat fields (forward pressure, kitchen-arrival splits, rallies-won, …) into
// the app before building their display.
func (s *Service) PBVisionRawJSON(threadID, userID, email string) (map[string]any, error) {
	if !s.coachingReady() || !s.pbvisionJobsReady() {
		return nil, ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return nil, err
	}
	row, err := s.sb.SelectOne("coaching_pbvision_jobs",
		"coach_student_id=eq."+store.Q(threadID)+
			"&status=eq.ready&order=updated_at.desc&limit=1&select=insights,stats")
	if err != nil {
		return nil, err
	}
	if row == nil {
		return map[string]any{}, nil
	}
	return map[string]any{"insights": row["insights"], "stats": row["stats"]}, nil
}

// ListCoachAnalyses returns every ready PB Vision analysis across the coach's
// students, so the instructor can distribute detected players from one hub
// instead of opening each student's thread. Only analyses INITIATED in a
// student's thread are listed (that student is the buyer/payer); a thread that
// merely had a player assigned to it isn't the buyer, so it's skipped — the
// buyer's own card already covers that match. Each item carries its own
// ThreadID so assignment calls route to the right (buyer's) thread.
func (s *Service) ListCoachAnalyses(userID, email string) ([]model.PBVisionAnalysis, error) {
	if !s.coachingReady() || !s.pbvisionJobsReady() {
		return []model.PBVisionAnalysis{}, nil
	}
	students, err := s.ListCoachStudents(userID)
	if err != nil {
		return nil, err
	}
	out := []model.PBVisionAnalysis{}
	seen := map[string]bool{}
	for _, st := range students {
		row, _ := s.sb.SelectOne("coaching_pbvision_jobs",
			"coach_student_id=eq."+store.Q(st.ID)+
				"&status=eq.ready&order=updated_at.desc&limit=1&select=id")
		if row == nil {
			continue // this thread didn't buy an analysis
		}
		a, aerr := s.GetThreadPBVisionAnalysis(st.ID, userID, email)
		if aerr != nil || !a.Ready || len(a.Players) == 0 || seen[a.JobID] {
			continue
		}
		seen[a.JobID] = true
		out = append(out, a)
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
				s.coachLabel(cs.CoachID)+" shared a match analysis — see your PB Vision stats",
				"coaching:"+targetThreadID+"?tab=pbvision")
		}
	}
	return nil
}

// parsePBVisionHighlights pulls the flagged moments out of insights.highlights:
// time-ranges (s..e seconds) into the source clip with a label.
// parsePBVisionMatchStats maps PB Vision's stats.json into the Team-Stats model.
// Field formulas verified against a real payload: per-player kitchen-arrival % =
// (oneself.num + partner.num) / (oneself.den + partner.den); shot share =
// team_shot_percentage; team→kitchen = game.team_percentage_to_kitchen (0–1).
func parsePBVisionMatchStats(raw any) *model.PBVisionMatchStats {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	game, _ := m["game"].(map[string]any)
	playersRaw, _ := m["players"].([]any)
	if game == nil || len(playersRaw) == 0 {
		return nil
	}
	out := &model.PBVisionMatchStats{
		AvgShots:       pbNum(game["avg_shots"]),
		KitchenRallies: int(pbNum(game["kitchen_rallies"])),
	}
	if s := pbFloatSlice(game["game_outcome"]); len(s) == 2 {
		out.Score = []int{int(s[0]), int(s[1])}
	}
	if s := pbFloatSlice(game["team_percentage_to_kitchen"]); len(s) == 2 {
		out.TeamPctToKitchen = []float64{s[0] * 100, s[1] * 100}
	}
	if lr, ok := game["longest_rally"].(map[string]any); ok {
		out.LongestRally = int(pbNum(lr["num_shots"]))
	}

	var shortA, shortB, medA, medB, longA, longB float64
	for i, pr := range playersRaw {
		p, ok := pr.(map[string]any)
		if !ok {
			continue
		}
		team := int(pbNum(p["team"]))
		ps := model.PBVisionPlayerStat{
			AvatarID:         i,
			Team:             team,
			ServeKitchenPct:  pbKitchenArrival(p, "serving"),
			ReturnKitchenPct: pbKitchenArrival(p, "returning"),
			ShotSharePct:     pbNum(p["team_shot_percentage"]),
			LeftSidePct:      pbNum(p["team_left_side_percentage"]),
			ShotCount:        int(pbNum(p["shot_count"])),
			ShotQuality:      pbNum(p["average_shot_quality"]),
		}
		if sp, ok := p["speedups"].(map[string]any); ok {
			ps.Speedups = int(pbNum(sp["count"]))
		}
		ps.Shots = pbShotStats(p)
		out.Players = append(out.Players, ps)
		// Rally-won bands are team-level (identical for both players on a team).
		if team == 0 {
			shortA = pbNum(p["team_short_length_rallies_won"])
			medA = pbNum(p["team_medium_length_rallies_won"])
			longA = pbNum(p["team_long_length_rallies_won"])
		} else {
			shortB = pbNum(p["team_short_length_rallies_won"])
			medB = pbNum(p["team_medium_length_rallies_won"])
			longB = pbNum(p["team_long_length_rallies_won"])
		}
	}
	out.RalliesWon = []model.PBVisionRallyBand{
		{Label: "Short rallies · 1–5 shots", TeamA: shortA, TeamB: shortB},
		{Label: "Medium rallies · 6–10 shots", TeamA: medA, TeamB: medB},
		{Label: "Long rallies · 11+ shots", TeamA: longA, TeamB: longB},
	}
	return out
}

// pbKitchenArrival returns a player's kitchen-arrival % for a role (serving |
// returning), combining their own + partner's shots: (Σnum)/(Σden)*100.
func pbKitchenArrival(p map[string]any, role string) float64 {
	kap, ok := p["kitchen_arrival_percentage"].(map[string]any)
	if !ok {
		return 0
	}
	r, ok := kap[role].(map[string]any)
	if !ok {
		return 0
	}
	var num, den float64
	for _, k := range []string{"oneself", "partner"} {
		if x, ok := r[k].(map[string]any); ok {
			num += pbNum(x["numerator"])
			den += pbNum(x["denominator"])
		}
	}
	if den == 0 {
		return 0
	}
	return num / den * 100
}

func pbFloatSlice(v any) []float64 {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]float64, 0, len(arr))
	for _, x := range arr {
		out = append(out, pbNum(x))
	}
	return out
}

// pbShotOrder is the curated shot-type list (key → label) shown in the per-
// player breakdown, in a sensible reading order.
var pbShotOrder = []struct{ key, label string }{
	{"serves", "Serves"}, {"returns", "Returns"},
	{"thirds", "3rd shots"}, {"third_drives", "3rd drives"},
	{"third_drops", "3rd drops"}, {"fourths", "4th shots"},
	{"fifths", "5th shots"}, {"drives", "Drives"}, {"drops", "Drops"},
	{"dinks", "Dinks"}, {"resets", "Resets"}, {"speedups", "Speed-ups"},
	{"smashes", "Smashes"}, {"lobs", "Lobs"}, {"poaches", "Poaches"},
	{"passing", "Passing"}, {"forehands", "Forehands"}, {"backhands", "Backhands"},
}

// pbShotStats extracts a player's per-shot-type breakdown (only types actually
// hit). Fields come from each shot object's outcome_stats + speed_stats.
func pbShotStats(p map[string]any) []model.PBVisionShotStat {
	out := []model.PBVisionShotStat{}
	for _, it := range pbShotOrder {
		st, ok := p[it.key].(map[string]any)
		if !ok {
			continue
		}
		cnt := int(pbNum(st["count"]))
		if cnt <= 0 {
			continue
		}
		s := model.PBVisionShotStat{
			Type: it.label, Count: cnt, Quality: pbNum(st["average_quality"]),
		}
		if os, ok := st["outcome_stats"].(map[string]any); ok {
			s.SuccessPct = pbNum(os["success_percentage"])
			s.WonPct = pbNum(os["rally_won_percentage"])
		}
		if sp, ok := st["speed_stats"].(map[string]any); ok {
			s.AvgSpeed = pbNum(sp["average"])
		}
		out = append(out, s)
	}
	return out
}

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
		// PB Vision reports highlight start/end in MILLISECONDS — convert to
		// seconds so the label + video seek land in the right place.
		out = append(out, model.PBVisionHighlight{
			StartSeconds: pbNum(h["s"]) / 1000.0,
			EndSeconds:   pbNum(h["e"]) / 1000.0,
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

// seedPBVisionJob inserts a canned status='ready' analysis job for a demo thread
// so the PB Vision tab's rich visuals — the detected-player picker and the team
// match-stats — render on seeded data. (The rating hero + history come from
// coaching_pbvision / _reports separately.) sourceVideoID points at one of the
// thread's clips; the highlights list only *plays* when that clip is signable,
// and the seeded sample clips live outside the coaching bucket, so highlights
// parse but stay hidden — the player picker + match stats are the marquee
// visuals this unblocks. Shapes mirror the real PB Vision payload exactly (see
// parsePBVisionPlayers / parsePBVisionMatchStats).
func (s *Service) seedPBVisionJob(threadID, sourceVideoID string) {
	if !s.pbvisionJobsReady() || threadID == "" {
		return
	}
	// A kitchen-arrival tree: serving/returning × oneself/partner (num/den).
	ka := func(so, sod, sp, spd, ro, rod, rp, rpd float64) map[string]any {
		leaf := func(n, d float64) map[string]any {
			return map[string]any{"numerator": n, "denominator": d}
		}
		return map[string]any{
			"serving":   map[string]any{"oneself": leaf(so, sod), "partner": leaf(sp, spd)},
			"returning": map[string]any{"oneself": leaf(ro, rod), "partner": leaf(rp, rpd)},
		}
	}
	// A per-shot-type stat block: count, quality, outcome %, avg speed.
	shot := func(count int, q, succ, won, speed float64) map[string]any {
		return map[string]any{
			"count":           count,
			"average_quality": q,
			"outcome_stats":   map[string]any{"success_percentage": succ, "rally_won_percentage": won},
			"speed_stats":     map[string]any{"average": speed},
		}
	}
	ka0 := ka(18, 22, 16, 22, 20, 24, 19, 24)
	ka1 := ka(16, 22, 18, 22, 18, 24, 20, 24)
	ka2 := ka(15, 23, 14, 23, 17, 25, 16, 25)
	ka3 := ka(14, 23, 15, 23, 16, 25, 17, 25)

	// insights.player_data — feeds the detected-player picker.
	playerData := []map[string]any{
		{"avatar_id": 0, "team": 0, "shot_count": 142, "left_side_percentage": 38, "total_team_shot_percentage": 52, "kitchen_arrival_percentage": ka0},
		{"avatar_id": 1, "team": 0, "shot_count": 131, "left_side_percentage": 62, "total_team_shot_percentage": 48, "kitchen_arrival_percentage": ka1},
		{"avatar_id": 2, "team": 1, "shot_count": 128, "left_side_percentage": 45, "total_team_shot_percentage": 51, "kitchen_arrival_percentage": ka2},
		{"avatar_id": 3, "team": 1, "shot_count": 119, "left_side_percentage": 55, "total_team_shot_percentage": 49, "kitchen_arrival_percentage": ka3},
	}
	highlights := []map[string]any{
		{"s": 800, "e": 3200, "kind": "rally", "short_description": "Long hands battle at the net — reset under pressure"},
		{"s": 3600, "e": 5200, "kind": "winner", "short_description": "Third-shot drop, advance, and put-away"},
		{"s": 5600, "e": 7400, "kind": "speedup", "short_description": "Speed-up won at the kitchen line"},
	}

	// stats.players — feeds the team match-stats breakdown. Rally-won bands are
	// team-level (repeated for both players on a team).
	player := func(team, shots, speedups int, shotPct, leftPct, quality, shortW, medW, longW float64, kap map[string]any, shots2 map[string]any) map[string]any {
		p := map[string]any{
			"team":                       team,
			"kitchen_arrival_percentage": kap,
			"team_shot_percentage":       shotPct,
			"team_left_side_percentage":  leftPct,
			"shot_count":                 shots,
			"average_shot_quality":       quality,
			// Full shot shape (not just {count}) so it satisfies BOTH readers:
			// the "Speed-ups share" scalar (reads .count) and pbShotStats, which
			// lists "speedups" as a shot type — a count-only object would render a
			// degenerate all-zero row in the per-player shot table.
			"speedups":                       shot(speedups, 3.4, 62, 48, 46),
			"team_short_length_rallies_won":  shortW,
			"team_medium_length_rallies_won": medW,
			"team_long_length_rallies_won":   longW,
		}
		for k, v := range shots2 {
			p[k] = v
		}
		return p
	}
	statsPlayers := []map[string]any{
		player(0, 142, 5, 52, 38, 3.6, 9, 7, 4, ka0, map[string]any{
			"serves": shot(21, 3.6, 96, 44, 34), "returns": shot(19, 3.5, 92, 47, 41),
			"dinks": shot(38, 3.9, 88, 58, 18), "drops": shot(14, 3.1, 71, 52, 22),
			"drives": shot(12, 3.3, 66, 49, 39), "thirds": shot(16, 3.2, 74, 51, 28),
		}),
		player(0, 131, 3, 48, 62, 3.3, 9, 7, 4, ka1, map[string]any{
			"serves": shot(20, 3.3, 90, 41, 33), "returns": shot(18, 3.2, 86, 43, 40),
			"dinks": shot(31, 3.5, 82, 53, 17), "drops": shot(13, 2.9, 66, 47, 21),
			"drives": shot(15, 3.4, 69, 51, 41), "thirds": shot(14, 3.0, 70, 48, 27),
		}),
		player(1, 128, 4, 51, 45, 3.2, 6, 4, 2, ka2, map[string]any{
			"serves": shot(19, 3.2, 89, 39, 32), "returns": shot(17, 3.1, 84, 40, 39),
			"dinks": shot(29, 3.3, 79, 48, 16), "drops": shot(12, 2.8, 63, 44, 20),
			"drives": shot(16, 3.3, 67, 50, 42), "thirds": shot(13, 2.9, 68, 45, 26),
		}),
		player(1, 119, 2, 49, 55, 3.0, 6, 4, 2, ka3, map[string]any{
			"serves": shot(18, 3.0, 87, 37, 31), "returns": shot(16, 2.9, 82, 38, 38),
			"dinks": shot(27, 3.1, 76, 45, 15), "drops": shot(11, 2.7, 61, 42, 19),
			"drives": shot(14, 3.1, 64, 47, 40), "thirds": shot(12, 2.8, 66, 43, 25),
		}),
	}
	game := map[string]any{
		"avg_shots":                  7.3,
		"kitchen_rallies":            41,
		"game_outcome":               []float64{11, 7},
		"team_percentage_to_kitchen": []float64{0.86, 0.79},
		"longest_rally":              map[string]any{"num_shots": 24},
	}

	row := map[string]any{
		"vid":              "seed-job-" + threadID,
		"coach_student_id": threadID,
		"status":           "ready",
		"insights":         map[string]any{"player_data": playerData, "highlights": highlights},
		"stats":            map[string]any{"game": game, "players": statsPlayers},
		"updated_at":       time.Now().UTC().Format(time.RFC3339),
	}
	if sourceVideoID != "" && s.columnReady("coaching_pbvision_jobs", "source_video_id") {
		row["source_video_id"] = sourceVideoID
	}
	_, _ = s.sb.Upsert("coaching_pbvision_jobs", "vid", row)
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
				wk := model.CoachingProgramWeek{
					Focus: asStr(m, "focus"),
					Done:  asBool(m, "done"),
					Due:   asStr(m, "due"),
				}
				if dr, ok := m["drills"].([]any); ok {
					for _, d := range dr {
						if dm, ok := d.(map[string]any); ok {
							wk.Drills = append(wk.Drills, model.CoachingProgramDrill{
								ID:    asStr(dm, "id"),
								Title: asStr(dm, "title"),
							})
						}
					}
				}
				p.Weeks = append(p.Weeks, wk)
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
		focus := strings.TrimSpace(f.Focus)
		if focus == "" {
			continue
		}
		wk := map[string]any{"focus": focus, "done": false}
		if due := strings.TrimSpace(f.Due); due != "" {
			wk["due"] = due
		}
		drills := make([]map[string]any, 0, len(f.Drills))
		for _, d := range f.Drills {
			if id := strings.TrimSpace(d.ID); id != "" {
				drills = append(drills, map[string]any{
					"id": id, "title": strings.TrimSpace(d.Title)})
			}
		}
		if len(drills) > 0 {
			wk["drills"] = drills
		}
		weeks = append(weeks, wk)
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
		s.coachLabel(cs.CoachID)+" assigned a new training program: "+title)
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
		// Re-opening a week (un-completing) re-arms both reminder tiers so a
		// genuinely-still-due week can nudge again.
		if !nowDone {
			delete(m, "reminded")
			delete(m, "reminded_soon")
		}
		weeks[index] = m
	}
	out, err := s.sb.Update("coaching_programs", "id=eq."+store.Q(programID),
		map[string]any{"weeks": weeks, "updated_at": now()})
	if err != nil {
		return model.CoachingProgram{}, err
	}
	s.bumpThreadActivity(threadID)
	// Completing a program week notifies the OTHER party, whichever side did it —
	// true parity with drills (SetAssignmentDone), which notifies for either actor.
	if nowDone {
		who := s.coachingName(userID)
		var body string
		if role == "coach" {
			body = s.coachLabel(userID) + " marked a program week complete"
			if focus != "" {
				body = s.coachLabel(userID) + " marked a week complete: " + focus
			}
		} else {
			if who == "" {
				who = "Your student"
			}
			body = who + " completed a program week"
			if focus != "" {
				body = who + " completed a week: " + focus
			}
		}
		s.notifyCoachingCounterpartLink(cs, role, userID, who, body,
			"coaching:"+threadID)
	}
	if len(out) > 0 {
		return mapProgram(out[0]), nil
	}
	return mapProgram(row), nil
}

func (s *Service) programTemplatesReady() bool {
	return s.columnReady("coach_program_templates", "id")
}

func mapProgramTemplate(row map[string]any) model.CoachProgramTemplate {
	t := model.CoachProgramTemplate{
		ID:        asStr(row, "id"),
		Title:     asStr(row, "title"),
		CreatedAt: asStr(row, "created_at"),
		Weeks:     []model.CoachingProgramWeek{},
	}
	if arr, ok := row["weeks"].([]any); ok {
		for _, w := range arr {
			if m, ok := w.(map[string]any); ok {
				wk := model.CoachingProgramWeek{Focus: asStr(m, "focus")}
				if dr, ok := m["drills"].([]any); ok {
					for _, d := range dr {
						if dm, ok := d.(map[string]any); ok {
							wk.Drills = append(wk.Drills, model.CoachingProgramDrill{
								ID: asStr(dm, "id"), Title: asStr(dm, "title")})
						}
					}
				}
				t.Weeks = append(t.Weeks, wk)
			}
		}
	}
	return t
}

// ListProgramTemplates returns a coach's saved templates (newest first).
func (s *Service) ListProgramTemplates(coachID string) ([]model.CoachProgramTemplate, error) {
	if !s.programTemplatesReady() {
		return []model.CoachProgramTemplate{}, nil
	}
	rows, err := s.sb.Select("coach_program_templates",
		"coach_id=eq."+store.Q(coachID)+"&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachProgramTemplate, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapProgramTemplate(r))
	}
	return out, nil
}

// SaveProgramTemplate stores a reusable template (coach). Due dates are dropped
// — they're set per-student at apply time.
func (s *Service) SaveProgramTemplate(coachID string, req model.CoachingProgramRequest) (model.CoachProgramTemplate, error) {
	if !s.programTemplatesReady() {
		return model.CoachProgramTemplate{}, ErrCoachingUnavailable
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return model.CoachProgramTemplate{}, errors.New("give the template a title")
	}
	weeks := make([]map[string]any, 0, len(req.Weeks))
	for _, f := range req.Weeks {
		focus := strings.TrimSpace(f.Focus)
		if focus == "" {
			continue
		}
		wk := map[string]any{"focus": focus}
		drills := make([]map[string]any, 0, len(f.Drills))
		for _, d := range f.Drills {
			if id := strings.TrimSpace(d.ID); id != "" {
				drills = append(drills, map[string]any{
					"id": id, "title": strings.TrimSpace(d.Title)})
			}
		}
		if len(drills) > 0 {
			wk["drills"] = drills
		}
		weeks = append(weeks, wk)
	}
	if len(weeks) == 0 {
		return model.CoachProgramTemplate{}, errors.New("add at least one week")
	}
	ins, err := s.sb.Insert("coach_program_templates", map[string]any{
		"coach_id": coachID, "title": title, "weeks": weeks,
	})
	if err != nil || len(ins) == 0 {
		if err == nil {
			err = errors.New("could not save the template")
		}
		return model.CoachProgramTemplate{}, err
	}
	return mapProgramTemplate(ins[0]), nil
}

// DeleteProgramTemplate removes a coach's template.
func (s *Service) DeleteProgramTemplate(coachID, id string) error {
	if !s.programTemplatesReady() {
		return ErrCoachingUnavailable
	}
	row, _ := s.sb.SelectOne("coach_program_templates",
		"id=eq."+store.Q(id)+"&select=coach_id")
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	return s.sb.Delete("coach_program_templates", "id=eq."+store.Q(id))
}

// ApplyProgramToStudents assigns the same program (title + weeks) to each
// selected student thread the coach owns. Reuses CreateProgram (which supersedes
// any prior active program per thread). Returns how many were assigned.
func (s *Service) ApplyProgramToStudents(coachID, email string, threadIDs []string, req model.CoachingProgramRequest) (int, error) {
	if !s.programsReady() {
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
		if _, err := s.CreateProgram(tid, coachID, email, req); err == nil {
			n++
		}
	}
	return n, nil
}

func (s *Service) practiceReady() bool {
	return s.columnReady("coaching_practice_logs", "id")
}

// LogPractice records a student's self-logged practice on a thread (the return
// hook). Bumps thread activity (resets the inactivity clock) and gives the coach
// a low-key heads-up. Returns the updated summary.
func (s *Service) LogPractice(threadID, userID, email, note string) (model.CoachingPracticeSummary, error) {
	if !s.coachingReady() || !s.practiceReady() {
		return model.CoachingPracticeSummary{}, ErrCoachingUnavailable
	}
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil {
		return model.CoachingPracticeSummary{}, err
	}
	_, err = s.sb.Insert("coaching_practice_logs", map[string]any{
		"coach_student_id": threadID,
		"user_id":          orNull(userID),
		"note":             orNull(strings.TrimSpace(note)),
	})
	if err != nil {
		return model.CoachingPracticeSummary{}, err
	}
	s.bumpThreadActivity(threadID)
	// Heads-up to the coach when the STUDENT logs (parity with drills/programs).
	if role == "student" {
		who := s.coachingName(userID)
		if who == "" {
			who = "Your student"
		}
		body := who + " logged a practice"
		if n := strings.TrimSpace(note); n != "" {
			body = who + " logged a practice: " + n
		}
		s.notifyUser(cs.CoachID, "coaching", userID, who, body,
			"coaching:"+threadID)
	}
	return s.GetPracticeSummary(threadID, userID, email)
}

// GetPracticeSummary returns a thread's recent practice logs, total count, a
// consecutive-day streak, and whether one was logged today (UTC).
func (s *Service) GetPracticeSummary(threadID, userID, email string) (model.CoachingPracticeSummary, error) {
	if !s.coachingReady() {
		return model.CoachingPracticeSummary{}, ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return model.CoachingPracticeSummary{}, err
	}
	out := model.CoachingPracticeSummary{Logs: []model.CoachingPracticeLog{}}
	if !s.practiceReady() {
		return out, nil
	}
	rows, err := s.sb.Select("coaching_practice_logs",
		"coach_student_id=eq."+store.Q(threadID)+
			"&order=created_at.desc&select=id,note,created_at,user_id&limit=200")
	if err != nil {
		return out, err
	}
	out.TotalLogs = len(rows)
	// Distinct calendar days (UTC) for the streak, newest first.
	daySeen := map[string]bool{}
	days := []string{}
	nameCache := map[string]string{}
	for i, r := range rows {
		created := asStr(r, "created_at")
		if i < 20 { // return only the most recent 20 rows
			uid := asStr(r, "user_id")
			name, ok := nameCache[uid]
			if !ok {
				name = s.coachingName(uid)
				nameCache[uid] = name
			}
			out.Logs = append(out.Logs, model.CoachingPracticeLog{
				ID:       asStr(r, "id"),
				Note:     asStr(r, "note"),
				LoggedAt: created,
				ByName:   name,
			})
		}
		if len(created) >= 10 {
			d := created[:10]
			if !daySeen[d] {
				daySeen[d] = true
				days = append(days, d)
			}
		}
	}
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	out.LoggedToday = daySeen[today]
	// Streak: consecutive days ending today or yesterday.
	if len(days) > 0 && (days[0] == today || days[0] == yesterday) {
		streak := 1
		cur, _ := time.Parse("2006-01-02", days[0])
		for i := 1; i < len(days); i++ {
			prev, perr := time.Parse("2006-01-02", days[i])
			if perr != nil {
				break
			}
			if cur.AddDate(0, 0, -1).Format("2006-01-02") == days[i] {
				streak++
				cur = prev
			} else {
				break
			}
		}
		out.CurrentStreak = streak
	}
	return out, nil
}

// DeleteProgram removes a program (coach who owns the thread).
func (s *Service) DeleteProgram(programID, userID, email string) error {
	if !s.programsReady() {
		return ErrCoachingUnavailable
	}
	row, _ := s.sb.SelectOne("coaching_programs",
		"id=eq."+store.Q(programID)+"&select=coach_student_id,title")
	if row == nil {
		return ErrNotFound
	}
	threadID := asStr(row, "coach_student_id")
	cs, role, err := s.threadMembership(threadID, userID, email)
	if err != nil || role != "coach" {
		return ErrForbidden
	}
	if derr := s.sb.Delete("coaching_programs", "id=eq."+store.Q(programID)); derr != nil {
		return derr
	}
	// The plan vanishes from the student's Goals tab — tell them, so their
	// in-progress program doesn't just silently disappear on next open.
	s.bumpThreadActivity(threadID)
	title := asStr(row, "title")
	body := s.coachLabel(userID) + " removed your training program"
	if title != "" {
		body = s.coachLabel(userID) + " removed the training program “" + title + "”"
	}
	s.notifyCoachingCounterpartLink(cs, "coach", userID, s.coachingName(cs.CoachID),
		body, "coaching:"+threadID)
	return nil
}

// --- PB Vision auto-import (highlights + stats into the coaching thread) ---

func (s *Service) pbvisionJobsReady() bool {
	return s.columnReady("coaching_pbvision_jobs", "id")
}

// AnalyzeThreadVideo submits a match video to PB Vision for a student's thread
// (coach only). The student's email is passed so PB Vision attributes/shares the
// report to them. On completion the webhook ingests highlights + stats. Returns
// the PB Vision video id.
// coachingAnalysisMonthlyCap bounds free coaching PB Vision analyses per student
// per rolling 30 days (cost guard; coaching analysis is currently platform-billed).
const coachingAnalysisMonthlyCap = 12

// upsertPBVisionJob creates (or refreshes to processing) the job row tying a PB
// Vision video (vid) to a coach thread. Manual upsert (select-then-write) so it
// never depends on the jobs unique constraint — safe before AND after the
// multi-coach migration. sourceVideoID (a coaching_videos id) links the clip for
// dedup/recheck; "" to skip.
func (s *Service) upsertPBVisionJob(vid, threadID, sourceVideoID string) error {
	set := map[string]any{"status": "processing", "updated_at": now()}
	if sourceVideoID != "" && s.columnReady("coaching_pbvision_jobs", "source_video_id") {
		set["source_video_id"] = sourceVideoID
	}
	existing, _ := s.sb.SelectOne("coaching_pbvision_jobs",
		"vid=eq."+store.Q(vid)+"&coach_student_id=eq."+store.Q(threadID)+"&select=id")
	if existing != nil {
		_, err := s.sb.Update("coaching_pbvision_jobs",
			"id=eq."+store.Q(asStr(existing, "id")), set)
		return err
	}
	set["vid"] = vid
	set["coach_student_id"] = threadID
	_, err := s.sb.Insert("coaching_pbvision_jobs", set)
	return err
}

// copyClipToThread duplicates the source coaching clip into another coach thread
// (so a shared analysis shows the video there too) and returns the new clip id
// ("" on failure or no URL). Best-effort.
func (s *Service) copyClipToThread(sourceVideoID, threadID, uploaderID, videoURL string) string {
	title := "Shared clip"
	if sourceVideoID != "" {
		if src, _ := s.sb.SelectOne("coaching_videos",
			"id=eq."+store.Q(sourceVideoID)+"&select=title,video_url"); src != nil {
			if t := strings.TrimSpace(asStr(src, "title")); t != "" {
				title = t
			}
			if u := strings.TrimSpace(asStr(src, "video_url")); u != "" {
				videoURL = u
			}
		}
	}
	if strings.TrimSpace(videoURL) == "" {
		return ""
	}
	row := map[string]any{
		"coach_student_id": threadID,
		"uploaded_by":      uploaderID,
		"uploader_role":    "student",
		"video_url":        videoURL,
		"title":            title,
	}
	if s.columnReady("coaching_videos", "source") {
		row["source"] = "upload"
	}
	ins, err := s.sb.Insert("coaching_videos", row)
	if err != nil || len(ins) == 0 {
		return ""
	}
	return asStr(ins[0], "id")
}

// shareThreadIDs are the caller's OTHER coach threads to also share this one
// analysis with (they picked "all my coaches"). PB Vision is called ONCE; each
// extra thread gets its own job row referencing the same vid + a copy of the clip.
// pbVisionLeagueGrants: leagues whose PLAYERS may run a PB Vision analysis even
// though their email isn't on the coaching beta allowlist. A deliberate one-off
// (every run bills our PB Vision key), so it is a short explicit list, capped per
// person by the caller, and overridable via PBVISION_LEAGUE_ALLOWLIST without a
// deploy — set that to a bogus value to revoke instantly.
const pbVisionLeagueGrants = "60bb75fb-3d1d-4f83-93fd-20ce7ece8bb5" // Women's Never ending league

// InPBVisionLeague reports whether this caller plays in a league that has been
// granted PB Vision analysis. Uses the same participation rule as MyLeagues, so
// a registered player, a bracket entrant and an invited member all qualify.
func (s *Service) InPBVisionLeague(userID, email string) bool {
	list := strings.TrimSpace(os.Getenv("PBVISION_LEAGUE_ALLOWLIST"))
	if list == "" {
		list = pbVisionLeagueGrants
	}
	allowed := map[string]bool{}
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = true
		}
	}
	if len(allowed) == 0 {
		return false
	}
	mine, err := s.leagueIDsForUser(userID, email)
	if err != nil {
		return false // fail CLOSED: a lookup error must not hand out billed runs
	}
	for id := range mine {
		if allowed[id] {
			return true
		}
	}
	return false
}

// lifetimeCap > 0 bounds this thread to that many analyses EVER (not per month) —
// used for the league grant, where the allowance is "one each, this once". 0
// leaves only the standing rolling-30-day cap.
func (s *Service) AnalyzeThreadVideo(threadID, userID, email, videoURL, videoID string, shareThreadIDs []string, lifetimeCap int) (string, error) {
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
	// The clip being analyzed MUST live in this (the caller's) own thread. Never
	// trust a caller-supplied videoID pointing at another user's private clip —
	// copyClipToThread would otherwise copy that clip's stored URL into the
	// caller's threads on a share (an IDOR / private-video leak).
	if videoID != "" {
		clip, _ := s.sb.SelectOne("coaching_videos",
			"id=eq."+store.Q(videoID)+"&select=coach_student_id")
		if clip == nil || asStr(clip, "coach_student_id") != threadID {
			return "", ErrForbidden
		}
	}
	// Guard against an accidental double-submit of the SAME clip: if a RECENT
	// analysis for this clip is still in flight, don't spend another PB Vision
	// run. A stale (>60-min) "processing" job is treated as dead so the student
	// can retry — the sweep below also fails those out.
	if videoID != "" && s.columnReady("coaching_pbvision_jobs", "source_video_id") {
		fresh := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339)
		if prev, _ := s.sb.SelectOne("coaching_pbvision_jobs",
			"source_video_id=eq."+store.Q(videoID)+
				"&status=eq.processing&updated_at=gte."+store.Q(fresh)+
				"&select=id&limit=1"); prev != nil {
			return "", errors.New("this clip is already being analyzed on PB Vision")
		}
	}
	// Cost cap: coaching PB Vision analysis is currently free (platform-billed), so
	// bound it per student — at most N runs per rolling 30 days on this thread — so
	// a coach-led league of many students can't run up an unbounded PB Vision bill.
	// Lifetime allowance (league grant): counts EVERY prior run on this thread, so
	// it can't be reset by waiting a month like the rolling cap below.
	if lifetimeCap > 0 && s.columnReady("coaching_pbvision_jobs", "coach_student_id") {
		if prior, _ := s.sb.Select("coaching_pbvision_jobs",
			"coach_student_id=eq."+store.Q(threadID)+
				"&select=id"); len(prior) >= lifetimeCap {
			return "", errors.New(
				"you've used your free video analysis — more coming when analysis opens up")
		}
	}
	if s.columnReady("coaching_pbvision_jobs", "coach_student_id") {
		since := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
		if prior, _ := s.sb.Select("coaching_pbvision_jobs",
			"coach_student_id=eq."+store.Q(threadID)+
				"&created_at=gte."+store.Q(since)+"&select=id"); len(prior) >= coachingAnalysisMonthlyCap {
			return "", errors.New("monthly video-analysis limit reached for this student — try again next month")
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
	// Primary job (this coach's thread). Manual upsert so it never depends on the
	// jobs unique constraint — works before AND after the multi-coach migration.
	if err := s.upsertPBVisionJob(vid, threadID, videoID); err != nil {
		return "", err
	}
	s.bumpThreadActivity(threadID)

	// Fan out to the student's OTHER selected coaches: the SAME analysis (one PB
	// Vision run, billed once) becomes viewable in each thread. Only threads where
	// the CALLER is the student. Best-effort per thread. Extra job rows share the
	// vid — the (vid, coach_student_id) unique index (multi-coach migration) lets
	// them coexist; before that migration the old vid-unique blocks them (no-op).
	seen := map[string]bool{threadID: true}
	for _, extra := range shareThreadIDs {
		extra = strings.TrimSpace(extra)
		if extra == "" || seen[extra] {
			continue
		}
		seen[extra] = true
		if _, erole, eerr := s.threadMembership(extra, userID, email); eerr != nil || erole != "student" {
			continue
		}
		// Copy the source clip into that coach's thread so they see the video too.
		clipID := s.copyClipToThread(videoID, extra, userID, videoURL)
		_ = s.upsertPBVisionJob(vid, extra, clipID)
		s.bumpThreadActivity(extra)
	}

	// Best-effort: if PB Vision already has this video processed (a duplicate
	// URL), its completion webhook won't fire again — pull the result now so the
	// analysis resolves immediately AND the ready/failed notification is sent,
	// instead of the chip hanging until the 60-min sweep. No-op (falls back to
	// the webhook) if the fetch endpoint is absent or the video's still cooking.
	go func(vid string) {
		gctx, gcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer gcancel()
		res, ok := s.PBV.GetVideo(gctx, vid)
		if !ok {
			return
		}
		// Skip if a webhook already handled it (don't double-notify).
		if j, _ := s.sb.SelectOne("coaching_pbvision_jobs",
			"vid=eq."+store.Q(vid)+"&select=status"); j == nil ||
			asStr(j, "status") != "processing" {
			return
		}
		s.handleCoachingPBVisionCallback(
			vid, res.Webpage, res.Insights, res.Stats, res.Error)
	}(vid)

	return vid, nil
}

// RecheckPBVisionJob force-polls PB Vision for a thread's in-flight analysis and
// ingests it if it's done — so a coach/student can resolve a stuck "analyzing…"
// clip on demand instead of waiting for the 60-min sweep. Returns the resulting
// status ("ready" | "failed" | "processing" | "none").
func (s *Service) RecheckPBVisionJob(threadID, userID, email, videoID string) (string, error) {
	if !s.coachingReady() || !s.pbvisionJobsReady() {
		return "", ErrCoachingUnavailable
	}
	if _, _, err := s.threadMembership(threadID, userID, email); err != nil {
		return "", err
	}
	if s.PBV == nil || !s.PBV.Configured() {
		return "", errors.New("PB Vision isn't configured yet")
	}
	// The processing job for this clip (or the thread's latest, if no clip id).
	filter := "coach_student_id=eq." + store.Q(threadID)
	if videoID != "" && s.columnReady("coaching_pbvision_jobs", "source_video_id") {
		filter = "source_video_id=eq." + store.Q(videoID)
	}
	row, _ := s.sb.SelectOne("coaching_pbvision_jobs",
		filter+"&status=eq.processing&order=updated_at.desc&limit=1&select=vid")
	if row == nil {
		return "none", nil // nothing in flight — already resolved
	}
	vid := asStr(row, "vid")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, ok := s.PBV.GetVideo(ctx, vid)
	if !ok {
		return "processing", nil // PB Vision still working (or no fetch endpoint)
	}
	s.handleCoachingPBVisionCallback(vid, res.Webpage, res.Insights, res.Stats, res.Error)
	if strings.TrimSpace(res.Error) != "" {
		return "failed", nil
	}
	return "ready", nil
}

// handleCoachingPBVisionCallback ingests a PB Vision completion for a coaching
// job — invoked from HandlePBVisionWebhook. Best-effort; a no-op when the vid
// isn't one of ours. Stores the raw payloads now; the highlight/stats PARSER is
// wired once the insights/stats field shapes are confirmed from a real payload.
func (s *Service) handleCoachingPBVisionCallback(vid, webpage string, insights, stats json.RawMessage, errReason string) {
	if !s.pbvisionJobsReady() {
		return
	}
	// ALL threads this analysis was shared to (one job row per coach). Fan the
	// result out to each — per-thread job update + highlights + notify.
	jobs, _ := s.sb.Select("coaching_pbvision_jobs",
		"vid=eq."+store.Q(vid)+"&select=id,coach_student_id,status")
	if len(jobs) == 0 {
		return
	}
	failed := strings.TrimSpace(errReason) != ""
	for _, job := range jobs {
		// Idempotency: PB Vision may REDELIVER the completion webhook. Only act on
		// a job that's still processing so redelivery never re-notifies the coach +
		// student (the paid analysis path guards this; the coaching path did not).
		if asStr(job, "status") != "processing" {
			continue
		}
		threadID := asStr(job, "coach_student_id")
		upd := map[string]any{"updated_at": now()}
		if failed {
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
		// ATOMIC claim: guard the transition on status=eq.processing so only the
		// caller that actually flips processing→ready/failed proceeds to ingest +
		// notify. A concurrent webhook / immediate-pull that lost the race updates
		// ZERO rows and skips — so redelivery AND the goroutine can't double-notify
		// (the earlier read-then-update guard alone was racy).
		claimed, _ := s.sb.Update("coaching_pbvision_jobs",
			"id=eq."+store.Q(asStr(job, "id"))+"&status=eq.processing", upd)
		if len(claimed) == 0 {
			continue
		}
		if !failed {
			s.ingestPBVisionHighlights(threadID, insights)
		}
		s.bumpThreadActivity(threadID)
		// Push + bell to BOTH the coach and the student, on success or failure.
		cs, _ := s.sb.SelectOne("coach_students", "id=eq."+store.Q(threadID))
		if cs == nil {
			continue
		}
		coachID := asStr(cs, "coach_id")
		sid := asStr(cs, "student_id")
		who := s.coachingName(sid)
		if strings.TrimSpace(who) == "" {
			who = asStr(cs, "student_name")
		}
		if strings.TrimSpace(who) == "" {
			who = "A student"
		}
		coachBody, studentBody := who+"'s PB Vision analysis is ready — review highlights",
			"Your PB Vision highlights are ready"
		if failed {
			coachBody = who + "'s PB Vision analysis failed — try a longer match clip"
			studentBody = "PB Vision couldn't analyze that clip — try a longer match video"
		}
		pbLink := "coaching:" + threadID + "?tab=pbvision"
		s.notifyUser(coachID, "coaching", "", "", coachBody, pbLink)
		if sid != "" && sid != coachID {
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
		cs.CoachNote = ""  // students never see the coach's private note about them
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
			ID:               asStr(f, "id"),
			VideoID:          asStr(f, "video_id"),
			AuthorID:         asStr(f, "author_id"),
			AuthorRole:       asStr(f, "author_role"),
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
	// Section-visibility prefs (default all-on for a coach with no settings row).
	set := s.coachSettings(cs.CoachID)
	out.ShowProgress = set.ShowProgress
	out.ShowAchievements = set.ShowAchievements
	out.ShowSkillRatings = set.ShowSkillRatings
	return out, nil
}

// coachSettingsReady reports whether the coach_settings table exists yet.
func (s *Service) coachSettingsReady() bool {
	return s.columnReady("coach_settings", "coach_id")
}

// coachSettings loads a coach's Goals-tab section prefs. A missing row or a
// pre-migration DB yields all-true, so nothing is hidden by default.
func (s *Service) coachSettings(coachID string) model.CoachSettings {
	def := model.CoachSettings{ShowProgress: true, ShowAchievements: true, ShowSkillRatings: true}
	if coachID == "" || !s.coachSettingsReady() {
		return def
	}
	row, err := s.sb.SelectOne("coach_settings", "coach_id=eq."+store.Q(coachID)+
		"&select=show_progress,show_achievements,show_skill_ratings")
	if err != nil || row == nil {
		return def
	}
	return model.CoachSettings{
		ShowProgress:     asBool(row, "show_progress"),
		ShowAchievements: asBool(row, "show_achievements"),
		ShowSkillRatings: asBool(row, "show_skill_ratings"),
	}
}

// GetCoachSettings returns the coach's own section prefs (for the settings UI).
func (s *Service) GetCoachSettings(coachID string) (model.CoachSettings, error) {
	if !s.coachingReady() {
		return model.CoachSettings{}, ErrCoachingUnavailable
	}
	return s.coachSettings(coachID), nil
}

// UpdateCoachSettings upserts a coach's Goals-tab section prefs.
func (s *Service) UpdateCoachSettings(coachID string, set model.CoachSettings) error {
	if !s.coachingReady() {
		return ErrCoachingUnavailable
	}
	if !s.coachSettingsReady() {
		return errors.New("coach settings aren't available yet")
	}
	_, err := s.sb.Upsert("coach_settings", "coach_id", map[string]any{
		"coach_id":           coachID,
		"show_progress":      set.ShowProgress,
		"show_achievements":  set.ShowAchievements,
		"show_skill_ratings": set.ShowSkillRatings,
		"updated_at":         now(),
	})
	return err
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
	// A bare object path must live under the caller's own upload folder —
	// otherwise they could attach (and get signed URLs for) someone else's
	// private clip by guessing its path.
	if err := ownCoachingPath(userID, url); err != nil {
		return model.CoachingVideo{}, err
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

// ShareVideoToLeagueCoach posts a feed video (public match-videos URL) to the
// coach LEADING the event's league, as a coaching clip on the sharer's own
// student thread. Reuses AddThreadVideo (membership check + coach notification).
// The public URL plays fine — coaching signing leaves non-coaching-bucket URLs
// as-is. Errors clearly when the league isn't coach-led or the player isn't yet
// on the coach's roster.
func (s *Service) ShareVideoToLeagueCoach(eventID, userID, email, videoURL, title string) error {
	videoURL = strings.TrimSpace(videoURL)
	if videoURL == "" {
		return errors.New("no video to share")
	}
	// Only an uploaded MATCH VIDEO may be shared (not an arbitrary URL or a photo)
	// — else it renders as a broken player on the coach's side.
	low := videoURL
	if i := strings.IndexByte(low, '?'); i >= 0 {
		low = low[:i]
	}
	low = strings.ToLower(low)
	if !strings.Contains(low, "/match-videos/") ||
		strings.HasSuffix(low, ".jpg") || strings.HasSuffix(low, ".jpeg") ||
		strings.HasSuffix(low, ".png") || strings.HasSuffix(low, ".webp") ||
		strings.HasSuffix(low, ".heic") || strings.HasSuffix(low, ".gif") {
		return errors.New("only an uploaded match video can be shared to a coach")
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=league_id")
	if err != nil || ev == nil {
		return errors.New("event not found")
	}
	leagueID := asStr(ev, "league_id")
	if leagueID == "" {
		return errors.New("this event isn't part of a league")
	}
	coach := s.leagueCoach(leagueID)
	if coach == "" {
		return errors.New("this league isn't coach-led")
	}
	threads, err := s.ListStudentThreads(userID, email)
	if err != nil {
		return err
	}
	var threadID string
	for _, t := range threads {
		if t.CoachID == coach {
			threadID = t.ID
			break
		}
	}
	if threadID == "" {
		return errors.New("you're not on the league coach's roster yet")
	}
	_, err = s.AddThreadVideo(threadID, userID, email,
		model.CoachingVideoRequest{VideoURL: videoURL, Title: title})
	return err
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
	// Carry the specific clip so the tap opens the annotated clip (with its
	// feedback list + per-comment "Jump to m:ss" buttons) — not just the newest
	// clip on the Videos tab.
	s.notifyCoachingCounterpartLink(cs, role, userID, name, notifyBody,
		"coaching:"+cs.ID+"?tab=videos&clip="+videoID)
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
			s.coachLabel(coachID)+" shared a note")
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
		s.coachLabel(coachID)+" shared a note")
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
		// Canned ready analysis so the PB Vision tab's player picker + match
		// stats render on demo data (summary/history seeded separately below).
		s.seedPBVisionJob(t, v)
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
			// Ready analysis so rolando's live login also shows the PB Vision
			// player picker + match stats (great for the two-device demo).
			s.seedPBVisionJob(rid, v1)

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
		CoachID:        asStr(row, "coach_id"),
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
		// A declined booking request is dead — keep it off the coach's schedule so
		// it isn't mistaken for a real session.
		if asStr(r, "status") == "declined" {
			continue
		}
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
		// Verify the thread belongs to THIS coach before attaching a session to
		// it — otherwise a coach could inject a fabricated session (and a spoofed
		// notification) into another coach's student thread.
		owner, _ := s.sb.SelectOne("coach_students",
			"id=eq."+store.Q(req.CoachStudentID)+
				"&select=coach_id,student_name,student_email")
		if owner == nil || asStr(owner, "coach_id") != coachID {
			return model.CoachingScheduleItem{}, ErrForbidden
		}
		row["coach_student_id"] = req.CoachStudentID
		if label == "" {
			label = asStr(owner, "student_name")
			if label == "" {
				label = asStr(owner, "student_email")
			}
		}
	}
	if label != "" {
		row["student_label"] = label
	}

	// Weekly recurrence for open/blocked windows: create this occurrence plus
	// RepeatWeeks-1 more, each 7 days later. Sessions never recur.
	weeks := req.RepeatWeeks
	if weeks < 1 {
		weeks = 1
	}
	if weeks > 26 {
		weeks = 26 // cap at ~6 months
	}
	// Parse preserving the client's OFFSET (not normalized to UTC) so every
	// weekly copy is stored in the same format as week 0 and holds the same local
	// wall-clock time. AddDate on a fixed-offset time keeps the wall-clock within
	// a DST period; without the IANA zone we can't do better than the offset.
	start, sErr := time.Parse(time.RFC3339, strings.TrimSpace(req.StartsAt))
	end, eErr := time.Parse(time.RFC3339, strings.TrimSpace(req.EndsAt))
	sOK, eOK := sErr == nil, eErr == nil
	if kind == "session" || !sOK {
		weeks = 1 // can't safely shift without a parseable start
	}

	var first map[string]any
	for k := 0; k < weeks; k++ {
		if sOK {
			// Normalize every occurrence (including k==0) to one consistent format.
			row["starts_at"] = start.AddDate(0, 0, 7*k).Format(time.RFC3339)
			if eOK {
				row["ends_at"] = end.AddDate(0, 0, 7*k).Format(time.RFC3339)
			}
		}
		ins, err := s.sb.Insert("coaching_schedule", row)
		if err != nil {
			if k == 0 {
				return model.CoachingScheduleItem{}, err
			}
			break // keep the ones already made
		}
		if len(ins) == 0 {
			if k == 0 {
				return model.CoachingScheduleItem{}, errors.New("could not save that")
			}
			break
		}
		if first == nil {
			first = ins[0]
		}
	}
	// Tell the student a session was scheduled for them (mirrors reschedule/cancel
	// which already notify). Without this the coach-booked session only surfaces
	// passively in the student's My Sessions list.
	if kind == "session" && strings.TrimSpace(req.CoachStudentID) != "" {
		s.notifyStudentOfThread(req.CoachStudentID, coachID, s.coachingName(coachID),
			s.coachLabel(coachID)+" scheduled a 1:1 session for you",
			"coaching:"+req.CoachStudentID)
	}
	return mapScheduleItem(first), nil
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
			s.coachingName(coachID), s.coachLabel(coachID)+" cancelled your 1:1 session",
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
		"id=eq."+store.Q(sessionID)+"&select=coach_id,kind,coach_student_id")
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return ErrForbidden
	}
	if asStr(row, "kind") != "session" {
		return errors.New("only a booked session has attendance")
	}
	if _, err := s.sb.Update("coaching_schedule", "id=eq."+store.Q(sessionID),
		map[string]any{"status": orNull(status)}); err != nil {
		return err
	}
	// Marking a session attended → nudge the student to book their next one
	// (turns captured attendance into a rebooking loop).
	if status == "attended" {
		if threadID := asStr(row, "coach_student_id"); threadID != "" {
			s.notifyStudentOfThread(threadID, coachID, s.coachingName(coachID),
				"Great session! Ready for the next one? Book from your coach's page",
				"coaching:"+threadID)
		}
	}
	return nil
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
			// Only allow attaching to a thread this coach owns.
			owner, _ := s.sb.SelectOne("coach_students",
				"id=eq."+store.Q(id)+"&select=coach_id,student_name,student_email")
			if owner == nil || asStr(owner, "coach_id") != coachID {
				return model.CoachingScheduleItem{}, ErrForbidden
			}
			upd["coach_student_id"] = id
			if label == "" {
				label = asStr(owner, "student_name")
				if label == "" {
					label = asStr(owner, "student_email")
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
			s.coachLabel(coachID)+" updated your 1:1 session time", "coaching:"+threadID)
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
		"or=(coach_id.eq."+store.Q(coachID)+",is_starter.is.true)&order=is_starter.asc,created_at.desc")
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
		// Only the coach's OWN drills or shared starters — never another coach's
		// private drill (scoping matches ListDrills). If it's neither, don't copy
		// its content and don't link the foreign id.
		d, _ := s.sb.SelectOne("coaching_drills",
			"id=eq."+store.Q(drillID)+
				"&or=(coach_id.eq."+store.Q(coachID)+",is_starter.is.true)")
		if d != nil {
			if title == "" {
				title = asStr(d, "title")
			}
			if skill == "" {
				skill = asStr(d, "skill_category")
			}
			if goal == "" {
				goal = asStr(d, "goal")
			}
		} else {
			drillID = ""
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
		who := s.coachingName(userID)
		title := asStr(row, "title")
		var body string
		if role == "coach" {
			body = s.coachLabel(userID) + " marked a drill complete: " + title
		} else {
			if strings.TrimSpace(who) == "" {
				who = "Your student"
			}
			body = who + " completed: " + title
		}
		s.notifyCoachingCounterpart(cs, role, userID, who, body)
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
			s.coachLabel(cs.CoachID)+" updated your skill assessment")
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
		s.coachLabel(cs.CoachID)+" rated your progress")
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
		BusinessName:    asStr(row, "business_name"),
		Address:         asStr(row, "address"),
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

// ownCoachingPath validates a caller-supplied coaching-videos OBJECT PATH before
// we store it. The bucket is private and the backend signs whatever path it has
// with the service key, so an unvalidated path lets a caller point at ANOTHER
// user's private clip and receive a signed URL for it. Uploads always land under
// "<uid>/…", so require that prefix and reject traversal. Full http(s) URLs are
// left alone (public match-video / shared links go through other checks).
func ownCoachingPath(userID, v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("no video uploaded")
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		// A full URL is fine ONLY if it isn't secretly a coaching-videos object
		// path — otherwise the caller could smuggle another user's private clip
		// past this check and have the backend sign it for them.
		low := strings.ToLower(v)
		if i := strings.Index(low, "/coaching-videos/"); i >= 0 {
			rest := v[i+len("/coaching-videos/"):]
			if strings.Contains(rest, "..") || !strings.HasPrefix(rest, userID+"/") {
				return ErrForbidden
			}
		}
		return nil
	}
	if strings.Contains(v, "..") || !strings.HasPrefix(v, userID+"/") {
		return ErrForbidden
	}
	return nil
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
	if err := ownCoachingPath(userID, path); err != nil {
		return err
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
	if s.columnReady("coach_profiles", "business_name") {
		row["business_name"] = orNull(strings.TrimSpace(req.BusinessName))
		row["address"] = orNull(strings.TrimSpace(req.Address))
	}
	// Geocode the most specific location we have — a street address pins the
	// coach far more precisely than a city; fall back to city.
	geoQuery := strings.TrimSpace(req.Address)
	if geoQuery == "" {
		geoQuery = strings.TrimSpace(req.City)
	}
	if geoQuery != "" {
		if lat, lng := bestEffortGeocode(geoQuery); lat != nil && lng != nil {
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
		ID:            asStr(row, "id"),
		CoachUserID:   asStr(row, "coach_user_id"),
		AuthorID:      asStr(row, "author_id"),
		AuthorName:    asStr(row, "author_name"),
		Rating:        asInt(row, "rating"),
		Body:          asStr(row, "body"),
		CoachResponse: asStr(row, "coach_response"),
		CreatedAt:     asStr(row, "created_at"),
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
	if err != nil {
		return err
	}
	// Tell the coach — a review (esp. an edited-down one) otherwise silently moves
	// their public rating with no signal. Deep-links to their reviews inbox.
	s.notifyUser(coachUserID, "coaching", authorID, authorName,
		authorName+" left you a "+strconv.Itoa(req.Rating)+"-star review", "coachreviews")
	return nil
}

// RespondToCoachReview lets the reviewed coach post a public reply to a review of
// them (one response per review; empty clears it).
func (s *Service) RespondToCoachReview(coachUserID, reviewID, response string) error {
	if !s.coachReviewsReady() {
		return ErrCoachingUnavailable
	}
	if !s.columnReady("coach_reviews", "coach_response") {
		return ErrCoachingUnavailable
	}
	row, _ := s.sb.SelectOne("coach_reviews",
		"id=eq."+store.Q(reviewID)+"&select=coach_user_id,author_id")
	if row == nil {
		return ErrNotFound
	}
	if asStr(row, "coach_user_id") != coachUserID {
		return ErrForbidden // only the reviewed coach may reply
	}
	response = strings.TrimSpace(response)
	if r := []rune(response); len(r) > 1000 {
		response = string(r[:1000])
	}
	if _, err := s.sb.Update("coach_reviews", "id=eq."+store.Q(reviewID),
		map[string]any{"coach_response": orNull(response), "updated_at": now()}); err != nil {
		return err
	}
	// Let the reviewer know the coach replied.
	if response != "" {
		if aid := asStr(row, "author_id"); aid != "" && aid != coachUserID {
			s.notifyUser(aid, "coaching", coachUserID, s.coachingName(coachUserID),
				s.coachingName(coachUserID)+" responded to your review", "")
		}
	}
	return nil
}

// ListCoachReviewsForOwner returns all reviews of the calling coach (their inbox),
// newest first — distinct from the public ListCoachReviews (which also computes
// the aggregate + the viewer's own review).
func (s *Service) ListCoachReviewsForOwner(coachUserID string) ([]model.CoachReview, error) {
	if !s.coachReviewsReady() {
		return []model.CoachReview{}, nil
	}
	rows, err := s.sb.Select("coach_reviews",
		"coach_user_id=eq."+store.Q(coachUserID)+"&order=updated_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachReview, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapCoachReview(r))
	}
	return out, nil
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
		"coach_id=eq."+store.Q(coachUserID)+"&select=kind,starts_at,ends_at,status")
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
			// A declined request no longer holds its slot.
			if asStr(r, "status") == "declined" {
				continue
			}
			// Overlap if start < existing end AND end > existing start. A still-
			// pending request also holds the slot, so two players can't grab it.
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
	// The player's agenda becomes the session notes (so the coach sees the goal);
	// fall back to a generic label when they didn't say.
	notes := strings.TrimSpace(req.WhatToWorkOn)
	if notes == "" {
		notes = "Booked by player"
	}
	// A player-initiated booking is a REQUEST: it starts 'pending' and the coach
	// must approve it (see RespondToBooking). Where the status column hasn't been
	// added yet, degrade gracefully to the old auto-confirmed behaviour.
	approvalReady := s.columnReady("coaching_schedule", "status")
	row := map[string]any{
		"coach_id":  coachUserID,
		"kind":      "session",
		"starts_at": start.Format(time.RFC3339),
		"ends_at":   end.Format(time.RFC3339),
		"all_day":   false,
		"location":  orNull(strings.TrimSpace(req.Location)),
		"notes":     notes,
	}
	if approvalReady {
		row["status"] = "pending"
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

	// Notify the coach (include the agenda so they can prep). When approval is on,
	// the copy + deep-link point them at the request to approve or decline.
	when := s.fmtSessionWhen(start)
	var bookMsg string
	if approvalReady {
		bookMsg = label + " requested a 1:1 session for " + when + " — approve or decline"
	} else {
		bookMsg = label + " booked a 1:1 session for " + when
	}
	if notes != "Booked by player" {
		bookMsg += " (wants to work on: " + notes + ")"
	}
	s.notifyUser(coachUserID, "coaching", playerID, label,
		bookMsg, "coachschedule")

	return mapScheduleItem(ins[0]), nil
}

// fmtSessionWhen renders a session start for notification copy, e.g.
// "Mon Aug 4, 3:30 PM". Times are shown in the server's local zone — good enough
// for a nudge; the app renders exact local times on the card.
func (s *Service) fmtSessionWhen(t time.Time) string {
	return t.Local().Format("Mon Jan 2, 3:04 PM")
}

// RespondToBooking lets a coach approve or decline a pending 1:1 booking request.
// Only the owning coach may act, only on a session that's still 'pending'. On
// approve the slot becomes 'confirmed'; on decline it becomes 'declined' (which
// frees the slot for others). The student is notified either way.
func (s *Service) RespondToBooking(coachID, sessionID string, approve bool) (model.CoachingScheduleItem, error) {
	if !s.scheduleReady() || !s.columnReady("coaching_schedule", "status") {
		return model.CoachingScheduleItem{}, ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_schedule",
		"id=eq."+store.Q(sessionID)+
			"&select=coach_id,kind,status,coach_student_id,starts_at,ends_at,student_label")
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}
	if row == nil {
		return model.CoachingScheduleItem{}, ErrNotFound
	}
	if asStr(row, "coach_id") != coachID {
		return model.CoachingScheduleItem{}, ErrForbidden
	}
	if asStr(row, "kind") != "session" {
		return model.CoachingScheduleItem{}, errors.New("that isn't a booking request")
	}
	if st := asStr(row, "status"); st != "pending" {
		// Idempotent-friendly: a session already handled just reports its state.
		return model.CoachingScheduleItem{}, errors.New("this request has already been " + respondedLabel(st))
	}

	// Approving commits the slot: re-check it doesn't collide with a session the
	// coach committed AFTER this request came in (a manually-added session or a
	// reschedule). Only committed sessions block — other pending requests and
	// declined rows don't.
	if approve {
		if start, ok := parseSchedTime(asStr(row, "starts_at")); ok {
			end, ok2 := parseSchedTime(asStr(row, "ends_at"))
			if !ok2 {
				end = start.Add(time.Hour)
			}
			others, oerr := s.sb.Select("coaching_schedule",
				"coach_id=eq."+store.Q(coachID)+"&kind=eq.session&id=neq."+
					store.Q(sessionID)+"&select=starts_at,ends_at,status")
			if oerr == nil {
				for _, o := range others {
					switch asStr(o, "status") {
					case "pending", "declined":
						continue // not committed — doesn't hold the slot
					}
					os, ok := parseSchedTime(asStr(o, "starts_at"))
					if !ok {
						continue
					}
					oe, ok := parseSchedTime(asStr(o, "ends_at"))
					if !ok {
						oe = os.Add(time.Hour)
					}
					if start.Before(oe) && end.After(os) {
						return model.CoachingScheduleItem{}, errors.New(
							"that time now overlaps another booked session — move the other booking first")
					}
				}
			}
		}
	}

	newStatus := "declined"
	if approve {
		newStatus = "confirmed"
	}
	out, err := s.sb.Update("coaching_schedule", "id=eq."+store.Q(sessionID),
		map[string]any{"status": newStatus})
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}

	// Tell the student. Land them on "My classes", where their sessions live.
	threadID := asStr(row, "coach_student_id")
	coachName := s.coachingName(coachID)
	when := ""
	if st, ok := parseSchedTime(asStr(row, "starts_at")); ok {
		when = s.fmtSessionWhen(st)
	}
	var msg string
	if approve {
		msg = coachName + " confirmed your session"
		if when != "" {
			msg += " for " + when
		}
	} else {
		msg = coachName + " couldn't take your session request"
		if when != "" {
			msg += " for " + when
		}
		msg += " — pick another open time"
	}
	if threadID != "" {
		s.notifyStudentOfThread(threadID, coachID, coachName, msg, "myclasses")
	}

	if len(out) > 0 {
		return mapScheduleItem(out[0]), nil
	}
	return mapScheduleItem(row), nil
}

// respondedLabel maps a non-pending status to friendly past-tense text.
func respondedLabel(status string) string {
	switch status {
	case "confirmed", "attended", "no_show":
		return "approved"
	case "declined":
		return "declined"
	default:
		return "handled"
	}
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
		// A declined request is dead — the student was already told; don't keep
		// showing it on their upcoming list. 'pending' and 'confirmed' stay so the
		// card can show its approval state.
		if it.Status == "declined" {
			continue
		}
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

// RescheduleMySession moves a player's booked 1:1 to a new time (their thread
// only). Validates the new slot fits one of the coach's open windows and doesn't
// collide with another session (the session being moved is excluded), and honors
// the coach's cancellation cutoff on the ORIGINAL time. Notifies the coach.
func (s *Service) RescheduleMySession(playerID, email, sessionID string, req model.CoachBookingRequest) (model.CoachingScheduleItem, error) {
	if !s.scheduleReady() {
		return model.CoachingScheduleItem{}, ErrCoachingUnavailable
	}
	row, err := s.sb.SelectOne("coaching_schedule",
		"id=eq."+store.Q(sessionID)+
			"&select=coach_id,coach_student_id,starts_at,kind")
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}
	if row == nil {
		return model.CoachingScheduleItem{}, ErrNotFound
	}
	if asStr(row, "kind") != "session" {
		return model.CoachingScheduleItem{}, errors.New("only a booked session can be rescheduled")
	}
	threadID := asStr(row, "coach_student_id")
	coachID := asStr(row, "coach_id")
	if threadID == "" || coachID == "" {
		return model.CoachingScheduleItem{}, ErrForbidden
	}
	// Verify the thread belongs to this player.
	if _, role, err := s.threadMembership(threadID, playerID, email); err != nil || role != "student" {
		return model.CoachingScheduleItem{}, ErrForbidden
	}
	// Moving off the original slot is governed by the same cutoff as cancelling.
	if err := s.enforceCancelCutoff(coachID, asStr(row, "starts_at")); err != nil {
		return model.CoachingScheduleItem{}, err
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

	// Confirm the new slot fits an open window and doesn't overlap another
	// session — excluding the one being moved.
	rows, err := s.sb.Select("coaching_schedule",
		"coach_id=eq."+store.Q(coachID)+"&select=id,kind,starts_at,ends_at")
	if err != nil {
		return model.CoachingScheduleItem{}, err
	}
	inWindow := false
	for _, r := range rows {
		if asStr(r, "id") == sessionID {
			continue // don't collide with ourselves
		}
		kind := asStr(r, "kind")
		ws, ok1 := parseSchedTime(asStr(r, "starts_at"))
		we, ok2 := parseSchedTime(asStr(r, "ends_at"))
		switch kind {
		case "open":
			if ok1 && ok2 && !start.Before(ws) && !end.After(we) {
				inWindow = true
			}
		case "session":
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

	upd := map[string]any{
		"starts_at": start.Format(time.RFC3339),
		"ends_at":   end.Format(time.RFC3339),
	}
	// Let a new reminder fire for the new time.
	if s.columnReady("coaching_schedule", "reminded_at") {
		upd["reminded_at"] = nil
	}
	out, err := s.sb.Update("coaching_schedule", "id=eq."+store.Q(sessionID), upd)
	if err != nil || len(out) == 0 {
		if err == nil {
			err = errors.New("could not reschedule the session")
		}
		return model.CoachingScheduleItem{}, err
	}
	s.bumpThreadActivity(threadID)
	who := s.coachingName(playerID)
	if who == "" {
		who = "Your student"
	}
	s.notifyUser(coachID, "coaching", playerID, who,
		who+" rescheduled their 1:1 session", "coaching:"+threadID)
	return mapScheduleItem(out[0]), nil
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
		return "", s.grantPackCredits(meta, "") // free pack: direct call, no redelivery
	}
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	return gw.CreatePlatformCheckout(meta, "pack_purchase", price, "usd",
		"PlanMyPickle — "+asStr(pack, "title"), email, successURL, cancelURL)
}

// grantPackCredits credits a player's balance from a "coachId:userId:credits"
// metadata string. dedupKey (the Stripe PaymentIntent id on the paid webhook path)
// makes it idempotent against Stripe's at-least-once redelivery: the grant is
// recorded in coaching_credit_grants keyed on dedupKey, and a replay is a no-op.
// Pass "" for the free-pack path (a direct call, never redelivered).
func (s *Service) grantPackCredits(meta, dedupKey string) error {
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
	// Idempotency: a coaching_credit_grants row keyed on dedupKey (the Stripe
	// PaymentIntent) records this grant. applied_at is set ONLY after the credit
	// add succeeds, so the claim marks completed WORK, not mere intent:
	//   - existing row, applied_at set   → true replay, skip.
	//   - existing row, applied_at unset → a prior attempt crashed after claiming
	//     but before granting → COMPLETE it now (don't skip).
	//   - no row, insert fails to persist → return the error so Stripe retries
	//     cleanly through the claim path (never grant unclaimed → no double-grant).
	tracked := dedupKey != "" && s.columnReady("coaching_credit_grants", "grant_key")
	if tracked {
		if e, _ := s.sb.SelectOne("coaching_credit_grants",
			"grant_key=eq."+store.Q(dedupKey)+"&select=id,applied_at"); e != nil {
			if asStr(e, "applied_at") != "" {
				return nil // already granted
			}
			// claimed-but-unapplied → fall through and complete the grant
		} else {
			if _, ierr := s.sb.Insert("coaching_credit_grants", map[string]any{
				"grant_key": dedupKey,
				"coach_id":  coachID,
				"user_id":   userID,
				"credits":   credits,
			}); ierr != nil {
				// A racing delivery may have inserted it; re-check.
				e2, _ := s.sb.SelectOne("coaching_credit_grants",
					"grant_key=eq."+store.Q(dedupKey)+"&select=id,applied_at")
				if e2 == nil {
					return ierr // truly not written → fail so Stripe retries cleanly
				}
				if asStr(e2, "applied_at") != "" {
					return nil // the racer already applied it
				}
				// racer claimed but not yet applied → complete the grant
			}
		}
	}
	cur, _ := s.MyCoachCredits(coachID, userID)
	_, err := s.sb.Upsert("coaching_credits", "coach_id,user_id", map[string]any{
		"coach_id":          coachID,
		"user_id":           userID,
		"credits_remaining": cur + credits,
		"updated_at":        now(),
	})
	if err != nil {
		return err // NOT applied → a redelivery finds the unapplied row and completes it
	}
	// Mark the grant applied so a future redelivery is a true no-op.
	if tracked {
		_, _ = s.sb.Update("coaching_credit_grants",
			"grant_key=eq."+store.Q(dedupKey),
			map[string]any{"applied_at": now()})
	}
	// Surface the purchase to BOTH sides — it's a real revenue/obligation event
	// that was previously completely silent (no bell, no balance signal).
	buyerName := s.coachingName(userID)
	if strings.TrimSpace(buyerName) == "" {
		buyerName = "A player"
	}
	// Buyer: confirm the credits landed (Stripe redirect may beat the webhook).
	s.notifyUser(userID, "coaching", coachID, s.coachingName(coachID),
		strconv.Itoa(credits)+" class credits were added to your balance", "myclasses")
	// Coach: they now owe N prepaid sessions.
	if coachID != userID {
		s.notifyUser(coachID, "coaching", userID, buyerName,
			buyerName+" purchased your "+strconv.Itoa(credits)+"-class pack", "coachcredits")
	}
	return nil
}

// consumeCredit spends one credit if the player has any. Prefers the atomic
// spend_coach_credit RPC (no lost update under concurrency); falls back to the
// read-then-write when the function isn't migrated yet. Returns true when spent.
func (s *Service) consumeCredit(coachID, userID string) bool {
	if !s.creditsReady() || coachID == "" || userID == "" {
		return false
	}
	if body, err := s.sb.RPC("spend_coach_credit",
		map[string]any{"p_coach": coachID, "p_user": userID}); err == nil {
		return strings.TrimSpace(string(body)) == "true"
	}
	// Fallback (RPC not present): non-atomic read-then-write.
	cur, _ := s.MyCoachCredits(coachID, userID)
	if cur <= 0 {
		return false
	}
	_, err := s.sb.Update("coaching_credits",
		"coach_id=eq."+store.Q(coachID)+"&user_id=eq."+store.Q(userID),
		map[string]any{"credits_remaining": cur - 1, "updated_at": now()})
	return err == nil
}

// restoreCredit returns one spent class credit to the player. Prefers the atomic
// restore_coach_credit RPC (no lost update when two seats of the same player
// settle at once); falls back to the read-then-write when it isn't migrated yet.
func (s *Service) restoreCredit(coachID, userID string) bool {
	if !s.creditsReady() || coachID == "" || userID == "" {
		return false
	}
	if _, err := s.sb.RPC("restore_coach_credit",
		map[string]any{"p_coach": coachID, "p_user": userID}); err == nil {
		return true
	}
	// Fallback (RPC not present): non-atomic read-then-write.
	cur, _ := s.MyCoachCredits(coachID, userID)
	_, err := s.sb.Upsert("coaching_credits", "coach_id,user_id", map[string]any{
		"coach_id":          coachID,
		"user_id":           userID,
		"credits_remaining": cur + 1,
		"updated_at":        now(),
	})
	return err == nil
}

// alreadyRefundedErr reports whether a Stripe refund error means the charge is
// ALREADY fully refunded — which we treat as success (the money is already back),
// so a retry loop converges instead of re-erroring forever.
func alreadyRefundedErr(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	// Prefer Stripe's STABLE machine code (present in the marshaled error), with
	// the human message wording as a fallback (Stripe owns/may reword the message).
	return strings.Contains(m, "charge_already_refunded") ||
		strings.Contains(m, "already refunded") ||
		strings.Contains(m, "already been refunded") ||
		strings.Contains(m, "no amount available")
}

// settleEnrollmentRefund returns the money for a torn-down PAID seat and OWNS the
// payment_ref lifecycle so a refund/restore happens AT MOST ONCE even under
// concurrent sweeps/retries. Returns (msg, failed): msg is the student-facing
// line (empty when nothing was owed); failed is true when money was owed but the
// settlement couldn't complete (payment_ref left live for a later retry).
//   - credit: compare-and-set payment_ref 'credit'→'refunded:credit' (only the
//     CAS winner restores, since restoreCredit isn't idempotent); revert on error.
//   - pi_: Stripe Refund is idempotent (an already-refunded PI is treated as
//     success), so we can safely retry and then mark refunded.
func (s *Service) settleEnrollmentRefund(enrollmentID, coachID, userID, paymentRef string, paid bool) (string, bool) {
	switch {
	case paymentRef == "credit":
		// Claim the settlement atomically: flip credit→refunded:credit only if it
		// is STILL 'credit'. Lose the race (0 rows) → someone else owns it.
		claimed, err := s.sb.Update("coaching_enrollments",
			"id=eq."+store.Q(enrollmentID)+"&payment_ref=eq.credit",
			map[string]any{"payment_ref": refundedRef("credit")})
		if err != nil || len(claimed) == 0 {
			return "", err != nil // 0 rows = already settled/claimed (not a failure)
		}
		if !s.restoreCredit(coachID, userID) {
			// Restore failed — revert the claim so a retry can re-attempt.
			if _, rerr := s.sb.Update("coaching_enrollments",
				"id=eq."+store.Q(enrollmentID)+"&payment_ref=eq."+store.Q(refundedRef("credit")),
				map[string]any{"payment_ref": "credit"}); rerr != nil {
				// Revert also failed → the row is stuck at refunded:credit with the
				// credit NOT restored. Log loudly so ops can hand-restore it.
				log.Printf("coaching: STRANDED CREDIT (revert failed) enrollment=%s coach=%s user=%s: %v",
					enrollmentID, coachID, userID, rerr)
			}
			return "", true
		}
		return "Your class credit was returned.", false
	case paid && strings.HasPrefix(paymentRef, "pi_"):
		gw, ok := s.stripeGW()
		if !ok {
			return "", true // a Stripe seat is owed a refund but the gateway is down
		}
		if err := gw.Refund(paymentRef); err != nil && !alreadyRefundedErr(err) {
			log.Printf("coaching: refund FAILED coach=%s user=%s pi=%s: %v",
				coachID, userID, paymentRef, err)
			return "", true
		}
		// Success (or already-refunded) → mark refunded. Idempotent: safe to repeat.
		_, _ = s.sb.Update("coaching_enrollments", "id=eq."+store.Q(enrollmentID),
			map[string]any{"payment_ref": refundedRef(paymentRef)})
		return "A refund was issued to your original payment.", false
	case paid && paymentRef == "manual":
		// Paid in cash off-platform — we can't auto-refund; tell them to expect
		// it back directly. No coach name here: this string is appended after a
		// body that already names the coach ("Coach Kay removed you from … — …").
		return "They'll return your payment directly.", false
	}
	return "", false // nothing was owed
}

// refundedRef marks a payment_ref as settled so a repeat teardown can't refund
// twice, while preserving what it was for the record.
func refundedRef(paymentRef string) string {
	if paymentRef == "" {
		return ""
	}
	if strings.HasPrefix(paymentRef, "refunded:") {
		return paymentRef
	}
	return "refunded:" + paymentRef
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
		Lat:         asFloatPtr(row, "lat"),
		Lng:         asFloatPtr(row, "lng"),
		Level:       asStr(row, "level"),
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
	// Return ALL of the coach's classes (upcoming + past), soonest first — a
	// class shouldn't vanish from the coach's management view the moment it ends;
	// the client groups them into Upcoming / Past.
	rows, err := s.sb.Select("coaching_classes",
		"coach_id=eq."+store.Q(coachID)+"&order=starts_at.asc")
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

func normalizeClassLevel(l string) string {
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "beginner", "intermediate", "advanced":
		return strings.ToLower(strings.TrimSpace(l))
	default:
		return ""
	}
}

// applyClassGeo stamps lat/lng onto a class row: an explicit map-picker pin
// wins; otherwise best-effort geocode the typed location. No-op if the columns
// aren't live or nothing resolves (the class just won't show on the map).
func (s *Service) applyClassGeo(row map[string]any, req model.CoachingClassRequest) {
	if !s.columnReady("coaching_classes", "lat") {
		return
	}
	if req.Lat != nil && req.Lng != nil {
		row["lat"] = *req.Lat
		row["lng"] = *req.Lng
		return
	}
	if lat, lng := bestEffortGeocode(strings.TrimSpace(req.Location)); lat != nil && lng != nil {
		row["lat"] = *lat
		row["lng"] = *lng
	}
}

// ListClassesNearby returns UPCOMING classes across all coaches that carry
// coordinates, ranked by distance from (lat,lng). radiusKm<=0 means no cap.
func (s *Service) ListClassesNearby(lat, lng, radiusKm float64, viewerID string) ([]model.CoachingClass, error) {
	if !s.classesReady() || !s.columnReady("coaching_classes", "lat") {
		return []model.CoachingClass{}, nil
	}
	rows, err := s.sb.Select("coaching_classes",
		"starts_at=gte."+store.Q(now())+"&lat=not.is.null&order=starts_at.asc")
	if err != nil {
		return nil, err
	}
	nameCache := map[string]string{}
	out := make([]model.CoachingClass, 0, len(rows))
	for _, r := range rows {
		c := mapClass(r)
		if c.Lat == nil || c.Lng == nil {
			continue
		}
		d := haversineKm(lat, lng, *c.Lat, *c.Lng)
		if radiusKm > 0 && d > radiusKm {
			continue
		}
		c.DistanceKm = &d
		n, ok := nameCache[c.CoachID]
		if !ok {
			n = s.coachingName(c.CoachID)
			nameCache[c.CoachID] = n
		}
		c.CoachName = n
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		const far = 1e308 // classes without a distance sort last
		di, dj := far, far
		if out[i].DistanceKm != nil {
			di = *out[i].DistanceKm
		}
		if out[j].DistanceKm != nil {
			dj = *out[j].DistanceKm
		}
		return di < dj
	})
	s.enrichClasses(out, viewerID)
	return out, nil
}

// PublicClassByID returns one class (with coach name + counts) for the crawlable
// SEO page — no auth, since a class listing is public. Errors if not found.
func (s *Service) PublicClassByID(id string) (model.CoachingClass, error) {
	if !s.classesReady() {
		return model.CoachingClass{}, ErrNotFound
	}
	row, err := s.sb.SelectOne("coaching_classes", "id=eq."+store.Q(id))
	if err != nil {
		return model.CoachingClass{}, err
	}
	if row == nil {
		return model.CoachingClass{}, ErrNotFound
	}
	c := mapClass(row)
	c.CoachName = s.coachingName(c.CoachID)
	list := []model.CoachingClass{c}
	s.enrichClasses(list, "")
	return list[0], nil
}

// PublicUpcomingClasses lists upcoming classes across all coaches (for the SEO
// sitemap). Cheap projection; capped by limit.
func (s *Service) PublicUpcomingClasses(limit int) ([]model.CoachingClass, error) {
	if !s.classesReady() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.sb.Select("coaching_classes",
		"starts_at=gte."+store.Q(now())+"&order=starts_at.asc&limit="+strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	out := make([]model.CoachingClass, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapClass(r))
	}
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
	if s.columnReady("coaching_classes", "level") {
		classRow["level"] = orNull(normalizeClassLevel(req.Level))
	}
	s.applyClassGeo(classRow, req)
	ins, err := s.sb.Insert("coaching_classes", classRow)
	if err != nil {
		return model.CoachingClass{}, err
	}
	if len(ins) == 0 {
		return model.CoachingClass{}, errors.New("could not save that class")
	}
	c := mapClass(ins[0])
	// Recurring v1: clone this class weekly for the next weeks as independent
	// classes (each with its own roster/enrollment). A full enroll-once-pay-once
	// course cohort is a larger follow-up; this captures most of the value and
	// fills the coach's date chip row.
	if weeks := req.RepeatWeeks; weeks > 1 {
		if weeks > 12 {
			weeks = 12
		}
		if bt, ok := parseTime(req.StartsAt); ok {
			be, haveEnd := time.Time{}, false
			if e := strings.TrimSpace(req.EndsAt); e != "" {
				if et, ok2 := parseTime(e); ok2 {
					be, haveEnd = et, true
				}
			}
			for w := 1; w < weeks; w++ {
				clone := make(map[string]any, len(classRow))
				for k, v := range classRow {
					clone[k] = v
				}
				clone["starts_at"] = bt.AddDate(0, 0, 7*w).Format(time.RFC3339)
				if haveEnd {
					clone["ends_at"] = be.AddDate(0, 0, 7*w).Format(time.RFC3339)
				}
				if _, err := s.sb.Insert("coaching_classes", clone); err != nil {
					break // base class is saved; stop cloning on first failure
				}
			}
		}
	}
	go s.notifyCoachFollowersOfClass(coachID, c.Title) // players who saved this coach
	return c, nil
}

// notifyCoachFollowersOfClass tells players who saved (follow) this coach that a
// new class was posted — turns one-time discovery into a recurring funnel.
func (s *Service) notifyCoachFollowersOfClass(coachID, title string) {
	if !s.favoritesReady() || coachID == "" {
		return
	}
	rows, _ := s.sb.Select("coach_favorites",
		"coach_user_id=eq."+store.Q(coachID)+"&select=user_id&limit=500")
	name := s.coachingName(coachID)
	for _, r := range rows {
		uid := asStr(r, "user_id")
		if uid == "" || uid == coachID {
			continue
		}
		s.notifyUser(uid, "coaching", coachID, name,
			name+" posted a new class: “"+title+"”", "")
	}
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
	if s.columnReady("coaching_classes", "level") {
		upd["level"] = orNull(normalizeClassLevel(req.Level))
	}
	// Re-pin only when the coach moved the pin or changed the location text, so
	// an unrelated edit doesn't spend a geocode call or clobber a good pin.
	if req.Lat != nil || strings.TrimSpace(req.Location) != asStr(cur, "location") {
		s.applyClassGeo(upd, req)
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
		// Deep-link to My classes so a "class updated/cancelled" tap lands somewhere
		// (the class lives there, not in a thread).
		s.notifyUser(uid, "coaching", actorID, actorName, body, "myclasses")
	}
}

// settleClassEnrollments refunds/returns every prepaid seat in a class before a
// delete (whose FK cascade would otherwise wipe the rows and strand the money).
// It settles both ACTIVE seats and PREVIOUSLY-CANCELED seats whose earlier refund
// failed (they still carry a live pi_/credit ref) — the latter would otherwise be
// cascade-deleted with no way for SweepFailedRefunds to ever recover them. Returns
// the count of seats that still couldn't be settled; the caller must NOT delete
// while that is > 0. settleEnrollmentRefund owns payment_ref; here we additionally
// flip a successfully-settled ACTIVE seat to canceled so an aborted delete doesn't
// leave a refunded-but-enrolled seat consuming capacity.
func (s *Service) settleClassEnrollments(classID, coachID string) int {
	if !s.enrollmentsReady() {
		return 0
	}
	rows, rerr := s.sb.SelectAll("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&status=in.(enrolled,offered,canceled)"+
			"&select=id,user_id,status,paid,payment_ref")
	if rerr != nil {
		return 1 // couldn't read the roster → treat as unsettled so the delete aborts
	}
	failed := 0
	for _, r := range rows {
		pref := asStr(r, "payment_ref")
		// Skip rows with nothing to settle so we don't touch already-clean seats.
		if pref != "credit" && !strings.HasPrefix(pref, "pi_") {
			continue
		}
		uid := asStr(r, "user_id")
		id := asStr(r, "id")
		msg, didFail := s.settleEnrollmentRefund(id, coachID, uid, pref, asBool(r, "paid"))
		if didFail {
			failed++ // leave the live payment_ref intact for a retry
			continue
		}
		if msg == "" {
			continue
		}
		// Free the seat so an aborted/partial delete doesn't leave phantom capacity.
		if st := asStr(r, "status"); st == "enrolled" || st == "offered" {
			_, _ = s.sb.Update("coaching_enrollments", "id=eq."+store.Q(id),
				map[string]any{"status": "canceled"})
		}
		if uid != "" && uid != coachID {
			s.notifyUser(uid, "coaching", coachID, s.coachingName(coachID), msg,
				"myclasses")
		}
	}
	return failed
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
	// Return everyone's money BEFORE the delete cascades the enrollment rows away.
	// If any refund/credit-restore FAILED, ABORT — deleting now would cascade the
	// row (and its live PaymentIntent handle) away, permanently stranding the money.
	// The coach can retry once Stripe recovers (already-refunded seats are no-ops).
	if failed := s.settleClassEnrollments(id, coachID); failed > 0 {
		return errors.New("couldn't refund every paid seat (payment provider " +
			"unavailable) — no one was charged twice; please try deleting again shortly")
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
		ID:             asStr(row, "id"),
		ClassID:        asStr(row, "class_id"),
		UserID:         asStr(row, "user_id"),
		Name:           asStr(row, "name"),
		Email:          asStr(row, "email"),
		Status:         asStr(row, "status"),
		AmountCents:    asInt(row, "amount_cents"),
		Paid:           asBool(row, "paid"),
		ChargeAt:       asStr(row, "charge_at"),
		OfferExpiresAt: asStr(row, "offer_expires_at"),
		CreatedAt:      asStr(row, "created_at"),
	}
}

func (s *Service) classEnrolledCount(classID string) int {
	// 'offered' seats are held during a claim window, so they count toward
	// capacity (the seat isn't free until the offer is claimed or expires).
	rows, err := s.sb.Select("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&status=in.(enrolled,offered)&select=id")
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
	policyReady := s.coachProfilesReady() &&
		s.columnReady("coach_profiles", "cancel_policy")
	policyCache := map[string]string{} // coach_id → cancel_policy (per batch)
	for i := range list {
		list[i].EnrolledCount = s.classEnrolledCount(list[i].ID)
		list[i].WaitlistCount = s.classStatusCount(list[i].ID, "waitlisted")
		if policyReady && list[i].CoachID != "" {
			cid := list[i].CoachID
			pol, ok := policyCache[cid]
			if !ok {
				r, _ := s.sb.SelectOne("coach_profiles",
					"user_id=eq."+store.Q(cid)+"&select=cancel_policy")
				pol = asStr(r, "cancel_policy")
				policyCache[cid] = pol
			}
			list[i].CancelPolicy = pol
		}
		if viewerID != "" {
			sel := "status"
			if s.columnReady("coaching_enrollments", "offer_expires_at") {
				sel += ",offer_expires_at"
			}
			r, _ := s.sb.SelectOne("coaching_enrollments",
				"class_id=eq."+store.Q(list[i].ID)+"&user_id=eq."+store.Q(viewerID)+
					"&status=in.(enrolled,waitlisted,offered)&select="+sel)
			st := asStr(r, "status")
			list[i].Enrolled = st == "enrolled"
			list[i].Waitlisted = st == "waitlisted"
			list[i].Offered = st == "offered"
			list[i].OfferExpiresAt = asStr(r, "offer_expires_at")
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
		"id=eq."+store.Q(enrollmentID)+"&select=class_id,user_id,name,email")
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
	// A manual promote enrolls the player, so link them onto the coach's roster
	// (mirrors promoteNextWaitlisted; idempotent). Without this a manually-promoted
	// student never appears in the coach's Students list / thread.
	s.ensureCoachStudentLink(coachID, asStr(row, "user_id"),
		asStr(row, "name"), asStr(row, "email"))
	s.notifyUser(asStr(row, "user_id"), "coaching", coachID, s.coachingName(coachID),
		"A seat opened up — you're in "+asStr(cls, "title"), "myclasses")
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
		"class_id=eq."+store.Q(classID)+"&status=eq.waitlisted&order=created_at.asc&limit=1&select=id,user_id,name,email")
	if next == nil {
		return
	}
	coachID := asStr(cls, "coach_id")
	coachName := s.coachingName(coachID)
	title := asStr(cls, "title")
	price := asInt(cls, "price_cents")
	nowT := time.Now().UTC()

	// PAID class → don't silently enroll/charge. Offer the seat with a claim
	// window: the player must confirm & pay before it rolls to the next person.
	if price > 0 && s.columnReady("coaching_enrollments", "offer_expires_at") {
		deadline := nowT.Add(12 * time.Hour)
		if t, ok := parseTime(asStr(cls, "starts_at")); ok && t.Before(deadline) {
			deadline = t
		}
		if deadline.After(nowT.Add(15 * time.Minute)) {
			upd := map[string]any{
				"status":           "offered",
				"amount_cents":     price,
				"paid":             false,
				"offer_expires_at": deadline.Format(time.RFC3339),
			}
			if _, err := s.sb.Update("coaching_enrollments",
				"id=eq."+store.Q(asStr(next, "id")), upd); err != nil {
				return
			}
			s.notifyUser(asStr(next, "user_id"), "coaching", coachID, coachName,
				"A spot opened in “"+title+"”! Claim & pay in My classes before "+
					deadline.Local().Format("Mon 3:04 PM")+
					" or it goes to the next person. You won't be charged until you confirm.",
				"myclasses")
			return
		}
		// Class too imminent for a claim window — fall through to direct enroll.
	}

	// FREE class (or paid with no time for a claim window) → auto-enroll.
	upd := map[string]any{"status": "enrolled"}
	if price > 0 {
		if t, ok := parseTime(asStr(cls, "starts_at")); ok {
			upd["charge_at"] = t.Add(-time.Hour).UTC().Format(time.RFC3339)
		}
	}
	if _, err := s.sb.Update("coaching_enrollments",
		"id=eq."+store.Q(asStr(next, "id")), upd); err != nil {
		return
	}
	// Now enrolled → make sure they appear on the coach's roster.
	s.ensureCoachStudentLink(coachID, asStr(next, "user_id"),
		asStr(next, "name"), asStr(next, "email"))
	s.notifyUser(asStr(next, "user_id"), "coaching", coachID, coachName,
		"A seat opened up — you're now enrolled in "+title, "myclasses")
}

// SweepExpiredOffers expires unclaimed paid-seat offers (status 'offered' past
// their deadline, still unpaid), freeing the seat and rolling it to the next
// waitlisted player. Inert until the offer_expires_at column exists.
// SweepFailedRefunds retries a canceled paid seat whose refund/credit-restore
// failed earlier (its row still carries a live pi_ or "credit" payment_ref). This
// closes the loop so a transient Stripe/DB outage during a cancel/remove doesn't
// permanently strand a paying customer's money.
func (s *Service) SweepFailedRefunds() error {
	if !s.enrollmentsReady() {
		return nil
	}
	rows, err := s.sb.Select("coaching_enrollments",
		"status=eq.canceled&paid=is.true"+
			"&or=(payment_ref=like.pi_*,payment_ref=eq.credit)"+
			"&select=id,user_id,class_id,payment_ref&limit=100")
	if err != nil {
		return err
	}
	for _, r := range rows {
		classID := asStr(r, "class_id")
		coachID := ""
		if c, _ := s.sb.SelectOne("coaching_classes",
			"id=eq."+store.Q(classID)+"&select=coach_id"); c != nil {
			coachID = asStr(c, "coach_id")
		}
		if coachID == "" {
			continue // class gone — can't resolve the credit's coach; leave for ops
		}
		uid := asStr(r, "user_id")
		// settleEnrollmentRefund owns payment_ref (marks refunded on success).
		msg, failed := s.settleEnrollmentRefund(asStr(r, "id"), coachID, uid,
			asStr(r, "payment_ref"), true)
		if failed || msg == "" {
			continue // still failing — try again next tick
		}
		if uid != "" {
			s.notifyUser(uid, "coaching", coachID, s.coachingName(coachID),
				msg, "myclasses")
		}
	}
	return nil
}

func (s *Service) SweepExpiredOffers() error {
	if !s.enrollmentsReady() ||
		!s.columnReady("coaching_enrollments", "offer_expires_at") {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.sb.Select("coaching_enrollments",
		"status=eq.offered&paid=is.false&offer_expires_at=lte."+store.Q(now)+
			"&select=id,user_id,class_id&limit=200")
	if err != nil {
		return err
	}
	for _, r := range rows {
		cid := asStr(r, "class_id")
		title := "your class"
		if c, _ := s.sb.SelectOne("coaching_classes",
			"id=eq."+store.Q(cid)+"&select=title"); c != nil {
			if t := asStr(c, "title"); t != "" {
				title = t
			}
		}
		// Compare-and-set: only expire a row that is STILL an unclaimed offer.
		// Re-applying the select predicates on the write closes the TOCTOU where a
		// payment webhook (markEnrollmentPaid) flipped this row to enrolled/paid
		// between our SELECT and this UPDATE — we must not stomp a paid seat back to
		// expired or double-offer it. 0 rows affected → someone claimed it; skip.
		out, err := s.sb.Update("coaching_enrollments",
			"id=eq."+store.Q(asStr(r, "id"))+"&status=eq.offered&paid=is.false",
			map[string]any{"status": "expired"})
		if err != nil || len(out) == 0 {
			continue // claimed/paid mid-sweep — leave it and don't roll the seat
		}
		s.notifyUser(asStr(r, "user_id"), "coaching", "", "PlanMyPickle",
			"Your open spot in “"+title+"” expired — it's been offered to the next person.",
			"myclasses")
		s.promoteNextWaitlisted(cid) // roll to the next waitlister
	}
	return nil
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
			"coach_id=eq."+store.Q(coachID)+"&student_id=eq."+store.Q(studentID)+"&select=id,student_id")
	}
	if found == nil && email != "" {
		found, _ = s.sb.SelectOne("coach_students",
			"coach_id=eq."+store.Q(coachID)+"&student_email=eq."+store.Q(email)+"&select=id,student_id")
	}
	if found != nil {
		upd := map[string]any{}
		// Backfill the account link onto an email-only row so a later booking
		// doesn't spawn a second roster row for the same person.
		if studentID != "" && asStr(found, "student_id") == "" {
			upd["student_id"] = studentID
		}
		// Re-engaging (new enroll/booking) UN-archives a thread the student had
		// left — otherwise the active, paying engagement stays hidden from both
		// rosters and the preserved clip history is orphaned.
		if s.columnReady("coach_students", "left_at") {
			upd["left_at"] = nil
		}
		if len(upd) > 0 {
			_, _ = s.sb.Update("coach_students",
				"id=eq."+store.Q(asStr(found, "id")), upd)
		}
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
func (s *Service) Enroll(classID, userID, name, email string, skipCredit bool) (model.CoachingEnrollment, error) {
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
		s.notifyCoachOfEnrollment(cls, userID, name, "joined the waitlist for")
		if len(out) > 0 {
			return mapEnrollment(out[0]), nil
		}
		return mapEnrollment(row), nil
	}

	// A paid class: if the player holds a class-credit with this coach, spend one
	// and the seat is paid instantly — no per-class charge.
	usedCredit := false
	if price > 0 && !skipCredit && s.consumeCredit(asStr(cls, "coach_id"), userID) {
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
	s.notifyCoachOfEnrollment(cls, userID, name, "enrolled in")
	if len(out) > 0 {
		return mapEnrollment(out[0]), nil
	}
	return mapEnrollment(row), nil
}

// notifyCoachOfEnrollment pings the class's coach that a player joined (enrolled
// or waitlisted) — parity with 1:1 booking, which notifies the coach. Best-effort;
// no coach-side class deep-link exists yet, so the link is empty (informational).
func (s *Service) notifyCoachOfEnrollment(cls map[string]any, userID, name, verb string) {
	coachID := asStr(cls, "coach_id")
	if coachID == "" || coachID == userID {
		return
	}
	if strings.TrimSpace(name) == "" {
		name = "A player"
	}
	title := asStr(cls, "title")
	body := name + " " + verb + " your class"
	if title != "" {
		body = name + " " + verb + " “" + title + "”"
	}
	s.notifyUser(coachID, "coaching", userID, name, body, "coachclasses")
}

// CancelClassEnrollment drops the player's seat (kept as a canceled row). Any
// prepaid money is settled (credit restored / Stripe seat refunded), and the
// coach is told the roster shrank.
func (s *Service) CancelClassEnrollment(classID, userID string) error {
	if !s.enrollmentsReady() {
		return ErrCoachingUnavailable
	}
	coachID, title := "", ""
	// Honor the coach's cancellation cutoff.
	if cls, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(classID)+"&select=coach_id,starts_at,title"); cls != nil {
		coachID = asStr(cls, "coach_id")
		title = asStr(cls, "title")
		if err := s.enforceCancelCutoff(coachID, asStr(cls, "starts_at")); err != nil {
			return err
		}
	}
	// Read the seat BEFORE canceling so we can settle its payment idempotently.
	enr, _ := s.sb.SelectOne("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&user_id=eq."+store.Q(userID)+
			"&status=in.(enrolled,offered,waitlisted)"+
			"&select=id,name,status,paid,payment_ref&limit=1")
	if enr == nil {
		return nil // nothing active to cancel
	}
	refundMsg, refundFailed := "", false
	// Only an active seat is settled; a waitlist row never held money.
	// settleEnrollmentRefund owns payment_ref (marks it refunded on success, or
	// leaves it live for SweepFailedRefunds to retry on failure).
	if st := asStr(enr, "status"); st == "enrolled" || st == "offered" {
		refundMsg, refundFailed = s.settleEnrollmentRefund(asStr(enr, "id"),
			coachID, userID, asStr(enr, "payment_ref"), asBool(enr, "paid"))
	}
	if _, err := s.sb.Update("coaching_enrollments",
		"id=eq."+store.Q(asStr(enr, "id")),
		map[string]any{"status": "canceled"}); err != nil {
		return err
	}
	// Tell the coach a player dropped (parity with 1:1 cancel + enroll).
	if coachID != "" && coachID != userID {
		who := asStr(enr, "name")
		if strings.TrimSpace(who) == "" {
			who = "A player"
		}
		label := "your class"
		if title != "" {
			label = "“" + title + "”"
		}
		s.notifyUser(coachID, "coaching", userID, who,
			who+" canceled "+label, "coachclasses")
	}
	// Tell the student what happened to their money.
	if refundMsg != "" {
		s.notifyUser(userID, "coaching", coachID, s.coachingName(coachID),
			refundMsg, "myclasses")
	} else if refundFailed {
		s.notifyUser(userID, "coaching", coachID, s.coachingName(coachID),
			"You've been unenrolled — your refund is processing and will arrive shortly.",
			"myclasses")
	}
	// If that freed a seat, pull in the next waitlisted player.
	s.promoteNextWaitlisted(classID)
	return nil
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
	// Marking paid also confirms a claimed offer: flip to enrolled + clear the
	// deadline so it isn't left stuck 'offered' (which SweepExpiredOffers skips
	// once paid, and which players still see as "claim & pay").
	upd := map[string]any{"paid": true, "payment_ref": "manual", "status": "enrolled"}
	if s.columnReady("coaching_enrollments", "offer_expires_at") {
		upd["offer_expires_at"] = nil
	}
	if _, err := s.sb.Update("coaching_enrollments", "id=eq."+store.Q(enrollmentID),
		upd); err != nil {
		return err
	}
	// Cash-marking a manually-promoted seat also confirms the player — link them
	// onto the coach's roster (idempotent), matching the Stripe webhook path.
	if row, _ := s.sb.SelectOne("coaching_enrollments",
		"id=eq."+store.Q(enrollmentID)+"&select=user_id,name,email"); row != nil {
		s.ensureCoachStudentLink(coachID, asStr(row, "user_id"),
			asStr(row, "name"), asStr(row, "email"))
	}
	return nil
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
		"id=eq."+store.Q(enrollmentID)+"&select=class_id,status,user_id,paid,payment_ref")
	prevStatus := asStr(row, "status")
	wasEnrolled := prevStatus == "enrolled"
	// Settle any prepaid money the removed player put down (blameless removal →
	// restore credit / refund the Stripe seat) BEFORE flipping to canceled.
	refundMsg, refundFailed := "", false
	if prevStatus == "enrolled" || prevStatus == "offered" {
		// settleEnrollmentRefund owns payment_ref (refunded on success / left live
		// for SweepFailedRefunds on failure).
		refundMsg, refundFailed = s.settleEnrollmentRefund(enrollmentID, coachID,
			asStr(row, "user_id"), asStr(row, "payment_ref"), asBool(row, "paid"))
	}
	_, err := s.sb.Update("coaching_enrollments", "id=eq."+store.Q(enrollmentID),
		map[string]any{"status": "canceled"})
	if err == nil {
		// Tell the removed player — their prior "you're enrolled" is never
		// otherwise retracted, so they'd show up to a class they're no longer in.
		if uid := asStr(row, "user_id"); uid != "" {
			title := "a class"
			if c, _ := s.sb.SelectOne("coaching_classes",
				"id=eq."+store.Q(asStr(row, "class_id"))+"&select=title"); c != nil {
				if t := asStr(c, "title"); t != "" {
					title = "“" + t + "”"
				}
			}
			body := s.coachLabel(coachID) + " removed you from " + title
			if refundMsg != "" {
				body += " — " + refundMsg
			} else if refundFailed {
				body += " — your refund is processing and will arrive shortly."
			}
			s.notifyUser(uid, "coaching", coachID, s.coachingName(coachID),
				body, "myclasses")
		}
		if wasEnrolled {
			s.promoteNextWaitlisted(asStr(row, "class_id"))
		}
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
	// Include 'offered' so a held-but-unclaimed seat is visible to the coach and
	// the roster seat-count matches classEnrolledCount (which counts enrolled+offered).
	rows, err := s.sb.Select("coaching_enrollments",
		"class_id=eq."+store.Q(classID)+"&status=in.(enrolled,waitlisted,offered)&order=status.asc,created_at.asc")
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
		"user_id=eq."+store.Q(userID)+"&status=in.(enrolled,waitlisted,offered)"+
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
		if e.Status == "waitlisted" && e.CreatedAt != "" {
			w, _ := s.sb.Select("coaching_enrollments",
				"class_id=eq."+store.Q(e.ClassID)+"&status=eq.waitlisted"+
					"&created_at=lt."+store.Q(e.CreatedAt)+"&select=id")
			e.WaitlistPosition = len(w) + 1
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt < out[j].StartsAt })
	return out, nil
}

// markEnrollmentPaid flips a paid class enrollment to paid (webhook path).
func (s *Service) markEnrollmentPaid(enrollmentID, paymentIntentID string) error {
	if !s.enrollmentsReady() {
		return nil
	}
	row, _ := s.sb.SelectOne("coaching_enrollments",
		"id=eq."+store.Q(enrollmentID)+"&select=user_id,name,email,class_id,status,paid")
	if row == nil {
		return nil
	}
	if asBool(row, "paid") {
		return nil // idempotent — Stripe may deliver the webhook more than once
	}
	prevStatus := asStr(row, "status")
	classID := asStr(row, "class_id")

	// Money was captured, so the paying player KEEPS a seat — we never leave a
	// charged customer stranded. Paying also confirms a claimed offer: flip to
	// enrolled + clear the deadline.
	upd := map[string]any{"paid": true, "status": "enrolled"}
	if s.columnReady("coaching_enrollments", "offer_expires_at") {
		upd["offer_expires_at"] = nil
	}
	// Store the PaymentIntent as the refund handle so a later teardown can
	// auto-refund this seat. Guarded so it's inert until the column exists.
	if paymentIntentID != "" && s.columnReady("coaching_enrollments", "payment_ref") {
		upd["payment_ref"] = paymentIntentID
	}
	if _, err := s.sb.Update("coaching_enrollments",
		"id=eq."+store.Q(enrollmentID), upd); err != nil {
		return err
	}
	// Ensure the now-confirmed player is on the coach's roster (covers a claimed
	// offer, whose waitlist row never linked; idempotent for direct enrollments).
	if c, _ := s.sb.SelectOne("coaching_classes",
		"id=eq."+store.Q(classID)+"&select=coach_id,capacity,title"); c != nil {
		coachID := asStr(c, "coach_id")
		s.ensureCoachStudentLink(coachID, asStr(row, "user_id"),
			asStr(row, "name"), asStr(row, "email"))
		// Tell the coach an OFFERED seat was claimed & paid — otherwise the
		// offer→claim→paid loop completes invisibly to them. A direct paid enroll
		// already pinged the coach in Enroll, so don't double up here.
		if uid := asStr(row, "user_id"); prevStatus == "offered" && coachID != "" && coachID != uid {
			who := asStr(row, "name")
			if strings.TrimSpace(who) == "" {
				who = "A player"
			}
			s.notifyUser(coachID, "coaching", uid, who,
				who+" claimed & paid for “"+asStr(c, "title")+"”", "coachclasses")
		}
		// Rare race backstop: if this payment landed just after the offer was swept
		// and the seat had already rolled to the next waitlister (prevStatus was
		// terminal), honoring it can push the class one over capacity. Honor the
		// payment but alert the coach to reconcile rather than silently oversell.
		if prevStatus == "expired" || prevStatus == "canceled" {
			if cap := asInt(c, "capacity"); cap > 0 && s.classEnrolledCount(classID) > cap {
				s.notifyUser(coachID, "coaching", "", "PlanMyPickle",
					"Heads up: a late payment for “"+asStr(c, "title")+"” put it one over capacity. Please reconcile a seat.", "coachclasses")
			}
		}
	}
	return nil
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
	// Only a live seat may be paid for. Block a stale/expired offer or a waitlist
	// row (queue-jump) from ever starting a checkout — a completed charge on one of
	// those would oversell the class. An 'offered' seat must still be within its
	// claim window. 'enrolled' covers a direct paid enroll awaiting its charge.
	switch asStr(row, "status") {
	case "enrolled":
		// ok — a held seat awaiting payment
	case "offered":
		if s.columnReady("coaching_enrollments", "offer_expires_at") {
			if t, ok := parseTime(asStr(row, "offer_expires_at")); ok && time.Now().After(t) {
				return "", errors.New("this spot's claim window has passed — it may have gone to the next person")
			}
		}
	default:
		return "", errors.New("this spot is no longer available to pay for")
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
// RemindUpcomingClasses notifies enrolled players ~24h and ~1h before a class
// (two tiers, each sent once) to cut no-shows. Inert until the reminder columns
// exist.
func (s *Service) RemindUpcomingClasses() error {
	if !s.enrollmentsReady() || !s.classesReady() ||
		!s.columnReady("coaching_enrollments", "reminded_24h") {
		return nil
	}
	nowT := time.Now().UTC()
	horizon := nowT.Add(24 * time.Hour)
	classes, err := s.sb.Select("coaching_classes",
		"starts_at=gte."+store.Q(nowT.Format(time.RFC3339))+
			"&starts_at=lte."+store.Q(horizon.Format(time.RFC3339))+
			"&select=id,title,starts_at,location&limit=300")
	if err != nil {
		return err
	}
	for _, c := range classes {
		cid := asStr(c, "id")
		st, ok := parseSchedTime(asStr(c, "starts_at"))
		if !ok {
			continue
		}
		until := st.Sub(nowT)
		// 1h tier fires within 90 min; otherwise the day-before tier.
		col := "reminded_24h"
		when := "is tomorrow"
		if until <= 90*time.Minute {
			col = "reminded_1h"
			when = "starts in about an hour"
		} else if st.YearDay() == nowT.YearDay() && st.Year() == nowT.Year() {
			when = "is later today"
		}
		title := asStr(c, "title")
		if title == "" {
			title = "your class"
		}
		body := "“" + title + "” " + when + "."
		if loc := strings.TrimSpace(asStr(c, "location")); loc != "" {
			body += " At " + loc + "."
		}
		ens, _ := s.sb.Select("coaching_enrollments",
			"class_id=eq."+store.Q(cid)+"&status=eq.enrolled&"+col+
				"=is.false&select=id,user_id&limit=300")
		for _, e := range ens {
			if uid := asStr(e, "user_id"); uid != "" {
				s.notifyUser(uid, "coaching", "", "PlanMyPickle", body, "myclasses")
			}
			_, _ = s.sb.Update("coaching_enrollments",
				"id=eq."+store.Q(asStr(e, "id")), map[string]any{col: true})
		}
	}
	return nil
}

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
			"&select=id,coach_id,coach_student_id,status&limit=300")
	if err != nil {
		return err
	}
	for _, r := range rows {
		// Only remind CONFIRMED sessions — never a request still awaiting the
		// coach's approval (or one already declined). Legacy/coach-added rows have
		// a null status and are treated as confirmed. (Filtered in code because a
		// PostgREST status=neq.pending would also drop those null rows.)
		if st := asStr(r, "status"); st == "pending" || st == "declined" {
			continue
		}
		threadID := asStr(r, "coach_student_id")
		coachID := asStr(r, "coach_id")
		coachName := s.coachingName(coachID)
		if strings.TrimSpace(coachName) == "" {
			coachName = "your coach"
		}
		if threadID != "" {
			s.notifyStudentOfThread(threadID, coachID, s.coachingName(coachID),
				"Reminder: your 1:1 session with "+coachName+" is coming up soon",
				"coaching:"+threadID)
		}
		// Remind the COACH too — they agreed to the session and can otherwise
		// no-show; only the student was being nudged before. Require threadID so
		// the deep-link opens a real thread (a session can have no student thread).
		if coachID != "" && threadID != "" {
			studentName := ""
			if cs, _ := s.sb.SelectOne("coach_students",
				"id=eq."+store.Q(threadID)+"&select=student_id,student_name"); cs != nil {
				studentName = s.coachingName(asStr(cs, "student_id"))
				if strings.TrimSpace(studentName) == "" {
					studentName = asStr(cs, "student_name")
				}
			}
			if strings.TrimSpace(studentName) == "" {
				studentName = "your student"
			}
			s.notifyUser(coachID, "coaching", "", "PlanMyPickle",
				"Reminder: your 1:1 session with "+studentName+" is coming up soon",
				"coaching:"+threadID)
		}
		_, _ = s.sb.Update("coaching_schedule", "id=eq."+store.Q(asStr(r, "id")),
			map[string]any{"reminded_at": nowT.Format(time.RFC3339)})
	}
	return nil
}

// ExpireStalePendingBookings auto-declines 1:1 booking requests that a coach
// never answered and whose time has already passed — they can no longer happen
// and, left 'pending', would hold the coach's slot forever. The student is
// nudged to rebook. Only PAST-start requests expire: a still-future request
// stays pending however far out it is, so genuine advance bookings aren't
// killed. Cron sweep; no-op until the status column exists.
func (s *Service) ExpireStalePendingBookings() error {
	if !s.scheduleReady() || !s.columnReady("coaching_schedule", "status") {
		return nil
	}
	nowT := time.Now().UTC()
	rows, err := s.sb.Select("coaching_schedule",
		"kind=eq.session&status=eq.pending"+
			"&starts_at=lt."+store.Q(nowT.Format(time.RFC3339))+
			"&select=id,coach_id,coach_student_id,starts_at,ends_at&limit=300")
	if err != nil {
		return err
	}
	for _, r := range rows {
		// A long session that started but hasn't ended yet isn't stale — wait.
		if end, ok := parseSchedTime(asStr(r, "ends_at")); ok && end.After(nowT) {
			continue
		}
		id := asStr(r, "id")
		// Reuse 'declined' as the terminal "not happening" state so every existing
		// filter (coach schedule, player sessions, slot-overlap) already excludes it.
		if _, uerr := s.sb.Update("coaching_schedule", "id=eq."+store.Q(id),
			map[string]any{"status": "declined"}); uerr != nil {
			log.Printf("coaching: expire pending booking %s failed: %v", id, uerr)
			continue
		}
		threadID := asStr(r, "coach_student_id")
		if threadID == "" {
			continue
		}
		coachID := asStr(r, "coach_id")
		coachName := s.coachingName(coachID)
		if strings.TrimSpace(coachName) == "" {
			coachName = "your coach"
		}
		msg := "Your session request"
		if st, ok := parseSchedTime(asStr(r, "starts_at")); ok {
			msg += " for " + s.fmtSessionWhen(st)
		}
		msg += " expired — " + coachName +
			" didn't confirm in time. Pick a new open time to try again."
		s.notifyStudentOfThread(threadID, coachID, coachName, msg, "myclasses")
	}
	return nil
}

// RemindDueProgramWeeks nudges the student (and FYIs the coach) when a training
// program week has reached its due date and isn't checked off yet. Each week is
// reminded at most once (a "reminded" flag inside the week's JSON). Due dates
// live inside the weeks blob, so this scans active programs in Go rather than
// filtering in SQL.
func (s *Service) RemindDueProgramWeeks() error {
	if !s.programsReady() {
		return nil
	}
	nowT := time.Now().UTC()
	today := nowT.Format("2006-01-02")
	soonCutoff := nowT.AddDate(0, 0, 2).Format("2006-01-02") // due within ~2 days
	rows, err := s.sb.Select("coaching_programs",
		"active=is.true&select=id,coach_student_id,weeks&limit=500")
	if err != nil {
		return err
	}
	for _, r := range rows {
		weeks, _ := r["weeks"].([]any)
		if len(weeks) == 0 {
			continue
		}
		threadID := asStr(r, "coach_student_id")
		var coachID, studentID string
		if cs, _ := s.sb.SelectOne("coach_students",
			"id=eq."+store.Q(threadID)+
				"&select=coach_id,student_id,student_email,student_phone"); cs != nil {
			coachID = asStr(cs, "coach_id")
			studentID = asStr(cs, "student_id")
			// Resolve an account even when the roster row isn't linked yet, so an
			// email/phone-invited student who has since signed up still gets the
			// nudge — and we don't consume the one-shot reminder before they can.
			if studentID == "" {
				if e := asStr(cs, "student_email"); e != "" {
					studentID = s.userIDByEmail(e)
				}
				if studentID == "" {
					if p := asStr(cs, "student_phone"); p != "" {
						studentID = s.userIDByPhone(p)
					}
				}
			}
		}
		// No student account to notify yet → leave the week un-reminded so it
		// fires once the student actually joins.
		if studentID == "" {
			continue
		}
		changed := false
		for wi, w := range weeks {
			m, ok := w.(map[string]any)
			if !ok || asBool(m, "done") {
				continue
			}
			due := strings.TrimSpace(asStr(m, "due"))
			if due == "" {
				continue
			}
			if len(due) >= 10 {
				due = due[:10] // compare date-only (handles RFC3339 too)
			}
			focus := asStr(m, "focus")
			link := "coaching:" + threadID
			// Two tiers: a proactive "coming up" nudge 1–2 days before due, then
			// a "due" nudge on/after the date. Each fires at most once via its own
			// flag (reminded_soon / reminded).
			var flag, studentBody, coachBody string
			switch {
			case due <= today && !asBool(m, "reminded"):
				flag = "reminded"
				studentBody = "Reminder: a training program week is due"
				if focus != "" {
					studentBody = "Reminder: “" + focus + "” is due in your training program"
				}
				coachBody = "A program week is due for "
			case due > today && due <= soonCutoff && !asBool(m, "reminded_soon"):
				flag = "reminded_soon"
				studentBody = "Coming up: a training program week is due soon"
				if focus != "" {
					studentBody = "Coming up: “" + focus + "” is due soon in your training program"
				}
				coachBody = "A program week is due soon for "
			default:
				continue
			}
			s.notifyUser(studentID, "coaching", coachID, s.coachingName(coachID),
				studentBody, link)
			if coachID != "" {
				who := s.coachingName(studentID)
				if who == "" {
					who = "your student"
				}
				s.notifyUser(coachID, "coaching", "", "",
					coachBody+who+": "+focus, link)
			}
			m[flag] = true
			weeks[wi] = m
			changed = true
		}
		if changed {
			_, _ = s.sb.Update("coaching_programs",
				"id=eq."+store.Q(asStr(r, "id")),
				map[string]any{"weeks": weeks, "updated_at": now()})
		}
	}
	return nil
}

// SweepStalePBVisionJobs fails coaching PB Vision jobs stuck "processing" for
// over an hour — PB Vision's completion webhook never arrived (e.g. the same
// video URL was already analyzed elsewhere, so no new callback fires). Marking
// them failed stops the endless "analyzing…" chip and lets the student retry.
func (s *Service) SweepStalePBVisionJobs() error {
	if !s.pbvisionJobsReady() {
		return nil
	}
	cutoff := time.Now().UTC().Add(-60 * time.Minute).Format(time.RFC3339)
	rows, err := s.sb.Select("coaching_pbvision_jobs",
		"status=eq.processing&updated_at=lt."+store.Q(cutoff)+
			"&select=id,coach_student_id&limit=200")
	if err != nil {
		return err
	}
	for _, r := range rows {
		id := asStr(r, "id")
		if id == "" {
			continue
		}
		_, _ = s.sb.Update("coaching_pbvision_jobs", "id=eq."+store.Q(id),
			map[string]any{
				"status":     "failed",
				"error":      "Analysis timed out — please try again.",
				"updated_at": now(),
			})
		// Notify BOTH parties the analysis died — mirroring the normal webhook
		// path, which pings coach + student on success or failure. Without this the
		// coach (who was told to expect a result) sees it silently never appear.
		if tid := asStr(r, "coach_student_id"); tid != "" {
			if cs, _ := s.sb.SelectOne("coach_students",
				"id=eq."+store.Q(tid)+"&select=coach_id,student_id"); cs != nil {
				coachID := asStr(cs, "coach_id")
				sid := asStr(cs, "student_id")
				if sid != "" {
					s.notifyUser(sid, "coaching", coachID, s.coachingName(coachID),
						"PB Vision couldn't finish that analysis — tap the clip to try again",
						"coaching:"+tid+"?tab=pbvision")
				}
				if coachID != "" && coachID != sid {
					s.notifyUser(coachID, "coaching", "", "PlanMyPickle",
						"PB Vision couldn't finish that analysis — try a longer match clip",
						"coaching:"+tid+"?tab=pbvision")
				}
			}
		}
	}
	return nil
}

// RemindCoachOfPendingClips nudges a coach when a student's uploaded clip has
// gone ~24h without any coach comment — closing the feedback-latency loop (slow
// replies are the top churn driver in video coaching). Each clip nudges at most
// once (coach_reminded_at). Inert until that column runs.
func (s *Service) RemindCoachOfPendingClips() error {
	if !s.coachingReady() ||
		!s.columnReady("coaching_videos", "coach_reminded_at") ||
		!s.columnReady("coaching_videos", "uploader_role") {
		return nil
	}
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	rows, err := s.sb.Select("coaching_videos",
		"uploader_role=eq.student&coach_reminded_at=is.null"+
			"&created_at=lte."+store.Q(cutoff)+
			"&select=id,coach_student_id&limit=300")
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	allIDs := make([]string, 0, len(rows))
	threadOf := make(map[string]string, len(rows))
	for _, r := range rows {
		id := asStr(r, "id")
		if id != "" {
			allIDs = append(allIDs, id)
			threadOf[id] = asStr(r, "coach_student_id")
		}
	}
	// Which candidate clips already have a coach comment?
	answered := map[string]bool{}
	if fb, ferr := s.sb.Select("coaching_feedback",
		"video_id="+store.In(allIDs)+"&author_role=eq.coach&select=video_id"); ferr == nil {
		for _, f := range fb {
			answered[asStr(f, "video_id")] = true
		}
	}
	// Count still-unanswered clips per thread.
	byThread := map[string]int{}
	for id, tid := range threadOf {
		if tid != "" && !answered[id] {
			byThread[tid]++
		}
	}
	for tid, cnt := range byThread {
		cs, _ := s.sb.SelectOne("coach_students",
			"id=eq."+store.Q(tid)+"&select=coach_id,student_name,student_email")
		if cs == nil {
			continue
		}
		coachID := asStr(cs, "coach_id")
		if coachID == "" {
			continue
		}
		who := asStr(cs, "student_name")
		if who == "" {
			who = asStr(cs, "student_email")
		}
		if who == "" {
			who = "A student"
		}
		clips := "a clip"
		if cnt > 1 {
			clips = fmt.Sprintf("%d clips", cnt)
		}
		s.notifyUser(coachID, "coaching", "", "",
			who+" has "+clips+" waiting for your feedback",
			"coaching:"+tid)
	}
	// Mark every candidate reminded (answered ones too) so we stop re-scanning.
	nowT := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.sb.Update("coaching_videos", "id="+store.In(allIDs),
		map[string]any{"coach_reminded_at": nowT})
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
			s.activeStudentFilter()+ // don't nudge a student who left the coach
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
			name := s.coachingName(coachID)
			s.notifyUser(sid, "coaching", coachID, name,
				coachLabelFrom(name)+" is ready when you are — upload a clip for feedback",
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

// RemindNeverUploaded nudges a linked student who joined but hasn't uploaded a
// single clip after 3 days — the moment coaching relationships die. Distinct
// warmer copy + a direct upload link, fired once (first_nudge_at). This is an
// ONBOARDING nudge, separate from the 14-day inactivity sweep.
func (s *Service) RemindNeverUploaded() error {
	if !s.coachingReady() || !s.columnReady("coach_students", "first_nudge_at") {
		return nil
	}
	nowT := time.Now().UTC()
	cutoff := nowT.AddDate(0, 0, -3).Format(time.RFC3339)
	rows, err := s.sb.Select("coach_students",
		"student_id=not.is.null&first_nudge_at=is.null"+
			s.activeStudentFilter()+ // don't nudge a student who left the coach
			"&created_at=lte."+store.Q(cutoff)+
			"&select=id,coach_id,student_id&limit=200")
	if err != nil {
		return err
	}
	for _, r := range rows {
		threadID := asStr(r, "id")
		markNudged := func() {
			_, _ = s.sb.Update("coach_students", "id=eq."+store.Q(threadID),
				map[string]any{"first_nudge_at": nowT.Format(time.RFC3339)})
		}
		// Already uploaded → nothing to onboard; mark so we never re-scan it.
		if s.threadVideoCount(threadID) > 0 {
			markNudged()
			continue
		}
		coachID := asStr(r, "coach_id")
		if sid := asStr(r, "student_id"); sid != "" {
			name := s.coachingName(coachID)
			s.notifyUser(sid, "coaching", coachID, name,
				coachLabelFrom(name)+" is ready for your first clip — record a rally and upload it for feedback",
				"coaching:"+threadID)
		}
		markNudged()
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
