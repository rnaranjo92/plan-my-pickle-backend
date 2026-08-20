package service

import (
	"errors"
	"log"
	"os"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// The Club plan — the third subscription product, beside Premium and the coach
// plan, on the same rails: matched by PRICE ID in the webhook, written to its
// own columns, held by the club OWNER's account and covering every club they
// own (the owner-entitlement rule the archive and coach seats already use).
//
// STRICT where the coach plan is permissive, and on purpose. CoachPlanActive
// answers "may this coach keep working?" — a gate that BLOCKS, so an
// unconfigured price fails open to everyone-free. ClubPlanActive answers "does
// this club get extra benefits?" — a GRANT, and granting nothing when the plan
// isn't configured is simply today's founding-access reality: comped founding
// clubs pass via `comped`, nobody else passes, and setting
// STRIPE_CLUB_PRICE_ID in Railway is the single act that opens the tier for
// purchase. No deploy, no flag day.

// clubPriceID is the Stripe price for the Club plan. Empty = the tier can't be
// bought yet, and club subscription events are ignored rather than mistaken
// for Premium.
func clubPriceID() string {
	return strings.TrimSpace(os.Getenv("STRIPE_CLUB_PRICE_ID"))
}

// isClubPlanEvent decides whether a subscription webhook is about the Club
// plan. Price-matching is the only safe test — see isCoachPlanEvent.
func isClubPlanEvent(priceID string) bool {
	cp := clubPriceID()
	return cp != "" && priceID == cp
}

// applyClubPlanEvent writes the Club subscription onto its own columns.
func (s *Service) applyClubPlanEvent(ev gateway.SubscriptionEvent) error {
	// Money path: a transient probe failure must NOT read as "column missing".
	// columnReady swallows the error and returns false, which would drop a paid
	// subscription on the floor — Stripe sees our 200, never redelivers, and the
	// organizer is charged $59 for a plan we never recorded. columnReadyErr lets
	// the error out so the webhook 500s and Stripe retries.
	ready, err := s.columnReadyErr("pmp_profiles", "club_plan")
	if err != nil {
		return err
	}
	if !ready {
		log.Printf("subscriptions: club plan event ignored — run add_club_plan.sql")
		return nil
	}
	row := map[string]any{
		"club_plan":                ev.Active,
		"club_subscription_status": orNull(ev.Status),
	}
	if ev.SubscriptionID != "" {
		row["club_subscription_id"] = ev.SubscriptionID
	}
	if ev.CustomerID != "" {
		row["stripe_customer_id"] = ev.CustomerID
	}
	if ev.UserID != "" {
		row["user_id"] = ev.UserID
		_, err := s.sb.Upsert("pmp_profiles", "user_id", row)
		return err
	}
	if ev.CustomerID != "" {
		_, err := s.sb.Update("pmp_profiles",
			"stripe_customer_id=eq."+store.Q(ev.CustomerID), row)
		return err
	}
	return nil
}

// ClubPlanActive reports whether this account holds the Club tier.
//
// True for: everyone while subscriptions are globally off (parity with
// IsPremium); comped accounts (founding clubs — the column Stripe never
// touches); and a live club_plan subscription. NOT true merely because the
// price is unconfigured — see the file comment for why this is strict where
// the coach plan is permissive.
func (s *Service) ClubPlanActive(userID string) bool {
	if !SubscriptionsEnabled() {
		return true
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	sel := "comped"
	withPlan := s.columnReady("pmp_profiles", "club_plan")
	if withPlan {
		sel = "comped,club_plan"
	}
	if !s.columnReady("pmp_profiles", "comped") {
		if !withPlan {
			return false
		}
		sel = "club_plan"
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select="+sel)
	if err != nil || row == nil {
		// Fail CLOSED: this grants paid benefits, and a lookup blip granting
		// them to everyone is the wrong direction. The benefits it gates are
		// extras (branding, seats, posters) — nothing a club needs to play.
		return false
	}
	return asBool(row, "comped") || asBool(row, "club_plan")
}

// StartClubCheckout opens a Stripe subscription Checkout for the Club plan.
func (s *Service) StartClubCheckout(
	userID, email, successURL, cancelURL string,
) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	priceID := clubPriceID()
	if priceID == "" {
		return "", errors.New(
			"the Club plan isn't open for purchase yet — clubs are in founding access")
	}
	if s.ClubPlanActive(userID) {
		return "", errors.New("you already have the Club plan")
	}
	// Stripe Checkout creates a NEW subscription; it never replaces an existing
	// one. Club is a superset of Premium, so the natural buyer is someone already
	// paying for Premium — and letting them through here bills them $15 AND $59,
	// every month, until they notice. Refuse and say what to do instead: they
	// keep Premium until the period they already paid for ends.
	if s.hasPremiumSubscription(userID) {
		return "", errors.New(
			"cancel your Premium subscription first, then upgrade to Club — " +
				"otherwise Stripe would bill you for both plans")
	}
	return gw.CreateSubscriptionCheckout(email, userID, priceID, successURL, cancelURL)
}

// hasPremiumSubscription reports whether a PAID Premium subscription is live on
// this account. Deliberately not IsPremium: that is true for comped accounts and
// (now) for club_plan holders, neither of which is a second thing Stripe bills.
// Only a real subscription id can double-charge.
func (s *Service) hasPremiumSubscription(userID string) bool {
	if !s.columnReady("pmp_profiles", "stripe_subscription_id") {
		return false
	}
	row, err := s.sb.SelectOne("pmp_profiles", "user_id=eq."+store.Q(userID)+
		"&select=premium,stripe_subscription_id")
	if err != nil || row == nil {
		return false
	}
	return asBool(row, "premium") &&
		strings.TrimSpace(asStr(row, "stripe_subscription_id")) != ""
}

// ClubPlanStatus is what the app renders: whether the caller holds the tier,
// and whether it can be bought at all (the UI shows no upgrade button while
// the tier isn't purchasable — founding access with a dead Buy button would
// contradict the page that says it's free).
//
// `active` reads the RAW columns rather than ClubPlanActive, which reports true
// for everybody while subscriptions are globally off. That is right for gating
// features (everything is free then) but wrong for a billing tile: it would tell
// the entire user base they hold a $59/mo plan, and promise monthly AI posters
// that the poster allowance refuses in that same state.
func (s *Service) ClubPlanStatus(userID string) map[string]any {
	held := s.clubPlanHeld(userID)
	return map[string]any{
		"active":      held,
		"purchasable": clubPriceID() != "" && !held && !s.ClubPlanActive(userID),
	}
}

// clubPlanHeld is "does this account actually hold Club" — a paid club_plan or a
// comp — ignoring the subscriptions-off global.
func (s *Service) clubPlanHeld(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	sel := ""
	if s.columnReady("pmp_profiles", "club_plan") {
		sel = "club_plan"
	}
	if s.columnReady("pmp_profiles", "comped") {
		if sel != "" {
			sel += ","
		}
		sel += "comped"
	}
	if sel == "" {
		return false
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select="+sel)
	if err != nil || row == nil {
		return false
	}
	return asBool(row, "club_plan") || asBool(row, "comped")
}
