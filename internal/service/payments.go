package service

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// platformFeeBPS is the platform's cut of each entry-fee charge, in basis
// points (1 bp = 0.01%). 500 bps = 5%. The fee is the platform's
// application_fee_amount on the destination charge; the rest settles to the
// organizer's connected account.
const platformFeeBPS = 500

// platformFeeCapCents caps the platform's per-registration cut so big-ticket
// entries aren't taxed at the full 5% — this keeps PMP inside the flat $2-$5
// band that capped-fee rivals use (RegFox ~$4.99, PickleballTournaments ~$10
// per 2 events) instead of looking expensive on a $150+ sanctioned entry. The
// cap only bites above feeCents where 5% exceeds it ($100 entry = $5 = the cap;
// anything pricier is capped). Set to 0 to disable. 500 = $5.00. This is the USD
// figure; platformFeeCapCentsFor scales it to the charge currency's minor units.
const platformFeeCapCents = 500

// platformFeeCapCentsFor returns the per-registration fee cap in the CHARGE
// CURRENCY's minor units (~$5 USD equivalent). Expressed per-currency so a
// ₱-priced entry isn't capped at ₱5 (= 500 centavos) — which would gut the
// platform's cut on every non-USD charge. All supportedCurrencies are 2-decimal,
// so "minor units" == cents throughout. Unknown → the USD figure (safe: prod is
// USD-only; new currencies get a row here as they go live).
func platformFeeCapCentsFor(currency string) int {
	switch strings.ToLower(strings.TrimSpace(currency)) {
	case "php":
		return 28000 // ~₱280
	case "inr":
		return 42000 // ~₹420
	case "mxn":
		return 9000 // ~MX$90
	case "aed":
		return 1800 // ~د.إ18
	case "nzd":
		return 800 // ~NZ$8
	case "aud", "sgd":
		return 700 // ~A$7 / S$7
	case "cad":
		return 700 // ~C$7
	case "eur":
		return 460 // ~€4.60
	case "gbp":
		return 400 // ~£4
	default: // usd + anything not yet tuned
		return platformFeeCapCents
	}
}

// platformFeeCents computes the platform's cut (rounded down) for an entry fee:
// platformFeeBPS of the fee, capped at the charge currency's cap.
func platformFeeCents(feeCents int, currency string) int {
	if feeCents <= 0 {
		return 0
	}
	fee := feeCents * platformFeeBPS / 10000
	if cap := platformFeeCapCentsFor(currency); cap > 0 && fee > cap {
		return cap
	}
	return fee
}

// Processing-fee pass-through: when enabled, the PLAYER covers Stripe's per-charge
// processing fee so the platform's application fee (its 5%) settles CLEAN instead
// of being eroded by Stripe's ~2.9%+30¢. The organizer's net is unchanged; the
// surcharge shows as its own "Card processing fee" line at Checkout. Rates are
// estimates (Stripe's real fee varies by card/country) — slight over/under-
// recovery is acceptable. Env knobs:
//
//	PASS_PROCESSING_FEE=false   → disable (platform absorbs Stripe's fee; old behavior)
//	STRIPE_FEE_BPS=290          → percent rate, basis points (default 2.9%)
//	STRIPE_FEE_FIXED_CENTS=30   → fixed per-charge cents (default $0.30)
func passProcessingFee() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PASS_PROCESSING_FEE"))) {
	case "false", "0", "off", "no":
		return false
	}
	return true // default ON
}

func stripeFeeBPS() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("STRIPE_FEE_BPS"))); err == nil && n > 0 && n < 2000 {
		return n
	}
	return 290
}

// stripeFeeFixedCentsFor is Stripe's per-charge FIXED fee in the charge
// currency's minor units. An env override (STRIPE_FEE_FIXED_CENTS) wins across
// currencies for manual tuning; otherwise a per-currency default (Stripe's real
// figure varies by region — e.g. $0.30 US vs ₱15 PH). Unknown → the US $0.30.
func stripeFeeFixedCentsFor(currency string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("STRIPE_FEE_FIXED_CENTS"))); err == nil && n >= 0 && n < 5000 {
		return n
	}
	switch strings.ToLower(strings.TrimSpace(currency)) {
	case "php":
		return 1500 // ~₱15
	case "inr":
		return 300 // ~₹3
	case "mxn":
		return 300 // ~MX$3
	case "aed":
		return 100 // ~د.إ1
	case "gbp":
		return 20 // ~£0.20
	case "eur":
		return 25 // ~€0.25
	case "sgd":
		return 50 // ~S$0.50
	default: // usd/cad/aud/nzd + anything not yet tuned
		return 30
	}
}

