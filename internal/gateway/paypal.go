package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PayPalGateway is the REAL PayPal processor (PayPal + Venmo) for registration
// fees, alongside Stripe. It uses Orders v2 with a redirect/hosted approval flow
// (the Flutter web app is CanvasKit/no-DOM, so a hosted page beats the JS SDK
// button): create order -> redirect the payer to the approval link -> capture
// after approval. The registration id rides along as the order's custom_id, so
// the synchronous capture AND the webhook attribute the payment to it.
//
// Built per the verified PayPal spec (see docs/paypal-integration.md). Wired in
// from main only when PAYPAL_CLIENT_ID / PAYPAL_CLIENT_SECRET are set; base URL
// defaults to sandbox (PAYPAL_BASE_URL overrides for live). PAYPAL_WEBHOOK_ID is
// needed to verify incoming webhooks.
type PayPalGateway struct {
	clientID     string
	clientSecret string
	baseURL      string // no trailing slash
	webhookID    string
	httpClient   *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewPayPalGateway builds the gateway. baseURL empty -> sandbox.
func NewPayPalGateway(clientID, clientSecret, baseURL, webhookID string) *PayPalGateway {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api-m.sandbox.paypal.com"
	}
	return &PayPalGateway{
		clientID:     clientID,
		clientSecret: clientSecret,
		baseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		webhookID:    webhookID,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Live reports that this is a real payment processor.
func (g *PayPalGateway) Live() bool { return true }

// accessToken returns a cached OAuth2 client_credentials token, refreshing it a
// minute before expiry. Thread-safe.
func (g *PayPalGateway) accessToken() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Now().Before(g.tokenExp) {
		return g.token, nil
	}
	req, err := http.NewRequest(http.MethodPost,
		g.baseURL+"/v1/oauth2/token",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(g.clientID, g.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("paypal oauth http %d: %s", resp.StatusCode, ppSnippet(raw))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("paypal oauth: no access_token")
	}
	g.token = out.AccessToken
	g.tokenExp = time.Now().Add(time.Duration(out.ExpiresIn-60) * time.Second)
	return g.token, nil
}

// authed issues a Bearer-authed JSON request, with optional extra headers
// (PayPal-Request-Id idempotency, Prefer, and the Phase-3 marketplace headers).
func (g *PayPalGateway) authed(method, path string, body any, extra map[string]string) ([]byte, int, error) {
	tok, err := g.accessToken()
	if err != nil {
		return nil, 0, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// OrderParams describes one registration-fee order.
type OrderParams struct {
	AmountCents    int
	Currency       string // "" -> USD
	RegistrationID string // -> purchase_units[].custom_id (attribution)
	InvoiceID      string // -> purchase_units[].invoice_id (PayPal-enforced uniqueness)
	FundingSource  string // "paypal" (default) or "venmo"
	BrandName      string // "" -> PlanMyPickle
	ReturnURL      string // where PayPal returns after approval (?token=ORDERID)
	CancelURL      string
	RequestID      string // -> PayPal-Request-Id idempotency header
}

// PayPalOrder is the result of creating an order: its id + the hosted approval
// URL the payer is redirected to (PayPal, or the Venmo app-switch/QR).
type PayPalOrder struct {
	ID         string
	Status     string
	ApproveURL string
}

// CreateOrder opens a CAPTURE-intent order for one registration fee, scoped to
// the chosen funding source (paypal|venmo). custom_id = registrationID so the
// capture + webhook attribute the payment; the approval link is a hosted page.
func (g *PayPalGateway) CreateOrder(p OrderParams) (PayPalOrder, error) {
	currency := p.Currency
	if currency == "" {
		currency = "USD"
	}
	brand := p.BrandName
	if brand == "" {
		brand = "PlanMyPickle"
	}
	source := strings.ToLower(p.FundingSource)
	if source != "venmo" {
		source = "paypal"
	}
	value := fmt.Sprintf("%d.%02d", p.AmountCents/100, p.AmountCents%100)

	expCtx := map[string]any{
		"brand_name":          brand,
		"shipping_preference": "NO_SHIPPING",
		"user_action":         "PAY_NOW",
		"return_url":          p.ReturnURL,
		"cancel_url":          p.CancelURL,
	}
	pu := map[string]any{
		"custom_id": p.RegistrationID,
		"amount":    map[string]any{"currency_code": currency, "value": value},
	}
	if p.InvoiceID != "" {
		pu["invoice_id"] = p.InvoiceID
	}
	body := map[string]any{
		"intent":         "CAPTURE",
		"purchase_units": []map[string]any{pu},
		"payment_source": map[string]any{
			source: map[string]any{"experience_context": expCtx},
		},
	}
	headers := map[string]string{}
	if p.RequestID != "" {
		headers["PayPal-Request-Id"] = p.RequestID
	}
	raw, code, err := g.authed(http.MethodPost, "/v2/checkout/orders", body, headers)
	if err != nil {
		return PayPalOrder{}, err
	}
	if code < 200 || code >= 300 {
		log.Printf("paypal: create order http %d: %s", code, ppSnippet(raw))
		return PayPalOrder{}, fmt.Errorf("paypal create order %d: %s", code, ppSnippet(raw))
	}
	var res struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Links  []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return PayPalOrder{}, err
	}
	o := PayPalOrder{ID: res.ID, Status: res.Status}
	for _, l := range res.Links {
		// Hosted approval link is rel "approve" (PayPal) or "payer-action"
		// (Venmo); both point at checkoutnow?token=ORDERID.
		if l.Rel == "approve" || l.Rel == "payer-action" ||
			strings.Contains(l.Href, "checkoutnow?token=") {
			o.ApproveURL = l.Href
		}
	}
	return o, nil
}

// PayPalCapture is the result of capturing an approved order.
type PayPalCapture struct {
	OrderID    string
	CaptureID  string // payments.captures[0].id — store for refunds
	Status     string // COMPLETED on success
	CustomID   string // = registrationID
	GrossValue string
	FeeValue   string // PayPal's own fee
	NetValue   string
	Currency   string
}

// Paid reports whether the capture landed money (order + capture COMPLETED).
func (c PayPalCapture) Paid() bool { return c.Status == "COMPLETED" && c.CaptureID != "" }

// CaptureOrder captures an order the payer already approved. requestID is the
// PayPal-Request-Id idempotency key (stable per registration -> safe retries).
func (g *PayPalGateway) CaptureOrder(orderID, requestID string) (PayPalCapture, error) {
	headers := map[string]string{"Prefer": "return=representation"}
	if requestID != "" {
		headers["PayPal-Request-Id"] = requestID
	}
	raw, code, err := g.authed(http.MethodPost,
		fmt.Sprintf("/v2/checkout/orders/%s/capture", orderID), map[string]any{}, headers)
	if err != nil {
		return PayPalCapture{}, err
	}
	if code < 200 || code >= 300 {
		log.Printf("paypal: capture http %d: %s", code, ppSnippet(raw))
		return PayPalCapture{}, fmt.Errorf("paypal capture %d: %s", code, ppSnippet(raw))
	}
	var res struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		PurchaseUnits []struct {
			Payments struct {
				Captures []struct {
					ID       string `json:"id"`
					Status   string `json:"status"`
					CustomID string `json:"custom_id"`
					Amount   struct {
						CurrencyCode string `json:"currency_code"`
						Value        string `json:"value"`
					} `json:"amount"`
					SellerReceivableBreakdown struct {
						GrossAmount ppMoney `json:"gross_amount"`
						PaypalFee   ppMoney `json:"paypal_fee"`
						NetAmount   ppMoney `json:"net_amount"`
					} `json:"seller_receivable_breakdown"`
				} `json:"captures"`
			} `json:"payments"`
		} `json:"purchase_units"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return PayPalCapture{}, err
	}
	out := PayPalCapture{OrderID: res.ID, Status: res.Status}
	if len(res.PurchaseUnits) > 0 && len(res.PurchaseUnits[0].Payments.Captures) > 0 {
		c := res.PurchaseUnits[0].Payments.Captures[0]
		out.CaptureID = c.ID
		out.CustomID = c.CustomID
		out.Currency = c.Amount.CurrencyCode
		out.GrossValue = c.SellerReceivableBreakdown.GrossAmount.Value
		out.FeeValue = c.SellerReceivableBreakdown.PaypalFee.Value
		out.NetValue = c.SellerReceivableBreakdown.NetAmount.Value
		// Order status is authoritative, but fall back to the capture's.
		if out.Status == "" {
			out.Status = c.Status
		}
	}
	return out, nil
}

type ppMoney struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

// GetOrder reads an order's status (CREATED|APPROVED|COMPLETED|...) + custom_id.
func (g *PayPalGateway) GetOrder(orderID string) (status, customID string, err error) {
	raw, code, err := g.authed(http.MethodGet, "/v2/checkout/orders/"+orderID, nil, nil)
	if err != nil {
		return "", "", err
	}
	if code < 200 || code >= 300 {
		return "", "", fmt.Errorf("paypal get order %d: %s", code, ppSnippet(raw))
	}
	var res struct {
		Status        string `json:"status"`
		PurchaseUnits []struct {
			CustomID string `json:"custom_id"`
		} `json:"purchase_units"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", "", err
	}
	if len(res.PurchaseUnits) > 0 {
		customID = res.PurchaseUnits[0].CustomID
	}
	return res.Status, customID, nil
}

// PayPalWebhook is a verified webhook event (only the fields we act on).
type PayPalWebhook struct {
	EventType string // PAYMENT.CAPTURE.COMPLETED | .DENIED | .PENDING | .REFUNDED | ...
	CaptureID string // resource.id — dedup key
	Status    string // resource.status
	CustomID  string // resource.custom_id == registration_id (flat on capture events)
}

// VerifyWebhook validates an incoming PayPal webhook via PayPal's online verify
// endpoint (signature check against the configured webhook id) and returns the
// decoded event. rawBody MUST be the bytes exactly as received — it's spliced in
// as json.RawMessage; re-marshalling would flip verification to FAILURE.
func (g *PayPalGateway) VerifyWebhook(h http.Header, rawBody []byte) (PayPalWebhook, error) {
	if strings.TrimSpace(g.webhookID) == "" {
		return PayPalWebhook{}, errors.New("paypal webhook id not configured")
	}
	verifyReq := map[string]any{
		"auth_algo":         h.Get("Paypal-Auth-Algo"),
		"cert_url":          h.Get("Paypal-Cert-Url"),
		"transmission_id":   h.Get("Paypal-Transmission-Id"),
		"transmission_sig":  h.Get("Paypal-Transmission-Sig"),
		"transmission_time": h.Get("Paypal-Transmission-Time"),
		"webhook_id":        g.webhookID,
		"webhook_event":     json.RawMessage(rawBody),
	}
	raw, code, err := g.authed(http.MethodPost,
		"/v1/notifications/verify-webhook-signature", verifyReq, nil)
	if err != nil {
		return PayPalWebhook{}, err
	}
	if code < 200 || code >= 300 {
		return PayPalWebhook{}, fmt.Errorf("paypal verify-webhook %d: %s", code, ppSnippet(raw))
	}
	var vr struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := json.Unmarshal(raw, &vr); err != nil {
		return PayPalWebhook{}, err
	}
	if vr.VerificationStatus != "SUCCESS" {
		return PayPalWebhook{}, fmt.Errorf("paypal webhook signature %s", vr.VerificationStatus)
	}
	// Verified — decode the event we act on. custom_id is flat on capture events.
	var ev struct {
		EventType string `json:"event_type"`
		Resource  struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			CustomID string `json:"custom_id"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(rawBody, &ev); err != nil {
		return PayPalWebhook{}, err
	}
	return PayPalWebhook{
		EventType: ev.EventType,
		CaptureID: ev.Resource.ID,
		Status:    ev.Resource.Status,
		CustomID:  ev.Resource.CustomID,
	}, nil
}

// ppSnippet trims a PayPal response body for error messages.
func ppSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
