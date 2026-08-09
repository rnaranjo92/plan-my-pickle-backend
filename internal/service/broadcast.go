package service

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// broadcastSmsRecip is one texting target: a player's phone plus the consent
// flag that gates whether we're allowed to send at all.
type broadcastSmsRecip struct {
	pid, name, phone string
	consent          bool
}

// broadcastSmsRecipients resolves phone numbers for an SMS blast to a segment of
// the event's people. Only "all" and "checkedIn" are supported — waitlist rows
// carry no phone/consent, so an SMS blast can't reach them (email can). Consent
// is read here but enforced at send time so the count reflects real reach.
func (s *Service) broadcastSmsRecipients(ev model.Event, segment string) ([]broadcastSmsRecip, error) {
	filter := "event_id=eq." + store.Q(ev.ID)
	switch segment {
	case "checkedIn":
		filter += "&checked_in=eq.true"
	case "all", "":
		// no extra filter — every registered player
	default:
		// Unknown segment (incl. "waitlist", which has no phone/consent) must NOT
		// fall through to the whole roster — fail CLOSED with zero recipients.
		return nil, nil
	}
	rows, err := s.sb.SelectAll("registrations",
		filter+"&select=player_id,player:players!player_id(full_name,phone,sms_consent)")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{} // one text per phone (partners sharing a phone → one)
	var out []broadcastSmsRecip
	for _, r := range rows {
		p := asMap(r, "player")
		if p == nil {
			continue
		}
		phone := strings.TrimSpace(asStr(p, "phone"))
		if phone == "" || seen[phone] {
			continue
		}
		seen[phone] = true
		out = append(out, broadcastSmsRecip{
			pid:     asStr(r, "player_id"),
			name:    asStr(p, "full_name"),
			phone:   phone,
			consent: asBool(p, "sms_consent"),
		})
	}
	return out, nil
}

// SendCustomSms texts a free-form organizer message to a segment of the event's
// roster. Returns the number of texts QUEUED (consented + reachable + within the
// owner's monthly allowance). Delivery is async/best-effort off the request path,
// throttled like the other blasts. Only opted-in (sms_consent) US/CA numbers are
// texted, each capped to one message; a STOP footer is appended for compliance.
func (s *Service) SendCustomSms(eventID, message, segment string) (int, error) {
	if s.Sms == nil {
		return 0, errors.New("SMS is not configured")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return 0, errors.New("message is required")
	}
	if len(message) > 400 {
		return 0, errors.New("message is too long (max 400 characters)")
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	recips, err := s.broadcastSmsRecipients(ev, segment)
	if err != nil {
		return 0, err
	}
	// Only consented, reachable numbers count toward the blast.
	var targets []broadcastSmsRecip
	for _, rc := range recips {
		if rc.consent && gateway.SmsReachable(rc.phone) {
			targets = append(targets, rc)
		}
	}
	// Owner's monthly SMS budget gates the whole send (same meter as court calls).
	owner := s.eventOwnerID(eventID)
	if !s.smsMeterAllows(owner) {
		return 0, errors.New("you've reached this month's SMS allowance")
	}
	// Refuse a second concurrent blast for the same event (double-click / retry),
	// which would otherwise double-text everyone and spike the send rate.
	s.customEmailMu.Lock()
	if s.broadcastSmsInFlight == nil {
		s.broadcastSmsInFlight = map[string]bool{}
	}
	if s.broadcastSmsInFlight[eventID] {
		s.customEmailMu.Unlock()
		return 0, errors.New("a text is already sending for this event — give it a moment")
	}
	s.broadcastSmsInFlight[eventID] = true
	s.customEmailMu.Unlock()

	body := "PlanMyPickle: " + message + " Reply STOP to opt out."
	go func() {
		defer func() {
			s.customEmailMu.Lock()
			delete(s.broadcastSmsInFlight, eventID)
			s.customEmailMu.Unlock()
		}()
		sent := 0
		for _, rc := range targets {
			ins, err := s.sb.Insert("notifications", map[string]any{
				"event_id": eventID, "type": "broadcast",
				"to_address": rc.phone, "body": body,
			})
			if err != nil {
				log.Printf("sms: broadcast log insert for %s failed: %v", eventID, err)
				continue
			}
			r, err := s.Sms.Send(rc.phone, body)
			if err != nil {
				log.Printf("sms: broadcast to %s failed: %v", rc.phone, err)
			}
			st, ref, sentAt := "failed", any(nil), any(nil)
			if err == nil && r.OK {
				st, ref, sentAt = "sent", r.ProviderRef, now()
				sent++
			}
			if len(ins) > 0 {
				s.sb.Update("notifications", "id=eq."+store.Q(asStr(ins[0], "id")),
					map[string]any{"status": st, "provider_ref": ref, "sent_at": sentAt})
			}
			time.Sleep(400 * time.Millisecond)
		}
		s.recordSmsSent(owner, sent)
		log.Printf("sms: broadcast for %s (segment=%q) dispatched to %d", eventID, segment, sent)
	}()
	return len(targets), nil
}

// SendCustomSmsTest texts ONE copy of the composed message to the caller's own
// phone so they can preview the real thing before blasting.
func (s *Service) SendCustomSmsTest(userID, message string) (string, error) {
	if s.Sms == nil {
		return "", errors.New("SMS is not configured")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", errors.New("message is required")
	}
	phone := ""
	if pr, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=phone"); err == nil && pr != nil {
		phone = strings.TrimSpace(asStr(pr, "phone"))
	}
	if phone == "" {
		if pl, err := s.sb.SelectOne("players",
			"user_id=eq."+store.Q(userID)+"&select=phone"); err == nil && pl != nil {
			phone = strings.TrimSpace(asStr(pl, "phone"))
		}
	}
	if phone == "" {
		return "", errors.New("no phone on file — add one in your profile first")
	}
	// Keep the "every send path gates on SmsReachable" invariant — don't attempt
	// (and get billed for) a guaranteed carrier failure on a non-US/CA number.
	if !gateway.SmsReachable(phone) {
		return phone, errors.New("your phone isn't in a texting region we support (US/Canada)")
	}
	body := fmt.Sprintf("PlanMyPickle: %s Reply STOP to opt out.", message)
	if _, err := s.Sms.Send(phone, body); err != nil {
		return phone, err
	}
	return phone, nil
}
