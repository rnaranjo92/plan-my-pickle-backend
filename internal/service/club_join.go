package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Asking to join a club, rather than simply appearing in it.
//
// A club's roster is what its dues, its announcements and its "24 of 31 paid"
// are counted from, so who is on it has to be the club's decision. The queue is
// the club's front door: people ask, and somebody who runs the club says yes.
//
// Two ways past it, both deliberate:
//   - Someone the club INVITED is admitted on accepting. Making an admin
//     approve a person they personally invited asks the same question twice.
//   - Before the migration runs there is no queue at all and joining is
//     immediate, exactly as it was. This ships inert.

// ClubJoinRequest is one person waiting at the door.
type ClubJoinRequest struct {
	UserID      string `json:"userId"`
	FullName    string `json:"fullName,omitempty"`
	PhotoURL    string `json:"photoUrl,omitempty"`
	RequestedAt string `json:"requestedAt,omitempty"`
}

// joinQueueReady reports whether add_club_join_requests.sql has run.
func (s *Service) joinQueueReady() bool {
	return s.columnReady("club_join_requests", "club_id")
}

// hasJoinRequest returns the pending request for a user, or nil.
func (s *Service) hasJoinRequest(clubID, userID string) map[string]any {
	if !s.joinQueueReady() {
		return nil
	}
	row, err := s.sb.SelectOne("club_join_requests",
		"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(userID)+
			"&select=user_id,invited")
	if err != nil {
		return nil
	}
	return row
}

// RequestJoinClub is what "Join" now does: it asks.
//
// Returns whether the caller is IN (true) or waiting (false), so the app can
// say which happened instead of guessing. Idempotent in both directions —
// asking twice is a double tap, not a second person in the queue.
func (s *Service) RequestJoinClub(clubID, userID string) (bool, error) {
	row, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=*")
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, ErrNotFound
	}
	// Already in? Nothing to ask for.
	if s.isClubMember(clubID, userID) {
		return true, nil
	}
	queue := s.joinQueueReady()
	// Invited by the club: admitted on accepting.
	//
	// FIRST, and that order is the point: a private club still invites people,
	// and an invitation the invitee cannot accept is not an invitation. Private
	// governs who may ASK, never who the club has already chosen.
	if queue {
		if req := s.hasJoinRequest(clubID, userID); req != nil &&
			asBool(req, "invited") {
			if err := s.JoinClub(clubID, userID); err != nil {
				return false, err
			}
			_ = s.sb.Delete("club_join_requests",
				"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(userID))
			return true, nil
		}
	}
	// Private: members come by invitation only, and nobody else gets in.
	//
	// Enforced HERE rather than by hiding the button, because a button is not a
	// control — anyone can post to this endpoint with a club id.
	//
	// ABOVE the no-queue fallback below, not under it. Privacy must not depend
	// on whether add_club_join_requests.sql has run: with the gate underneath,
	// a database without the queue auto-admitted every stranger to a private
	// club. Without a queue there are no invitations either, so a private club
	// simply takes nobody — which is the safe end of that trade.
	if !asBoolDefaultTrue(row, "is_public") {
		return false, ErrForbidden
	}
	// No queue yet (migration not run) — behave exactly as before.
	if !queue {
		return true, s.JoinClub(clubID, userID)
	}
	// Was there already a pending request? If so this is a repeat tap — upsert
	// it (idempotent) but do NOT re-notify the admins, or a script looping the
	// join endpoint floods every admin's bell.
	already := s.hasJoinRequest(clubID, userID) != nil
	if _, err := s.sb.Upsert("club_join_requests", "club_id,user_id",
		map[string]any{"club_id": clubID, "user_id": userID}); err != nil {
		return false, err
	}
	if already {
		return false, nil
	}
	// Tell the people who can act on it, or the queue rots — a request nobody
	// is told about is a person who concludes the club ignored them.
	name := strings.TrimSpace(asStr(row, "name"))
	if name == "" {
		name = "your club"
	}
	who := strings.TrimSpace(s.nameOfUser(userID))
	if who == "" {
		who = "Someone"
	}
	if admins := s.clubAdminIDs(clubID); len(admins) > 0 {
		msg := who + " asked to join " + name
		s.recordNotifications(admins, "club_join_request", msg, "club:"+clubID)
		// And push. The bell alone meant an admin learned of the queue only by
		// opening the app for some other reason; the repeat-tap guard above is
		// what stops this becoming a flood.
		_ = s.sendPush(admins, name, msg, "")
	}
	return false, nil
}

