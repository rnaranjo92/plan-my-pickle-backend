package service

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// notifyUser writes one entry to a user's in-app activity feed (the bell) and,
// best-effort, sends a matching push. Fully best-effort: it logs and swallows
// every error (a missing user_notifications table pre-migration, a push
// failure) so it NEVER breaks the action that triggered it (a follow, a
// reaction, a registration). Self-notifications (actor == recipient) and empty
// recipients are skipped.
//
// link is an app deep-link target the client routes on tap: "event:<id>",
// "profile:<id>", or "feed".
func (s *Service) notifyUser(recipientID, typ, actorID, actorName, body, link string) {
	if recipientID == "" || recipientID == actorID {
		return
	}
	s.recordNotification(recipientID, typ, actorID, actorName, body, link)
	// Push to the recipient's linked device(s) (external_id = Supabase user id).
	_ = s.sendPush([]string{recipientID}, "PlanMyPickle", body,
		notifPushURL(link))
}

// recordNotification files a bell row WITHOUT sending a push — for events that
// ALREADY push/SMS on their own (match start, on-deck, score confirm, delays,
// disputes), so the activity feed captures them without a duplicate push.
// Best-effort: logs+swallows errors (incl. a missing table pre-migration).
func (s *Service) recordNotification(recipientID, typ, actorID, actorName, body, link string) {
	if recipientID == "" || recipientID == actorID {
		return
	}
	if _, err := s.sb.Insert("user_notifications", map[string]any{
		"recipient_id": recipientID,
		"type":         typ,
		"actor_id":     orNull(actorID),
		"actor_name":   actorName,
		"body":         body,
		"link":         link,
	}); err != nil {
		log.Printf("recordNotification: insert (%s): %v", typ, err)
	}
}

// recordNotifications files the same bell row for many recipients (bulk sites
// like a court call or a delay blast). De-dupes the recipient list.
func (s *Service) recordNotifications(recipientIDs []string, typ, body, link string) {
	seen := map[string]bool{}
	for _, uid := range recipientIDs {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		s.recordNotification(uid, typ, "", "", body, link)
	}
}

// feedItemRecipient is who should be notified about activity on a post: the
// community author (author_id) for a user post, else the owning event's
// organizer for an event post. "" when neither resolves.
func (s *Service) feedItemRecipient(feedItemID string) string {
	row, err := s.sb.SelectOne("feed_items",
		"id=eq."+store.Q(feedItemID)+"&select=author_id,event_id")
	if err != nil || row == nil {
		return ""
	}
	if a := asStr(row, "author_id"); a != "" {
		return a
	}
	if ev := asStr(row, "event_id"); ev != "" {
		owner, _ := s.OwnerOf("event", ev)
		return owner
	}
	return ""
}

// notifPushURL turns a deep-link target into a web launch URL for the push
// (tapping the notification opens the right place). Only event targets have a
// distinct web URL; "feed" IS the home tab and "profile:<userId>" has no web
// route (there's no user-id profile page), so both correctly fall back to home
// — the bell row itself still carries the precise in-app link for tap routing.
func notifPushURL(link string) string {
	const base = "https://app.planmypickle.com"
	switch {
	case strings.HasPrefix(link, "event:"):
		return base + "/?event=" + strings.TrimPrefix(link, "event:")
	case strings.HasPrefix(link, "playevent:"):
		return base + "/?event=" + strings.TrimPrefix(link, "playevent:")
	default:
		return base
	}
}

