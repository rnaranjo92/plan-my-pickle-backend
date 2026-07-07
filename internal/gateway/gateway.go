// Package gateway defines the external-integration boundaries — payments, SMS,
// and DUPR submission — plus mock implementations for local/dev. Real Stripe /
// Twilio / DUPR clients implement the same interfaces and drop in via config.
// These secrets + webhooks belong on the server, which is why they live here.
package gateway

import "fmt"

// ---- payments ----
type PaymentResult struct {
	OK          bool
	Provider    string
	ProviderRef string
	AmountCents int
	Currency    string
}

type PaymentGateway interface {
	Charge(registrationID string, amountCents int, currency, provider string) (PaymentResult, error)
	// Live reports whether this is a real payment processor. The public pay
	// endpoint only marks a fee-bearing registration "paid" when Live() is true,
	// so the always-succeeds mock can't be used to self-confirm payment.
	Live() bool
}

type MockPayment struct {
	ShouldSucceed bool
	seq           int
	Charges       []PaymentResult
}

func NewMockPayment() *MockPayment { return &MockPayment{ShouldSucceed: true} }

// Live is false: the mock is not a real processor.
func (m *MockPayment) Live() bool { return false }

func (m *MockPayment) Charge(_ string, amountCents int, currency, provider string) (PaymentResult, error) {
	m.seq++
	r := PaymentResult{OK: m.ShouldSucceed, Provider: provider, AmountCents: amountCents, Currency: currency}
	if m.ShouldSucceed {
		r.ProviderRef = fmt.Sprintf("mock_pay_%d", m.seq)
	}
	m.Charges = append(m.Charges, r)
	return r, nil
}

// ---- SMS ----
type SmsResult struct {
	OK          bool
	ProviderRef string
}

type SentSms struct{ To, Body string }

type SmsGateway interface {
	Send(to, body string) (SmsResult, error)
}

type MockSms struct {
	ShouldSucceed bool
	seq           int
	Sent          []SentSms
}

func NewMockSms() *MockSms { return &MockSms{ShouldSucceed: true} }

func (m *MockSms) Send(to, body string) (SmsResult, error) {
	m.seq++
	if m.ShouldSucceed {
		m.Sent = append(m.Sent, SentSms{To: to, Body: body})
		return SmsResult{OK: true, ProviderRef: fmt.Sprintf("mock_sms_%d", m.seq)}, nil
	}
	return SmsResult{OK: false}, nil
}

// ---- DUPR ----
type DuprPayload struct {
	EventID     string
	DuprEventID string
	EventName   string
	MatchID     string // our match id (fallback identifier / delete reference)
	// Identifier is the DUPR idempotency key sent on create. DUPR forbids reusing
	// an identifier (even after a delete → "Match with identifier already exists"),
	// so the caller derives a fresh one per create generation (e.g.
	// "<matchID>-g<gen>"); empty falls back to MatchID.
	Identifier   string
	MatchCode    string // existing DUPR matchCode, for UpdateMatch
	MatchDate    string // when the match was played (yyyy-MM-dd); empty = today
	Team1DuprIDs []string
	Team2DuprIDs []string
	Team1Score   int // game 1 (legacy single-game field)
	Team2Score   int
	Games        [][2]int // per-game scores for a best-of-N match ([t1, t2] each)
}

type DuprResult struct {
	OK          bool
	DuprMatchID string
	Error       string
	// Permanent marks a failure that retrying can't fix (a DUPR 4xx: bad payload,
	// invalid dupr_id, "identifier already exists") vs a transient one (5xx / 429 /
	// network) worth backing off and retrying.
	Permanent bool
}

// DuprRating is a player's current DUPR ratings, looked up by DUPR id. Found is
// false (with a nil error) when the id isn't known to DUPR.
type DuprRating struct {
	Found              bool
	DuprID             string
	FullName           string
	Singles            float64
	Doubles            float64
	SinglesProvisional bool
	DoublesProvisional bool
}

// DuprMember is one member of a partner's DUPR club roster. Ratings are the raw
// DUPR strings (e.g. "3.80", or "" / "NR" when unrated).
type DuprMember struct {
	DuprID   string
	FullName string
	Singles  string
	Doubles  string
}

