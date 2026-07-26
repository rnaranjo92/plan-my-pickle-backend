package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/account"
	"github.com/stripe/stripe-go/v79/accountlink"
	billingportalsession "github.com/stripe/stripe-go/v79/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v79/checkout/session"
	"github.com/stripe/stripe-go/v79/webhook"
)

// StripeGateway is the REAL payment processor: Stripe Connect with STANDARD
// connected accounts + DESTINATION CHARGES. The platform (PlanMyPickle's Stripe
// account, identified by the secret key) creates Checkout Sessions; funds route
// to the ORGANIZER's connected account via transfer_data.destination, and the
// platform keeps an application_fee_amount as its cut. Organizers keep their own
// Stripe — they connect once via the onboarding (Account Link) flow.
//
// It implements PaymentGateway. Live() is true, so CollectPayment treats it as a
// real processor (it never self-confirms a fee-bearing registration from the
// mock). The actual money movement happens through Checkout + webhooks, not the
// Charge() method — Charge here only signals "this is a live processor".
//
// Wired in from main only when STRIPE_SECRET_KEY is set; otherwise the server
// keeps the MockPayment (always-succeeds, Live()=false).
type StripeGateway struct {
	client *stripeClient
	// webhookSecret verifies the signature on incoming Stripe webhooks
	// (STRIPE_WEBHOOK_SECRET). Empty disables verification → events are rejected.
	webhookSecret string
	// mode is "live" | "test" | "unknown", derived from the secret-key prefix, so
	// /healthz can report which Stripe environment prod is pointed at.
	mode string
}

// stripeClient bundles the per-instance Stripe API client built from the secret
// key, so we don't mutate the package-global stripe.Key.
type stripeClient struct {
	accounts *account.Client
	links    *accountlink.Client
	sessions *checkoutsession.Client
	portal   *billingportalsession.Client
}

// NewStripeGateway builds a Stripe-backed payment gateway from the platform's
// secret key (sk_…) and the webhook signing secret (whsec_…).
func NewStripeGateway(secretKey, webhookSecret string) *StripeGateway {
	backends := stripe.NewBackends(nil) // default HTTP backends (Stripe's API)
	mode := "unknown"
	switch {
	case strings.HasPrefix(secretKey, "sk_live"), strings.HasPrefix(secretKey, "rk_live"):
		mode = "live"
	case strings.HasPrefix(secretKey, "sk_test"), strings.HasPrefix(secretKey, "rk_test"):
		mode = "test"
	}
	return &StripeGateway{
		client: &stripeClient{
			accounts: &account.Client{B: backends.API, Key: secretKey},
			links:    &accountlink.Client{B: backends.API, Key: secretKey},
			sessions: &checkoutsession.Client{B: backends.API, Key: secretKey},
			portal:   &billingportalsession.Client{B: backends.API, Key: secretKey},
		},
		webhookSecret: webhookSecret,
		mode:          mode,
	}
}

// Mode reports the Stripe environment ("live" | "test" | "unknown") from the
// secret-key prefix, so /healthz can confirm which keys prod is using.
func (g *StripeGateway) Mode() string { return g.mode }

// Live reports that this is a real payment processor.
func (g *StripeGateway) Live() bool { return true }

// WebhookReady reports whether the webhook signing secret is set. Without it,
// incoming Stripe webhooks are rejected — so paid registrations would never get
// marked paid. Surfaced on /healthz so a missing secret is caught immediately.
func (g *StripeGateway) WebhookReady() bool { return g.webhookSecret != "" }

// HostedCheckout: Stripe money moves only via CreateCheckoutSession + the
// webhook (CollectPaidFromStripe). Charge() below is a no-op, so the public
// /pay path must record PENDING, never mark the registration paid.
func (g *StripeGateway) HostedCheckout() bool { return true }

// Charge satisfies PaymentGateway. Stripe Connect collects money through hosted
// Checkout + webhooks (CreateCheckoutSession / VerifyWebhook), not a synchronous
// server-side charge, so this is intentionally a no-op success: its only role in
// the existing flow is that Live()=true keeps the public /pay path from
// self-confirming a fee-bearing registration.
func (g *StripeGateway) Charge(registrationID string, amountCents int, currency, provider string) (PaymentResult, error) {
	return PaymentResult{
		OK:          true,
		Provider:    "stripe",
		ProviderRef: "",
		AmountCents: amountCents,
		Currency:    currency,
	}, nil
}