// clubAdminIDs lists everyone who can act on a request: the owner and any
// co-owners. Organization staff are deliberately NOT notified — head office
// does not want a phone buzz every time somebody joins a site.
func (s *Service) clubAdminIDs(clubID string) []string {
	out := []string{}
	seen := map[string]bool{}
	if owner, err := s.clubOwner(clubID); err == nil && owner != "" {
		seen[owner] = true
		out = append(out, owner)
	}
	rows, err := s.sb.SelectAll("club_members",
		"club_id=eq."+store.Q(clubID)+"&role=eq."+ClubRoleCoOwner+"&select=user_id")
	if err != nil {
		return out
	}
	for _, r := range rows {
		if u := strings.TrimSpace(asStr(r, "user_id")); u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// ClubJoinRequests lists who is waiting, oldest first — the person who has been
// waiting longest is the one most likely to have given up.
func (s *Service) ClubJoinRequests(clubID, callerID string) ([]ClubJoinRequest, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return nil, err
	}
	out := []ClubJoinRequest{}
	if !s.joinQueueReady() {
		return out, nil
	}
	rows, err := s.sb.SelectAll("club_join_requests",
		"club_id=eq."+store.Q(clubID)+"&invited=eq.false"+
			"&select=user_id,requested_at&order=requested_at.asc")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		if u := strings.TrimSpace(asStr(r, "user_id")); u != "" {
			uids = append(uids, u)
		}
	}
	if len(uids) == 0 {
		return out, nil
	}
	// Names and photos in batched lookups — a queue of anonymous user ids is not
	// something anyone can approve.
	names := s.clubDisplayNames(uids)
	photos := s.photosByUser(uids)
	for _, r := range rows {
		uid := strings.TrimSpace(asStr(r, "user_id"))
		if uid == "" {
			continue
		}
		out = append(out, ClubJoinRequest{
			UserID:      uid,
			FullName:    names[uid],
			PhotoURL:    photos[uid],
			RequestedAt: asStr(r, "requested_at"),
		})
	}
	return out, nil
}

// ApproveJoinRequests admits people, in BULK.
//
// Bulk because the realistic case is a club opening its doors and twenty people
// asking in a week: an approve button that only ever takes one person is the
// reason a queue stops being read at all. Returns how many were admitted.
//
// Each person is added before their request is cleared, so a failure part-way
// through leaves them still in the queue rather than dropped from both — the
// recoverable direction.
func (s *Service) ApproveJoinRequests(
	clubID, callerID string, userIDs []string,
) (int, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return 0, err
	}
	if !s.joinQueueReady() {
		return 0, errors.New(
			"the join queue isn't enabled yet — run add_club_join_requests.sql")
	}
	club, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=name")
	if err != nil {
		return 0, err
	}
	if club == nil {
		return 0, ErrNotFound
	}
	name := strings.TrimSpace(asStr(club, "name"))
	if name == "" {
		name = "the club"
	}
	admitted := make([]string, 0, len(userIDs))
	for _, uid := range userIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		// Only people who actually ASKED (invited=false). An invited-but-not-
		// accepted row must never be admitted by an admin tapping Approve —
		// that would enroll someone who never consented, exactly what the
		// invitation-not-enrolment split exists to prevent.
		req := s.hasJoinRequest(clubID, uid)
		if req == nil || asBool(req, "invited") {
			continue
		}
		if err := s.JoinClub(clubID, uid); err != nil {
			continue // leave them queued; a retry picks them up
		}
		_ = s.sb.Delete("club_join_requests",
			"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(uid))
		admitted = append(admitted, uid)
	}
	if len(admitted) > 0 {
		s.recordNotifications(admitted, "club_approved",
			"You're in — welcome to "+name+".", "club:"+clubID)
		_ = s.sendPush(admitted, name, "You're in — welcome to "+name+".", "")
	}
	return len(admitted), nil
}

// DeclineJoinRequests removes people from the queue, in bulk.
//
// No notification. "You were turned down" is a message a club should deliver
// itself if it wants to, in its own words — an app announcing a rejection on
// the club's behalf is speaking for them about the one thing they'd want to
// phrase carefully. Nothing stops the person asking again.
func (s *Service) DeclineJoinRequests(
	clubID, callerID string, userIDs []string,
) (int, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return 0, err
	}
	if !s.joinQueueReady() {
		return 0, nil
	}
	n := 0
	for _, uid := range userIDs {
		if uid = strings.TrimSpace(uid); uid == "" {
			continue
		}
		if err := s.sb.Delete("club_join_requests",
			"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(uid)); err == nil {
			n++
		}
	}
	return n, nil
}

// pendingJoinCount is the number on the owner's "N waiting" pill.
func (s *Service) pendingJoinCount(clubID string) int {
	if !s.joinQueueReady() {
		return 0
	}
	return s.countRows("club_join_requests",
		"club_id=eq."+store.Q(clubID)+"&invited=eq.false&select=user_id", "user_id")
}

// markClubInvited records that the club asked for this person, so accepting
// admits them straight away instead of putting them back in the queue.
func (s *Service) markClubInvited(clubID, userID string) {
	if !s.joinQueueReady() {
		return
	}
	_, _ = s.sb.Upsert("club_join_requests", "club_id,user_id", map[string]any{
		"club_id": clubID, "user_id": userID, "invited": true,
	})
}

// clubJoinState fills the caller-facing flags on a club: whether they're
// waiting, and (for admins) how many others are.
func (s *Service) clubJoinState(c *model.Club, callerID string) {
	if c == nil {
		return
	}
	if callerID != "" && !c.IsMember {
		if req := s.hasJoinRequest(c.ID, callerID); req != nil &&
			!asBool(req, "invited") {
			c.JoinPending = true
		}
	}
	// Org admins can approve these too (requireClubAdmin admits them), so they
	// get the count as well or the queue is invisible to the person on duty.
	if c.IsOwner || c.IsCoOwner || c.IsOrgAdmin {
		c.PendingJoins = s.pendingJoinCount(c.ID)
	}
}
