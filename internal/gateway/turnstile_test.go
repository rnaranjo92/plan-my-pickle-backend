package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func newCaptcha(secret string, rt rtFunc) *Captcha {
	return &Captcha{secret: secret, http: newClient(rt)}
}

func TestCaptchaEnabled(t *testing.T) {
	if NewTurnstile("").Enabled() {
		t.Error("empty secret should be disabled")
	}
	if !NewTurnstile("  s3cr3t  ").Enabled() {
		t.Error("non-empty secret should be enabled")
	}
}

func TestCaptchaUnconfiguredFailsOpen(t *testing.T) {
	// No secret -> Verify returns true without any HTTP call (fail-open).
	called := false
	c := newCaptcha("", rtFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return resp(200, `{"success":true}`), nil
	}))
	if !c.Verify("anything", "") {
		t.Error("unconfigured Verify should fail open (true)")
	}
	if called {
		t.Error("unconfigured Verify must not hit the network")
	}
}

func TestCaptchaEmptyTokenFailsClosed(t *testing.T) {
	called := false
	c := newCaptcha("secret", rtFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return resp(200, `{"success":true}`), nil
	}))
	if c.Verify("   ", "1.2.3.4") {
		t.Error("empty token with secret should fail closed (false)")
	}
	if called {
		t.Error("empty token must not hit the network")
	}
}

func TestCaptchaGoodToken(t *testing.T) {
	var gotBody string
	c := newCaptcha("secret", rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return resp(200, `{"success":true}`), nil
	}))
	if !c.Verify("good-token", "9.9.9.9") {
		t.Error("expected success for a good token")
	}
	if !strings.Contains(gotBody, "secret=secret") ||
		!strings.Contains(gotBody, "response=good-token") ||
		!strings.Contains(gotBody, "remoteip=9.9.9.9") {
		t.Errorf("siteverify body missing fields: %s", gotBody)
	}
}

func TestCaptchaBadToken(t *testing.T) {
	c := newCaptcha("secret", rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"success":false,"error-codes":["invalid-input-response"]}`), nil
	}))
	if c.Verify("bad-token", "") {
		t.Error("expected failure for a rejected token")
	}
}

func TestCaptchaRequestErrorFailsClosed(t *testing.T) {
	c := newCaptcha("secret", rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	}))
	if c.Verify("token", "") {
		t.Error("transport error should fail closed (false)")
	}
}
