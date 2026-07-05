package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EmailGateway sends transactional email (registration confirmations, …).
// Real implementation is Resend; the mock records sends for tests and keeps
// the app fully functional with no key configured.
type EmailGateway interface {
	// SendEmail delivers one message. html is the rich body; text is the
	// plain-text alternative (never empty — some clients and spam filters
	// want it).
	SendEmail(to, subject, html, text string) error
	// Live reports whether real emails go out (false for the mock).
	Live() bool
}

// MockEmail records sends in memory; used in tests and whenever RESEND_API_KEY
// is unset.
type MockEmail struct {
	Sent []MockEmailMsg
}

type MockEmailMsg struct{ To, Subject, HTML, Text string }

func (m *MockEmail) SendEmail(to, subject, html, text string) error {
	m.Sent = append(m.Sent, MockEmailMsg{To: to, Subject: subject, HTML: html, Text: text})
	return nil
}
func (m *MockEmail) Live() bool { return false }

// ResendGateway sends through resend.com's REST API. The from address must be
// on a domain verified in the Resend dashboard or sends are rejected.
type ResendGateway struct {
	apiKey string
	from   string // e.g. `PlanMyPickle <hello@planmypickle.com>`
	http   *http.Client
}

func NewResendGateway(apiKey, from string) *ResendGateway {
	return &ResendGateway{
		apiKey: apiKey,
		from:   from,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (r *ResendGateway) Live() bool { return true }

func (r *ResendGateway) SendEmail(to, subject, html, text string) error {
	payload, err := json.Marshal(map[string]any{
		"from":    r.from,
		"to":      []string{to},
		"subject": subject,
		"html":    html,
		"text":    text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost,
		"https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("resend %d: %s", res.StatusCode, string(body))
	}
	return nil
}
