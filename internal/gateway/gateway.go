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
	EventID      string
	DuprEventID  string
	Team1DuprIDs []string
	Team2DuprIDs []string
	Team1Score   int      // game 1 (legacy single-game field)
	Team2Score   int
	Games        [][2]int // per-game scores for a best-of-N match ([t1, t2] each)
}

type DuprResult struct {
	OK          bool
	DuprMatchID string
	Error       string
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

type DuprGateway interface {
	SubmitMatch(p DuprPayload) (DuprResult, error)
	// GetPlayerRating looks up a player's current ratings by DUPR id, for
	// verifying a registrant's real rating against a division's band.
	GetPlayerRating(duprID string) (DuprRating, error)
	// SsoURL returns the iframe URL a user is sent to to connect (consent) their
	// DUPR account — base64(clientKey) embedded — plus the origin to validate the
	// postMessage from. Empty when DUPR isn't configured.
	SsoURL() (url string, origin string)
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
func (m *MockDupr) SsoURL() (string, string) { return "", "" }
