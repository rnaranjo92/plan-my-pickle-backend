package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Who can run a club.
//
// A club outlives whoever created the account. The person who set it up goes on
// holiday, hands the Tuesday night to someone else, or stops running it
// entirely — and until now every one of those meant sharing a password, because
// the owner was a single user id on the clubs row and nothing else could edit
// anything.
//
// Two levels, deliberately not more:
//
//   OWNER    — one person. Everything, including deleting the club and choosing
//              who else may run it. Ownership is the thing you cannot delegate,
//              because delegating it is what co-owners are for.
//   CO-OWNER — runs the club day to day: edit its details and logo, create
//              events under it, invite people. Cannot delete the club and
//              cannot appoint other co-owners.
//
// A third "can invite but not edit" tier was considered and skipped: nobody
// asked for it, and every role that exists has to be explained to somebody.

// ClubRoleCoOwner is the stored role for a co-owner in club_members.role.
// Members are "member"; the owner is on the clubs row and has no member role.
const ClubRoleCoOwner = "co_owner"

// IsClubAdmin reports whether this user may RUN the club — the owner, or a
// co-owner they appointed.
//
// Deliberately not named OwnsClub: ownership and administration are now
// different things, and a check called "owns" that returned true for someone
// who cannot delete the club would be lying about which one it answers.
func (s *Service) IsClubAdmin(clubID, userID string) bool {
	if userID == "" {
		return false
	}
	if s.OwnsClub(clubID, userID) {
		return true
	}
	row, err := s.sb.SelectOne("club_members",
		"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(userID)+"&select=role")
	if err != nil || row == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(asStr(row, "role")), ClubRoleCoOwner)
}

// requireClubAdmin gates the day-to-day management actions.
func (s *Service) requireClubAdmin(clubID, callerID string) error {
	if s.IsClubAdmin(clubID, callerID) {
		return nil
	}
	return ErrForbidden
}

// SetClubRole promotes a member to co-owner or demotes them back.
//
// OWNER ONLY. A co-owner who could appoint co-owners is an owner with extra
// steps — and the way a club quietly changes hands without its owner noticing.
//
// The target must already be a MEMBER: promoting a stranger would put someone
// in charge of a club they have never joined, and joining is the moment a
// person agrees to be associated with it.
func (s *Service) SetClubRole(clubID, callerID, targetUserID, role string) error {
	if err := s.requireClubOwner(clubID, callerID); err != nil {
		return err
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return errors.New("which member?")
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role != ClubRoleCoOwner && role != "member" {
		return errors.New("role must be co_owner or member")
	}
	// The owner is not a member row, so there is nothing here to change — and an
	// owner "demoting themselves to member" would leave the club with an owner
	// who reads as a member everywhere it is displayed.
	if owner, _ := s.clubOwner(clubID); owner == targetUserID {
		return errors.New("that's the club owner — transfer ownership instead")
	}
	rows, err := s.sb.Update("club_members",
		"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(targetUserID),
		map[string]any{"role": role})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("they're not a member of this club yet — they need to " +
			"join before they can help run it")
	}
	return nil
}

// InviteToClub asks someone already using the app to join this club.
//
// An INVITATION, not an enrolment: it files a notification they can act on
// rather than adding them. Being added to a club without asking is how an app
// starts speaking for people — the club's name would appear on their profile,
// and its organizers could then reach them.
//
// Someone who isn't in the app yet can't be invited this way, because there is
// no account to notify. They register first, and the club's join link is what
// gets shared with them — which is why the caller is told that plainly rather
// than being handed a silent failure.
func (s *Service) InviteToClub(clubID, callerID, targetUserID string) error {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return errors.New("who are we inviting?")
	}
	club, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=name")
	if err != nil {
		return err
	}
	if club == nil {
		return ErrNotFound
	}
	// Already in? Say so rather than sending an invitation to somewhere they
	// are already standing.
	if row, _ := s.sb.SelectOne("club_members",
		"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(targetUserID)+
			"&select=user_id"); row != nil {
		return errors.New("they're already in this club")
	}
	name := strings.TrimSpace(asStr(club, "name"))
	if name == "" {
		name = "a club"
	}
	inviter := strings.TrimSpace(s.nameOfUser(callerID))
	body := "You've been invited to join " + name
	if inviter != "" {
		body = inviter + " invited you to join " + name
	}
	s.notifyUser(targetUserID, "club_invite", callerID, inviter, body, "club:"+clubID)
	return nil
}

// nameOfUser resolves a display name for an account, or "" when there isn't
// one. Best-effort: used for message copy, never for authorization.
func (s *Service) nameOfUser(userID string) string {
	if strings.TrimSpace(userID) == "" {
		return ""
	}
	row, err := s.sb.SelectOne("players",
		"user_id=eq."+store.Q(userID)+"&select=full_name&limit=1")
	if err != nil || row == nil {
		return ""
	}
	return asStr(row, "full_name")
}
