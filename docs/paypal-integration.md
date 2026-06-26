# PlanMyPickle — PayPal + Venmo Integration Spec

_Generated from verified PayPal docs research (workflow wq9fxwvvf). Build reference for the Go gateway (internal/gateway/paypal.go)._

I'll synthesize the four research tracks into a build-ready integration spec.

# PayPal (+ Venmo) Integration Spec — Go Gateway for PlanMyPickle Registration Fees

## 0. Conventions, Hosts, and Auth

### Base hosts
| Environment | API base | Approval/onboarding host |
|---|---|---|
| Sandbox | `https://api-m.sandbox.paypal.com` | `https://www.sandbox.paypal.com` |
| Live | `https://api-m.paypal.com` | `https://www.paypal.com` |

Same paths on both. Sandbox and live credentials/BN codes are distinct.

### OAuth — get an access token (every call needs one)

`POST /v1/oauth2/token`

Headers:
- `Authorization: Basic base64(CLIENT_ID:CLIENT_SECRET)`
- `Content-Type: application/x-www-form-urlencoded`

Body (form-encoded): `grant_type=client_credentials`

Response `200`:
```json
{ "access_token": "A21AA...", "token_type": "Bearer", "app_id": "APP-...", "expires_in": 32400, "scope": "..." }
```
Cache the token in-process; refresh on `expires_in` (≈9h) minus a safety margin. Reuse across all order/capture/webhook-verify calls. This is the same client_credentials flow you already verified.

> The same OAuth token is the "partner access token" used for Phase 3 marketplace calls — there is no separate partner OAuth. What changes in Phase 3 is the addition of `PayPal-Auth-Assertion` + `PayPal-Partner-Attribution-Id` headers on the order/capture calls.

---

## 1. Exact API Calls the Gateway Needs

These are the four core calls for Phase 1 (platform-collects). The marketplace variants (extra headers + `payee`/`platform_fees`) are additive and covered in §4.

### 1a. Create order (with `custom_id = registration_id`)

`POST /v2/checkout/orders`

Headers:
- `Authorization: Bearer {access_token}` — required
- `Content-Type: application/json` — required
- `PayPal-Request-Id: reg-{registrationId}-create` — optional, recommended (idempotency; safe retries)

Request body (Phase 1, platform collects; redirect-friendly for CanvasKit):
```json
{
  "intent": "CAPTURE",
  "purchase_units": [
    {
      "custom_id": "reg_8f2c1a90",
      "invoice_id": "PMP-REG-8f2c1a90",
      "amount": { "currency_code": "USD", "value": "45.00" }
    }
  ],
  "payment_source": {
    "paypal": {
      "experience_context": {
        "brand_name": "PlanMyPickle",
        "landing_page": "LOGIN",
        "shipping_preference": "NO_SHIPPING",
        "user_action": "PAY_NOW",
        "return_url": "https://app.planmypickle.com/paypal/return?reg=reg_8f2c1a90",
        "cancel_url": "https://app.planmypickle.com/paypal/cancel?reg=reg_8f2c1a90"
      }
    }
  }
}
```

Field notes:
- `purchase_units[].custom_id` (max 127 chars) = **`registration_id`**. Purpose-built for reconciliation; shows in settlement reports; propagates flat to `resource.custom_id` on the capture webhook (see §3).
- `purchase_units[].invoice_id` (max 127 chars) = secondary idempotency key. PayPal enforces **uniqueness** on `invoice_id` — it blocks a duplicate payment for the same registration. Set it to a stable per-registration value.
- `intent: "CAPTURE"` (one-step money move; correct for registration fees vs. `AUTHORIZE`/hold).
- `experience_context` is the current location for redirect fields (legacy top-level `application_context` still works but is deprecated). `user_action: "PAY_NOW"` makes the button read "Pay Now". `shipping_preference: "NO_SHIPPING"` since there's nothing to ship.
- After approval, PayPal appends `token={ORDERID}` (and `PayerID`) to `return_url`. **That `token` IS the order id** — your return handler already knows which order to capture. (We also carry `reg` in the URL as a convenience, but treat `token` as authoritative.)