// stripeCtx bounds every outbound Stripe API call so a slow Stripe response
// can't tie up a request goroutine.
func stripeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// ConnectAccount represents a connected account's onboarding/charge state.
type ConnectAccount struct {
	AccountID      string
	ChargesEnabled bool
}

// CreateConnectedAccount creates a STANDARD connected account for an organizer
// and returns its id (acct_…). The organizer then completes onboarding via an
// Account Link (CreateOnboardingLink).
func (g *StripeGateway) CreateConnectedAccount(email string) (string, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	// Express accounts match the "you collect payments and pay recipients"
	// (destination-charge) model: the platform is the merchant of record, keeps an
	// application fee, and pays out the organizer. The connected account only needs
	// the `transfers` capability to RECEIVE the payout — it never processes card
	// charges itself, so we deliberately do NOT request card_payments (that would
	// gate readiness on a capability we don't use, and can sit "pending" on a new
	// account). Stripe runs KYC via the hosted onboarding link.
	params := &stripe.AccountParams{
		Type: stripe.String(string(stripe.AccountTypeExpress)),
		Capabilities: &stripe.AccountCapabilitiesParams{
			Transfers: &stripe.AccountCapabilitiesTransfersParams{
				Requested: stripe.Bool(true),
			},
		},
	}
	params.Context = ctx
	if email != "" {
		params.Email = stripe.String(email)
	}
	acct, err := g.client.accounts.New(params)
	if err != nil {
		return "", err
	}
	return acct.ID, nil
}

// CreateOnboardingLink returns a one-time Stripe-hosted onboarding URL for a
// connected account (type=account_onboarding). returnURL/refreshURL are where
// Stripe sends the organizer back when they finish or the link expires.
func (g *StripeGateway) CreateOnboardingLink(accountID, returnURL, refreshURL string) (string, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	params := &stripe.AccountLinkParams{
		Account:    stripe.String(accountID),
		ReturnURL:  stripe.String(returnURL),
		RefreshURL: stripe.String(refreshURL),
		Type:       stripe.String(string(stripe.AccountLinkTypeAccountOnboarding)),
	}
	params.Context = ctx
	link, err := g.client.links.New(params)
	if err != nil {
		return "", err
	}
	return link.URL, nil
}

// RetrieveAccount fetches a connected account's current charges-enabled state
// from Stripe (used to refresh the cached organizer_payments row).
func (g *StripeGateway) RetrieveAccount(accountID string) (ConnectAccount, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	params := &stripe.AccountParams{}
	params.Context = ctx
	acct, err := g.client.accounts.GetByID(accountID, params)
	if err != nil {
		return ConnectAccount{}, err
	}
	// Readiness = payouts_enabled (this account receives payouts, never processes
	// card charges), stored under the legacy ChargesEnabled field.
	return ConnectAccount{AccountID: acct.ID, ChargesEnabled: acct.PayoutsEnabled}, nil
}

// CheckoutSessionInfo is a minimal, payer-safe summary of a Checkout Session,
// used to confirm the exact amount captured after a redirect back to the app.
type CheckoutSessionInfo struct {
	AmountTotalCents int
	Currency         string // ISO code, uppercased
	PaymentStatus    string // "paid" | "unpaid" | "no_payment_required"
	Status           string // "complete" | "open" | "expired"
}

// RetrieveCheckoutSession fetches a Checkout Session by id (cs_…) so we can show
// the payer the exact amount they were charged. All our sessions are created on
// the platform account (destination charges included), so a plain Get works.
func (g *StripeGateway) RetrieveCheckoutSession(sessionID string) (CheckoutSessionInfo, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	params := &stripe.CheckoutSessionParams{}
	params.Context = ctx
	sess, err := g.client.sessions.Get(sessionID, params)
	if err != nil {
		return CheckoutSessionInfo{}, err
	}
	return CheckoutSessionInfo{
		AmountTotalCents: int(sess.AmountTotal),
		Currency:         strings.ToUpper(string(sess.Currency)),
		PaymentStatus:    string(sess.PaymentStatus),
		Status:           string(sess.Status),
	}, nil
}

