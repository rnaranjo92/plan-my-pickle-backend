package service

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// SMS metering — the safety net that makes a flat "unlimited SMS" plan (Club)
// safe to sell in ANY country. A2P SMS costs vary ~20× across markets (US
// ~$0.012/segment vs Philippines ~$0.24), so an un-capped plan can be eaten
// alive by a heavy international organizer. The meter counts sends per event
// owner per calendar month and, once the owner passes their monthly allowance,
// DEGRADES TO PUSH — the SMS is skipped (push already fired on every send path),
// never the notification. The subscription can never go underwater on carrier
// fees, and the same meter later fronts a cheaper channel (WhatsApp) or an
// overage-credit line with no changes to the send sites.
//
// It is INERT until switched on: with no SMS_MONTHLY_ALLOWANCE set (or 0) the
// meter allows every send, preserving today's behavior. It also self-heals
// around the migration — if the sms_usage table isn't present yet, allow. So
// this ships safely ahead of both the migration and the config.

// smsMonthlyAllowance is the per-owner monthly SMS-segment budget. 0 (the
// default, unset) means metering is OFF — every send is allowed. Set
// SMS_MONTHLY_ALLOWANCE to a positive integer to enforce a cap (e.g. size it so
// the worst-case market can't exceed the plan's SMS margin).
func smsMonthlyAllowance() int {
	v := strings.TrimSpace(os.Getenv("SMS_MONTHLY_ALLOWANCE"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// smsPeriod is the current metering bucket: the UTC calendar month (YYYY-MM).
// A month is coarse enough that clock skew never matters and matches how a
// monthly subscription bills.
func smsPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// smsMeterOn reports whether metering should actually be enforced: an allowance
// is configured AND the usage table exists (migration applied). When off, every
// gate short-circuits to "allow" so behavior is identical to pre-meter.
func (s *Service) smsMeterOn() bool {
	return smsMonthlyAllowance() > 0 && s.columnReady("sms_usage", "sent")
}

// smsMeterAllows reports whether ownerID may send another SMS this month. It is
// best-effort and FAILS OPEN: metering off, an empty owner, or any read error
// returns true — a metering hiccup must never silence a real match alert. Call
// this ONCE per send batch (a round start texts a whole round) and gate every
// send in the batch on the result, then recordSmsSent the number actually sent.
func (s *Service) smsMeterAllows(ownerID string) bool {
	if ownerID == "" || !s.smsMeterOn() {
		return true
	}
	allowance := smsMonthlyAllowance()
	return s.smsUsedThisPeriod(ownerID) < allowance
}

// smsUsedThisPeriod returns the owner's SMS count for the current month (0 on
// miss or any error — fail open).
func (s *Service) smsUsedThisPeriod(ownerID string) int {
	row, err := s.sb.SelectOne("sms_usage",
		"owner_id=eq."+store.Q(ownerID)+"&period=eq."+store.Q(smsPeriod())+"&select=sent")
	if err != nil || row == nil {
		return 0
	}
	return asInt(row, "sent")
}

// recordSmsSent adds n to the owner's current-month tally (best-effort,
// read-modify-write upsert on the (owner_id, period) key). Skipped entirely when
// metering is off. A lost update under concurrency only ever UNDER-counts, which
// fails open — acceptable for a soft budget; bursts to one owner are effectively
// serialized by the single notify goroutine anyway.
func (s *Service) recordSmsSent(ownerID string, n int) {
	if n <= 0 || ownerID == "" || !s.smsMeterOn() {
		return
	}
	period := smsPeriod()
	used := s.smsUsedThisPeriod(ownerID)
	if _, err := s.sb.Upsert("sms_usage", "owner_id,period", map[string]any{
		"owner_id":   ownerID,
		"period":     period,
		"sent":       used + n,
		"updated_at": now(),
	}); err != nil {
		log.Printf("sms meter: recording %d send(s) for owner %s failed: %v", n, ownerID, err)
	}
}

// SmsUsageStatus is an organizer's current-month SMS position for the dashboard.
type SmsUsageStatus struct {
	Used      int    `json:"used"`      // segments sent this month across all their events
	Allowance int    `json:"allowance"` // monthly cap (0 when metering is off)
	Remaining int    `json:"remaining"` // max(0, allowance-used); 0 when unmetered
	Period    string `json:"period"`    // "YYYY-MM"
	Metered   bool   `json:"metered"`   // false → no cap (unlimited); UI can hide the bar
}

// SmsUsageStatus reports how many SMS segments an owner has spent this month
// against their allowance. When metering is off (no allowance configured or the
// table isn't present) Metered=false and the counts are zero — the UI then
// shows "no cap" rather than a progress bar.
func (s *Service) SmsUsageStatus(userID string) SmsUsageStatus {
	st := SmsUsageStatus{Period: smsPeriod(), Metered: s.smsMeterOn()}
	if !st.Metered || userID == "" {
		return st
	}
	st.Allowance = smsMonthlyAllowance()
	st.Used = s.smsUsedThisPeriod(userID)
	if r := st.Allowance - st.Used; r > 0 {
		st.Remaining = r
	}
	return st
}

// eventOwnerID resolves an event to its owning organizer's user id (owner_id),
// the metering key. Empty on any miss/error so the caller fails open.
func (s *Service) eventOwnerID(eventID string) string {
	if eventID == "" {
		return ""
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=owner_id")
	if err != nil || ev == nil {
		return ""
	}
	return asStr(ev, "owner_id")
}
