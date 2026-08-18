package service

import (
	"os"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// What a league costs money for.
//
// The decision, already stated where leagues are created and implemented here:
// ORGANIZING IS FREE. Creating a league, running its sessions, the live
// standings, the TV board, the court calls and the whole of the CURRENT season
// stay free forever. What's paid is the durable system-of-record — the archive
// a club accumulates by using the product for a year: past seasons' final
// standings, and carrying a roster forward into the next one.
//
// That line is deliberate. Charging for the current season would take away
// something an organizer is in the middle of; charging for the archive takes
// away nothing they have, and the value grows the longer they stay. A club that
// has never rolled a season sees no paywall at all, because it has no archive.

// paywallEpoch is the moment league archiving became a paid feature.
//
// Anything created BEFORE this keeps everything, forever. Not a grace period —
// a permanent exemption, for the same reason the ladder gate only ever checks
// at create time: taking a feature away from a league that is already running
// punishes its players for their organizer's billing, and those players did not
// sign up for a product that changes underneath them.
//
// It is a constant rather than a config value because the exemption has to mean
// the same thing forever. An env var could be edited later, silently re-gating
// leagues that were promised they never would be.
//
// Set in the future so this ships INERT: nothing is gated until the date passes
// AND subscriptions are switched on. Move it to the launch date when billing
// goes live.
const paywallEpoch = "2027-01-01T00:00:00Z"

// paywallActive reports whether the archive gate applies at all yet.
func paywallActive(now time.Time) bool {
	epoch, err := time.Parse(time.RFC3339, paywallEpoch)
	if err != nil {
		return false // an unparseable epoch must never paywall anybody
	}
	return now.After(epoch)
}

// predatesPaywall reports whether [createdAt] is early enough to be
// permanently exempt. Shared by the league archive and the club history gate —
// the exemption means the same thing for both. An unreadable or missing timestamp counts as exempt: when
// we cannot tell how old a league is, the answer that cannot hurt anyone is to
// leave it alone.
func predatesPaywall(createdAt string) bool {
	created, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
	if err != nil {
		return true
	}
	epoch, eerr := time.Parse(time.RFC3339, paywallEpoch)
	if eerr != nil {
		return true
	}
	return created.Before(epoch)
}

// ArchiveAllowed reports whether this event's season archive may be read.
//
// Gated on the OWNER's entitlement, not the viewer's: the archive is the
// organizer's record of their club, so it is either on or off for the league as
// a whole. A player looking at their own club's history is not the customer and
// must never be asked to pay to see it.
//
// Fails OPEN on every uncertainty — a lookup error, a missing event, an
// unparseable date. A billing check that guesses "no" locks people out of their
// own history over a network blip, and the cost of guessing "yes" is one club
// seeing an archive for free.
func (s *Service) ArchiveAllowed(eventID string) bool {
	if !SubscriptionsEnabled() || !paywallActive(time.Now()) {
		return true
	}
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=owner_id,created_at")
	if err != nil || row == nil {
		return true
	}
	if predatesPaywall(asStr(row, "created_at")) {
		return true
	}
	// The owner's entitlement covers a real subscription AND the `comped`
	// column, which is what founding accounts hold and what the Stripe
	// reconciler is forbidden from touching.
	if s.IsPremium(asStr(row, "owner_id")) {
		return true
	}
	// A last escape hatch that needs no deploy: if something about the gate is
	// wrong on the night, this switches it off for everyone.
	return strings.EqualFold(
		strings.TrimSpace(os.Getenv("ARCHIVE_PAYWALL_OFF")), "true")
}
