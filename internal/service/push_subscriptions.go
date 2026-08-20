package service

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Device-level push addressing.
//
// Targeted sends used to resolve exclusively through include_aliases.external_id,
// which depends on OneSignal's identity model being intact. It isn't always: a
// login-user 409 against a user that already holds the external_id pauses the
// SDK's op queue, leaving a live subscription owned by no addressable user. The
// send is then ACCEPTED and delivered to nobody, because the alias did resolve —
// to a user with no devices. No error on either side.
//
// A subscription id skips all of that. The client records its own once it has
// verified the subscription is opted in, and sends prefer these.

// pushSubsReady reports whether add_push_subscriptions.sql has run. Everything
// here degrades to the alias path until it has, so the code ships before the
// migration does.
func (s *Service) pushSubsReady() bool {
	return s.columnReady("push_subscriptions", "subscription_id")
}

// RecordPushSubscription stores (or refreshes) one device's subscription id.
//
// Keyed on the subscription id, so a device that re-registers updates its row.
// Keying on the user instead would either cap them at one device or accumulate a
// duplicate row per sign-in, and duplicates mean one court call arriving twice.
func (s *Service) RecordPushSubscription(userID, subID, platform string) error {
	userID, subID = strings.TrimSpace(userID), strings.TrimSpace(subID)
	if userID == "" || subID == "" {
		return nil // nothing to record — not an error worth surfacing
	}
	if !s.pushSubsReady() {
		return nil
	}
	_, err := s.sb.Upsert("push_subscriptions", "subscription_id", map[string]any{
		"subscription_id": subID,
		"user_id":         userID,
		"platform":        strings.TrimSpace(platform),
		"updated_at":      time.Now().UTC().Format(time.RFC3339),
	})
	return err
}

// pushSubscriptionsFor returns the known subscription ids for [userIDs], and the
// set of users that had at least one.
//
// The second return is what makes the split send correct: users NOT in it still
// need the alias path, or a device that never recorded an id silently stops
// getting notifications the moment this table exists.
func (s *Service) pushSubscriptionsFor(userIDs []string) (subIDs []string, covered map[string]bool) {
	covered = map[string]bool{}
	if len(userIDs) == 0 || !s.pushSubsReady() {
		return nil, covered
	}
	// Only rows a device has refreshed RECENTLY count.
	//
	// Being covered means "we have a better address than the alias", and it
	// silences the alias path for that user completely. A device that has gone
	// dark — permission revoked, browser data cleared, subscription rotated —
	// leaves a row that OneSignal will accept sends for and deliver nowhere, and
	// never reports as invalid, so nothing here can learn it died. That user
	// would be covered forever and reachable by nothing: push that works for a
	// while and then quietly stops for good.
	//
	// Every sign-in and token refresh re-records the id (see ensurePushReady),
	// so a row older than this means the app hasn't run in a month and a half.
	// The alias is the better address for someone that far out of contact.
	cutoff := time.Now().UTC().AddDate(0, 0, -45).Format(time.RFC3339)
	rows, err := s.selectAllPushRows(userIDs, cutoff)
	if err != nil {
		return nil, covered // fall back to aliases for everyone
	}
	for _, r := range rows {
		id := asStr(r, "subscription_id")
		if id == "" {
			continue
		}
		subIDs = append(subIDs, id)
		covered[asStr(r, "user_id")] = true
	}
	return subIDs, covered
}

// selectAllPushRows reads every matching row, in pages.
//
// PostgREST caps a response at ~1000 rows whatever the limit says, so the old
// `limit=2000` was a request for two pages that silently returned one. On a
// broadcast that meant the users past the cap fell through to the alias path —
// which works, but hid the cap: it never looked like a bug, just like push
// being unreliable for some people.
// allPushSubscriptionIDs returns every device we can currently reach — the
// audience for a broadcast.
//
// Same 45-day freshness rule as pushSubscriptionsFor: a row older than that
// means the app hasn't run in a month and a half, and sending to it delivers
// nowhere while inflating the count.
//
// Deduped, because one person with a phone AND the web app has two rows and
// must not get the same joke twice.
func (s *Service) allPushSubscriptionIDs() ([]string, error) {
	if !s.pushSubsReady() {
		return nil, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -45).Format(time.RFC3339)
	const page = 1000
	seen := map[string]bool{}
	var out []string
	for offset := 0; ; offset += page {
		rows, err := s.sb.Select("push_subscriptions",
			"updated_at=gte."+cutoff+
				"&select=subscription_id"+
				"&order=updated_at.desc"+
				"&limit="+strconv.Itoa(page)+
				"&offset="+strconv.Itoa(offset))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if id := asStr(r, "subscription_id"); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		if len(rows) < page {
			return out, nil
		}
	}
}