// CheckoutParams describes a destination-charge Checkout Session for an entry
// fee: AmountCents is the entry fee, DestinationAccount is the organizer's
// connected account, ApplicationFeeCents is the platform's cut.
type CheckoutParams struct {
	RegistrationID string
	// VendorID, when set (booth-fee checkout), rides in metadata instead of a
	// registration id so the webhook marks the vendor paid.
	VendorID            string
	AmountCents         int
	Currency            string
	ProductName         string
	DestinationAccount  string
	ApplicationFeeCents int
	// ServiceFeeCents, when > 0, adds a separate "Card processing fee" line item
	// so the pass-through surcharge is transparent to the player. It's part of the
	// charge total; recovering it is handled via ApplicationFeeCents.
	ServiceFeeCents int
	// Add-on cart snapshot at checkout creation — stamped into metadata so the
	// webhook grants EXACTLY what was paid for, immune to later cart edits.
	AddonTee   bool
	AddonGrips bool
	SuccessURL string
	CancelURL  string
}

// CreateCheckoutSession opens a hosted Checkout Session (mode=payment) for one
// entry-fee line item, routing funds to the organizer's connected account via
// payment_intent_data.transfer_data.destination with an application_fee_amount
// as the platform's cut. The registration id rides along in metadata so the
// webhook can mark the right registration paid. Returns the Checkout URL.
func (g *StripeGateway) CreateCheckoutSession(p CheckoutParams) (string, error) {
	ctx, cancel := stripeCtx()
	defer cancel()

	// Stripe requires lowercase ISO currency codes; events store them uppercase
	// (e.g. "PHP") for display, so normalize at the API boundary.
	currency := strings.ToLower(p.Currency)
	if currency == "" {
		currency = "usd"
	}
	name := p.ProductName
	if name == "" {
		name = "Tournament entry fee"
	}

	lineItems := []*stripe.CheckoutSessionLineItemParams{
		{
			Quantity: stripe.Int64(1),
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String(currency),
				UnitAmount: stripe.Int64(int64(p.AmountCents)),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String(name),
				},
			},
		},
	}
	// Transparent pass-through: the processing surcharge is its own line so the
	// player sees exactly what they're paying on top of the entry fee.
	if p.ServiceFeeCents > 0 {
		lineItems = append(lineItems, &stripe.CheckoutSessionLineItemParams{
			Quantity: stripe.Int64(1),
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String(currency),
				UnitAmount: stripe.Int64(int64(p.ServiceFeeCents)),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String("Card processing fee"),
				},
			},
		})
	}
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String(p.SuccessURL),
		CancelURL:  stripe.String(p.CancelURL),
		LineItems:  lineItems,
		PaymentIntentData: &stripe.CheckoutSessionPaymentIntentDataParams{
			TransferData: &stripe.CheckoutSessionPaymentIntentDataTransferDataParams{
				Destination: stripe.String(p.DestinationAccount),
			},
		},
	}
	if p.ApplicationFeeCents > 0 {
		params.PaymentIntentData.ApplicationFeeAmount = stripe.Int64(int64(p.ApplicationFeeCents))
	}
	params.Context = ctx
	if p.VendorID != "" {
		params.AddMetadata("vendor_id", p.VendorID)
		params.PaymentIntentData.AddMetadata("vendor_id", p.VendorID)
	} else {
		params.AddMetadata("registration_id", p.RegistrationID)
		// Also stamp the PaymentIntent so the metadata survives onto the charge.
		params.PaymentIntentData.AddMetadata("registration_id", p.RegistrationID)
		// Snapshot the paid-for add-on cart so the webhook can't be tricked by a
		// cart edit made after this session's amount was locked in.
		bit := func(b bool) string {
			if b {
				return "1"
			}
			return "0"
		}
		params.AddMetadata("addon_tee", bit(p.AddonTee))
		params.AddMetadata("addon_grips", bit(p.AddonGrips))
		params.PaymentIntentData.AddMetadata("addon_tee", bit(p.AddonTee))
		params.PaymentIntentData.AddMetadata("addon_grips", bit(p.AddonGrips))
	}

	sess, err := g.client.sessions.New(params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// CreateSubscriptionCheckout opens a hosted Checkout Session (mode=subscription)
// for the Premium plan (priceID). client_reference_id = userID so the webhook
// flips premium for the right account; customer_email pre-fills + lets Stripe
// reuse/create the customer. Returns the Checkout URL.
func (g *StripeGateway) CreateSubscriptionCheckout(email, userID, priceID, successURL, cancelURL string) (string, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	params := &stripe.CheckoutSessionParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		SuccessURL:        stripe.String(successURL),
		CancelURL:         stripe.String(cancelURL),
		ClientReferenceID: stripe.String(userID),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
	}
	if email != "" {
		params.CustomerEmail = stripe.String(email)
	}
	params.Context = ctx
	params.AddMetadata("user_id", userID)
	sess, err := g.client.sessions.New(params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// CreateBillingPortalSession opens the Stripe-hosted billing portal so a
// subscriber can manage or cancel their plan. Returns the portal URL.
func (g *StripeGateway) CreateBillingPortalSession(customerID, returnURL string) (string, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	}
	params.Context = ctx
	sess, err := g.client.portal.New(params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// CreateOneTimeCheckout opens a hosted Checkout Session (mode=payment) for a
// one-time charge (e.g. the per-event Premium pass) keyed to refID — set as
// client_reference_id AND metadata[metaKey] so the webhook can attribute the
// payment. Returns the Checkout URL.
func (g *StripeGateway) CreateOneTimeCheckout(email, refID, metaKey, priceID, successURL, cancelURL string) (string, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	params := &stripe.CheckoutSessionParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL:        stripe.String(successURL),
		CancelURL:         stripe.String(cancelURL),
		ClientReferenceID: stripe.String(refID),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
	}
	if email != "" {
		params.CustomerEmail = stripe.String(email)
	}
	params.Context = ctx
	params.AddMetadata(metaKey, refID)
	sess, err := g.client.sessions.New(params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// CreatePlatformCheckout opens a hosted Checkout Session (mode=payment) for a
// one-time DIRECT platform charge (NOT Connect — the platform keeps all of it),
// with an ad-hoc price so no dashboard Price ID is needed. refID is set as
// client_reference_id AND metadata[metaKey] so the webhook can attribute it.
func (g *StripeGateway) CreatePlatformCheckout(refID, metaKey string, amountCents int, currency, productName, email, successURL, cancelURL string) (string, error) {
	ctx, cancel := stripeCtx()
	defer cancel()
	cur := strings.ToLower(strings.TrimSpace(currency))
	if cur == "" {
		cur = "usd"
	}
	if strings.TrimSpace(productName) == "" {
		productName = "PlanMyPickle"
	}
	params := &stripe.CheckoutSessionParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL:        stripe.String(successURL),
		CancelURL:         stripe.String(cancelURL),
		ClientReferenceID: stripe.String(refID),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Quantity: stripe.Int64(1),
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String(cur),
					UnitAmount: stripe.Int64(int64(amountCents)),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(productName),
					},
				},
			},
		},
	}
	if email != "" {
		params.CustomerEmail = stripe.String(email)
	}
	params.Context = ctx
	params.AddMetadata(metaKey, refID)
	sess, err := g.client.sessions.New(params)
	if err != nil {
		return "", err
	}
	return sess.URL, nil
}

