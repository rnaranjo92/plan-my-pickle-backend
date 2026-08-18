package service

import (
	"os"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// What a CLUB costs money for — the same line the leagues already draw.
//
// Leagues: the current season is free, the archive is paid. Clubs: this year is
// free, the all-time record is paid. One sentence covers the whole product —
// *what you're doing now is free, what you've accumulated is Club* — and both
// halves turn on at the same moment, because both read the same paywallEpoch.
//
// A club's all-time cross-event leaderboard is the thing a spreadsheet cannot
// do and the reason a club stays. It is also worth nothing to a club in its
// first year, which is exactly why the free window is a year: a new club sees
// no paywall at all (every event it has ever run is in this year), and the
// value it would be buying only becomes real once it has history to lose.

// clubLeaderboardWindow is the year a free club's leaderboard is scoped to.
// Zero means "no window" — the genuine all-time record.
func (s *Service) clubLeaderboardWindow(clubID string) int {
	if s.ClubHistoryAllowed(clubID) {
		return 0
	}
	return time.Now().Year()
}

// eventInLeaderboardYear reports whether an event belongs in a leaderboard
// scoped to [year]. year==0 means all-time and everything counts.
//
// An event with no readable start date COUNTS. The window exists to decide
// which records to show, not to quietly drop a club's results because a date
// was never filled in — and an undated event is far more likely to be an old
// import than an attempt to dodge a paywall.
func eventInLeaderboardYear(startsAt string, year int) bool {
	if year == 0 {
		return true
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(startsAt))
	if err != nil {
		return true
	}
	return t.Year() >= year
}

// ClubHistoryAllowed reports whether this club's ALL-TIME record may be read.
//
// Gated on the club OWNER's entitlement, exactly like the league archive: the
// history is the club's record of itself, so it is on or off for the club as a
// whole. A member looking at their own club's leaderboard is not the customer
// and must never be the one asked to pay.
//
// Fails OPEN on every uncertainty, for the same reason ArchiveAllowed does: the
// cost of wrongly saying yes is one club seeing its history free, and the cost
// of wrongly saying no is a club losing its own record over a network blip.
func (s *Service) ClubHistoryAllowed(clubID string) bool {
	if !SubscriptionsEnabled() || !paywallActive(time.Now()) {
		return true
	}
	row, err := s.sb.SelectOne("clubs",
		"id=eq."+store.Q(clubID)+"&select=owner_id,created_at")
	if err != nil || row == nil {
		return true
	}
	// Clubs that existed before billing keep their history forever — the same
	// permanent exemption leagues get, and for the same reason.
	if predatesPaywall(asStr(row, "created_at")) {
		return true
	}
	if s.IsPremium(asStr(row, "owner_id")) {
		return true
	}
	// The same no-deploy escape hatch as the league archive: one switch turns
	// the whole paywall off if something is wrong on the night.
	return strings.EqualFold(
		strings.TrimSpace(os.Getenv("ARCHIVE_PAYWALL_OFF")), "true")
}