type DuprGateway interface {
	SubmitMatch(p DuprPayload) (DuprResult, error)
	// UpdateMatch revises a previously-submitted match (p.MatchCode identifies it).
	UpdateMatch(p DuprPayload) (DuprResult, error)
	// DeleteMatch removes a submitted match from DUPR (reverses its rating impact).
	DeleteMatch(matchCode, identifier string) error
	// GetPlayerRating looks up a player's current ratings by DUPR id, for
	// verifying a registrant's real rating against a division's band.
	GetPlayerRating(duprID string) (DuprRating, error)
	// SsoURL returns the iframe URL a user is sent to to connect (consent) their
	// DUPR account — base64(clientKey) embedded — plus the origin to validate the
	// postMessage from. Empty when DUPR isn't configured.
	SsoURL() (url string, origin string)
	// RegisterWebhook registers our HTTPS URL to receive RATING webhook events.
	RegisterWebhook(webhookURL string) error
	// SubscribeUserRating subscribes a connected user to RATING events; DUPR then
	// immediately posts a RATING_SEED with their current rating.
	SubscribeUserRating(duprID string) error
	// ClubMembers fetches a DUPR club's member roster (clubID "" -> the gateway's
	// configured club). Restricted partners get only connected users.
	ClubMembers(clubID string) ([]DuprMember, error)
	// GetEntitlements fetches a user's entitlement codes (BASIC_L1, PREMIUM_L1,
	// VERIFIED_L1, ...) via POST /subscription/active using the USER's SSO
	// access token (not the partner token), so DUPR+ registration gating can
	// be enforced. Returns ErrDuprUserTokenExpired on a 401 — refresh + retry.
	GetEntitlements(userToken string) ([]string, error)
	// RefreshUserToken exchanges a user's SSO refresh token for a fresh access
	// token (GET /auth/{v}/refresh with x-refresh-token).
	RefreshUserToken(refreshToken string) (string, error)
}

type MockDupr struct {
	ShouldSucceed bool
	seq           int
	Submitted     []DuprPayload
}

func NewMockDupr() *MockDupr { return &MockDupr{ShouldSucceed: true} }

func (m *MockDupr) SubmitMatch(p DuprPayload) (DuprResult, error) {
	m.seq++
	if !m.ShouldSucceed {
		return DuprResult{OK: false, Error: "DUPR rejected (mock)"}, nil
	}
	m.Submitted = append(m.Submitted, p)
	return DuprResult{OK: true, DuprMatchID: fmt.Sprintf("mock_dupr_%d", m.seq)}, nil
}

func (m *MockDupr) UpdateMatch(p DuprPayload) (DuprResult, error) {
	ref := p.MatchCode
	if ref == "" {
		m.seq++
		ref = fmt.Sprintf("mock_dupr_%d", m.seq)
	}
	return DuprResult{OK: true, DuprMatchID: ref}, nil
}

func (m *MockDupr) DeleteMatch(matchCode, identifier string) error { return nil }

func (m *MockDupr) GetPlayerRating(duprID string) (DuprRating, error) {
	if duprID == "" {
		return DuprRating{Found: false}, nil
	}
	// Deterministic stub so dev flows have a rating to work with.
	return DuprRating{
		Found: true, DuprID: duprID, FullName: "Mock Player",
		Doubles: 3.5, Singles: 3.5,
	}, nil
}

// SsoURL is empty for the mock — the connect UI shows "DUPR not configured".
func (m *MockDupr) SsoURL() (string, string)         { return "", "" }
func (m *MockDupr) RegisterWebhook(string) error     { return nil }
func (m *MockDupr) SubscribeUserRating(string) error { return nil }
func (m *MockDupr) ClubMembers(string) ([]DuprMember, error) {
	return []DuprMember{
		{DuprID: "MOCK01", FullName: "Mock Member One", Singles: "3.50", Doubles: "3.50"},
		{DuprID: "MOCK02", FullName: "Mock Member Two", Singles: "4.00", Doubles: "3.90"},
	}, nil
}

// GetEntitlements grants a mock user with any token every tier so dev/test
// flows can register into DUPR+ events; an empty token gets nothing.
func (m *MockDupr) GetEntitlements(userToken string) ([]string, error) {
	if userToken == "" {
		return nil, nil
	}
	return []string{"BASIC_L1", "PREMIUM_L1", "VERIFIED_L1"}, nil
}

func (m *MockDupr) RefreshUserToken(refreshToken string) (string, error) {
	return "mock-user-token", nil
}
