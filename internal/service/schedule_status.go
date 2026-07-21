package service

import (
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// scheduleBehindThreshold is how far behind (minutes) the projected finish must
// slip past the plan before we raise the organizer's "behind schedule" flag. A
// small buffer keeps normal court-turnover jitter from tripping it.
const scheduleBehindThreshold = 15

// ScheduleStatus computes whether an in-flight event is running behind its plan.
// It's cheap enough to poll: two light selects (match statuses + unfinished
// participants) plus arithmetic. Returns a zeroed (ShowFlag=false) status for
// events that aren't in progress or have no start time — nothing to flag.
func (s *Service) ScheduleStatus(eventID string) (model.ScheduleStatus, error) {
	var st model.ScheduleStatus
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return st, err
	}
	courts := ev.NumCourts
	if courts < 1 {
		courts = 1
	}
	st.NumCourts = courts

	rows, err := s.sb.SelectAll("matches",
		"event_id=eq."+store.Q(eventID)+"&select=status")
	if err != nil {
		return st, err
	}
	st.Total = len(rows)
	for _, r := range rows {
		if asStr(r, "status") == "completed" {
			st.Completed++
		}
	}
	st.Remaining = st.Total - st.Completed

	// Only in-progress events with a planned start and real matches can be
	// "behind" — anything else has no baseline to compare against.
	st.InProgress = ev.Status == "in_progress" && st.Total > 0
	if !st.InProgress || ev.StartsAt == nil {
		return st, nil
	}
	start, perr := time.Parse(time.RFC3339, *ev.StartsAt)
	if perr != nil {
		return st, nil
	}
	// Multi-day events break the single-session projection: a continuous
	// slot model reads an overnight gap as hours "behind". If the event
	// explicitly ends on a later calendar day, don't compute a flag.
	if ev.EndsAt != nil {
		if end, e := time.Parse(time.RFC3339, *ev.EndsAt); e == nil &&
			end.UTC().Format("2006-01-02") != start.UTC().Format("2006-01-02") {
			return st, nil
		}
	}
	gameMin := ev.GameDurationMinutes
	if gameMin <= 0 {
		gameMin = 15
	}

	// Planned finish: every court runs a slot in parallel, so the event needs
	// ceil(total/courts) slots end-to-end. Projected finish: from NOW, the
	// remaining matches still need ceil(remaining/courts) slots.
	plannedSlots := int(math.Ceil(float64(st.Total) / float64(courts)))
	remainingSlots := int(math.Ceil(float64(st.Remaining) / float64(courts)))
	plannedEnd := start.Add(time.Duration(plannedSlots*gameMin) * time.Minute)
	projectedEnd := time.Now().UTC().Add(time.Duration(remainingSlots*gameMin) * time.Minute)
	st.PlannedEnd = plannedEnd.UTC().Format(time.RFC3339)
	st.ProjectedEnd = projectedEnd.UTC().Format(time.RFC3339)

	behind := int(math.Round(projectedEnd.Sub(plannedEnd).Minutes()))
	if behind < 0 {
		behind = 0
	}
	st.BehindMinutes = behind
	st.Behind = behind >= scheduleBehindThreshold

	// Distinct players still waiting on an unfinished match — the population an
	// organizer would notify.
	uids, phones, count, aerr := s.scheduleAffected(eventID)
	if aerr == nil {
		st.Affected = count
	}
	_ = uids
	_ = phones

	st.AckMinutes = s.scheduleAck(eventID)
	// Show once we cross the threshold, and re-show only after the delay grows a
	// further threshold past the last acknowledgement — so an ack quiets the
	// banner until things get materially worse.
	st.ShowFlag = st.Behind && behind >= st.AckMinutes+scheduleBehindThreshold
	return st, nil
}

// scheduleAck reads the last-acknowledged delay (minutes) for an event; 0 when
// never acknowledged or the column/row is missing.
func (s *Service) scheduleAck(eventID string) int {
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=schedule_ack_minutes")
	if err != nil || row == nil {
		return 0
	}
	return asInt(row, "schedule_ack_minutes")
}

// AcknowledgeSchedule records the current behind-by minutes as the organizer's
// acknowledgement, silencing the flag until the delay grows another threshold.
func (s *Service) AcknowledgeSchedule(eventID string) error {
	st, err := s.ScheduleStatus(eventID)
	if err != nil {
		return err
	}
	_, err = s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"schedule_ack_minutes": st.BehindMinutes})
	return err
}

