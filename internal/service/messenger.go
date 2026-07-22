package service

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Messenger channel plumbing — capturing a player's Page-Scoped ID (PSID) when
// they opt in via the check-in QR, and knowing whether we can reach them for
// free right now. The gateway (internal/gateway/messenger.go) does the Meta
// calls; this file maps PSIDs to players and enforces the 24-hour window.
//
// All player writes are columnReady-guarded so this is inert until
// add_messenger.sql is applied — safe to deploy ahead of the migration.

// messengerWindow is Meta's standard messaging window: after a user's last
// inbound message we may send free-form messages for 24h (each inbound resets
// it). We keep a small safety margin so a call at the edge doesn't 400.
const messengerWindow = 23*time.Hour + 30*time.Minute

// messengerRefPrefix tags an m.me opt-in link/QR with the player it belongs to:
// m.me/<page>?ref=ply_<playerID>. Meta echoes this ref back in the opt-in
// webhook, letting us bind the returned PSID to the right player.
const messengerRefPrefix = "ply_"

// MessengerRef builds the ?ref= value for a player's opt-in link.
func MessengerRef(playerID string) string { return messengerRefPrefix + playerID }

// playerIDFromRef extracts the player id from an opt-in ref, or "" if it isn't
// one of ours (some referral entry points carry unrelated refs).
func playerIDFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, messengerRefPrefix) {
		return ""
	}
	return strings.TrimPrefix(ref, messengerRefPrefix)
}

// MessengerPageUsername is the FB Page username the frontend uses to build
// m.me links (m.me/<username>?ref=...). Empty when unset → the frontend hides
// the "get alerts on Messenger" option.
func MessengerPageUsername() string {
	return strings.TrimSpace(os.Getenv("MESSENGER_PAGE_USERNAME"))
}

// messengerOn reports whether the Messenger channel is live: the players table
// has the PSID columns (migration applied). The gateway itself is a mock until
// a Page token is set, so this only guards the DB writes/reads.
func (s *Service) messengerOn() bool {
	return s.columnReady("players", "messenger_psid")
}

// CaptureMessengerOptIn binds a PSID to the player named in an opt-in ref and
// opens their messaging window (last_in = now). Idempotent: re-opting just
// refreshes the window. Best-effort — a bad ref or DB error is logged, not
// surfaced (the webhook must always 200 fast).
func (s *Service) CaptureMessengerOptIn(ref, psid string) {
	psid = strings.TrimSpace(psid)
	playerID := playerIDFromRef(ref)
	if psid == "" || playerID == "" || !s.messengerOn() {
		return
	}
	if _, err := s.sb.Update("players", "id=eq."+store.Q(playerID), map[string]any{
		"messenger_psid":    psid,
		"messenger_last_in": now(),
	}); err != nil {
		log.Printf("messenger: binding psid to player %s failed: %v", playerID, err)
	}
}

// BumpMessengerWindow refreshes the 24h window for whoever owns this PSID, on
// any inbound message. Best-effort; a PSID we don't recognize simply no-ops.
func (s *Service) BumpMessengerWindow(psid string) {
	psid = strings.TrimSpace(psid)
	if psid == "" || !s.messengerOn() {
		return
	}
	if _, err := s.sb.Update("players", "messenger_psid=eq."+store.Q(psid),
		map[string]any{"messenger_last_in": now()}); err != nil {
		log.Printf("messenger: bumping window for psid failed: %v", err)
	}
}

// messengerReachable reports whether a player row (carrying messenger_psid and
// messenger_last_in from the caller's select) can be messaged for free right
// now: a PSID on file whose window is still open.
func messengerReachable(player map[string]any) (psid string, ok bool) {
	psid = strings.TrimSpace(asStr(player, "messenger_psid"))
	if psid == "" {
		return "", false
	}
	last := asStr(player, "messenger_last_in")
	if last == "" {
		return "", false
	}
	t, err := time.Parse(time.RFC3339, last)
	if err != nil {
		// Supabase may return a fractional/space-separated stamp; try a looser parse.
		if t, err = time.Parse("2006-01-02T15:04:05", strings.SplitN(last, ".", 2)[0]); err != nil {
			return "", false
		}
	}
	if time.Since(t) > messengerWindow {
		return "", false
	}
	return psid, true
}

// notifyMatchStartMessenger sends the FREE Messenger court call to every player
// in the match who opted in (has a PSID) and is still inside the 24h window.
// Returns the set of playerIDs reached so the SMS loop substitutes rather than
// duplicates. prows carries player rows selected with messenger_psid +
// messenger_last_in. Best-effort — a send failure just leaves that player to
// fall through to SMS.
func (s *Service) notifyMatchStartMessenger(prows []map[string]any, eventID, matchID, court string, roundNumber int) map[string]bool {
	done := map[string]bool{}
	if !s.messengerOn() {
		return done
	}
	body := fmt.Sprintf("🥒 PlanMyPickle: You're up! Head to %s for round %d. See you on court!", court, roundNumber)
	for _, r := range prows {
		p := asMap(r, "player")
		if p == nil {
			continue
		}
		pid := asStr(p, "id")
		if pid == "" || done[pid] {
			continue
		}
		psid, ok := messengerReachable(p)
		if !ok {
			continue
		}
		res, err := s.Msgr.Send(psid, body)
		if err != nil || !res.OK {
			continue // leave them for the SMS fallback
		}
		done[pid] = true
		// Log the delivery alongside the SMS/push rows (delivery log, not the bell).
		_, _ = s.sb.Insert("notifications", map[string]any{
			"event_id": eventID, "match_id": matchID, "type": "game_starting",
			"to_address": "(messenger)", "body": body,
			"status": "sent", "sent_at": now(),
		})
	}
	return done
}
