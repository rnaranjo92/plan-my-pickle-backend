package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TwilioSms is the real SMS gateway: it posts to Twilio's Messages API. It is
// wired in from main only when the TWILIO_* env vars are present; otherwise the
// server keeps the MockSms (records but never sends).
//
// Send is intentionally best-effort — it NEVER returns a non-nil error. A bad
// phone number or a Twilio hiccup must not abort "Start round", which texts
// every player in the round; the failure is reported via SmsResult.OK=false
// (recorded as a 'failed' notification) and logged for debugging.
type TwilioSms struct {
	accountSID string
	authToken  string
	// from is either a Twilio phone number in E.164 ("+1512…") or a Messaging
	// Service SID ("MG…"); we route on the prefix.
	from string
	http *http.Client
}

// NewTwilioSms builds a Twilio-backed SMS gateway. from may be an E.164 number
// or a Messaging Service SID (MG…).
func NewTwilioSms(accountSID, authToken, from string) *TwilioSms {
	return &TwilioSms{
		accountSID: accountSID,
		authToken:  authToken,
		from:       strings.TrimSpace(from),
		http:       &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *TwilioSms) Send(to, body string) (SmsResult, error) {
	dest := toE164(to)
	if dest == "+" || dest == "" {
		log.Printf("twilio: skipping send to unparseable number %q", to)
		return SmsResult{OK: false}, nil
	}

	form := url.Values{}
	form.Set("To", dest)
	if strings.HasPrefix(t.from, "MG") {
		form.Set("MessagingServiceSid", t.from)
	} else {
		form.Set("From", t.from)
	}
	form.Set("Body", body)

	endpoint := "https://api.twilio.com/2010-04-01/Accounts/" + t.accountSID + "/Messages.json"
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("twilio: build request failed: %v", err)
		return SmsResult{OK: false}, nil
	}
	req.SetBasicAuth(t.accountSID, t.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.http.Do(req)
	if err != nil {
		log.Printf("twilio: send to %s failed: %v", dest, err)
		return SmsResult{OK: false}, nil
	}
	defer resp.Body.Close()

	var out struct {
		SID     string `json:"sid"`
		Status  string `json:"status"`
		Code    int    `json:"code"`    // Twilio error code on failure
		Message string `json:"message"` // Twilio error message on failure
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &out)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && out.SID != "" {
		return SmsResult{OK: true, ProviderRef: out.SID}, nil
	}
	log.Printf("twilio: send to %s rejected (http %d, code %d): %s",
		dest, resp.StatusCode, out.Code, out.Message)
	return SmsResult{OK: false}, nil
}

// toE164 best-effort-normalizes a stored phone number to E.164 for Twilio.
// Already-prefixed numbers are kept; bare US 10-digit and 1+10 forms get "+1".
func toE164(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "+") {
		return "+" + digitsOnly(raw)
	}
	d := digitsOnly(raw)
	switch {
	case len(d) == 10:
		return "+1" + d
	case len(d) == 11 && d[0] == '1':
		return "+" + d
	default:
		return "+" + d // best effort for anything else
	}
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
