package service

import (
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Transactional email composition. Sends are best-effort and always run off
// the request path (the caller wraps them in a goroutine): a mail hiccup must
// never fail a registration.

// SendRegistrationEmail emails a branded confirmation to a just-registered
// player. Called from the register HTTP handler only — bulk imports and QA
// seeders deliberately don't email (a 150-row CSV import must not fire 150
// messages into the Resend quota). No-ops without an email address or when
// only the mock gateway is configured.
func (s *Service) SendRegistrationEmail(eventID, email, fullName, bracketID string) {
	email = strings.TrimSpace(email)
	if email == "" || s.Email == nil || !s.Email.Live() {
		return
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		log.Printf("email: registration confirm skipped (event fetch): %v", err)
		return
	}
	division := ""
	if bracketID != "" {
		if b, err := s.sb.SelectOne("brackets",
			"id=eq."+store.Q(bracketID)+"&select=name"); err == nil && b != nil {
			division = asStr(b, "name")
		}
	}
	when := "Date to be announced"
	if ev.StartsAt != nil && *ev.StartsAt != "" {
		if t, err := time.Parse(time.RFC3339, *ev.StartsAt); err == nil {
			when = t.Format("Monday, January 2, 2006 · 3:04 PM")
		}
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	where := deref(ev.Location)
	if v := deref(ev.VenueName); v != "" {
		where = strings.TrimSpace(strings.TrimSuffix(v+" — "+where, " — "))
	}
	eventURL := "https://app.planmypickle.com/?event=" + ev.ID

	subject := "You're in! " + ev.Name
	htmlBody, textBody := registrationEmailBody(
		fullName, ev.Name, when, where, division, eventURL, ev.OwnerPremium)
	if err := s.Email.SendEmail(email, subject, htmlBody, textBody); err != nil {
		log.Printf("email: registration confirm to %s failed: %v", email, err)
	}
}

// registrationEmailBody renders the branded confirmation (HTML + plain text).
// Free-tier events carry the "Powered by PlanMyPickle" footer; Premium
// organizers' emails are unbranded (same rule as the app views / TV board).
func registrationEmailBody(fullName, eventName, when, where, division, eventURL string,
	ownerPremium bool) (string, string) {
	esc := html.EscapeString
	firstName := fullName
	if i := strings.IndexByte(fullName, ' '); i > 0 {
		firstName = fullName[:i]
	}

	row := func(label, value string) string {
		if value == "" {
			return ""
		}
		return fmt.Sprintf(`<tr>
  <td style="padding:6px 14px 6px 0;color:#5b6b80;font-size:14px;white-space:nowrap;vertical-align:top">%s</td>
  <td style="padding:6px 0;color:#16203a;font-size:14px;font-weight:600">%s</td>
</tr>`, esc(label), esc(value))
	}

	footer := ""
	if !ownerPremium {
		footer = `<p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">
  Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a>
  — tournaments, minus the chaos.</p>`
	}

	htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">YOU'RE REGISTERED</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">%s</h1>
    </div>
    <div style="padding:24px 26px">
      <p style="margin:0 0 14px;color:#16203a;font-size:15px">Hi %s — you're locked in. Here's your event at a glance:</p>
      <table cellpadding="0" cellspacing="0" style="border-collapse:collapse">%s%s%s</table>
      <a href="%s" style="display:block;margin:22px 0 4px;background:#f5c518;color:#16203a;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">Open the event — schedule &amp; live scores</a>
      <p style="margin:12px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">On game day you'll check in with a QR code — no clipboard, no line.</p>
    </div>
  </div>%s
</div>`,
		esc(eventName), esc(firstName),
		row("When", when), row("Where", where), row("Division", division),
		eventURL, footer)

	var tb strings.Builder
	fmt.Fprintf(&tb, "You're registered — %s\n\nHi %s, you're locked in.\n\n", eventName, firstName)
	if when != "" {
		fmt.Fprintf(&tb, "When: %s\n", when)
	}
	if where != "" {
		fmt.Fprintf(&tb, "Where: %s\n", where)
	}
	if division != "" {
		fmt.Fprintf(&tb, "Division: %s\n", division)
	}
	fmt.Fprintf(&tb, "\nSchedule & live scores: %s\n", eventURL)
	fmt.Fprintf(&tb, "On game day you'll check in with a QR code.\n")
	if !ownerPremium {
		tb.WriteString("\n— Powered by PlanMyPickle (planmypickle.com)\n")
	}
	return htmlBody, tb.String()
}
