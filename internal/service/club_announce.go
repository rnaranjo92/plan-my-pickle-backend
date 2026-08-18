package service

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Reaching your own club.
//
// A league organizer could broadcast to their roster; a club owner could reach
// nobody. So the way to tell forty members that Tuesday is cancelled was to
// find them somewhere else — which is precisely the moment a club decides the
// app isn't where the club lives.

// maxClubAnnouncement bounds one message.
//
// Long enough for "Tuesday is cancelled, courts are flooded, back next week"
// and short enough that it survives being a push notification, which is where
// most people will actually read it.
const maxClubAnnouncement = 400

// clubAnnounceCooldown is the minimum gap between a club's announcements.
//
// Not a rate limit for the server's sake — a limit for the MEMBERS' sake. Every
// one of these is a push on forty phones, and the fastest way to make a club's
// notifications worthless is to let a fat thumb send four of them. An organizer
// who genuinely needs to send twice in five minutes can wait, and usually
// discovers they wanted to edit rather than resend.
const clubAnnounceCooldown = 5 * time.Minute

// AnnounceToClub sends one message to every member of the club.
//
// Owner or co-owner: this is a day-to-day running task, which is exactly what
// co-owners exist for, and the person who tells the club Tuesday is cancelled
// is rarely the person whose name is on the account.
func (s *Service) AnnounceToClub(clubID, callerID, message string) (int, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return 0, err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return 0, errors.New("what would you like to say?")
	}
	if len([]rune(message)) > maxClubAnnouncement {
		return 0, fmt.Errorf("keep it under %d characters — this arrives as a "+
			"notification", maxClubAnnouncement)
	}
	if wait := s.clubAnnounceWait(clubID, time.Now()); wait > 0 {
		return 0, fmt.Errorf("you just sent one — give it %s before sending "+
			"again so this doesn't read as spam", roundedWait(wait))
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
		name = "Your club"
	}

	rows, err := s.sb.SelectAll("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id")
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		uid := strings.TrimSpace(asStr(r, "user_id"))
		// The sender doesn't need to be told what they just said.
		if uid == "" || uid == callerID || seen[uid] {
			continue
		}
		seen[uid] = true
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return 0, errors.New("there's nobody else in this club yet")
	}

	// The bell first, then the push.
	//
	// The bell entry is the durable copy — a push is a notification someone can
	// swipe away and never see again, and "I never got told" about a cancelled
	// session is the complaint this feature exists to prevent. Writing it first
	// means a failed push still leaves the message somewhere it can be read.
	s.recordNotifications(uids, "club_announcement", message, "club:"+clubID)
	_ = s.sendPush(uids, name, message, "")
	s.markClubAnnounced(clubID, time.Now())
	log.Printf("club announce: %s (%s) to %d member(s)", clubID, name, len(uids))
	return len(uids), nil
}

// clubAnnounceWait returns how long this club must wait, or 0 if it may send.
func (s *Service) clubAnnounceWait(clubID string, now time.Time) time.Duration {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()
	last, ok := s.lastClubAnnounce[clubID]
	if !ok {
		return 0
	}
	if elapsed := now.Sub(last); elapsed < clubAnnounceCooldown {
		return clubAnnounceCooldown - elapsed
	}
	return 0
}

// markClubAnnounced records a successful send. Only called AFTER the message
// went out — a failed attempt must not lock the organizer out of retrying.
func (s *Service) markClubAnnounced(clubID string, now time.Time) {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()
	if s.lastClubAnnounce == nil {
		s.lastClubAnnounce = map[string]time.Time{}
	}
	s.lastClubAnnounce[clubID] = now
}

// roundedWait phrases a duration the way somebody waiting would say it.
func roundedWait(d time.Duration) string {
	if d < time.Minute {
		secs := int(d.Seconds()) + 1
		return fmt.Sprintf("%d seconds", secs)
	}
	mins := int(d.Minutes()) + 1
	if mins == 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", mins)
}