// processingSurchargeCents is the amount to ADD to a charge so that, after
// Stripe's fee on the grossed-up total, the platform recovers that fee exactly —
// leaving its 5% clean. surcharge = ceil((p·amount + f) / (1 − p)), integer minor
// units. The fixed fee f is taken in the charge currency. 0 when pass-through is
// disabled or the amount is non-positive.
func processingSurchargeCents(amountCents int, currency string) int {
	if amountCents <= 0 || !passProcessingFee() {
		return 0
	}
	bps := stripeFeeBPS()
	denom := 10000 - bps
	if denom <= 0 {
		return 0
	}
	num := bps*amountCents + stripeFeeFixedCentsFor(currency)*10000
	return (num + denom - 1) / denom // ceil(num/denom)
}

// CheckoutSessionSummary is a payer-facing confirmation of a completed Checkout
// (amount actually captured), for the post-redirect "you paid $X" message.
type CheckoutSessionSummary struct {
	AmountCents int    `json:"amountCents"`
	Currency    string `json:"currency"`
	Paid        bool   `json:"paid"`
}

// sessionSummaryCache memoizes settled (paid) Checkout-session summaries. A
// session's total is immutable once paid, so caching lets the whole venue's
// post-payment confirmations (often behind one shared Wi-Fi IP) be served without
// re-hitting Stripe — sparing both the Stripe API budget and the caller. Only
// settled results are cached; pending ones can still change.
var (
	sessSumMu    sync.Mutex
	sessSumCache = map[string]sessSumEntry{}
)

type sessSumEntry struct {
	sum CheckoutSessionSummary
	at  time.Time
}

const sessSumTTL = 30 * time.Minute

// CachedCheckoutSummary returns a settled summary straight from the in-memory
// cache WITHOUT contacting Stripe (ok=false on a miss). The handler uses this to
// serve cache hits BEFORE charging the Stripe-budget rate limiter, so a venue's
// repeat/refresh confirmations never get throttled.
func (s *Service) CachedCheckoutSummary(sessionID string) (CheckoutSessionSummary, bool) {
	sessSumMu.Lock()
	defer sessSumMu.Unlock()
	if e, ok := sessSumCache[sessionID]; ok && time.Since(e.at) < sessSumTTL {
		return e.sum, true
	}
	return CheckoutSessionSummary{}, false
}

// GetCheckoutSessionSummary looks up a Stripe Checkout Session by id and returns
// the exact amount captured. Safe to expose unauthenticated: session ids (cs_…)
// are unguessable single-use tokens and we return only amount/currency/paid — no
// customer, card, or payment-intent detail. Settled results are cached.
func (s *Service) GetCheckoutSessionSummary(sessionID string) (CheckoutSessionSummary, error) {
	sessSumMu.Lock()
	if e, ok := sessSumCache[sessionID]; ok && time.Since(e.at) < sessSumTTL {
		sessSumMu.Unlock()
		return e.sum, nil
	}
	sessSumMu.Unlock()

	gw, ok := s.stripeGW()
	if !ok {
		return CheckoutSessionSummary{}, ErrPaymentsNotConfigured
	}
	info, err := gw.RetrieveCheckoutSession(sessionID)
	if err != nil {
		return CheckoutSessionSummary{}, err
	}
	sum := CheckoutSessionSummary{
		AmountCents: info.AmountTotalCents,
		Currency:    info.Currency,
		Paid:        info.PaymentStatus == "paid" || info.PaymentStatus == "no_payment_required",
	}
	if sum.Paid {
		sessSumMu.Lock()
		// Bound memory: settled entries are read within the TTL; if the map grows
		// large, drop expired entries (and, worst case, reset) before inserting.
		if len(sessSumCache) > 10000 {
			for k, e := range sessSumCache {
				if time.Since(e.at) >= sessSumTTL {
					delete(sessSumCache, k)
				}
			}
			if len(sessSumCache) > 10000 {
				sessSumCache = map[string]sessSumEntry{}
			}
		}
		sessSumCache[sessionID] = sessSumEntry{sum, time.Now()}
		sessSumMu.Unlock()
	}
	return sum, nil
}

// stripeGW returns the StripeGateway if the live Stripe processor is wired up,
// else (nil, false). Stripe Connect endpoints require it.
func (s *Service) stripeGW() (*gateway.StripeGateway, bool) {
	gw, ok := s.Pay.(*gateway.StripeGateway)
	return gw, ok
}

// PaymentsConfigured reports whether real Stripe payments are wired
// (STRIPE_SECRET_KEY set). Surfaced on /healthz.
func (s *Service) PaymentsConfigured() bool {
	_, ok := s.stripeGW()
	return ok
}

