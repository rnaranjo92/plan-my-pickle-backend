package service

import (
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
	rows, err := s.sb.Select("push_subscriptions",
		"user_id="+store.In(userIDs)+"&select=subscription_id,user_id&limit=2000")
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

// forgetPushSubscriptions drops ids OneSignal has told us are invalid.
//
// Without this the table only grows: every reinstall, cleared browser and
// revoked permission leaves an id that can never receive anything, and each one
// is retried on every send forever.
func (s *Service) forgetPushSubscriptions(ids []string) {
	if len(ids) == 0 || !s.pushSubsReady() {
		return
	}
	_ = s.sb.Delete("push_subscriptions", "subscription_id="+store.In(ids))
}
