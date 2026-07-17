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
	// Never spend a carrier processing fee on a fictional/unroutable number (demo
	// +1555… seeds, reserved 555-01XX). It would only fail — skip before Twilio.
	if IsFictionalNANP(dest) {
		log.Printf("twilio: skipping fictional/unroutable number %q", dest)
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

// IsNANP reports whether a stored phone number belongs to the North American
// Numbering Plan (US/Canada, country code +1) — the only region our Twilio A2P
// 10DLC campaign can reach. International numbers should fall back to push
// notifications instead of a guaranteed-to-fail SMS. Matches toE164's rules:
// an explicit "+" prefix must be +1 with 11 digits; bare 10-digit and 1+10
// forms are treated as US.
func IsNANP(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "+") {
		d := digitsOnly(raw)
		return len(d) == 11 && d[0] == '1'
	}
	d := digitsOnly(raw)
	return len(d) == 10 || (len(d) == 11 && d[0] == '1')
}

// IsFictionalNANP reports whether a NANP number is reserved/fictional or
// structurally invalid — so carriers can't route it. Catches the +1555… demo/
// seed placeholders (area code 555 is unassigned) and the 555-0100…0199
// directory range. Used to skip sends that would only fail + get billed.
func IsFictionalNANP(raw string) bool {
	d := digitsOnly(raw)
	if len(d) == 11 && d[0] == '1' {
		d = d[1:]
	}
	if len(d) != 10 {
		return false // not a 10-digit NANP number — leave reachability to IsNANP
	}
	npa, nxx, line := d[0:3], d[3:6], d[6:10]
	// Area code must be [2-9]XX and never the unassigned 555 (the demo's NPA).
	if npa[0] < '2' || npa == "555" {
		return true
	}
	// Reserved fictional exchange 555-0100..555-0199.
	if nxx == "555" && line >= "0100" && line <= "0199" {
		return true
	}
	return false
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
