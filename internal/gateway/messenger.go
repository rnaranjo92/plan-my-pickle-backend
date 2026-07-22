package gateway

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Facebook Messenger channel — free court calls for players who opted in by
// opening a conversation with our Page (the check-in QR is an m.me?ref= link).
// Unlike SMS/WhatsApp we can't cold-message a phone: Meta only lets us reach a
// person by their Page-Scoped ID (PSID), captured when they message the Page.
// Inside the 24-hour standard messaging window every send is free — and a
// tournament runs within a day, so court calls almost always land in-window.
//
// Wired in from main only when MESSENGER_PAGE_TOKEN is present; otherwise the
// service keeps MockMessenger (records but never calls Meta).

// MessengerResult mirrors SmsResult: OK plus the provider's message id.
type MessengerResult struct {
	OK        bool
	MessageID string
}

// MessengerGateway sends a Messenger message to a PSID. Send is best-effort and
// must NEVER return a non-nil error (a bad PSID or a Meta hiccup can't abort a
// round start) — failure is reported via MessengerResult.OK=false.
type MessengerGateway interface {
	Send(psid, text string) (MessengerResult, error)
	// VerifySignature reports whether a webhook body carries a valid
	// X-Hub-Signature-256 for our app secret. Returns true when no app secret is
	// configured (dev/mock) so local testing isn't blocked.
	VerifySignature(body []byte, header string) bool
}

// MockMessenger records sends without calling Meta (the default until a Page
// token is configured). VerifySignature always passes.
type MockMessenger struct {
	seq  int
	Sent []struct{ PSID, Text string }
}

func NewMockMessenger() *MockMessenger { return &MockMessenger{} }

func (m *MockMessenger) Send(psid, text string) (MessengerResult, error) {
	m.seq++
	m.Sent = append(m.Sent, struct{ PSID, Text string }{psid, text})
	return MessengerResult{OK: true, MessageID: fmt.Sprintf("mock_msgr_%d", m.seq)}, nil
}

func (m *MockMessenger) VerifySignature([]byte, string) bool { return true }

// MetaMessenger is the real Send API gateway.
type MetaMessenger struct {
	pageToken string // Page Access Token (from the connected FB Page)
	appSecret string // App Secret — signs/verifies webhook payloads
	graphVer  string // Graph API version, e.g. "v23.0"
	http      *http.Client
}

// NewMetaMessenger builds a Messenger gateway. appSecret may be empty (webhook
// signature checks then pass through — configure it in production).
func NewMetaMessenger(pageToken, appSecret string) *MetaMessenger {
	return &MetaMessenger{
		pageToken: strings.TrimSpace(pageToken),
		appSecret: strings.TrimSpace(appSecret),
		graphVer:  "v23.0",
		http:      &http.Client{Timeout: 10 * time.Second},
	}
}

func (m *MetaMessenger) Send(psid, text string) (MessengerResult, error) {
	psid = strings.TrimSpace(psid)
	if psid == "" || m.pageToken == "" {
		return MessengerResult{OK: false}, nil
	}
	// messaging_type RESPONSE = a reply within the standard 24h window (free).
	payload := map[string]any{
		"recipient":      map[string]string{"id": psid},
		"messaging_type": "RESPONSE",
		"message":        map[string]string{"text": text},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("messenger: marshal failed: %v", err)
		return MessengerResult{OK: false}, nil
	}
	endpoint := fmt.Sprintf("https://graph.facebook.com/%s/me/messages?access_token=%s",
		m.graphVer, m.pageToken)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		log.Printf("messenger: build request failed: %v", err)
		return MessengerResult{OK: false}, nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		log.Printf("messenger: send to %s failed: %v", psid, err)
		return MessengerResult{OK: false}, nil
	}
	defer resp.Body.Close()

	var out struct {
		MessageID string `json:"message_id"`
		Error     struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(body, &out)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && out.MessageID != "" {
		return MessengerResult{OK: true, MessageID: out.MessageID}, nil
	}
	log.Printf("messenger: send to %s rejected (http %d, code %d): %s",
		psid, resp.StatusCode, out.Error.Code, out.Error.Message)
	return MessengerResult{OK: false}, nil
}

// VerifySignature validates Meta's X-Hub-Signature-256 header (format
// "sha256=<hex>") as HMAC-SHA256(body, appSecret), in constant time. With no app
// secret configured it returns true so dev/mock isn't blocked.
func (m *MetaMessenger) VerifySignature(body []byte, header string) bool {
	if m.appSecret == "" {
		return true
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(m.appSecret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}