Response `201`:
```json
{
  "id": "5O190127TN364715T",
  "status": "CREATED",
  "links": [
    { "href": ".../checkoutnow?token=5O190127TN364715T", "rel": "approve", "method": "GET" }
  ]
}
```

> **Do NOT hardcode `rel == "approve"`.** Newer/Venmo responses use `rel: "payer-action"`. Select the redirect link by matching the one whose `href` contains `checkoutnow?token=` OR accept either `approve`/`payer-action` rel. (Plain `payment_source.paypal` typically returns `approve`; `payment_source.venmo` returns `payer-action` — see §2.)

### 1b. Get order (optional confirm before capture)

`GET /v2/checkout/orders/{id}`

Headers: `Authorization: Bearer {access_token}`
Optional query: `?fields=payment_source`

Check `status` in the `200` response. Relevant enum values:
| status | meaning |
|---|---|
| `CREATED` | order created, not yet approved |
| `APPROVED` | buyer approved via wallet — **ready to capture** |
| `COMPLETED` | already captured |
| `PAYER_ACTION_REQUIRED` | needs another payer action (e.g. 3DS); redirect to `rel:"payer-action"` link |
| `VOIDED` | voided |

You may gate capture on `status == "APPROVED"`, or skip the GET and capture directly — a premature capture fails with `422 ORDER_NOT_APPROVED`.

### 1c. Capture order (the money move)

`POST /v2/checkout/orders/{id}/capture`

Headers:
- `Authorization: Bearer {access_token}` — required
- `Content-Type: application/json` — required (even with empty body)
- `PayPal-Request-Id: reg-{registrationId}-capture` — **strongly recommended.** Idempotency key; PayPal replays the original response on retries (key retained ~6h), preventing double-charges. Use a stable per-registration key.
- `Prefer: return=representation` — optional; returns the full capture object incl. `seller_receivable_breakdown` (gross/fee/net) in one call. Default `return=minimal` is slimmer.

Request body: optional — send `{}` with `Content-Type: application/json`. No `payment_source` needed when the buyer already approved via the hosted flow.