func (s *Service) selectAllPushRows(
	userIDs []string, cutoff string,
) ([]map[string]any, error) {
	const page = 1000
	var all []map[string]any
	for offset := 0; ; offset += page {
		rows, err := s.sb.Select("push_subscriptions",
			"user_id="+store.In(userIDs)+
				"&updated_at=gte."+cutoff+
				"&select=subscription_id,user_id"+
				"&order=updated_at.desc"+
				"&limit="+strconv.Itoa(page)+
				"&offset="+strconv.Itoa(offset))
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < page {
			return all, nil
		}
	}
}

// forgetPushSubscriptions drops ids OneSignal has told us are invalid.
//
// Without this the table only grows: every reinstall, cleared browser and
// revoked permission leaves an id that can never receive anything, and each one
// is retried on every send forever.
func (s *Service) forgetPushSubscriptions(ids []string) {
	if len(ids) == 0 || !s.pushSubsReady() {
		return
	}
	// Name the ACCOUNT before deleting the row, because an invalid id is the
	// signature of a wedged identity, not just a dead device: OneSignal issues
	// ids, so one it has never seen means the device generated it locally and
	// its registration never completed — which is what happens when login-user
	// 409s against a stale user already holding that external_id and the SDK's
	// op queue stalls.
	//
	// Without this line the row vanishes and the account looks merely
	// unregistered, which is indistinguishable from someone who has not opened
	// the app yet. With it, `grep 'push WEDGED'` is the list of people whose
	// external_id needs freeing in OneSignal → Audience → Users.
	if rows, err := s.sb.Select("push_subscriptions",
		"subscription_id="+store.In(ids)+
			"&select=user_id,platform,subscription_id"); err == nil {
		for _, r := range rows {
			log.Printf("push WEDGED: user=%s platform=%s held an id OneSignal "+
				"never issued (%s) — its registration never completed; free the "+
				"external_id in OneSignal Audience → Users, then clear the app's "+
				"storage on that device",
				asStr(r, "user_id"), asStr(r, "platform"),
				asStr(r, "subscription_id"))
		}
	}
	_ = s.sb.Delete("push_subscriptions", "subscription_id="+store.In(ids))
}

// PushStatus answers "can this account actually receive a push, and why not".
//
// It exists because every previous round of this bug was diagnosed by reading
// server logs — which the person who needs the answer usually can't reach. Each
// field is one of the ways a send silently reaches nobody.
type PushStatus struct {
	// Configured: the OneSignal REST key is present. Without it every push in
	// the product is a silent no-op.
	Configured bool `json:"configured"`
	// TableReady: push_subscriptions exists. When it doesn't, recorded device
	// ids are DISCARDED and every send falls back to the alias — the exact path
	// that wedges and delivers to nobody.
	TableReady bool `json:"tableReady"`
	// Devices recorded for this user, and how many are fresh enough to be used
	// (a stale row hands the user back to the alias — see pushSubscriptionsFor).
	Devices int `json:"devices"`
	Fresh   int `json:"fresh"`
	// LastSeen is the most recent recording, RFC3339, "" if none.
	LastSeen string `json:"lastSeen,omitempty"`
}

// PushStatusFor reports the push reachability of one account.
func (s *Service) PushStatusFor(userID string) PushStatus {
	out := PushStatus{
		Configured: os.Getenv("ONESIGNAL_REST_API_KEY") != "",
		TableReady: s.pushSubsReady(),
	}
	if !out.TableReady || strings.TrimSpace(userID) == "" {
		return out
	}
	rows, err := s.sb.Select("push_subscriptions",
		"user_id=eq."+store.Q(userID)+
			"&select=updated_at&order=updated_at.desc&limit=50")
	if err != nil {
		return out
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -45)
	for i, r := range rows {
		out.Devices++
		at := asStr(r, "updated_at")
		if i == 0 {
			out.LastSeen = at
		}
		if t, perr := time.Parse(time.RFC3339, at); perr == nil && t.After(cutoff) {
			out.Fresh++
		}
	}
	return out
}
