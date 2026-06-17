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

// Captcha verifies a Cloudflare Turnstile token server-side (siteverify). It
// guards the PUBLIC self-registration endpoint, which bypasses Supabase Auth
// and so can't lean on Supabase's CAPTCHA.
//
// It is enabled only when a secret is configured (TURNSTILE_SECRET). When
// unconfigured, Verify returns true (fail-open) so deploying this code never
// breaks registration before the secret is set; once the secret is present it
// fails closed on a missing/invalid token or a verification error.
type Captcha struct {
	secret string
	http   *http.Client
}

// NewTurnstile builds a verifier. An empty secret leaves it disabled (skip).
func NewTurnstile(secret string) *Captcha {
	return &Captcha{
		secret: strings.TrimSpace(secret),
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether a secret is configured (and thus verification runs).
func (c *Captcha) Enabled() bool { return c.secret != "" }

// Verify reports whether the Turnstile token is valid. Disabled (no secret) ->
// true. Enabled -> false unless Cloudflare confirms success. remoteip is
// optional ("" to omit).
func (c *Captcha) Verify(token, remoteip string) bool {
	if c.secret == "" {
		return true // not configured — skip the check
	}
	if strings.TrimSpace(token) == "" {
		return false
	}

	form := url.Values{}
	form.Set("secret", c.secret)
	form.Set("response", token)
	if remoteip != "" {
		form.Set("remoteip", remoteip)
	}

	resp, err := c.http.PostForm("https://challenges.cloudflare.com/turnstile/v0/siteverify", form)
	if err != nil {
		// Enabled but couldn't reach Cloudflare: fail closed (the page is meant
		// to be protected). siteverify is highly available; this is rare.
		log.Printf("turnstile: siteverify request failed: %v", err)
		return false
	}
	defer resp.Body.Close()

	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &out)
	if !out.Success {
		log.Printf("turnstile: token rejected: %v", out.ErrorCodes)
	}
	return out.Success
}
