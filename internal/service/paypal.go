package service

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// ErrPayPalNotConfigured is returned when the PayPal/Venmo path is hit but no
// PayPal gateway is wired (PAYPAL_CLIENT_ID/SECRET unset) — the client should
// fall back to Stripe / manual.
var ErrPayPalNotConfigured = errors.New("paypal is not configured")

// StripeConfigured reports whether a live Stripe gateway is wired (for the
// client's payment-method picker).
func (s *Service) StripeConfigured() bool {
	_, ok := s.stripeGW()
	return ok
}

// PayPalConfigured reports whether the PayPal/Venmo gateway is wired.
func (s *Service) PayPalConfigured() bool { return s.PayPal != nil }

// dollarsToCents parses a PayPal money string ("12.34") into integer cents.
func dollarsToCents(s string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int(math.Round(f * 100))
}

// CreatePayPalCheckout starts a PayPal (or Venmo) order for a registration's
// entry fee and returns the approval URL to redirect the payer to. It records a
// PENDING payment intent (settled on capture, like the Stripe hosted-checkout
// path). fundingSource is "paypal" (default) or "venmo". Single-merchant today:
// funds land in the platform's PayPal account behind the API creds; per-organizer
// marketplace payout is a follow-up (SetMarketplace + PayeeMerchantID).
func (s *Service) CreatePayPalCheckout(registrationID, fundingSource, returnURL, cancelURL string) (string, error) {
	if s.PayPal == nil {
		return "", ErrPayPalNotConfigured
	}
	fee, currency, _, err := s.registrationChargeCents(registrationID)
	if err != nil {
		return "", err
	}
	if fee <= 0 {
		return "", errors.New("this event has no entry fee")
	}
	if fundingSource != "venmo" {
		fundingSource = "paypal"
	}
	order, err := s.PayPal.CreateOrder(gateway.OrderParams{
		AmountCents:    fee,
		Currency:       currency,
		RegistrationID: registrationID,
		// invoice_id must be unique per PayPal capture — namespace by reg + time so
		// a retried checkout doesn't collide with a prior order.
		InvoiceID:     registrationID + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		FundingSource: fundingSource,
		ReturnURL:     returnURL,
		CancelURL:     cancelURL,
		RequestID:     registrationID + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
	if err != nil {
		return "", err
	}
	// Pending intent keyed to the order id — the capture (return handler + webhook)
	// flips it to paid. Recorded under "paypal" for both funding sources (Venmo is
	// a PayPal funding source; the money processor is PayPal).
	_, _ = s.recordPayment(registrationID, "paypal", order.ID, fee, currency, "pending", "pending")
	return order.ApproveURL, nil
}

// CapturePayPalOrder captures an approved order (called when PayPal returns the
// payer to the app) and settles the registration as paid on success. Returns the
// registration id the order was for. Idempotent via recordPayment's paid-once
// guard, so it's safe alongside the webhook.
func (s *Service) CapturePayPalOrder(orderID string) (string, error) {
	if s.PayPal == nil {
		return "", ErrPayPalNotConfigured
	}
	cap, err := s.PayPal.CaptureOrder(orderID, orderID, "")
	if err != nil {
		return "", err
	}
	if cap.Status != "COMPLETED" || cap.CaptureStatus != "COMPLETED" {
		return cap.CustomID, fmt.Errorf("paypal order not completed (order=%s capture=%s)",
			cap.Status, cap.CaptureStatus)
	}
	amount := dollarsToCents(cap.GrossValue)
	if err := s.collectPaidFromPayPal(cap.CustomID, amount, cap.CaptureID); err != nil {
		return cap.CustomID, err
	}
	return cap.CustomID, nil
}

// collectPaidFromPayPal records the exact captured amount as a paid payment and
// flips the registration paid (mirrors CollectPaidFromStripe). ref is the PayPal
// capture id (stored for refunds).
func (s *Service) collectPaidFromPayPal(registrationID string, amountCents int, ref string) error {
	if strings.TrimSpace(registrationID) == "" || amountCents <= 0 {
		return errors.New("paypal settle: missing registration or amount")
	}
	currency := "usd"
	if reg, _ := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(registrationID)+"&select=event_id"); reg != nil {
		if ev, _ := s.sb.SelectOne("events",
			"id=eq."+store.Q(asStr(reg, "event_id"))+"&select=currency"); ev != nil {
			if c := asStr(ev, "currency"); c != "" {
				currency = c
			}
		}
	}
	_, err := s.recordPayment(registrationID, "paypal", ref, amountCents, currency, "paid", "paid")
	return err
}

// HandlePayPalWebhook verifies an incoming PayPal webhook and settles the
// registration on a completed capture (a backup to the return-path capture; both
// are idempotent). A verification failure returns an error (handler → 400); an
// unhandled-but-verified event returns nil (ack 200).
func (s *Service) HandlePayPalWebhook(h http.Header, body []byte) error {
	if s.PayPal == nil {
		return ErrPayPalNotConfigured
	}
	wh, err := s.PayPal.VerifyWebhook(h, body)
	if err != nil {
		if errors.Is(err, gateway.ErrUnhandledWebhook) {
			return nil
		}
		return err
	}
	if wh.EventType == "PAYMENT.CAPTURE.COMPLETED" && wh.CustomID != "" {
		// The webhook carries no amount — recompute the charge (== the order
		// amount). recordPayment's paid-once guard makes a double settle (this +
		// the return-path capture) a no-op.
		fee, _, _, err := s.registrationChargeCents(wh.CustomID)
		if err != nil {
			return err
		}
		return s.collectPaidFromPayPal(wh.CustomID, fee, wh.CaptureID)
	}
	return nil
}