// ---- webhooks ----

// ErrUnhandledWebhook signals a verified-but-ignored event type, so the caller
// can ack it (200) without acting.
var ErrUnhandledWebhook = errors.New("unhandled stripe webhook event")

// WebhookEvent is the decoded payload of a handled Stripe webhook. Exactly one
// of CheckoutCompleted / AccountUpdated is set, per Type.
type WebhookEvent struct {
	Type string
	// checkout.session.completed (mode=payment)
	RegistrationID string
	// The amount Stripe actually captured (smallest currency unit) + the add-on
	// cart that was paid for — the source of truth for what to record/grant.
	AmountCents int
	AddonTee    bool
	AddonGrips  bool
	// checkout.session.completed (mode=payment) — a one-time per-event Premium pass.
	EventPassID string
	// checkout.session.completed (mode=payment) — a vendor booth fee.
	VendorID string
	// checkout.session.completed (mode=payment) — a paid Match Video Analysis.
	AnalysisID string
	// account.updated
	AccountID      string
	ChargesEnabled bool
	// subscription events (checkout.session.completed mode=subscription;
	// customer.subscription.updated / .deleted). Nil for non-subscription events.
	Subscription *SubscriptionEvent
}

// SubscriptionEvent carries Premium subscription state from a Stripe webhook.
type SubscriptionEvent struct {
	UserID         string // client_reference_id, set on the subscription Checkout
	CustomerID     string
	SubscriptionID string
	Status         string // active | trialing | past_due | canceled | unpaid | ...
	Active         bool   // status grants Premium (active or trialing)
}