Response `201 Created` (first success) / `200 OK` (idempotent replay). Key fields:
- `id` — the **order** id (not the capture id).
- `status` — `COMPLETED` on success.
- `purchase_units[0].payments.captures[0].id` — the **capture/transaction id** → store it; needed for refunds via `POST /v2/payments/captures/{capture_id}/refund`.
- `purchase_units[0].payments.captures[0].status` — `COMPLETED`.
- `purchase_units[0].payments.captures[0].seller_receivable_breakdown` — `gross_amount`, `paypal_fee`, `net_amount` (PayPal's own fee, separate from your ~5%).
- `purchase_units[0].payments.captures[0].custom_id` — your `registration_id` (echoed).

```json
{
  "id": "5O190127TN364715T",
  "status": "COMPLETED",
  "purchase_units": [
    { "payments": { "captures": [
      {
        "id": "3C679366HH908993F",
        "status": "COMPLETED",
        "amount": { "currency_code": "USD", "value": "45.00" },
        "final_capture": true,
        "custom_id": "reg_8f2c1a90",
        "seller_receivable_breakdown": {
          "gross_amount": { "currency_code": "USD", "value": "45.00" },
          "paypal_fee":   { "currency_code": "USD", "value": "1.61" },
          "net_amount":   { "currency_code": "USD", "value": "43.39" }
        }
      }
    ] } }
  ]
}
```

**Mark registration paid when** top-level `status == "COMPLETED"` **AND** `purchase_units[0].payments.captures[0].status == "COMPLETED"`; persist `captures[0].id` and the fee breakdown.

> **Sync vs. webhook:** unlike Stripe Connect's webhook-marks-paid, the PayPal hosted flow gives you the capture result *synchronously* in the `return_url` handler. Treat the synchronous capture as the happy path, and the `PAYMENT.CAPTURE.COMPLETED` webhook as belt-and-suspenders reconciliation. Both must be idempotent on `captures[0].id`.

### 1d. Refund (for cancellations/parity)

`POST /v2/payments/captures/{capture_id}/refund` — `{capture_id}` = stored `captures[0].id`. Body optional (`{}` = full refund; `{ "amount": {...} }` = partial). Drives `PAYMENT.CAPTURE.REFUNDED` webhook.

---

## 2. Venmo: How It's Surfaced + Honest Sandbox-vs-Live Story

### The core constraint
Venmo is primarily a **client-side, JS-SDK-driven funding source**. PayPal's own docs state Venmo "isn't displayed as a payment option in Checkout integrations by default" and "must be integrated using the JavaScript SDK." **The bare `checkoutnow?token=ORDERID` redirect (your verified `rel:"approve"` link) will NOT reliably present Venmo.** Do not assume it does.

### Two ways to get Venmo, ranked for CanvasKit (no DOM)

**Option 1 — RECOMMENDED for CanvasKit: server-created order with `payment_source.venmo` + `payer-action` redirect.**
Set `payment_source.venmo` (with `return_url`/`cancel_url`) on the create-order body. This is the documented way to make a server-created order a Venmo order, and it keeps you DOM-free.

```json
POST /v2/checkout/orders
{
  "intent": "CAPTURE",
  "purchase_units": [
    { "custom_id": "reg_8f2c1a90", "amount": { "currency_code": "USD", "value": "45.00" } }
  ],
  "payment_source": {
    "venmo": {
      "experience_context": {
        "brand_name": "PlanMyPickle",
        "shipping_preference": "NO_SHIPPING",
        "return_url": "https://app.planmypickle.com/paypal/return?reg=reg_8f2c1a90",
        "cancel_url": "https://app.planmypickle.com/paypal/cancel?reg=reg_8f2c1a90"
      }
    }
  }
}
```
- `payment_source.venmo` fields: `email_address`, `experience_context`, `vault_id`, `attributes`.
- **Critical response difference:** with `payment_source.venmo` + `return_url`, the create response returns the redirect link as **`"rel": "payer-action"`** (NOT `approve`). This is exactly why the link selector in §1a must accept either rel. Redirect the buyer to the `payer-action` URL → routes into the Venmo app-switch (mobile) / QR (desktop) experience → back to `return_url` → you `POST .../capture` as usual.

**Option 2 — fallback if `payer-action` proves unreliable: JS SDK Venmo button.** Requires an HTML/DOM surface (an `HtmlElementView`/iframe or a thin non-CanvasKit payment page outside the Flutter canvas):
```html
<script src="https://www.paypal.com/sdk/js?client-id=YOUR_CLIENT_ID&enable-funding=venmo&currency=USD&buyer-country=US"></script>
```
```javascript
if (paypal.isFundingEligible(paypal.FUNDING.VENMO)) {
  paypal.Buttons({ fundingSource: paypal.FUNDING.VENMO }).render('#venmo-container');
}
```
- `enable-funding=venmo` is **required** (Venmo not shown by default).
- `buyer-country=US` is **sandbox-only** to force the button to render; **drop it in live** (real eligibility = buyer IP geolocation).

### Venmo button eligibility (both options)
Venmo only surfaces when ALL are true: merchant is a **US PayPal Business account**; buyer is in the **US**; currency is **USD**; and either **mobile** (iOS Safari / Android Chrome with Venmo app installed → app-switch) or **desktop** (any major browser → QR code scanned with phone's Venmo app).

### Honest sandbox-vs-live story
- **Venmo IS sandbox-testable** for the button + approval UX (per PayPal's official `pay-with-venmo/test/` page — trust this over community claims that it "isn't available in sandbox"). Requires `buyer-country=US`, sandbox business + personal accounts.
- **Desktop QR is NOT testable in sandbox** ("currently unavailable") — sandbox desktop falls back to a web login flow; live desktop uses QR. **Best validated on real mobile.**
- **Not available in sandbox:** settlement, disbursement, disputes, merchant reporting, vault subsequent purchases. So you **cannot validate money movement / payout reporting for Venmo in sandbox** — that only proves out in live.
- **For LIVE:** live US PayPal Business account, live client ID, `enable-funding=venmo` (no `buyer-country`), real US buyer on a supported browser with the Venmo app.
- **Fees/settlement:** Venmo settles through the same PayPal rails as your PayPal payments — it is **not a separate processor relationship**, and your ~5% platform-fee + payout mechanics carry over unchanged (incl. Phase-3 marketplace).

**Action item before committing to Option 1:** validate the `payer-action` redirect end-to-end on a real mobile device. If it's flaky, fall back to Option 2 (DOM surface).

---

## 3. Webhooks: Events, Signature Verification, custom_id Attribution

### Authoritative "paid" event
For `intent=CAPTURE`, mark paid on **`PAYMENT.CAPTURE.COMPLETED`** (`resource.status == "COMPLETED"`, funds captured).

| Event | Action |
|---|---|
| `PAYMENT.CAPTURE.COMPLETED` | **Mark PAID** (authoritative). Carries fee breakdown + flat `custom_id`. |
| `CHECKOUT.ORDER.APPROVED` | Do **not** mark paid — buyer consent only, no funds yet (you still must capture). |
| `CHECKOUT.ORDER.COMPLETED` | At most a redundant signal; no capture fee breakdown. |
| `PAYMENT.CAPTURE.DENIED` | Mark failed / unpaid. |
| `PAYMENT.CAPTURE.PENDING` | Leave pending — do not grant the spot. |
| `PAYMENT.CAPTURE.REFUNDED` | Reverse paid status / handle cancellation. |

Subscribe to all five capture events above (`.COMPLETED`, `.DENIED`, `.PENDING`, `.REFUNDED`) — and the Phase-3 onboarding events in §4.

### Create the webhook
`POST /v1/notifications/webhooks` — headers `Authorization: Bearer {access_token}`, `Content-Type: application/json`:
```json
{
  "url": "https://api.planmypickle.com/paypal/webhook",
  "event_types": [
    { "name": "PAYMENT.CAPTURE.COMPLETED" },
    { "name": "PAYMENT.CAPTURE.DENIED" },
    { "name": "PAYMENT.CAPTURE.PENDING" },
    { "name": "PAYMENT.CAPTURE.REFUNDED" }
  ]
}
```
`url` must be HTTPS (≤2048 chars). Response `201` returns `id` = your **`webhook_id`** — store it (needed for signature verification). Or create via Dashboard → Apps & Credentials → app → Webhooks. (≤10 webhook URLs per app.)

### Signature verification

PayPal sends these headers on each delivery → map to verify-request fields:
| Header | Field |
|---|---|
| `paypal-transmission-id` | `transmission_id` |
| `paypal-transmission-time` | `transmission_time` |
| `paypal-transmission-sig` | `transmission_sig` |
| `paypal-cert-url` | `cert_url` |
| `paypal-auth-algo` | `auth_algo` (e.g. `SHA256withRSA`) |

**Option A (ship this first) — online verify:** `POST /v1/notifications/verify-webhook-signature`, headers `Authorization: Bearer {access_token}`, `Content-Type: application/json`:
```json
{
  "auth_algo": "SHA256withRSA",
  "cert_url": "https://api.paypal.com/v1/notifications/certs/CERT-...",
  "transmission_id": "69cd13f0-...",
  "transmission_sig": "lmI95Jx3...==",
  "transmission_time": "2016-02-18T20:01:35Z",
  "webhook_id": "0EH40505U7160970P",
  "webhook_event": { /* FULL raw webhook JSON, byte-for-byte as received */ }
}
```
Response `{ "verification_status": "SUCCESS" }` → trust; `"FAILURE"` → reject with non-2xx.

> **Go gotcha (critical):** `webhook_event` must be the body **byte-for-byte as received** — any re-marshal (whitespace/key-order changes) flips it to `FAILURE`. Capture the **raw request body bytes** before unmarshalling and splice them in via `json.RawMessage`. Never re-marshal a parsed struct.

**Option B (later, no round-trip) — offline cert verify:** build `transmission_id|transmission_time|webhook_id|CRC32(raw_body)`, RSA-SHA256 verify `transmission_sig` against the public key from `cert_url`. **Validate `cert_url` host is `*.paypal.com` before fetching (SSRF guard)**; cache the cert. More code; Option A is simpler and is what to ship first.

### Where `custom_id` (= registration_id) appears

Only present if you set `custom_id` on the purchase_unit at create time (§1a). On `PAYMENT.CAPTURE.COMPLETED` it sits **flat at `resource.custom_id`** (sibling of `amount`/`status`):
```json
{
  "event_type": "PAYMENT.CAPTURE.COMPLETED",
  "resource": {
    "id": "2GG279541U471931P",
    "status": "COMPLETED",
    "amount": { "currency_code": "USD", "value": "45.00" },
    "custom_id": "reg_8f2c1a90",
    "invoice_id": "PMP-REG-8f2c1a90",
    "seller_receivable_breakdown": {
      "gross_amount": { "currency_code": "USD", "value": "45.00" },
      "paypal_fee":   { "currency_code": "USD", "value": "1.61" },
      "net_amount":   { "currency_code": "USD", "value": "43.39" }
    },
    "supplementary_data": { "related_ids": { "order_id": "5O190127TN364715T" } },
    "payee": { "merchant_id": "X5XAHHCG636FA", "email_address": "organizer@example.com" }
  }
}
```
Attribution in the handler:
- **Primary:** `resource.custom_id` → `registration_id` (flip to paid).
- **Dedup / idempotency:** `resource.id` (capture id) — dedupe so retried webhooks don't double-apply. Also `resource.invoice_id`.
- **Order linkage:** `resource.supplementary_data.related_ids.order_id` → the order `id` from create.

> Caveat: on `CHECKOUT.ORDER.APPROVED`, `custom_id` lives nested at `resource.purchase_units[0].custom_id` instead — another reason to key paid-attribution off `PAYMENT.CAPTURE.COMPLETED`, where it's flat.

### Handler flow
1. Read **raw body bytes**.
2. Verify (Option A) with 5 `paypal-*` headers + stored `webhook_id` + raw body as `webhook_event`. Not `SUCCESS` → return 400/401.
3. Switch on `event_type`: `COMPLETED` → look up `resource.custom_id`, dedupe on `resource.id`, mark paid, record gross/fee/net; `DENIED` → failed; `PENDING` → pending; `REFUNDED` → reverse.
4. Return **HTTP 200** fast (PayPal retries non-2xx with backoff).

---

## 4. Marketplace Payee / Platform-Fee Fields + Headers (Phase 3) — Design For It Now

PayPal's Stripe Connect analog is **PayPal Complete Payments — Marketplaces & Platforms (Multiparty)**. PlanMyPickle = partner; each organizer = seller/merchant. Funds settle to the organizer; you skim ~5% via `platform_fees`.

### Stripe Connect → PayPal mapping
| Concept | Stripe Connect | PayPal Multiparty |
|---|---|---|
| Onboard organizer | Account Link redirect | `POST /v2/customer/partner-referrals` → redirect `action_url` |
| Organizer id | `acct_…` | `merchant_id` / `payer_id` (`merchantIdInPayPal`) |
| Act on behalf of organizer | `Stripe-Account` header | `PayPal-Auth-Assertion` JWT |
| Route funds to organizer | `transfer_data.destination` | `purchase_units[].payee.merchant_id` |
| Platform ~5% cut | `application_fee_amount` | `payment_instruction.platform_fees[].amount` (omit fee `payee` → lands in your account) |
| Mark paid | webhook | `PAYMENT.CAPTURE.COMPLETED` webhook |
| Partner identity (required) | — | `PayPal-Partner-Attribution-Id` (BN code) |
| Hold/escrow | manual payout | `disbursement_mode: DELAYED` (needs `DELAY_FUNDS_DISBURSEMENT` feature) |

### 4a. Onboard an organizer — Partner Referrals
`POST /v2/customer/partner-referrals` (headers: `Authorization: Bearer {access_token}`, `Content-Type: application/json`):
```json
{
  "tracking_id": "ORGANIZER-1234",
  "partner_config_override": {
    "return_url": "https://app.planmypickle.com/paypal/onboard/return"
  },
  "operations": [
    { "operation": "API_INTEGRATION",
      "api_integration_preference": { "rest_api_integration": {
        "integration_method": "PAYPAL",
        "integration_type": "THIRD_PARTY",
        "third_party_details": { "features": ["PAYMENT", "REFUND", "PARTNER_FEE", "DELAY_FUNDS_DISBURSEMENT"] }
      }}}
  ],
  "products": ["EXPRESS_CHECKOUT"],
  "legal_consents": [ { "type": "SHARE_DATA_CONSENT", "granted": true } ]
}
```
- `tracking_id` = your internal organizer id (echoed back as `merchantId`).
- `features`: `PAYMENT`+`REFUND` baseline; **`PARTNER_FEE` required to use `platform_fees`**; `DELAY_FUNDS_DISBURSEMENT` only if you want `DELAYED` disbursement.
- `products: ["EXPRESS_CHECKOUT"]` for the PayPal/Venmo redirect flow (no JS SDK/card processing needed).

Response `201` → redirect organizer to the `rel:"action_url"` link (`sandbox.paypal.com/merchantsignup/...`). **Full-page redirect — CanvasKit-compatible** (no DOM button).

After onboarding, capture the organizer's PayPal id two ways:
- **Return-URL params (hint only):** `merchantId` (your tracking_id), **`merchantIdInPayPal`** (the organizer's PayPal merchant/payer id → store this, it's `payee.merchant_id`), `permissionsGranted`, `consentStatus`, `isEmailConfirmed`, `accountStatus`.
- **Authoritative server check:** `GET /v1/customer/partners/{partner_merchant_id}/merchant-integrations/{seller_merchant_id}` (or `?tracking_id={tracking_id}`). Gate "organizer can accept fees" on `payments_receivable: true` **AND** `primary_email_confirmed: true` (plus partner-fee/payment scopes). Webhook equivalents: `MERCHANT.ONBOARDING.COMPLETED`, `MERCHANT.PARTNER-CONSENT.REVOKED`.

### 4b. Order with payee + platform fee
`POST /v2/checkout/orders` — **extra headers vs. Phase 1:**
| Header | Value | Purpose |
|---|---|---|
| `Authorization` | `Bearer {access_token}` | Your platform OAuth token |
| `PayPal-Partner-Attribution-Id` | `{BN_CODE}` | **Required for multiparty.** Same BN on referral + order/capture. From Dashboard → Apps & Credentials → app → App Settings → Reports section. |
| `PayPal-Auth-Assertion` | `{JWT}` | Act-on-behalf-of this organizer (≈ `Stripe-Account`) |
| `PayPal-Request-Id` | idempotency key | recommended |

Body:
```json
{
  "intent": "CAPTURE",
  "purchase_units": [
    {
      "reference_id": "REG-78910",
      "custom_id": "reg_8f2c1a90",
      "payee": { "merchant_id": "WNM9VDLXSZPFW" },
      "amount": { "currency_code": "USD", "value": "45.00" },
      "payment_instruction": {
        "disbursement_mode": "INSTANT",
        "platform_fees": [ { "amount": { "currency_code": "USD", "value": "2.25" } } ]
      }
    }
  ]
}
```
- `payee.merchant_id` = organizer's `merchantIdInPayPal`. If omitted, funds go to **you** — so it must be set. (`payee.email_address` allowed but `merchant_id` preferred — email can change.)
- `platform_fees[].amount` = your ~5% cut. **Omit the fee's `payee`** → fee lands in your (partner) account (per docs: "If `PLATFORM_FEES.PAYEE` is not set, the fee amount will go to the live API caller account").
- Requires organizer onboarded with **`PARTNER_FEE`** or the `platform_fees` array is rejected.
- `disbursement_mode: INSTANT` (organizer paid immediately). `DELAYED` needs the `DELAY_FUNDS_DISBURSEMENT` feature (escrow/hold-until-event).
- **Keep `custom_id` here too** so the webhook attribution in §3 still works unchanged.

`PayPal-Auth-Assertion` construction — **unsigned JWT (`alg:none`)**, three base64url parts joined by dots with a **trailing dot** (empty signature):
- Header: `{"alg":"none"}`
- Payload: `{"iss":"<YOUR_PARTNER_CLIENT_ID>","payer_id":"<ORGANIZER_MERCHANT_ID>"}`
- Signature: empty → `base64url(header).base64url(payload).`

Capture: `POST /v2/checkout/orders/{id}/capture` with the **same extra headers** (`Authorization`, `PayPal-Partner-Attribution-Id`, `PayPal-Auth-Assertion`), empty body. Mark paid via `PAYMENT.CAPTURE.COMPLETED` (unchanged); platform fee + breakdown surface in `resource.seller_receivable_breakdown` / `platform_fees`.

### 4c. What onboarding is needed (production)
- You must be an **approved PayPal partner** for **production** multiparty calls — unapproved live calls return **`401 Unauthorized`**. Approval = PayPal's partner/marketplace application form (no upfront/monthly fees; rates negotiable).
- **BN code** auto-generated per app (Dashboard → Apps & Credentials → app → App Settings → Reports, last line). Same BN on referral + order/capture.
- **Platform-fee feature must be enabled by PayPal on your account** for production ("our team will also need to configure this feature").
- **Sandbox works before approval:** "you can call and test the … APIs with your sandbox credentials before you are approved." So you can build and prove the entire marketplace flow (Partner Referrals, `payee`, `platform_fees`) in sandbox now — only live needs approval + feature enablement.

### Design-for-it-now implications (do these in Phase 1 so Phase 3 is additive)
1. **Store an `organizer_paypal_merchant_id` (`merchantIdInPayPal`) column** now (nullable), even though Phase 1 collects to the platform.
2. **Centralize header injection** so adding `PayPal-Partner-Attribution-Id` + `PayPal-Auth-Assertion` to order/capture is a config flip, not a refactor.
3. **Keep `custom_id = registration_id` on the purchase_unit always** — identical attribution path across all phases.
4. **Build the auth-assertion JWT helper** (trivial `alg:none`) behind an interface now; no-op in Phase 1.
5. **Apply for partner approval early** — it's the long-lead gating item for live Phase 3.

---

## 5. Concrete Phased Build Plan

### Phase 1 — Platform-collects: order + capture + webhook (sandbox)
*Goal: prove the full money-move + reconciliation loop end-to-end, mirroring how Phase 3 will work, minus payee routing.*

1. **OAuth client** — `POST /v1/oauth2/token` (Basic auth, client_credentials); cache token w/ refresh. (Already verified.)
2. **Create order** (§1a) — `intent:CAPTURE`, `purchase_units[0].custom_id = registration_id`, `invoice_id` = stable per-reg key, `payment_source.paypal.experience_context` with `return_url`/`cancel_url`, `PayPal-Request-Id: reg-{id}-create`. Persist `order.id ↔ registration_id`.
3. **Redirect link selector** — pick the link whose `href` contains `checkoutnow?token=` (accept `approve` OR `payer-action`). Return it to the Flutter app for a full-page redirect.
4. **Return handler** (`/paypal/return`) — read `token` (= order id) from query → optional `GET /v2/checkout/orders/{id}` to confirm `APPROVED` → `POST /v2/checkout/orders/{id}/capture` with `PayPal-Request-Id: reg-{id}-capture` + `Prefer: return=representation`. On `status==COMPLETED` && `captures[0].status==COMPLETED`: mark paid, store `captures[0].id` + fee breakdown. Cancel handler (`/paypal/cancel`) → leave unpaid.
5. **Webhook endpoint** (`/paypal/webhook`) — create subscription for the 4 capture events; verify via Option A (raw-body `json.RawMessage`); on `PAYMENT.CAPTURE.COMPLETED` look up `resource.custom_id`, dedupe on `resource.id`, mark paid (idempotent with step 4); handle `DENIED`/`PENDING`/`REFUNDED`. Return 200 fast.
6. **Idempotency** everywhere keyed on capture id; sync-capture and webhook must converge to one paid state.
7. **Design-for-Phase-3 stubs:** nullable `organizer_paypal_merchant_id`, centralized header injector, `alg:none` JWT helper (no-op), keep `custom_id` always.

**Exit criteria:** sandbox personal account pays → registration flips to paid via sync capture; webhook independently confirms; refund flips it back. Stripe parity for the platform-collects case.

### Phase 2 — Venmo + pay-screen redirect flow
*Goal: offer Venmo within the CanvasKit redirect flow.*

1. **Pay-screen with method choice** (PayPal vs. Venmo) in the Flutter app — purely a flag passed to the create-order call.
2. **Venmo order path** (§2, Option 1) — when Venmo chosen, create order with `payment_source.venmo.experience_context` (`return_url`/`cancel_url`). Link selector already accepts `rel:"payer-action"`.
3. **Validate `payer-action` end-to-end on REAL mobile** (sandbox: `buyer-country=US` for the SDK path if used; desktop QR not sandbox-testable). If reliable → ship redirect-only (DOM-free).
4. **Fallback** if `payer-action` is flaky — thin DOM payment surface (`HtmlElementView`/iframe) rendering the JS SDK Venmo button (`enable-funding=venmo`, `FUNDING.VENMO`, `buyer-country=US` sandbox-only).
5. **Same capture + webhook path** as Phase 1 — Venmo settles on the same rails; no new reconciliation code.

**Exit criteria:** Venmo payment completes via redirect on real mobile, captures, and reconciles through the existing webhook. (Acknowledge: Venmo settlement/reporting only fully provable in live.)

### Phase 3 — Marketplace payout to organizer
*Goal: route fees to the organizer's PayPal, skim ~5% — the Stripe Connect analog.*

1. **Apply for PayPal partner approval early** (long lead). Capture the **BN code**; have PayPal enable the **platform-fee feature** for live.
2. **Organizer onboarding** (§4a) — `POST /v2/customer/partner-referrals` (request `PARTNER_FEE`), redirect to `action_url` (CanvasKit-friendly), capture `merchantIdInPayPal` into the column added in Phase 1; gate payable on `GET …/merchant-integrations` (`payments_receivable` + `primary_email_confirmed`). Subscribe to `MERCHANT.ONBOARDING.COMPLETED` / `MERCHANT.PARTNER-CONSENT.REVOKED`.
3. **Flip order/capture to multiparty** (§4b) — add `payee.merchant_id`, `payment_instruction.platform_fees[]`, `disbursement_mode:INSTANT`; add `PayPal-Partner-Attribution-Id` + `PayPal-Auth-Assertion` headers (the centralized injector + JWT helper from Phase 1 light up here). `custom_id` unchanged → webhook attribution unchanged.
4. **Prove in sandbox first** (partner + seller business + buyer personal sandbox accounts) before live; then switch to live creds/BN + approved partner status.

**Exit criteria:** sandbox player pays → organizer (seller sandbox account) is the `payee`, platform fee lands in partner account, registration marked paid via the same webhook. Then live after approval + feature enablement.

---

## 6. Build Checklist (cross-phase)
- [ ] Token cache w/ refresh on `expires_in`.
- [ ] Link selector matches `checkoutnow?token=` / accepts `approve`|`payer-action`.
- [ ] `custom_id = registration_id` on every order (max 127 chars); `invoice_id` as unique idempotency key.
- [ ] `PayPal-Request-Id` on create + capture (stable per-registration keys).
- [ ] Sync-capture path AND webhook path, both idempotent on capture id.
- [ ] Webhook signature verify with **raw body bytes** (`json.RawMessage`), never re-marshalled.
- [ ] SSRF guard on `cert_url` if/when using offline verify (Option B).
- [ ] Nullable `organizer_paypal_merchant_id`, centralized header injector, `alg:none` JWT helper — all stubbed in Phase 1.
- [ ] Distinct sandbox vs. live creds + BN codes; never send `buyer-country` in live.
- [ ] PayPal partner approval + platform-fee feature requested early (Phase 3 long-lead).