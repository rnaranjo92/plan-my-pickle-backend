package service

import (
	"errors"
	"os"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
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
// anything pricier is capped). Set to 0 to disable. 500 = $5.00.
const platformFeeCapCents = 500

// platformFeeCents computes the platform's cut (rounded down) for an entry fee:
// platformFeeBPS of the fee, capped at platformFeeCapCents.
func platformFeeCents(feeCents int) int {
	if feeCents <= 0 {
		return 0
	}
	fee := feeCents * platformFeeBPS / 10000
	if platformFeeCapCents > 0 && fee > platformFeeCapCents {
		return platformFeeCapCents
	}
	return fee
}

// stripeGW returns the StripeGateway if the live Stripe processor is wired up,
// else (nil, false). Stripe Connect endpoints require it.
func (s *Service) stripeGW() (*gateway.StripeGateway, bool) {
	gw, ok := s.Pay.(*gateway.StripeGateway)
	return gw, ok
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
				_, _ = s.sb.Update("organizer_payments", "owner_id="+store.Q(ownerID),
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
		"id=eq."+store.Q(eventID)+"&select=name,registration_fee_cents,currency,owner_id")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	fee := asInt(ev, "registration_fee_cents")
	if fee <= 0 {
		return "", errors.New("this event has no entry fee")
	}
	currency := strings.ToLower(asStr(ev, "currency"))
	if currency == "" {
		currency = "usd"
	}
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

	return gw.CreateCheckoutSession(gateway.CheckoutParams{
		RegistrationID:      registrationID,
		AmountCents:         fee,
		Currency:            currency,
		ProductName:         name,
		DestinationAccount:  accountID,
		ApplicationFeeCents: platformFeeCents(fee),
		SuccessURL:          successURL,
		CancelURL:           cancelURL,
	})
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
		if evt.RegistrationID == "" {
			return nil // nothing to attribute the payment to
		}
		// Reuse the existing mark-paid path so the registration's payment_status
		// and payments row are written exactly as the organizer's manual confirm.
		return s.CollectPaymentManually(evt.RegistrationID)
	case "account.updated":
		if evt.AccountID == "" {
			return nil
		}
		_, err := s.sb.Update("organizer_payments",
			"stripe_account_id="+store.Q(evt.AccountID),
			map[string]any{"charges_enabled": evt.ChargesEnabled, "updated_at": now()})
		return err
	default:
		return nil
	}
}

// applySubscriptionEvent writes Premium subscription state onto the account.
// Checkout-completed carries the user_id (upsert the row); a later
// subscription.updated/deleted only has the Stripe customer id (update by it).
func (s *Service) applySubscriptionEvent(ev gateway.SubscriptionEvent) error {
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

// IsPremium reports whether the account currently has an active Premium plan.
func (s *Service) IsPremium(userID string) bool {
	if userID == "" {
		return false
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=premium")
	return err == nil && row != nil && asBool(row, "premium")
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