// WebhookConfigured reports whether the Stripe webhook signing secret is set —
// required to mark registrations paid. False when payments aren't configured.
func (s *Service) WebhookConfigured() bool {
	gw, ok := s.stripeGW()
	return ok && gw.WebhookReady()
}

// PaymentsMode reports the Stripe environment: "live", "test", or "off" (no
// Stripe wired). Lets /healthz confirm which keys prod is using at a glance.
func (s *Service) PaymentsMode() string {
	gw, ok := s.stripeGW()
	if !ok {
		return "off"
	}
	return gw.Mode()
}

// ErrPaymentsNotConfigured means online payments (Stripe) aren't wired up on the
// server (no STRIPE_SECRET_KEY) — the caller should fall back to manual mark-paid.
var ErrPaymentsNotConfigured = errors.New("online payments are not enabled")

// ErrOrganizerNotConnected means the event's organizer hasn't finished Stripe
// onboarding (no connected account, or charges not yet enabled), so a registrant
// can't pay online yet.
var ErrOrganizerNotConnected = errors.New("organizer has not connected a payout account yet")

// AccountStatus is the organizer's Stripe Connect onboarding state.
type AccountStatus struct {
	Connected      bool `json:"connected"`
	ChargesEnabled bool `json:"chargesEnabled"`
}

// organizerPaymentRow loads an organizer's organizer_payments row (or nil).
func (s *Service) organizerPaymentRow(ownerID string) (map[string]any, error) {
	return s.sb.SelectOne("organizer_payments",
		"owner_id=eq."+store.Q(ownerID)+"&select=owner_id,stripe_account_id,charges_enabled")
}

// StripeConnectStart begins (or resumes) an organizer's Stripe Connect
// onboarding. If they have no connected account yet, it creates a Standard
// account and stores its id in organizer_payments. Either way it returns a fresh
// Account Link (account_onboarding) URL to send the organizer to. returnURL is
// where Stripe sends them when done; refreshURL when the link expires.
func (s *Service) StripeConnectStart(ownerID, returnURL, refreshURL string) (string, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return "", errors.New("not signed in")
	}
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}

	row, err := s.organizerPaymentRow(ownerID)
	if err != nil {
		return "", err
	}
	accountID := ""
	if row != nil {
		accountID = asStr(row, "stripe_account_id")
	}

	if accountID == "" {
		// No connected account yet — create a Standard account and persist it.
		// (Email is optional; Stripe collects the real one during onboarding.)
		accountID, err = gw.CreateConnectedAccount("")
		if err != nil {
			return "", err
		}
		if _, err := s.sb.Upsert("organizer_payments", "owner_id", map[string]any{
			"owner_id":          ownerID,
			"stripe_account_id": accountID,
			"charges_enabled":   false,
			"updated_at":        now(),
		}); err != nil {
			return "", err
		}
	}

	return gw.CreateOnboardingLink(accountID, returnURL, refreshURL)
}

// StripeAccountStatus reports an organizer's Stripe Connect state. It reads the
// cached organizer_payments row and, when a connected account exists, refreshes
// charges_enabled from Stripe (and writes it back), so the dashboard reflects
// onboarding completion without waiting on the webhook.
func (s *Service) StripeAccountStatus(ownerID string) (AccountStatus, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return AccountStatus{}, errors.New("not signed in")
	}
	row, err := s.organizerPaymentRow(ownerID)
	if err != nil {
		return AccountStatus{}, err
	}
	if row == nil {
		return AccountStatus{}, nil // never started onboarding
	}
	accountID := asStr(row, "stripe_account_id")
	if accountID == "" {
		return AccountStatus{}, nil
	}
	status := AccountStatus{Connected: true, ChargesEnabled: asBool(row, "charges_enabled")}

	// Best-effort refresh from Stripe (live state may be ahead of our cache).
	if gw, ok := s.stripeGW(); ok {
		if acct, err := gw.RetrieveAccount(accountID); err == nil {
			if acct.ChargesEnabled != status.ChargesEnabled {
				status.ChargesEnabled = acct.ChargesEnabled
				_, _ = s.sb.Update("organizer_payments", "owner_id=eq."+store.Q(ownerID),
					map[string]any{"charges_enabled": acct.ChargesEnabled, "updated_at": now()})
			}
		}
	}
	return status, nil
}

