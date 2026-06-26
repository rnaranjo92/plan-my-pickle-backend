// Command paypal_smoke exercises the real PayPal gateway against the sandbox:
// create an order (intent CAPTURE, custom_id=registration id) and read it back.
// Capture needs buyer approval (interactive), so it's a two-step:
//
//	# 1) create + get the approval link:
//	PAYPAL_CLIENT_ID=... PAYPAL_CLIENT_SECRET=... go run ./cmd/paypal_smoke
//	# 2) open the approve link, log in as your SANDBOX BUYER, approve, then:
//	ORDER_ID=<id> PAYPAL_CLIENT_ID=... PAYPAL_CLIENT_SECRET=... go run ./cmd/paypal_smoke
//
// FUNDING=venmo creates a Venmo order instead of PayPal.
// PAYPAL_BASE_URL overrides the sandbox base; defaults to sandbox.
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

	const reg = "reg-smoke-123"

	if oid := os.Getenv("ORDER_ID"); oid != "" {
		fmt.Println("== CAPTURE", oid, "==")
		cap, err := gw.CaptureOrder(oid, "cap-"+reg)
		if err != nil {
			log.Fatalf("capture failed: %v", err)
		}
		fmt.Printf("  orderStatus=%s captureStatus=%s paid=%v captureId=%s custom_id=%s\n",
			cap.Status, cap.CaptureStatus, cap.Paid(), cap.CaptureID, cap.CustomID)
		fmt.Printf("  gross=%s fee=%s net=%s %s\n",
			cap.GrossValue, cap.FeeValue, cap.NetValue, cap.Currency)
		return
	}

	funding := os.Getenv("FUNDING") // "" -> paypal
	fmt.Printf("== CREATE order ($1.00, custom_id=%s, funding=%s) ==\n", reg, funding)
	o, err := gw.CreateOrder(gateway.OrderParams{
		AmountCents:    100,
		Currency:       "USD",
		RegistrationID: reg,
		InvoiceID:      "PMP-" + reg,
		FundingSource:  funding,
		BrandName:      "PlanMyPickle",
		ReturnURL:      "https://app.planmypickle.com/paypal/return?reg=" + reg,
		CancelURL:      "https://app.planmypickle.com/paypal/cancel?reg=" + reg,
		RequestID:      "create-" + reg,
	})
	if err != nil {
		log.Fatalf("create failed: %v", err)
	}
	fmt.Printf("  order id=%s status=%s\n", o.ID, o.Status)

	status, custom, gerr := gw.GetOrder(o.ID)
	fmt.Printf("  get order: status=%s custom_id=%s err=%v\n", status, custom, gerr)

	fmt.Printf("\n  APPROVE (log in as your SANDBOX BUYER): %s\n", o.ApproveURL)
	fmt.Printf("  then capture with:\n    ORDER_ID=%s go run ./cmd/paypal_smoke\n", o.ID)
}