// SeedDemoNotifications fills the caller's OWN bell with one of each notification
// type (staggered timestamps + left unread) so the feed can be previewed. Links
// are intentionally empty so a tapped demo row navigates nowhere. Returns the
// count inserted. Dev/QA convenience — only ever touches the caller's own rows.
func (s *Service) SeedDemoNotifications(userID string) (int, error) {
	if userID == "" {
		return 0, ErrForbidden
	}
	demos := []struct {
		typ, actor, body string
		minsAgo          int
	}{
		{"match_start", "", "You're up on Court 3 — round 2", 1},
		{"ondeck", "", "You're on deck — warm up for Court 3", 6},
		{"score", "", "Confirm your score: opponents reported 11-7", 12},
		{"registration", "Taylor Kim", "Taylor Kim registered for Spring Paddle Mixer", 40},
		{"comment", "Sam Carter", "Sam Carter commented on your post", 95},
		{"reaction", "Jordan Lee", "Jordan Lee reacted to your post", 140},
		{"follow", "Alex Rivera", "Alex Rivera started following you", 190},
		{"schedule", "", "Running about 15 min behind — hang tight, we'll call your court soon.", 260},
		{"announcement", "", "Organizer: lunch break at noon — courts pause 12:00–12:45.", 400},
		{"dispute", "", "A reported score was disputed — enter the final score to resolve it", 700},
		{"session", "", "Tuesday Night Mixer: a new session is up — tap to RSVP", 1500},
	}
	n := 0
	for _, d := range demos {
		ts := time.Now().Add(-time.Duration(d.minsAgo) * time.Minute).
			UTC().Format("2006-01-02T15:04:05.000Z")
		if _, err := s.sb.Insert("user_notifications", map[string]any{
			"recipient_id": userID,
			"type":         d.typ,
			"actor_name":   d.actor,
			"body":         d.body,
			"link":         "",
			"created_at":   ts,
		}); err == nil {
			n++
		}
	}
	return n, nil
}

// ListNotifications returns a user's activity feed, newest first (capped).
// Best-effort: any error (incl. a missing table pre-migration) yields an empty
// list rather than a 500 so the bell always renders.
func (s *Service) ListNotifications(userID string, limit int) ([]model.UserNotification, error) {
	if userID == "" {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	// Single bounded page (Select, not SelectAll — SelectAll owns its own
	// windowing and forbids an embedded limit).
	rows, err := s.sb.Select("user_notifications",
		"recipient_id=eq."+store.Q(userID)+
			"&order=created_at.desc&limit="+fmt.Sprint(limit)+
			"&select=id,type,actor_id,actor_name,title,body,link,read,created_at")
	if err != nil {
		log.Printf("ListNotifications: %v", err)
		return []model.UserNotification{}, nil
	}
	out := make([]model.UserNotification, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.UserNotification{
			ID:        asStr(r, "id"),
			Type:      asStr(r, "type"),
			ActorID:   asStr(r, "actor_id"),
			ActorName: asStr(r, "actor_name"),
			Title:     asStr(r, "title"),
			Body:      asStr(r, "body"),
			Link:      asStr(r, "link"),
			Read:      asBool(r, "read"),
			CreatedAt: asStr(r, "created_at"),
		})
	}
	return out, nil
}

// UnreadNotificationCount powers the bell's badge. Best-effort → 0 on any error.
// Bounded to 100 (single Select page); the badge renders 100+ as "99+" anyway,
// so an exact count past that isn't needed and we avoid scanning a huge backlog.
func (s *Service) UnreadNotificationCount(userID string) (int, error) {
	if userID == "" {
		return 0, ErrForbidden
	}
	rows, err := s.sb.Select("user_notifications",
		"recipient_id=eq."+store.Q(userID)+"&read=eq.false&select=id&limit=100")
	if err != nil {
		log.Printf("UnreadNotificationCount: %v", err)
		return 0, nil
	}
	return len(rows), nil
}

// MarkNotificationsRead flips the read flag. With all=true it clears the whole
// feed for the user (the "mark all read" the client fires when the bell opens);
// otherwise it clears the given ids (scoped to the caller so one user can't
// touch another's rows). Best-effort like its read siblings: a transient DB
// error (or a missing table pre-migration) is logged and swallowed so opening
// the bell never surfaces a 500 for a purely cosmetic mark-read.
func (s *Service) MarkNotificationsRead(userID string, ids []string, all bool) error {
	if userID == "" {
		return ErrForbidden
	}
	q := ""
	if all {
		q = "recipient_id=eq." + store.Q(userID) + "&read=eq.false"
	} else {
		if len(ids) == 0 {
			return nil
		}
		q = "recipient_id=eq." + store.Q(userID) + "&id=" + store.In(ids)
	}
	if _, err := s.sb.Update("user_notifications", q,
		map[string]any{"read": true}); err != nil {
		log.Printf("MarkNotificationsRead: %v", err)
	}
	return nil
}