// CreateCheckoutSession opens a Stripe Checkout Session for a registration's
// entry fee, routed to the event organizer's connected account via a destination
// charge (with the platform's application_fee_amount as its cut). Returns the
// hosted Checkout URL. Errors: ErrNotFound (registration/event missing),
// ErrPaymentsNotConfigured (no Stripe), a clear error if the fee is 0, and
// ErrOrganizerNotConnected if the organizer hasn't finished onboarding.
func (s *Service) CreateCheckoutSession(registrationID, successURL, cancelURL string) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=event_id")
	if err != nil {
		return "", err
	}
	if reg == nil {
		return "", ErrNotFound
	}
	eventID := asStr(reg, "event_id")
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=name,owner_id")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	// Entry fee + any opted-in add-ons (tee / overgrips) in one charge.
	fee, currency, _, err := s.registrationChargeCents(registrationID)
	if err != nil {
		return "", err
	}
	if fee <= 0 {
		return "", errors.New("this event has no entry fee")
	}
	currency = strings.ToLower(currency)
	// Snapshot the add-on cart being charged, so the webhook grants exactly this
	// selection regardless of any /addons edit made after the amount is locked.
	cart, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=addon_tee,addon_grips")
	if err != nil {
		return "", err
	}
	teeSel, gripsSel := asBool(cart, "addon_tee"), asBool(cart, "addon_grips")
	ownerID := asStr(ev, "owner_id")
	if ownerID == "" {
		return "", ErrOrganizerNotConnected
	}

	// The organizer must have a connected account with charges enabled.
	orow, err := s.organizerPaymentRow(ownerID)
	if err != nil {
		return "", err
	}
	if orow == nil {
		return "", ErrOrganizerNotConnected
	}
	accountID := asStr(orow, "stripe_account_id")
	if accountID == "" || !asBool(orow, "charges_enabled") {
		return "", ErrOrganizerNotConnected
	}

	name := strings.TrimSpace(asStr(ev, "name"))
	if name == "" {
		name = "Tournament entry fee"
	} else {
		name = name + " — entry fee"
	}

	// Pass Stripe's processing fee to the player (when enabled) so our 5% settles
	// clean. The surcharge rides as its own Checkout line item and is added to the
	// application fee, so the organizer's net (fee − platform 5%) is unchanged.
	surcharge := processingSurchargeCents(fee, currency)
	return gw.CreateCheckoutSession(gateway.CheckoutParams{
		RegistrationID:      registrationID,
		AmountCents:         fee,
		Currency:            currency,
		ProductName:         name,
		DestinationAccount:  accountID,
		ApplicationFeeCents: platformFeeCents(fee, currency) + surcharge,
		ServiceFeeCents:     surcharge,
		AddonTee:            teeSel,
		AddonGrips:          gripsSel,
		SuccessURL:          successURL,
		CancelURL:           cancelURL,
	})
}

// paymentsSelfTestName is the fixed name of the hidden, reusable event that backs
// the QA one-tap payment check (find-or-create keyed on it).
const paymentsSelfTestName = "Payments self-test"

// PaymentsSelfTest gives a QA organizer a one-tap end-to-end payment check: it
// finds-or-creates a hidden "Payments self-test" event they own (a $1 entry fee,
// unlisted), clears any prior dummy registrations, adds a fresh one, and returns a
// real Stripe Checkout URL for it. The organizer must have finished payout
// onboarding. Handler-gated to QA accounts. On LIVE keys this is a real ~$1 charge
// (refund it); on TEST keys it's free. Exercises the exact production path
// (destination charge + processing-fee pass-through + the paid webhook).
func (s *Service) PaymentsSelfTest(ownerID, successURL, cancelURL string) (string, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return "", errors.New("not signed in")
	}
	if _, ok := s.stripeGW(); !ok {
		return "", ErrPaymentsNotConfigured
	}
	// The organizer must be connected (charges enabled) or Checkout can't route.
	orow, err := s.organizerPaymentRow(ownerID)
	if err != nil {
		return "", err
	}
	if orow == nil || asStr(orow, "stripe_account_id") == "" || !asBool(orow, "charges_enabled") {
		return "", ErrOrganizerNotConnected
	}

	// Find-or-create the hidden self-test event so runs don't pile up new events.
	row, err := s.sb.SelectOne("events",
		"owner_id=eq."+store.Q(ownerID)+"&name=eq."+store.Q(paymentsSelfTestName)+"&select=id&limit=1")
	if err != nil {
		return "", err
	}
	eventID := ""
	if row != nil {
		eventID = asStr(row, "id")
	} else {
		eventID, err = s.CreateEvent(model.CreateEventRequest{
			Name:                 paymentsSelfTestName,
			Format:               "doubles",
			TournamentFormat:     "round_robin",
			NumCourts:            1,
			PointsToWin:          11,
			WinBy:                2,
			BestOf:               1,
			RegistrationFeeCents: 100, // $1.00 — smallest sensible real charge
			Currency:             "USD",
			Brackets:             []model.BracketInput{{Name: "Open", DivisionType: "open"}},
		}, ownerID)
		if err != nil {
			return "", err
		}
	}

	// NB: do NOT delete prior dummy registrations here — a still-in-flight
	// checkout.session.completed webhook needs its registration to exist to mark
	// it paid. They just accumulate in this hidden QA event (harmless).

	// A trusted dummy registrant (skips the public phone/code/approval gates).
	bracketID := ""
	if bks, berr := s.GetBrackets(eventID); berr == nil && len(bks) > 0 {
		bracketID = bks[0].ID
	}
	// Unique phone per run so the bracket-scoped duplicate guard never blocks a
	// re-test (registrations persist for their webhooks, so we can't reuse one).
	uniq := time.Now().UnixNano() % 10000000
	reg, err := s.RegisterPlayer(eventID, model.RegisterRequest{
		FullName:   "Self Test",
		Phone:      fmt.Sprintf("+1555%07d", uniq),
		BracketID:  bracketID,
		TrustedAdd: true,
	}, "")
	if err != nil {
		return "", err
	}

	return s.CreateCheckoutSession(reg.ID, successURL, cancelURL)
}