// VerifyWebhook validates a webhook's signature (against STRIPE_WEBHOOK_SECRET)
// and decodes the events we care about: checkout.session.completed (→ mark a
// registration paid) and account.updated (→ refresh charges_enabled). Any other
// verified event returns ErrUnhandledWebhook so the caller can ack-and-ignore.
func (g *StripeGateway) VerifyWebhook(payload []byte, sigHeader string) (WebhookEvent, error) {
	if g.webhookSecret == "" {
		return WebhookEvent{}, errors.New("stripe webhook secret not configured")
	}
	// IgnoreAPIVersionMismatch: the Stripe account's default API version (e.g.
	// 2026-06-24.dahlia) is newer than what this stripe-go release pins, and plain
	// ConstructEvent REJECTS a mismatched-version event. We only read stable fields
	// (metadata, amount_total, payouts_enabled), so accept the event regardless.
	event, err := webhook.ConstructEventWithOptions(payload, sigHeader, g.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return WebhookEvent{}, fmt.Errorf("stripe webhook signature verification failed: %w", err)
	}

	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted:
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			return WebhookEvent{}, err
		}
		// A Premium subscription checkout — flip premium for the account.
		if sess.Mode == stripe.CheckoutSessionModeSubscription {
			sub := &SubscriptionEvent{
				UserID: sess.ClientReferenceID,
				Status: "active",
				Active: true,
			}
			if sub.UserID == "" {
				sub.UserID = sess.Metadata["user_id"]
			}
			if sess.Customer != nil {
				sub.CustomerID = sess.Customer.ID
			}
			if sess.Subscription != nil {
				sub.SubscriptionID = sess.Subscription.ID
			}
			return WebhookEvent{Type: string(event.Type), Subscription: sub}, nil
		}
		return WebhookEvent{
			Type:           string(event.Type),
			RegistrationID: sess.Metadata["registration_id"],
			EventPassID:    sess.Metadata["event_pass_id"],
			VendorID:       sess.Metadata["vendor_id"],
			AnalysisID:     sess.Metadata["analysis_id"],
			AmountCents:    int(sess.AmountTotal),
			AddonTee:       sess.Metadata["addon_tee"] == "1",
			AddonGrips:     sess.Metadata["addon_grips"] == "1",
		}, nil
	case stripe.EventTypeCustomerSubscriptionUpdated,
		stripe.EventTypeCustomerSubscriptionDeleted:
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			return WebhookEvent{}, err
		}
		active := sub.Status == stripe.SubscriptionStatusActive ||
			sub.Status == stripe.SubscriptionStatusTrialing
		if event.Type == stripe.EventTypeCustomerSubscriptionDeleted {
			active = false
		}
		ev := &SubscriptionEvent{
			SubscriptionID: sub.ID,
			Status:         string(sub.Status),
			Active:         active,
		}
		if sub.Customer != nil {
			ev.CustomerID = sub.Customer.ID
		}
		return WebhookEvent{Type: string(event.Type), Subscription: ev}, nil
	case stripe.EventTypeAccountUpdated:
		var acct stripe.Account
		if err := json.Unmarshal(event.Data.Raw, &acct); err != nil {
			return WebhookEvent{}, err
		}
		return WebhookEvent{
			Type:      string(event.Type),
			AccountID: acct.ID,
			// Readiness = payouts_enabled (see RetrieveAccount): the organizer's
			// account receives payouts, it doesn't process card charges.
			ChargesEnabled: acct.PayoutsEnabled,
		}, nil
	default:
		return WebhookEvent{Type: string(event.Type)}, ErrUnhandledWebhook
	}
}
