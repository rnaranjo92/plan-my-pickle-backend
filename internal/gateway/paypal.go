package gateway

import (
	"bytes"
	"encoding/base64"
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

	// Marketplace (Phase 3): set via SetMarketplace once the platform is an
	// approved PayPal partner. partnerID = the platform's PayPal merchant id (for
	// merchant-status lookups); bnCode = PayPal-Partner-Attribution-Id (BN).
	partnerID string
	bnCode    string

	httpClient *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// SetMarketplace enables Phase-3 multiparty calls (route funds to an organizer,
// skim a platform fee). partnerID is the platform's PayPal merchant id; bnCode
// is the PayPal-Partner-Attribution-Id (BN) for the partner app. Leave unset for
// platform-collects (Phase 1).
func (g *PayPalGateway) SetMarketplace(partnerID, bnCode string) {
	g.partnerID = strings.TrimSpace(partnerID)
	g.bnCode = strings.TrimSpace(bnCode)
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

	// Marketplace (Phase 3). When PayeeMerchantID is set, funds route to that
	// organizer's PayPal account and PlatformFeeCents is the platform's cut
	// (which lands in the partner account). Empty -> platform-collects.
	PayeeMerchantID  string
	PlatformFeeCents int
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
	if p.AmountCents <= 0 {
		return PayPalOrder{}, fmt.Errorf("paypal create order: amount must be positive, got %d cents", p.AmountCents)
	}
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
	// Marketplace: route funds to the organizer (payee) and skim the platform fee.
	if p.PayeeMerchantID != "" {
		pu["payee"] = map[string]any{"merchant_id": p.PayeeMerchantID}
		if p.PlatformFeeCents > 0 {
			if p.PlatformFeeCents >= p.AmountCents {
				return PayPalOrder{}, fmt.Errorf("paypal create order: platform fee %d cents must be less than amount %d cents", p.PlatformFeeCents, p.AmountCents)
			}
			feeVal := fmt.Sprintf("%d.%02d", p.PlatformFeeCents/100, p.PlatformFeeCents%100)
			pu["payment_instruction"] = map[string]any{
				"disbursement_mode": "INSTANT",
				// Omit the fee's payee -> the fee lands in the partner (platform) account.
				"platform_fees": []map[string]any{{
					"amount": map[string]any{"currency_code": currency, "value": feeVal},
				}},
			}
		}
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
	// Marketplace orders need the partner BN + an act-on-behalf-of assertion.
	if p.PayeeMerchantID != "" {
		if g.bnCode != "" {
			headers["PayPal-Partner-Attribution-Id"] = g.bnCode
		}
		headers["PayPal-Auth-Assertion"] = g.authAssertion(p.PayeeMerchantID)
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
	OrderID       string
	CaptureID     string // payments.captures[0].id — store for refunds
	Status        string // ORDER status — COMPLETED on success
	CaptureStatus string // captures[0].status — must ALSO be COMPLETED to be paid
	CustomID      string // = registrationID
	GrossValue    string
	FeeValue      string // PayPal's own fee
	NetValue      string
	Currency      string
}

// Paid reports whether money actually landed. Per the PayPal spec, BOTH the
// order status AND the individual capture status must be COMPLETED: a capture
// can be PENDING (held / under review) while the order already reads COMPLETED,
// and a PENDING capture must NOT grant the registration spot.
func (c PayPalCapture) Paid() bool {
	return c.Status == "COMPLETED" && c.CaptureStatus == "COMPLETED" && c.CaptureID != ""
}

// CaptureOrder captures an order the payer already approved. requestID is the
// PayPal-Request-Id idempotency key (stable per registration -> safe retries).
// payeeMerchantID is set only for marketplace orders (adds the BN + auth-assertion
// headers so the capture acts on behalf of the organizer); "" for platform-collects.
func (g *PayPalGateway) CaptureOrder(orderID, requestID, payeeMerchantID string) (PayPalCapture, error) {
	headers := map[string]string{"Prefer": "return=representation"}
	if requestID != "" {
		headers["PayPal-Request-Id"] = requestID
	}
	if payeeMerchantID != "" {
		if g.bnCode != "" {
			headers["PayPal-Partner-Attribution-Id"] = g.bnCode
		}
		headers["PayPal-Auth-Assertion"] = g.authAssertion(payeeMerchantID)
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
		out.CaptureStatus = c.Status
		out.CustomID = c.CustomID
		out.Currency = c.Amount.CurrencyCode
		out.GrossValue = c.SellerReceivableBreakdown.GrossAmount.Value
		out.FeeValue = c.SellerReceivableBreakdown.PaypalFee.Value
		out.NetValue = c.SellerReceivableBreakdown.NetAmount.Value
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

// authAssertion builds the PayPal-Auth-Assertion header: an UNSIGNED JWT
// (alg:none) telling PayPal to act on behalf of the seller (organizer). Format:
// base64url({"alg":"none"}).base64url({"iss":clientID,"payer_id":seller}). (empty sig)
func (g *PayPalGateway) authAssertion(sellerMerchantID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"iss":%q,"payer_id":%q}`, g.clientID, sellerMerchantID)))
	return header + "." + payload + "."
}

// CreatePartnerReferral starts onboarding an organizer as a PayPal seller under
// this platform (Phase 3). trackingID is our internal organizer id (echoed back
// as merchantId on return); returnURL is where PayPal sends the organizer after
// they grant permission. Returns the action_url to redirect the organizer to —
// a full-page redirect (CanvasKit-friendly). Requests the PARTNER_FEE feature so
// platform_fees work.
func (g *PayPalGateway) CreatePartnerReferral(trackingID, returnURL string) (string, error) {
	body := map[string]any{
		"tracking_id":             trackingID,
		"partner_config_override": map[string]any{"return_url": returnURL},
		"operations": []map[string]any{{
			"operation": "API_INTEGRATION",
			"api_integration_preference": map[string]any{
				"rest_api_integration": map[string]any{
					"integration_method": "PAYPAL",
					"integration_type":   "THIRD_PARTY",
					"third_party_details": map[string]any{
						"features": []string{"PAYMENT", "REFUND", "PARTNER_FEE"},
					},
				},
			},
		}},
		"products":       []string{"EXPRESS_CHECKOUT"},
		"legal_consents": []map[string]any{{"type": "SHARE_DATA_CONSENT", "granted": true}},
	}
	raw, code, err := g.authed(http.MethodPost, "/v2/customer/partner-referrals", body, nil)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("paypal partner-referrals %d: %s", code, ppSnippet(raw))
	}
	var res struct {
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	for _, l := range res.Links {
		if l.Rel == "action_url" {
			return l.Href, nil
		}
	}
	return "", errors.New("paypal partner-referrals: no action_url in response")
}

// MerchantStatus checks whether an onboarded organizer can actually receive
// money (the Phase-3 "can this organizer accept fees" gate). sellerMerchantID is
// the organizer's merchantIdInPayPal captured at onboarding. Gate payouts on
// paymentsReceivable AND emailConfirmed both true.
func (g *PayPalGateway) MerchantStatus(sellerMerchantID string) (paymentsReceivable, emailConfirmed bool, err error) {
	if g.partnerID == "" {
		return false, false, errors.New("paypal: partner id not configured (call SetMarketplace)")
	}
	path := fmt.Sprintf("/v1/customer/partners/%s/merchant-integrations/%s",
		g.partnerID, sellerMerchantID)
	headers := map[string]string{}
	if g.bnCode != "" {
		headers["PayPal-Partner-Attribution-Id"] = g.bnCode
	}
	raw, code, err := g.authed(http.MethodGet, path, nil, headers)
	if err != nil {
		return false, false, err
	}
	if code < 200 || code >= 300 {
		return false, false, fmt.Errorf("paypal merchant-status %d: %s", code, ppSnippet(raw))
	}
	var res struct {
		PaymentsReceivable    bool `json:"payments_receivable"`
		PrimaryEmailConfirmed bool `json:"primary_email_confirmed"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return false, false, err
	}
	return res.PaymentsReceivable, res.PrimaryEmailConfirmed, nil
}

// ppSnippet trims a PayPal response body for error messages.
func ppSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