// HandleStripeWebhook verifies an incoming Stripe webhook and applies it:
//   - checkout.session.completed → mark the registration (from metadata) paid,
//     reusing the existing mark-paid path (CollectPaymentManually).
//   - account.updated → sync charges_enabled for that connected account.
//
// Other (verified) events are ignored. A signature/verification failure returns
// an error so the handler can respond 400; a successfully-ignored event returns
// nil (ack 200).
func (s *Service) HandleStripeWebhook(payload []byte, sigHeader string) error {
	gw, ok := s.stripeGW()
	if !ok {
		return ErrPaymentsNotConfigured
	}
	evt, err := gw.VerifyWebhook(payload, sigHeader)
	if err != nil {
		if errors.Is(err, gateway.ErrUnhandledWebhook) {
			return nil // verified but not a type we act on — ack and ignore
		}
		return err
	}
	// Premium subscription lifecycle (subscription checkout / updated / deleted)
	// — flip premium on the account.
	if evt.Subscription != nil {
		return s.applySubscriptionEvent(*evt.Subscription)
	}
	switch evt.Type {
	case "checkout.session.completed":
		// A one-time per-event Premium pass — unlock the event.
		if evt.EventPassID != "" {
			return s.grantEventPass(evt.EventPassID)
		}
		// A vendor booth fee — confirm the booth.
		if evt.VendorID != "" {
			return s.MarkVendorPaid(evt.VendorID)
		}
		// A paid Match Video Analysis — mark paid + submit the video to PB Vision.
		if evt.AnalysisID != "" {
			return s.markAnalysisPaid(evt.AnalysisID)
		}
		// A paid class enrollment — mark the seat paid (store the PaymentIntent so
		// the seat can be refunded if the class/seat is later torn down).
		if evt.EnrollmentID != "" {
			return s.markEnrollmentPaid(evt.EnrollmentID, evt.PaymentIntentID)
		}
		// A class-pack purchase — grant the credits (idempotent on the PaymentIntent
		// so a redelivered webhook can't double-grant).
		if evt.PackPurchase != "" {
			return s.grantPackCredits(evt.PackPurchase, evt.PaymentIntentID)
		}
		if evt.RegistrationID == "" {
			return nil // nothing to attribute the payment to
		}
		// Record what Stripe ACTUALLY captured (not a recomputed total), and set
		// the add-on flags to exactly what the paid session covered — so a cart
		// edited after the session's amount was locked can neither overstate the
		// recorded revenue nor grant unpaid goods.
		return s.CollectPaidFromStripe(evt.RegistrationID, evt.AmountCents, evt.AddonTee, evt.AddonGrips)
	case "account.updated":
		if evt.AccountID == "" {
			return nil
		}
		_, err := s.sb.Update("organizer_payments",
			"stripe_account_id=eq."+store.Q(evt.AccountID),
			map[string]any{"charges_enabled": evt.ChargesEnabled, "updated_at": now()})
		return err
	default:
		return nil
	}
}

// applySubscriptionEvent writes Premium subscription state onto the account.
// Checkout-completed carries the user_id (upsert the row); a later
// subscription.updated/deleted only has the Stripe customer id (update by it).
// coachPriceID is the Stripe price for the coach plan. Empty = the plan isn't
// configured, and every coach event is then ignored rather than being mistaken
// for Premium.
func coachPriceID() string {
	return strings.TrimSpace(os.Getenv("STRIPE_COACH_PRICE_ID"))
}

func premiumPriceID() string {
	return strings.TrimSpace(os.Getenv("STRIPE_PREMIUM_PRICE_ID"))
}

