package service

import (
	"fmt"
	"log"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Subscription reconciliation: ask Stripe who is ACTUALLY subscribed, and
// repair our copy where it disagrees.
//
// The Stripe webhook is push-only and fire-once. If a delivery fails — Railway
// restarting mid-request, a deploy, a signature mismatch, an outage — nothing
// ever retries it from our side, and the disagreement is permanent. Someone
// pays and never gets Premium; someone cancels and keeps it. Neither shows up
// in any log, because from our end nothing happened at all.
//
// This pass runs hourly and is deliberately lopsided:
//
//   - GRANTING is cheap and safe, so it happens on Stripe's word alone.
//   - REVOKING takes something away from a person, so it happens only when
//     every one of the guards below agrees, and never on partial information.
//
// The guards on revocation, and why each exists:
//
//  1. `comped` rows are never touched. Comps are manual access with no Stripe
//     subscription behind them, so "not in Stripe" is their NORMAL state, not
//     evidence of anything. This is also why comps have their own column —
//     see IsPremium, which ORs the two.
//  2. A row with no stripe_subscription_id is never revoked. Stripe never
//     granted it, so Stripe doesn't get to take it away: that row is a hand-set
//     flag or a pre-comped-column legacy grant.
//  3. Nothing is revoked unless a direct read of THAT subscription confirms it
//     is inactive. Not its absence from a list — a truncated page or a
//     half-failed listing would otherwise revoke real subscribers en masse.
//  4. Any API error skips the row entirely. An unreachable Stripe means we know
//     nothing, and knowing nothing must never read as "not subscribed".
//
// The failure mode this leaves is: someone who cancelled keeps Premium a while
// longer. That is the correct way for this to break.

// ReconcileReport is what one pass did, for the log line and the admin endpoint.
type ReconcileReport struct {
	// Plan is "premium" or "coach".
	Plan string `json:"plan"`
	// Checked is how many subscriptions Stripe reported on this price.
	Checked int `json:"checked"`
	// Granted/Revoked are rows actually repaired.
	Granted int `json:"granted"`
	Revoked int `json:"revoked"`
	// Unmatched is active subscriptions we could not tie to a user at all.
	// Non-zero means somebody is paying and we cannot tell who — the one
	// outcome here that needs a human.
	Unmatched int `json:"unmatched"`
	// Skipped counts rows a guard protected (comped, no subscription id, or an
	// API error on the confirming read).
	Skipped int `json:"skipped"`
}

func (r ReconcileReport) String() string {
	return fmt.Sprintf("%s: checked=%d granted=%d revoked=%d unmatched=%d skipped=%d",
		r.Plan, r.Checked, r.Granted, r.Revoked, r.Unmatched, r.Skipped)
}

// planColumns names the per-plan columns, so premium and the coach plan can run
// through the same pass without one being able to write the other's flag. The
// coach plan has the identical missed-webhook exposure and no reason to differ.
type planColumns struct {
	name     string
	priceID  string
	flag     string // boolean column granting the plan
	status   string // stripe status column
	subID    string // stripe subscription id column
	customer string
}

func premiumColumns() planColumns {
	return planColumns{
		name: "premium", priceID: premiumPriceID(),
		flag: "premium", status: "subscription_status",
		subID: "stripe_subscription_id", customer: "stripe_customer_id",
	}
}

func coachColumns() planColumns {
	return planColumns{
		name: "coach", priceID: coachPriceID(),
		flag: "coach_plan", status: "coach_subscription_status",
		subID: "coach_subscription_id", customer: "stripe_customer_id",
	}
}

func clubColumns() planColumns {
	return planColumns{
		name: "club", priceID: clubPriceID(),
		flag: "club_plan", status: "club_subscription_status",
		subID: "club_subscription_id", customer: "stripe_customer_id",
	}
}

// ReconcileSubscriptions repairs both plans. Errors are logged per plan rather
// than aborting, so a coach-plan problem can't stop premium from reconciling.
func (s *Service) ReconcileSubscriptions() []ReconcileReport {
	if !SubscriptionsEnabled() {
		return nil
	}
	out := []ReconcileReport{}
	for _, cols := range []planColumns{premiumColumns(), coachColumns(), clubColumns()} {
		if cols.priceID == "" {
			continue // plan not sold on this install
		}
		if !s.columnReady("pmp_profiles", cols.flag) {
			continue
		}
		rep, err := s.reconcilePlan(cols)
		if err != nil {
			log.Printf("reconcile: %s pass failed: %v", cols.name, err)
			continue
		}
		// Only log when something happened or needs attention — an hourly
		// no-op line would bury the ones that matter.
		if rep.Granted > 0 || rep.Revoked > 0 || rep.Unmatched > 0 {
			log.Printf("reconcile: %s", rep)
		}
		out = append(out, rep)
	}
	return out
}

// revokeAction is what the cheap, no-API-call guards decide about one row.
type revokeAction int

const (
	// revokeProtected: a guard forbids revocation outright — counts as Skipped.
	revokeProtected revokeAction = iota
	// revokeLeaveAlone: the row is fine as it is, nothing to report.
	revokeLeaveAlone
	// revokeConfirmFirst: possibly stale — go ask Stripe about this one.
	revokeConfirmFirst
)

// revokeStep applies guards 1 and 2, plus the "still active" fast path, before
// any network call. Split out from the loop so the rules that protect people's
// access are directly testable rather than buried in a pass that needs Stripe.
func revokeStep(comped bool, subID string, inActiveList bool) revokeAction {
	// Guard 1: a comp has no Stripe subscription behind it, so Stripe's opinion
	// is not evidence about it, and it is not Stripe's to withdraw.
	if comped {
		return revokeProtected
	}
	// Guard 2: Stripe never granted this row, so Stripe can't revoke it — it's a
	// hand-set flag or a grant predating the comped column.
	if subID == "" {
		return revokeProtected
	}
	if inActiveList {
		return revokeLeaveAlone
	}
	return revokeConfirmFirst
}

// revokeConfirmed applies guards 3 and 4 to the confirming read: revoke ONLY on
// a successful read that says inactive. An error means we know nothing, and
// knowing nothing must never read as "not subscribed".
// wantPrice is the price this plan is keyed to. A subscription SWITCHED to
// another plan's price in the Stripe billing portal stays Active, so an
// active-only test would leave the old plan's flag set forever — a Club
// subscriber who downgrades to Premium would keep Club benefits for $15. A live
// price that isn't ours means this plan no longer applies, whatever its status.
func revokeConfirmed(live gateway.SubscriptionEvent, err error, wantPrice string) bool {
	if err != nil {
		return false
	}
	if !live.Active {
		return true
	}
	// Only when BOTH ids are known — an empty price on either side is missing
	// information, and missing information must never read as "not subscribed".
	return wantPrice != "" && live.PriceID != "" && live.PriceID != wantPrice
}

func (s *Service) reconcilePlan(cols planColumns) (ReconcileReport, error) {
	rep := ReconcileReport{Plan: cols.name}
	gw, ok := s.stripeGW()
	if !ok {
		return rep, ErrPaymentsNotConfigured
	}
	subs, err := gw.ListSubscriptionsForPrice(cols.priceID)
	if err != nil {
		return rep, err
	}
	rep.Checked = len(subs)

	// Active subscription ids, for the cheap "is this row still fine?" check in
	// the revoke pass. Absence from this set is a PROMPT to confirm, never a
	// conclusion (guard 3).
	activeSubIDs := map[string]bool{}
	for _, sub := range subs {
		if sub.Active {
			activeSubIDs[sub.SubscriptionID] = true
		}
	}

	// ---- grant pass ----
	for _, sub := range subs {
		if !sub.Active {
			continue
		}
		userID, err := s.resolveSubscriber(gw, sub, cols)
		if err != nil {
			rep.Skipped++
			log.Printf("reconcile: %s sub %s lookup failed: %v", cols.name, sub.SubscriptionID, err)
			continue
		}
		if userID == "" {
			rep.Unmatched++
			log.Printf("reconcile: %s sub %s (customer %s) is ACTIVE but matches no user — "+
				"somebody is paying and we can't tell who",
				cols.name, sub.SubscriptionID, sub.CustomerID)
			continue
		}
		row, err := s.sb.SelectOne("pmp_profiles",
			"user_id=eq."+store.Q(userID)+"&select="+cols.flag+","+cols.subID+","+cols.customer)
		if err != nil {
			rep.Skipped++
			continue
		}
		// Already granted AND already carrying this subscription id: nothing to do.
		// The id has to match too — otherwise a row granted by an OLD subscription
		// keeps a stale id, and the revoke pass would later confirm that dead id
		// and revoke a live subscriber.
		if row != nil && asBool(row, cols.flag) && asStr(row, cols.subID) == sub.SubscriptionID {
			continue
		}
		patch := map[string]any{
			cols.flag:   true,
			cols.status: orNull(sub.Status),
			cols.subID:  sub.SubscriptionID,
			"user_id":   userID,
		}
		// Only fill in the customer id when the profile has none.
		//
		// Premium and the coach plan share this one column, and separate
		// checkouts can create separate Stripe customers for the same person. If
		// both passes wrote it, they would overwrite each other every hour —
		// turning a harmless ambiguity into a column that flips forever and a
		// billing portal that opens the wrong account half the time.
		if sub.CustomerID != "" && (row == nil || asStr(row, cols.customer) == "") {
			patch[cols.customer] = sub.CustomerID
		}
		if _, err := s.sb.Upsert("pmp_profiles", "user_id", patch); err != nil {
			rep.Skipped++
			log.Printf("reconcile: %s grant for %s failed: %v", cols.name, userID, err)
			continue
		}
		rep.Granted++
		log.Printf("reconcile: %s GRANTED to %s (sub %s, status %s) — webhook was missed",
			cols.name, userID, sub.SubscriptionID, sub.Status)
	}

	// ---- revoke pass ----
	sel := "user_id," + cols.flag + "," + cols.subID
	comped := s.columnReady("pmp_profiles", "comped")
	if comped {
		sel += ",comped"
	}
	rows, err := s.sb.Select("pmp_profiles",
		cols.flag+"=is.true&select="+sel+"&limit=5000")
	if err != nil {
		// The grant pass already ran and is reported; only revocation is lost.
		return rep, err
	}
	for _, r := range rows {
		userID := asStr(r, "user_id")
		subID := asStr(r, cols.subID)
		isComped := comped && asBool(r, "comped")
		switch revokeStep(isComped, subID, activeSubIDs[subID]) {
		case revokeProtected:
			rep.Skipped++
			continue
		case revokeLeaveAlone:
			continue
		}
		// Guards 3 and 4: confirm against the subscription itself before taking
		// anything, and treat an unreachable Stripe as "we don't know".
		live, err := gw.GetSubscription(subID)
		if !revokeConfirmed(live, err, cols.priceID) {
			if err != nil {
				rep.Skipped++
				log.Printf("reconcile: %s confirm of %s failed, leaving %s alone: %v",
					cols.name, subID, userID, err)
			}
			continue
		}
		patch := map[string]any{
			cols.flag:   false,
			cols.status: orNull(live.Status),
		}
		if _, err := s.sb.Update("pmp_profiles",
			"user_id=eq."+store.Q(userID), patch); err != nil {
			rep.Skipped++
			log.Printf("reconcile: %s revoke for %s failed: %v", cols.name, userID, err)
			continue
		}
		rep.Revoked++
		log.Printf("reconcile: %s REVOKED from %s (sub %s, status %s) — cancellation webhook was missed",
			cols.name, userID, subID, live.Status)
	}
	return rep, nil
}

// resolveSubscriber works out which user a Stripe subscription belongs to.
//
// Three routes, most to least trustworthy:
//
//  1. subscription metadata — stamped at checkout, exact, but only present on
//     subscriptions created after that stamping was added;
//  2. the customer id already stored on a profile — reliable, but absent in
//     exactly the missed-FIRST-webhook case this pass most needs to recover;
//  3. the customer's billing email, against the two tables that key on email
//     and carry a user_id (`players`, `instructors`).
//
// Route 3 deliberately stops there rather than searching auth users by email.
// The admin user-list filter is a PARTIAL match, and a partial match that hits
// the wrong account would grant a stranger somebody else's paid plan — a worse
// outcome than not resolving at all. An unresolved subscription becomes a named
// log line the owner can act on; a mis-resolved one is silent and wrong.
//
// Returns "" (not an error) when the subscription can't be tied to anyone; the
// caller reports that as Unmatched, because it needs a human, not a retry.
func (s *Service) resolveSubscriber(gw *gateway.StripeGateway, sub gateway.SubscriptionEvent, cols planColumns) (string, error) {
	if sub.UserID != "" {
		return sub.UserID, nil
	}
	if sub.CustomerID == "" {
		return "", nil
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		cols.customer+"=eq."+store.Q(sub.CustomerID)+"&select=user_id")
	if err != nil {
		return "", err
	}
	if row != nil {
		if uid := asStr(row, "user_id"); uid != "" {
			return uid, nil
		}
	}
	email, err := gw.CustomerEmail(sub.CustomerID)
	if err != nil {
		return "", err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", nil
	}
	// players first (every registered player has a row), then instructors —
	// which matters because an ORGANIZER may have no players row at all, and
	// organizers are exactly who buys Premium.
	if uid := s.userIDByEmail(email); uid != "" {
		return uid, nil
	}
	if s.columnReady("instructors", "id") {
		row, err := s.sb.SelectOne("instructors",
			"email=eq."+store.Q(email)+"&user_id=not.is.null&select=user_id&limit=1")
		if err == nil && row != nil {
			if uid := asStr(row, "user_id"); uid != "" {
				return uid, nil
			}
		}
	}
	log.Printf("reconcile: %s sub %s billed to %s matches no account", cols.name, sub.SubscriptionID, email)
	return "", nil
}