// scheduleAffected returns the distinct linked-account user ids (for push) and
// phone numbers (for SMS) of players who still have an unfinished match, plus
// the distinct player count. These are the people a delay actually affects.
func (s *Service) scheduleAffected(eventID string) (userIDs, phones []string, count int, err error) {
	rows, err := s.sb.SelectAll("matches",
		"event_id=eq."+store.Q(eventID)+"&status=neq.completed"+
			"&select=match_participants(player:players!player_id(id,phone,user_id,sms_consent))")
	if err != nil {
		return nil, nil, 0, err
	}
	seenPlayer := map[string]bool{}
	seenUser := map[string]bool{}
	for _, m := range rows {
		parts, ok := m["match_participants"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			pl := asMap(pm, "player")
			if pl == nil {
				continue
			}
			pid := asStr(pl, "id")
			if pid == "" || seenPlayer[pid] {
				continue
			}
			seenPlayer[pid] = true
			// Only text players who opted in (sms_consent); the phone is stored
			// regardless so organizers can reach them.
			if ph := asStr(pl, "phone"); ph != "" && asBool(pl, "sms_consent") {
				phones = append(phones, ph)
			}
			if uid := asStr(pl, "user_id"); uid != "" && !seenUser[uid] {
				seenUser[uid] = true
				userIDs = append(userIDs, uid)
			}
		}
	}
	return userIDs, phones, len(seenPlayer), nil
}

// NotifyScheduleDelay tells the players still waiting on an unfinished match
// that the event is running late. Push goes to every linked account; SMS is sent
// only when sms=true (and only to US/Canada numbers, per the A2P campaign). A
// blank customMsg falls back to a default that names the current delay. Returns
// how many push + SMS messages were dispatched.
func (s *Service) NotifyScheduleDelay(eventID string, sms bool, customMsg string) (pushCount, smsCount int, err error) {
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, 0, err
	}
	st, err := s.ScheduleStatus(eventID)
	if err != nil {
		return 0, 0, err
	}
	uids, phones, _, err := s.scheduleAffected(eventID)
	if err != nil {
		return 0, 0, err
	}

	customMsg = strings.TrimSpace(customMsg)
	// Push body: short heading + detail. Default names the delay when we have one.
	detail := customMsg
	if detail == "" {
		if st.BehindMinutes > 0 {
			detail = fmt.Sprintf("Running about %d min behind — hang tight, we'll call your court soon.",
				st.BehindMinutes)
		} else {
			detail = "Schedule update — hang tight, we'll call your court soon."
		}
	}
	if len(uids) > 0 {
		if perr := s.sendPush(uids, ev.Name, detail,
			"https://app.planmypickle.com/?event="+ev.ID); perr == nil {
			pushCount = len(uids)
		}
		// File the delay update in each affected player's bell.
		s.recordNotifications(uids, "schedule", detail, "playevent:"+ev.ID)
	}

	// SMS is the premium "both channels" add-on — only text the delay update when
	// the organizer asked (sms) AND the event opted into SMS (push always went out).
	if sms && s.eventSmsEnabled(eventID) {
		body := customMsg
		if body == "" {
			if st.BehindMinutes > 0 {
				body = fmt.Sprintf("PlanMyPickle: %s is running ~%d min behind. Hang tight — we'll call your court soon.",
					ev.Name, st.BehindMinutes)
			} else {
				body = fmt.Sprintf("PlanMyPickle: schedule update for %s. Hang tight — we'll call your court soon.",
					ev.Name)
			}
		} else {
			body = "PlanMyPickle: " + customMsg
		}
		body += " Reply STOP to opt out."
		seen := map[string]bool{}
		for _, phone := range phones {
			if phone == "" || seen[phone] || !gateway.SmsReachable(phone) {
				continue
			}
			seen[phone] = true
			ins, ierr := s.sb.Insert("notifications", map[string]any{
				"event_id": eventID, "type": "delay",
				"to_address": phone, "body": body,
			})
			if ierr != nil || len(ins) == 0 {
				log.Printf("delay-sms: notification insert failed for %s: %v", eventID, ierr)
				continue
			}
			notifID := asStr(ins[0], "id")
			r, serr := s.Sms.Send(phone, body)
			stt := "failed"
			var ref, sentAt any
			if serr == nil && r.OK {
				stt, ref, sentAt = "sent", r.ProviderRef, now()
				smsCount++
			}
			_, _ = s.sb.Update("notifications", "id=eq."+store.Q(notifID), map[string]any{
				"status": stt, "provider_ref": ref, "sent_at": sentAt,
			})
		}
	}
	return pushCount, smsCount, nil
}