// isCoachPlanEvent decides which product a subscription webhook is about.
//
// Matching on the price is the ONLY safe test: the two plans are separate
// products bought by different people, and without this every subscription
// looked alike and set the same `premium` flag — so a coach would have been
// granted organizer Premium they never bought, and cancelling one plan would
// have revoked the other.
func isCoachPlanEvent(priceID string) bool {
	cp := coachPriceID()
	return cp != "" && priceID == cp
}

func (s *Service) applySubscriptionEvent(ev gateway.SubscriptionEvent) error {
	if isCoachPlanEvent(ev.PriceID) {
		return s.applyCoachPlanEvent(ev)
	}
	// A price we don't recognise is NOT assumed to be Premium. An unknown price
	// means a plan added in the Stripe dashboard that this build doesn't know
	// about, and silently granting Premium for it is how a cheap add-on becomes
	// a free upgrade. Only an unset premium price (older installs) falls through.
	if pp := premiumPriceID(); pp != "" && ev.PriceID != "" && ev.PriceID != pp {
		log.Printf("subscriptions: ignoring event for unknown price %q", ev.PriceID)
		return nil
	}
	row := map[string]any{
		"premium":             ev.Active,
		"subscription_status": orNull(ev.Status),
	}
	if ev.SubscriptionID != "" {
		row["stripe_subscription_id"] = ev.SubscriptionID
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

// applyCoachPlanEvent writes the coach subscription onto its OWN columns, so
// the two plans can be held, managed and cancelled independently.
func (s *Service) applyCoachPlanEvent(ev gateway.SubscriptionEvent) error {
	if !s.columnReady("pmp_profiles", "coach_plan") {
		log.Printf("subscriptions: coach plan event ignored — run add_coach_plan.sql")
		return nil
	}
	row := map[string]any{
		"coach_plan":                ev.Active,
		"coach_subscription_status": orNull(ev.Status),
	}
	if ev.SubscriptionID != "" {
		row["coach_subscription_id"] = ev.SubscriptionID
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

// SubscriptionsEnabled gates the paid Premium plan. Default OFF: while off, the
// whole subscription flow is disabled (so no one can accidentally subscribe) and
// EVERYONE is treated as Premium (all features free). Flip SUBSCRIPTIONS_ENABLED
// =true in the env to turn the paid plan on — no code deploy needed.
func SubscriptionsEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSCRIPTIONS_ENABLED")), "true")
}

// IsPremium reports whether the account currently has an active Premium plan.
// While subscriptions are OFF, everyone is Premium (all features free).
func (s *Service) IsPremium(userID string) bool {
	if !SubscriptionsEnabled() {
		return true
	}
	if userID == "" {
		return false
	}
	sel := "premium"
	// `comped` is manual access, recorded separately from `premium` because
	// Stripe OWNS premium: a subscription webhook overwrites it, and a
	// reconciler asking Stripe "who is actually subscribed?" would revoke every
	// tester, since none of them are. Reading both means a comp survives billing.
	comped := s.columnReady("pmp_profiles", "comped")
	if comped {
		sel = "premium,comped"
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select="+sel)
	if err != nil || row == nil {
		return false
	}
	return asBool(row, "premium") || (comped && asBool(row, "comped"))
}

// CompedAccount is one manually-granted account, for the owner's review list.
type CompedAccount struct {
	UserID   string `json:"userId"`
	Name     string `json:"name,omitempty"`
	Reason   string `json:"reason,omitempty"`
	CompedAt string `json:"compedAt,omitempty"`
	CompedBy string `json:"compedBy,omitempty"`
	// Premium is the Stripe-owned flag. True alongside a comp usually means the
	// comp was granted by hand on `premium` before comps had their own column.
	Premium bool `json:"premium"`
}

// ListCompedAccounts returns every manually-granted account.
//
// Comps used to live in six places — two code allowlists, two env vars, and
// hand-set premium rows — with no record of who granted one or why. This is the
// list that makes them reviewable instead of archaeological.
func (s *Service) ListCompedAccounts() ([]CompedAccount, error) {
	if !s.columnReady("pmp_profiles", "comped") {
		return nil, fmt.Errorf("%w: run add_comped_access.sql", ErrCoachingUnavailable)
	}
	rows, err := s.sb.Select("pmp_profiles",
		"comped=is.true&select=user_id,full_name,premium,comp_reason,comped_at,comped_by"+
			"&order=comped_at.desc&limit=500")
	if err != nil {
		return nil, err
	}
	out := make([]CompedAccount, 0, len(rows))
	for _, r := range rows {
		uid := asStr(r, "user_id")
		// full_name is often empty — an organizer who never filled in a profile
		// has none — and a comp list that says NULL is a list nobody can review.
		// resolveDisplayName falls back through the players row, the signup name
		// in auth metadata, and finally the email's local part, so every entry
		// identifies somebody.
		name := strings.TrimSpace(asStr(r, "full_name"))
		if name == "" {
			name = s.resolveDisplayName(uid, "")
		}
		out = append(out, CompedAccount{
			UserID:   uid,
			Name:     name,
			Reason:   asStr(r, "comp_reason"),
			CompedAt: asStr(r, "comped_at"),
			CompedBy: asStr(r, "comped_by"),
			Premium:  asBool(r, "premium"),
		})
	}
	return out, nil
}

// SetComped grants or revokes a comp, recording WHY and by whom.
func (s *Service) SetComped(userID, reason, by string, on bool) error {
	// Argument checks BEFORE touching the database: rejecting a blank user id
	// shouldn't cost a round trip, and the caller gets the useful error rather
	// than a migration message that isn't their problem.
	if strings.TrimSpace(userID) == "" {
		return errors.New("a user is required")
	}
	if on && strings.TrimSpace(reason) == "" {
		return errors.New("give a reason for the comp")
	}
	if !s.columnReady("pmp_profiles", "comped") {
		return fmt.Errorf("%w: run add_comped_access.sql", ErrCoachingUnavailable)
	}
	row := map[string]any{"user_id": userID, "comped": on}
	if on {
		row["comp_reason"] = strings.TrimSpace(reason)
		row["comped_at"] = now()
		row["comped_by"] = orNull(strings.TrimSpace(by))
	}
	_, err := s.sb.Upsert("pmp_profiles", "user_id", row)
	return err
}

// eventPremiumUnlocked reports whether an event has Premium features available —
// either its owner has an active subscription, or a one-time per-event pass was
// purchased for it (events.premium_pass). The passed row must include owner_id
// and premium_pass (GetEvent uses select=*; callers with a narrow select must
// add premium_pass to it).
func (s *Service) eventPremiumUnlocked(ev map[string]any) bool {
	return s.IsPremium(asStr(ev, "owner_id")) || asBool(ev, "premium_pass")
}

// EventPremiumUnlocked loads an event and reports whether its Premium features
// are unlocked (owner subscribed OR a per-event pass was purchased). Used to
// server-side gate Premium-only per-event features (sponsor watermark, scoreboard
// theme) so a UI-only lock can't be bypassed via a direct API call.
func (s *Service) EventPremiumUnlocked(eventID string) bool {
	row, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=owner_id,premium_pass")
	if err != nil || row == nil {
		return false
	}
	return s.eventPremiumUnlocked(row)
}

// grantEventPass marks an event Premium-unlocked after its one-time pass is paid.
func (s *Service) grantEventPass(eventID string) error {
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"premium_pass": true, "updated_at": now()})
	return err
}

// PremiumStatus is the caller's Premium plan state for the Profile UI.
type PremiumStatus struct {
	Premium   bool   `json:"premium"`
	Status    string `json:"status,omitempty"`
	CanManage bool   `json:"canManage"` // has a Stripe customer → billing portal works
}

// GetPremiumStatus returns the caller's Premium state (best-effort).
func (s *Service) GetPremiumStatus(userID string) PremiumStatus {
	if userID == "" {
		return PremiumStatus{}
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=premium,subscription_status,stripe_customer_id")
	if err != nil || row == nil {
		return PremiumStatus{}
	}
	return PremiumStatus{
		Premium:   asBool(row, "premium"),
		Status:    asStr(row, "subscription_status"),
		CanManage: asStr(row, "stripe_customer_id") != "",
	}
}

// StartPremiumCheckout opens a Stripe subscription Checkout for the Premium plan.
func (s *Service) StartPremiumCheckout(userID, email, successURL, cancelURL string) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	priceID := strings.TrimSpace(os.Getenv("STRIPE_PREMIUM_PRICE_ID"))
	if priceID == "" {
		return "", errors.New("premium plan is not configured")
	}
	return gw.CreateSubscriptionCheckout(email, userID, priceID, successURL, cancelURL)
}

// kFreeCoachStudents is how many students a coach can carry for free.
//
// The limit is on USAGE, not features: a coach on the free tier gets video
// feedback, drills, skill ratings and bookings in full — they just can't grow
// past a handful of students. Products that cap usage convert roughly 1.5-2x
// better than products that withhold features, and it means nobody evaluates
// the product with the good parts switched off.
const kFreeCoachStudents = 3

// CoachPlanActive reports whether this coach holds the paid coach plan.
// Independent of Premium — the two are different products.
//
// While SUBSCRIPTIONS_ENABLED is off, everyone is treated as subscribed, so the
// student cap can't strand a coach on a build where billing isn't live yet.
func (s *Service) CoachPlanActive(userID string) bool {
	if !SubscriptionsEnabled() {
		return true
	}
	// If the plan can't be BOUGHT, it can't be enforced. SUBSCRIPTIONS_ENABLED is
	// already on in production for organizer Premium, so without this the coach
	// cap would switch itself on the moment add_coach_plan.sql ran — capping
	// coaches while StartCoachPlanCheckout still refuses for want of a price.
	// Blocked with no way to pay is worse than free.
	if coachPriceID() == "" {
		return true
	}
	if strings.TrimSpace(userID) == "" {
		return false
	}
	// Founding and comped coaches, by email, from the env — so a comp can be
	// granted or revoked in Railway without a deploy. These are the people who
	// used the product before it had a price; charging them retroactively is
	// how you lose the coaches who vouched for you.
	if s.coachComped(userID) {
		return true
	}
	if !s.columnReady("pmp_profiles", "coach_plan") {
		return true // plan not migrated in yet — don't gate on a column that isn't there
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=coach_plan")
	if err != nil || row == nil {
		// Fail OPEN. A lookup failure must not lock a coach out of students they
		// already teach; the worst case is a free month, not a broken roster.
		return true
	}
	if asBool(row, "coach_plan") {
		return true
	}
	// LAST, and only for a coach who isn't paying for themselves: is a club
	// carrying them? Checked here rather than earlier because it costs a couple
	// of queries and almost every caller is answered before reaching it.
	return s.clubSponsoredCoach(userID)
}

// foundingCoachEmails are comped in CODE, not config.
//
// These are the coaches who used the product before it had a price. The comp is
// hardcoded rather than left to an env var because "Austen keeps his free plan"
// must not depend on anyone remembering to set a variable in Railway — a
// forgotten env is exactly how a founding user gets billed by accident.
// COMPED_COACH_EMAILS extends this list; it can't shorten it.
var foundingCoachEmails = []string{
	"asveom@lt.life", // Austen — first coach on the platform, Life Time
}

// compedCoachEmails is the founding list plus the COMPED_COACH_EMAILS env
// allowlist, so later comps can be granted without a deploy.
func compedCoachEmails() map[string]bool {
	out := map[string]bool{}
	for _, e := range foundingCoachEmails {
		out[strings.ToLower(strings.TrimSpace(e))] = true
	}
	for _, e := range strings.Split(os.Getenv("COMPED_COACH_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out[e] = true
		}
	}
	return out
}

// coachComped reports whether this coach is on the comped allowlist.
//
// Resolved through `instructors`, which is keyed by email and carries user_id —
// the same table that grants coach access in the first place, so a comp can
// only ever apply to somebody who is actually a coach.
func (s *Service) coachComped(userID string) bool {
	list := compedCoachEmails()
	if len(list) == 0 || !s.columnReady("instructors", "id") {
		return false
	}
	row, err := s.sb.SelectOne("instructors",
		"user_id=eq."+store.Q(userID)+"&select=email")
	if err != nil || row == nil {
		return false
	}
	return list[strings.ToLower(strings.TrimSpace(asStr(row, "email")))]
}

// StartCoachPlanCheckout opens a Stripe subscription Checkout for the coach plan.
func (s *Service) StartCoachPlanCheckout(
	userID, email, successURL, cancelURL string) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	priceID := coachPriceID()
	if priceID == "" {
		return "", errors.New("the coach plan is not configured")
	}
	return gw.CreateSubscriptionCheckout(email, userID, priceID, successURL, cancelURL)
}

// StartEventPassCheckout opens a one-time Stripe Checkout for the per-event
// Premium pass (event ownership is enforced by the route). On success the
// webhook flips events.premium_pass via grantEventPass.
func (s *Service) StartEventPassCheckout(eventID, email, successURL, cancelURL string) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	priceID := strings.TrimSpace(os.Getenv("STRIPE_EVENT_PASS_PRICE_ID"))
	if priceID == "" {
		return "", errors.New("the per-event pass is not configured")
	}
	return gw.CreateOneTimeCheckout(email, eventID, "event_pass_id", priceID, successURL, cancelURL)
}

// BillingPortal opens the Stripe billing portal for the caller to manage/cancel.
func (s *Service) BillingPortal(userID, returnURL string) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=stripe_customer_id")
	if err != nil {
		return "", err
	}
	cust := ""
	if row != nil {
		cust = asStr(row, "stripe_customer_id")
	}
	if cust == "" {
		return "", errors.New("no subscription to manage")
	}
	return gw.CreateBillingPortalSession(cust, returnURL)
}
