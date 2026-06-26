// Command paypal_phase3_smoke exercises the REAL Phase-3 (marketplace) gateway
// against the sandbox: onboard an organizer via Partner Referrals, then — if a
// sandbox seller is provided — open a marketplace order that routes funds to that
// organizer with a platform fee, and read the organizer's merchant status.
//
//	PAYPAL_CLIENT_ID=... PAYPAL_CLIENT_SECRET=... \
//	PAYPAL_BN_CODE=...                 # PayPal-Partner-Attribution-Id (Apps&Creds -> app -> Reports)
//	PAYPAL_PARTNER_ID=...              # the platform's PayPal merchant id (for MerchantStatus)
//	PAYPAL_SELLER_MERCHANT_ID=...      # a sandbox seller's merchantIdInPayPal (to test payee+fees)
//	go run ./cmd/paypal_phase3_smoke
//
// Partner Referrals works with just the OAuth token. The marketplace order +
// merchant-status steps need the seller/partner ids and are SKIPPED if unset.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
)

func main() {
	cid, sec := os.Getenv("PAYPAL_CLIENT_ID"), os.Getenv("PAYPAL_CLIENT_SECRET")
	if cid == "" || sec == "" {
		log.Fatal("set PAYPAL_CLIENT_ID and PAYPAL_CLIENT_SECRET")
	}
	gw := gateway.NewPayPalGateway(cid, sec,
		os.Getenv("PAYPAL_BASE_URL"), os.Getenv("PAYPAL_WEBHOOK_ID"))
	partnerID := os.Getenv("PAYPAL_PARTNER_ID")
	gw.SetMarketplace(partnerID, os.Getenv("PAYPAL_BN_CODE"))

	// 1) Onboarding link for an organizer (proves Partner Referrals).
	fmt.Println("== CreatePartnerReferral (organizer onboarding) ==")
	url, err := gw.CreatePartnerReferral("ORGANIZER-smoke-1",
		"https://app.planmypickle.com/paypal/onboard/return")
	if err != nil {
		fmt.Println("  err:", err)
	} else {
		fmt.Println("  action_url (redirect the organizer here):", url)
	}

	// 2) Marketplace order — needs a sandbox seller's merchantIdInPayPal + BN.
	seller := os.Getenv("PAYPAL_SELLER_MERCHANT_ID")
	if seller == "" {
		fmt.Println("\n== Marketplace order + MerchantStatus: SKIPPED ==")
		fmt.Println("  set PAYPAL_SELLER_MERCHANT_ID (a sandbox seller's merchantIdInPayPal)")
		fmt.Println("  + PAYPAL_BN_CODE (+ PAYPAL_PARTNER_ID for status) to test payee + platform_fees")
		return
	}

	fmt.Println("\n== Marketplace order ($45.00 to organizer, $2.25 platform fee) ==")
	o, err := gw.CreateOrder(gateway.OrderParams{
		AmountCents:      4500,
		Currency:         "USD",
		RegistrationID:   "reg-mp-1",
		InvoiceID:        "PMP-reg-mp-1",
		PayeeMerchantID:  seller,
		PlatformFeeCents: 225,
		ReturnURL:        "https://app.planmypickle.com/paypal/return?reg=reg-mp-1",
		CancelURL:        "https://app.planmypickle.com/paypal/cancel?reg=reg-mp-1",
		RequestID:        "mp-reg-1",
	})
	if err != nil {
		fmt.Println("  create err:", err)
	} else {
		fmt.Printf("  order id=%s status=%s\n  APPROVE: %s\n", o.ID, o.Status, o.ApproveURL)
	}

	// 3) Can this organizer actually receive money?
	if partnerID != "" {
		recv, conf, err := gw.MerchantStatus(seller)
		fmt.Printf("\n== MerchantStatus(%s) ==\n  paymentsReceivable=%v primaryEmailConfirmed=%v err=%v\n",
			seller, recv, conf, err)
	}
}
