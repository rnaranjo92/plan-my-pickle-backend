package service

import (
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Transactional email composition. Sends are best-effort and always run off
// the request path (the caller wraps them in a goroutine): a mail hiccup must
// never fail a registration.

// poweredByFooter is the free-tier PlanMyPickle sign-off appended below the card.
// Premium events omit it (branding removed).
const poweredByFooter = `<p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">
  Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a>
  — tournaments, minus the chaos.</p>`

// emailBrand is the resolved organizer branding for one event's emails. The zero
// value renders the PlanMyPickle default look (navy header, green eyebrow, yellow
// button). Branding is a Premium perk, so brandFor returns the zero value for
// free-tier owners regardless of what's stored.
type emailBrand struct {
	logoURL   string
	color     string // #rrggbb accent; "" = default palette
	signature string
}

func brandFor(ev model.Event) emailBrand {
	if !ev.OwnerPremium {
		return emailBrand{}
	}
	// Re-sanitize at render time: these values are interpolated into style
	// attributes / an <img src>, so a junk value written directly in the DB (or
	// by an older code path) must never inject CSS or a bad scheme into a sent
	// email. color falls back to "" (→ default palette) if it isn't #rrggbb.
	return emailBrand{
		logoURL:   sanitizeHTTPURL(strings.TrimSpace(ev.EmailBrandLogoURL), 400),
		color:     sanitizeHexColor(ev.EmailBrandColor),
		signature: strings.TrimSpace(ev.EmailSignature),
	}
}

// readableText returns "#16203a" (dark) or "#ffffff" (white) — whichever has
// enough contrast to sit on the given #rrggbb background (perceived-luminance
// threshold). Keeps a header/button legible under ANY organizer accent color.
func readableText(hex string) string {
	if len(hex) != 7 || hex[0] != '#' {
		return "#ffffff"
	}
	v := func(a, b byte) int {
		hv := func(c byte) int {
			switch {
			case c >= '0' && c <= '9':
				return int(c - '0')
			case c >= 'a' && c <= 'f':
				return int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				return int(c-'A') + 10
			}
			return 0
		}
		return hv(a)*16 + hv(b)
	}
	r, g, b := v(hex[1], hex[2]), v(hex[3], hex[4]), v(hex[5], hex[6])
	if (299*r+587*g+114*b)/1000 > 150 {
		return "#16203a"
	}
	return "#ffffff"
}

// header renders the card's top band. Default = navy bg + green eyebrow + white
// title. With a brand color, the band takes the accent and the text flips to the
// readable shade. A brand logo, when set, sits above the eyebrow.
func (b emailBrand) header(eyebrow, title string) string {
	bg, eye, txt := "#16245c", "#8dc63f", "#ffffff"
	if b.color != "" {
		bg = b.color
		txt = readableText(bg)
		eye = txt
	}
	logo := ""
	if b.logoURL != "" {
		logo = fmt.Sprintf(`<img src="%s" alt="" style="height:38px;max-width:220px;margin:0 0 12px;display:block;object-fit:contain">`,
			html.EscapeString(b.logoURL))
	}
	return fmt.Sprintf(`<div style="background:%s;padding:22px 26px">%s<p style="margin:0;color:%s;font-size:12px;font-weight:800;letter-spacing:1.4px;opacity:0.92">%s</p><h1 style="margin:6px 0 0;color:%s;font-size:22px;line-height:1.25">%s</h1></div>`,
		bg, logo, eye, html.EscapeString(eyebrow), txt, html.EscapeString(title))
}

// button renders the primary CTA. Default yellow; a brand color overrides it with
// a contrast-picked label color.
func (b emailBrand) button(label, href string) string {
	bg, txt := "#f5c518", "#16203a"
	if b.color != "" {
		bg = b.color
		txt = readableText(bg)
	}
	return fmt.Sprintf(`<a href="%s" style="display:block;margin:22px 0 4px;background:%s;color:%s;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">%s</a>`,
		html.EscapeString(href), bg, txt, html.EscapeString(label))
}

// shell wraps body HTML in the branded card: header + body + optional signature,
// with the free-tier powered-by mark appended below the card. bodyHTML is trusted
// (assembled by the caller from escaped parts).
func (b emailBrand) shell(eyebrow, title, bodyHTML string, ownerPremium bool) string {
	sig := ""
	if b.signature != "" {
		sig = fmt.Sprintf(`<p style="margin:20px 0 0;padding-top:14px;border-top:1px solid #eef2e6;font-size:13px;color:#5b6b80;line-height:1.5;white-space:pre-line">%s</p>`,
			html.EscapeString(b.signature))
	}
	pw := ""
	if !ownerPremium {
		pw = poweredByFooter
	}
	return fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    %s
    <div style="padding:24px 26px">
%s%s
    </div>
  </div>%s
</div>`, b.header(eyebrow, title), bodyHTML, sig, pw)
}

// customEmailBody renders a free-form organizer email (subject → title, message →
// body) through the shared branded shell. The message is plain text: escaped,
// newlines kept as <br>.
func customEmailBody(brand emailBrand, fullName, eventName, subject, message, eventURL string,
	ownerPremium bool) (string, string) {
	esc := html.EscapeString
	firstName := fullName
	if i := strings.IndexByte(fullName, ' '); i > 0 {
		firstName = fullName[:i]
	}
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\r", "\n")
	bodyHTML := fmt.Sprintf(
		`      <p style="margin:0 0 12px;color:#16203a;font-size:15px">Hi %s —</p>
      <div style="color:#16203a;font-size:14.5px;line-height:1.6">%s</div>
      %s`,
		esc(firstName), strings.ReplaceAll(esc(message), "\n", "<br>"),
		brand.button("Open the event", eventURL))
	title := subject
	if strings.TrimSpace(title) == "" {
		title = eventName
	}
	htmlBody := brand.shell(eventName, title, bodyHTML, ownerPremium)

	var tb strings.Builder
	fmt.Fprintf(&tb, "%s\n\nHi %s,\n\n%s\n\nEvent page: %s\n",
		title, firstName, message, eventURL)
	if brand.signature != "" {
		fmt.Fprintf(&tb, "\n%s\n", brand.signature)
	}
	if !ownerPremium {
		tb.WriteString("\n— Powered by PlanMyPickle (planmypickle.com)\n")
	}
	return htmlBody, tb.String()
}

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

	// Organizer overrides (optional): a custom subject and a personal note added
	// to the top of the confirmation. Empty/unset → the branded defaults. Sanitized
	// again at send time (clamp + strip CRLF) so a stale/direct-DB value can never
	// break the send or inject a header.
	subject := "You're in! " + ev.Name
	if ev.ConfirmEmailSubject != nil {
		if s := sanitizeEmailField(*ev.ConfirmEmailSubject, 120, true); s != "" {
			subject = s
		}
	}
	customMsg := ""
	if ev.ConfirmEmailMessage != nil {
		customMsg = sanitizeEmailField(*ev.ConfirmEmailMessage, 1000, false)
	}
	htmlBody, textBody := registrationEmailBody(
		brandFor(ev), fullName, ev.Name, when, where, division, eventURL, ev.OwnerPremium, customMsg)
	if err := s.Email.SendEmail(email, subject, htmlBody, textBody); err != nil {
		log.Printf("email: registration confirm to %s failed: %v", email, err)
	}
}

// registrationEmailBody renders the branded confirmation (HTML + plain text).
// Free-tier events carry the "Powered by PlanMyPickle" footer; Premium
// organizers' emails are unbranded (same rule as the app views / TV board).
func registrationEmailBody(brand emailBrand, fullName, eventName, when, where, division, eventURL string,
	ownerPremium bool, customMessage string) (string, string) {
	esc := html.EscapeString
	firstName := fullName
	if i := strings.IndexByte(fullName, ' '); i > 0 {
		firstName = fullName[:i]
	}

	// Optional organizer note, shown above the event details. Escaped, with
	// newlines preserved as <br> in HTML.
	noteHTML, noteText := "", ""
	if m := strings.TrimSpace(customMessage); m != "" {
		// Normalize line endings first so \r\n / \r don't leave a stray CR before
		// each <br> (or in the plain-text note).
		m = strings.ReplaceAll(strings.ReplaceAll(m, "\r\n", "\n"), "\r", "\n")
		noteHTML = fmt.Sprintf(`<div style="margin:0 0 16px;padding:12px 14px;background:#f2f8ea;border-left:4px solid #4f8b3b;border-radius:8px;color:#16203a;font-size:14px;line-height:1.5">%s</div>`,
			strings.ReplaceAll(esc(m), "\n", "<br>"))
		noteText = m + "\n\n"
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

	bodyHTML := fmt.Sprintf(`      <p style="margin:0 0 14px;color:#16203a;font-size:15px">Hi %s — you're locked in. Here's your event at a glance:</p>
      %s<table cellpadding="0" cellspacing="0" style="border-collapse:collapse">%s%s%s</table>
      %s
      <p style="margin:12px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">On game day you'll check in with a QR code — no clipboard, no line.</p>`,
		esc(firstName), noteHTML,
		row("When", when), row("Where", where), row("Division", division),
		brand.button("Open the event — schedule & live scores", eventURL))
	htmlBody := brand.shell("YOU'RE REGISTERED", eventName, bodyHTML, ownerPremium)

	var tb strings.Builder
	fmt.Fprintf(&tb, "You're registered — %s\n\nHi %s, you're locked in.\n\n", eventName, firstName)
	tb.WriteString(noteText)
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
	if brand.signature != "" {
		fmt.Fprintf(&tb, "\n%s\n", brand.signature)
	}
	if !ownerPremium {
		tb.WriteString("\n— Powered by PlanMyPickle (planmypickle.com)\n")
	}
	return htmlBody, tb.String()
}

// scheduleRecipient is one player queued for a schedule email.
type scheduleRecipient struct {
	email string
	name  string
	lines []string
}

// EmailScheduleToPlayers queues a personal game-schedule email (their matches:
// court, round, partner + opponents, plus a link to the live public schedule)
// for every registered player. Owner-triggered from the app.
//
// The recipient list is assembled synchronously (fast — DB reads only), but the
// actual Resend sends run OFF the request path in a throttled goroutine: a bulk
// blast must never block or time out the HTTP handler, and a timeout-driven
// retry must never double-send. Returns the number of players the blast was
// QUEUED for (delivery is async/best-effort). Works for both registration
// events (players in `registrations`) and team/MLP events (players in
// `event_team_members`).
func (s *Service) EmailScheduleToPlayers(eventID string) (int, error) {
	if s.Email == nil || !s.Email.Live() {
		return 0, fmt.Errorf("email is not configured")
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	matches, err := s.EventScheduleMatches(eventID)
	if err != nil {
		return 0, err
	}
	lines := scheduleLinesByPlayer(matches)

	idents, err := s.scheduleIdentities(ev)
	if err != nil {
		return 0, err
	}

	// Dedup by player_id, NOT by email: a player entered in two divisions has
	// two rows but should get ONE email (lines[pid] already aggregates both
	// divisions); conversely two distinct players who share a household inbox
	// each get their own personalized schedule.
	seen := map[string]bool{}
	var recips []scheduleRecipient
	for _, id := range idents {
		if id.pid == "" || id.email == "" || seen[id.pid] {
			continue
		}
		seen[id.pid] = true
		recips = append(recips, scheduleRecipient{
			email: id.email, name: id.name, lines: lines[id.pid],
		})
	}

	when := "Date to be announced"
	if ev.StartsAt != nil && *ev.StartsAt != "" {
		if t, err := time.Parse(time.RFC3339, *ev.StartsAt); err == nil {
			when = t.Format("Monday, January 2, 2006 · 3:04 PM")
		}
	}
	where := ""
	if ev.Location != nil {
		where = *ev.Location
	}
	if ev.VenueName != nil && *ev.VenueName != "" {
		where = strings.TrimSpace(strings.TrimSuffix(*ev.VenueName+" — "+where, " — "))
	}
	scheduleURL := "https://app.planmypickle.com/?schedule=" + ev.ID
	eventName, premium := ev.Name, ev.OwnerPremium
	brand := brandFor(ev)

	go func() {
		for _, rc := range recips {
			htmlBody, textBody := scheduleEmailBody(
				brand, rc.name, eventName, when, where, rc.lines, scheduleURL, premium)
			if err := s.Email.SendEmail(
				rc.email, "Your schedule — "+eventName, htmlBody, textBody); err != nil {
				log.Printf("email: schedule to %s failed: %v", rc.email, err)
			}
			// Stay well under Resend's default rate limit (~2 req/s); this runs
			// in the background so the extra wall-clock is invisible to the user.
			time.Sleep(400 * time.Millisecond)
		}
		log.Printf("email: schedule blast for %s dispatched to %d players",
			eventID, len(recips))
	}()

	return len(recips), nil
}

// EmailInstructionsToPlayers sends the organizer's pre-tournament briefing to
// every player in the event with an email on file — the "player instructions in
// writing" a Tournament Director provides under USA Pickleball 13.B (check-in
// window, court-call/forfeit timing, warm-up, parking, contact, etc.). The
// message is free text the organizer composes. Best-effort, throttled, and off
// the request path (a mail hiccup must never fail the call). Returns how many
// recipients were queued.
func (s *Service) EmailInstructionsToPlayers(eventID, message string) (int, error) {
	if s.Email == nil || !s.Email.Live() {
		return 0, fmt.Errorf("email is not configured")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return 0, fmt.Errorf("message is empty")
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	idents, err := s.scheduleIdentities(ev)
	if err != nil {
		return 0, err
	}
	// Dedup by player_id (a player in two divisions gets ONE email); two players
	// sharing a household inbox each still get their own.
	seen := map[string]bool{}
	type recip struct{ email, name string }
	var recips []recip
	for _, id := range idents {
		if id.pid == "" || id.email == "" || seen[id.pid] {
			continue
		}
		seen[id.pid] = true
		recips = append(recips, recip{email: id.email, name: id.name})
	}
	eventURL := "https://app.planmypickle.com/?event=" + ev.ID
	eventName, premium := ev.Name, ev.OwnerPremium
	brand := brandFor(ev)
	go func() {
		for _, rc := range recips {
			htmlBody, textBody := instructionsEmailBody(
				brand, rc.name, eventName, message, eventURL, premium)
			if err := s.Email.SendEmail(
				rc.email, "Tournament info — "+eventName, htmlBody, textBody); err != nil {
				log.Printf("email: instructions to %s failed: %v", rc.email, err)
			}
			time.Sleep(400 * time.Millisecond)
		}
		log.Printf("email: instructions blast for %s dispatched to %d players",
			eventID, len(recips))
	}()
	return len(recips), nil
}

// customEmailRecip is one resolved recipient for a custom organizer blast.
type customEmailRecip struct{ email, name string }

// customEmailRecipients resolves the audience for a custom email by segment:
//
//	"" | "all"  → every registered player (registrations, or team members)
//	"checkedIn" → only players checked in at the event
//	"waitlist"  → the event's waitlist entries
//
// Dedup keys differ per source (player_id for players, lowercased email for the
// waitlist, which has no player_id) so a person is emailed once. Rows without an
// email are skipped.
func (s *Service) customEmailRecipients(ev model.Event, segment string) ([]customEmailRecip, error) {
	seen := map[string]bool{}
	var out []customEmailRecip
	add := func(key, email, name string) {
		email = strings.TrimSpace(email)
		if email == "" || key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, customEmailRecip{email: email, name: name})
	}
	switch segment {
	case "waitlist":
		rows, err := s.sb.SelectAll("event_waitlist",
			"event_id=eq."+store.Q(ev.ID)+"&select=full_name,email")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			e := asStr(r, "email")
			add(strings.ToLower(strings.TrimSpace(e)), e, asStr(r, "full_name"))
		}
	case "checkedIn":
		rows, err := s.sb.SelectAll("registrations",
			"event_id=eq."+store.Q(ev.ID)+"&checked_in=eq.true"+
				"&select=player_id,player:players!player_id(full_name,email)")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			p := asMap(r, "player")
			if p == nil {
				continue
			}
			add(asStr(r, "player_id"), asStr(p, "email"), asStr(p, "full_name"))
		}
	default: // "all" and ""
		idents, err := s.scheduleIdentities(ev)
		if err != nil {
			return nil, err
		}
		for _, id := range idents {
			add(id.pid, id.email, id.name)
		}
	}
	return out, nil
}

// SendCustomEmail sends a free-form organizer email (subject + message) to a
// segment of the event's people (see customEmailRecipients). Returns the number
// of recipients the blast was QUEUED for — delivery is async/best-effort, run off
// the request path in a throttled goroutine (same as the schedule/instructions
// blasts) so a bulk send never blocks or times out the handler.
func (s *Service) SendCustomEmail(eventID, subject, message, segment string) (int, error) {
	if s.Email == nil || !s.Email.Live() {
		return 0, fmt.Errorf("email is not configured")
	}
	subject = sanitizeEmailField(subject, 140, true)
	message = strings.TrimSpace(message)
	if subject == "" {
		return 0, fmt.Errorf("subject is required")
	}
	if message == "" {
		return 0, fmt.Errorf("message is required")
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	recips, err := s.customEmailRecipients(ev, segment)
	if err != nil {
		return 0, err
	}
	// Refuse a second concurrent blast for the same event: a double-click or a
	// timeout-retry would otherwise stack goroutines that each walk the whole
	// segment — double-sending to everyone and spiking the send rate past Resend's
	// limit (starving transactional mail like registration confirmations).
	s.customEmailMu.Lock()
	if s.customEmailInFlight == nil {
		s.customEmailInFlight = map[string]bool{}
	}
	if s.customEmailInFlight[eventID] {
		s.customEmailMu.Unlock()
		return 0, fmt.Errorf("an email is already sending for this event — give it a moment")
	}
	s.customEmailInFlight[eventID] = true
	s.customEmailMu.Unlock()

	eventURL := "https://app.planmypickle.com/?event=" + ev.ID
	brand := brandFor(ev)
	eventName, premium := ev.Name, ev.OwnerPremium
	go func() {
		defer func() {
			s.customEmailMu.Lock()
			delete(s.customEmailInFlight, eventID)
			s.customEmailMu.Unlock()
		}()
		for _, rc := range recips {
			htmlBody, textBody := customEmailBody(
				brand, rc.name, eventName, subject, message, eventURL, premium)
			if err := s.Email.SendEmail(rc.email, subject, htmlBody, textBody); err != nil {
				log.Printf("email: custom to %s failed: %v", rc.email, err)
			}
			time.Sleep(400 * time.Millisecond)
		}
		log.Printf("email: custom blast for %s (segment=%q) dispatched to %d",
			eventID, segment, len(recips))
	}()
	return len(recips), nil
}

// SendCustomEmailTest sends ONE copy of the composed email to a single address
// (the organizer's own), so they can preview the real thing before blasting.
func (s *Service) SendCustomEmailTest(eventID, subject, message, toEmail, toName string) error {
	if s.Email == nil || !s.Email.Live() {
		return fmt.Errorf("email is not configured")
	}
	subject = sanitizeEmailField(subject, 140, true)
	message = strings.TrimSpace(message)
	toEmail = strings.TrimSpace(toEmail)
	if subject == "" || message == "" {
		return fmt.Errorf("subject and message are required")
	}
	if toEmail == "" {
		return fmt.Errorf("no address to send the test to")
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return err
	}
	if toName == "" {
		toName = "there"
	}
	eventURL := "https://app.planmypickle.com/?event=" + ev.ID
	htmlBody, textBody := customEmailBody(
		brandFor(ev), toName, ev.Name, subject, message, eventURL, ev.OwnerPremium)
	return s.Email.SendEmail(toEmail, "[Test] "+subject, htmlBody, textBody)
}

// instructionsEmailBody renders the organizer's pre-tournament briefing (HTML +
// text). The organizer's message is plain text — escaped, with newlines kept as
// line breaks. Free-tier events carry the PlanMyPickle footer; Premium is clean.
func instructionsEmailBody(brand emailBrand, fullName, eventName, message, eventURL string,
	ownerPremium bool) (string, string) {
	esc := html.EscapeString
	firstName := fullName
	if i := strings.IndexByte(fullName, ' '); i > 0 {
		firstName = fullName[:i]
	}
	bodyHTML := fmt.Sprintf(
		`      <p style="margin:0 0 12px;color:#16203a;font-size:15px">Hi %s —</p>
      <div style="color:#16203a;font-size:14.5px;line-height:1.6">%s</div>
      %s`,
		esc(firstName), strings.ReplaceAll(esc(message), "\n", "<br>"),
		brand.button("Open the event", eventURL))
	htmlBody := brand.shell("PLAYER INSTRUCTIONS", eventName, bodyHTML, ownerPremium)

	var tb strings.Builder
	fmt.Fprintf(&tb, "Player instructions — %s\n\nHi %s,\n\n%s\n\nEvent page: %s\n",
		eventName, firstName, message, eventURL)
	if brand.signature != "" {
		fmt.Fprintf(&tb, "\n%s\n", brand.signature)
	}
	if !ownerPremium {
		tb.WriteString("\n— Powered by PlanMyPickle (planmypickle.com)\n")
	}
	return htmlBody, tb.String()
}

// scheduleIdent is a player's id + display name + email, from whichever source
// holds this event's players.
type scheduleIdent struct {
	pid, name, email string
}

// scheduleIdentities returns every player in the event with their email — from
// `registrations` for normal events, or from `event_team_members` for team/MLP
// events (which have no registrations). Emails live on the `players` row.
func (s *Service) scheduleIdentities(ev model.Event) ([]scheduleIdent, error) {
	if ev.TeamSize > 0 {
		teams, err := s.ListTeams(ev.ID)
		if err != nil {
			return nil, err
		}
		name := map[string]string{}
		var pids []string
		for _, t := range teams {
			for _, m := range t.Members {
				if m.PlayerID == nil || *m.PlayerID == "" {
					continue
				}
				if _, ok := name[*m.PlayerID]; ok {
					continue
				}
				name[*m.PlayerID] = m.FullName
				pids = append(pids, *m.PlayerID)
			}
		}
		email := map[string]string{}
		if len(pids) > 0 {
			// SelectAll (not Select) — Select silently caps at PostgREST's
			// max-rows, which would drop players past the cap on a big roster.
			prows, err := s.sb.SelectAll("players", "id="+store.In(pids)+"&select=id,email")
			if err != nil {
				return nil, err
			}
			for _, pr := range prows {
				email[asStr(pr, "id")] = strings.TrimSpace(asStr(pr, "email"))
			}
		}
		out := make([]scheduleIdent, 0, len(pids))
		for _, pid := range pids {
			out = append(out, scheduleIdent{pid: pid, name: name[pid], email: email[pid]})
		}
		return out, nil
	}

	rows, err := s.sb.SelectAll("registrations",
		"event_id=eq."+store.Q(ev.ID)+
			"&select=player_id,player:players!player_id(full_name,email)")
	if err != nil {
		return nil, err
	}
	out := make([]scheduleIdent, 0, len(rows))
	for _, r := range rows {
		p := asMap(r, "player")
		if p == nil {
			continue
		}
		out = append(out, scheduleIdent{
			pid:   asStr(r, "player_id"),
			name:  asStr(p, "full_name"),
			email: strings.TrimSpace(asStr(p, "email")),
		})
	}
	return out, nil
}

// scheduleLinesByPlayer maps each player_id to their own ordered schedule lines
// ("Court 3 · Round 2 — with Jane vs Bob / Sue"). Matches arrive already sorted
// round→court, so append order is the play order.
func scheduleLinesByPlayer(matches []model.Match) map[string][]string {
	lines := map[string][]string{}
	for _, m := range matches {
		court := "Court TBD"
		if m.CourtNumber != nil {
			court = fmt.Sprintf("Court %d", *m.CourtNumber)
		}
		when := ""
		if m.RoundNumber != nil {
			when = fmt.Sprintf("Round %d", *m.RoundNumber)
		} else if m.Stage == "bracket" {
			when = "Bracket"
		}
		for si, side := range m.Sides {
			opp := ""
			for oj, other := range m.Sides {
				if oj != si {
					opp = strings.Join(other.Players, " / ")
					break
				}
			}
			for pi, pid := range side.PlayerIDs {
				if pid == "" {
					continue
				}
				// Partner = same-side names EXCEPT this player, by INDEX (so two
				// partners who happen to share a display name don't cancel out).
				mates := make([]string, 0, len(side.Players))
				for j, nm := range side.Players {
					if j != pi && strings.TrimSpace(nm) != "" {
						mates = append(mates, nm)
					}
				}
				mate := strings.Join(mates, " / ")
				vs := "vs " + opp
				if opp == "" {
					vs = "(bye)"
				}
				parts := []string{court}
				if when != "" {
					parts = append(parts, when)
				}
				detail := strings.Join(parts, " · ")
				if mate != "" {
					detail += " — with " + mate + " " + vs
				} else {
					detail += " — " + vs
				}
				lines[pid] = append(lines[pid], detail)
			}
		}
	}
	return lines
}

// scheduleEmailBody renders a player's personal schedule email (HTML + text).
// Free-tier events carry the "Powered by PlanMyPickle" footer; Premium is
// unbranded — same rule as the confirmation email and the TV board.
func scheduleEmailBody(brand emailBrand, fullName, eventName, when, where string, lines []string,
	scheduleURL string, ownerPremium bool) (string, string) {
	esc := html.EscapeString
	firstName := fullName
	if i := strings.IndexByte(fullName, ' '); i > 0 {
		firstName = fullName[:i]
	}

	var items strings.Builder
	if len(lines) == 0 {
		items.WriteString(`<p style="margin:0;color:#5b6b80;font-size:14px">Your matches aren't posted yet — tap below to check the live schedule anytime.</p>`)
	} else {
		items.WriteString(`<table cellpadding="0" cellspacing="0" style="border-collapse:collapse;width:100%">`)
		for i, ln := range lines {
			fmt.Fprintf(&items, `<tr>
  <td style="padding:9px 12px 9px 0;color:#8dc63f;font-size:13px;font-weight:800;vertical-align:top">%d</td>
  <td style="padding:9px 0;color:#16203a;font-size:14.5px;border-bottom:1px solid #eef2e6">%s</td>
</tr>`, i+1, esc(ln))
		}
		items.WriteString(`</table>`)
	}

	meta := ""
	if when != "" || where != "" {
		w := esc(when)
		if where != "" {
			w += ` · ` + esc(where)
		}
		meta = fmt.Sprintf(`<p style="margin:0 0 16px;color:#5b6b80;font-size:13.5px">%s</p>`, w)
	}

	bodyHTML := fmt.Sprintf(`      <p style="margin:0 0 6px;color:#16203a;font-size:15px">Hi %s — here are your matches:</p>
      %s
      %s
      %s
      <p style="margin:12px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">Times &amp; courts can shift on game day — the live schedule always has the latest.</p>`,
		esc(firstName), meta, items.String(),
		brand.button("Open the live schedule", scheduleURL))
	htmlBody := brand.shell("YOUR GAME SCHEDULE", eventName, bodyHTML, ownerPremium)

	var tb strings.Builder
	fmt.Fprintf(&tb, "Your schedule — %s\n\nHi %s, here are your matches:\n\n", eventName, firstName)
	if when != "" {
		fmt.Fprintf(&tb, "%s\n", when)
	}
	if where != "" {
		fmt.Fprintf(&tb, "%s\n", where)
	}
	tb.WriteString("\n")
	if len(lines) == 0 {
		tb.WriteString("Your matches aren't posted yet — check the live schedule.\n")
	}
	for i, ln := range lines {
		fmt.Fprintf(&tb, "%d. %s\n", i+1, ln)
	}
	fmt.Fprintf(&tb, "\nLive schedule: %s\n", scheduleURL)
	fmt.Fprintf(&tb, "Times & courts can shift on game day — the live schedule has the latest.\n")
	if brand.signature != "" {
		fmt.Fprintf(&tb, "\n%s\n", brand.signature)
	}
	if !ownerPremium {
		tb.WriteString("\n— Powered by PlanMyPickle (planmypickle.com)\n")
	}
	return htmlBody, tb.String()
}

// SendVendorApprovedEmail tells an applicant their booth was approved — with
// the payment link when a booth fee is set. Best-effort, off the request path.
func (s *Service) SendVendorApprovedEmail(v model.Vendor) {
	email := strings.TrimSpace(v.ContactEmail)
	if email == "" || s.Email == nil || !s.Email.Live() {
		return
	}
	ev, err := s.GetEvent(v.EventID)
	if err != nil {
		log.Printf("email: vendor approval skipped (event fetch): %v", err)
		return
	}
	payURL := ""
	if v.FeeCents > 0 && v.PaymentStatus != "paid" && v.PayToken != "" {
		payURL = fmt.Sprintf(
			"https://app.planmypickle.com/?vendorpay=%s&t=%s", v.ID, v.PayToken)
	}
	fee := ""
	if v.FeeCents > 0 {
		fee = fmt.Sprintf("$%.2f", float64(v.FeeCents)/100)
	}
	subject := "You're in! Vendor spot approved — " + ev.Name

	esc := html.EscapeString
	action := ""
	textAction := "You're all set — see you at the event!\n"
	if payURL != "" {
		action = fmt.Sprintf(`<a href="%s" style="display:block;margin:22px 0 4px;background:#f5c518;color:#16203a;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">Pay the booth fee (%s)</a>
<p style="margin:10px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">Secure checkout — funds go to the event organizer.</p>`, payURL, esc(fee))
		textAction = fmt.Sprintf("Pay the booth fee (%s): %s\n", fee, payURL)
	}
	htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">VENDOR SPOT APPROVED</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">%s</h1>
    </div>
    <div style="padding:24px 26px">
      <p style="margin:0 0 10px;color:#16203a;font-size:15px">Great news — <b>%s</b> is approved for the Vendor Village at <b>%s</b>. Your booth now shows on the event page for every player and spectator.</p>
      %s
    </div>
  </div>
  <p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a></p>
</div>`, esc(ev.Name), esc(v.Name), esc(ev.Name), action)

	text := fmt.Sprintf("Vendor spot approved — %s\n\n%s is approved for the Vendor Village. Your booth now shows on the event page.\n\n%s",
		ev.Name, v.Name, textAction)
	if err := s.Email.SendEmail(email, subject, htmlBody, text); err != nil {
		log.Printf("email: vendor approval to %s failed: %v", email, err)
	}
}

// EmailVendorRecap queues a post-event "booth recap" email to each approved
// vendor with a contact email — a thank-you with their booth's exposure (link
// tap-throughs, court sponsorship) plus a soft "run it back" CTA to re-apply at
// the organizer's next event. Owner-triggered from the Vendor Village manager.
// Sends run OFF the request path in a throttled goroutine (same rules as the
// schedule blast). Returns how many vendors it was queued for.
func (s *Service) EmailVendorRecap(eventID string) (int, error) {
	if s.Email == nil || !s.Email.Live() {
		return 0, fmt.Errorf("email is not configured")
	}
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	vendors, err := s.ListVendors(eventID, true)
	if err != nil {
		return 0, err
	}
	type recip struct {
		email, name          string
		clicks, sponsorCourt int
	}
	var recips []recip
	for _, v := range vendors {
		if v.Status != "approved" {
			continue
		}
		email := strings.TrimSpace(v.ContactEmail)
		if email == "" {
			continue
		}
		recips = append(recips, recip{email, v.Name, v.Clicks, v.SponsorCourt})
	}

	eventName := ev.Name
	// The public "Become a vendor" form for the organizer's next event = the
	// retention CTA. (Points at this event's form; organizers reuse the link.)
	applyURL := "https://app.planmypickle.com/?vendor=" + ev.ID

	go func() {
		for _, rc := range recips {
			htmlBody, textBody := vendorRecapEmailBody(
				rc.name, eventName, rc.clicks, rc.sponsorCourt, applyURL)
			if err := s.Email.SendEmail(
				rc.email, "Your booth recap — "+eventName, htmlBody, textBody); err != nil {
				log.Printf("email: vendor recap to %s failed: %v", rc.email, err)
			}
			time.Sleep(400 * time.Millisecond)
		}
		log.Printf("email: vendor recap for %s dispatched to %d vendors",
			eventID, len(recips))
	}()
	return len(recips), nil
}

// vendorRecapEmailBody renders the post-event vendor thank-you/recap (always
// branded — this is PlanMyPickle reaching the vendor, B2B).
func vendorRecapEmailBody(vendorName, eventName string, clicks, sponsorCourt int,
	applyURL string) (string, string) {
	esc := html.EscapeString

	// Exposure line — graceful when there were no tracked taps yet.
	var exposure string
	if clicks > 0 {
		unit := "tap-throughs"
		if clicks == 1 {
			unit = "tap-through"
		}
		exposure = fmt.Sprintf("Your booth link earned <b>%d %s</b> from players and spectators.", clicks, unit)
	} else {
		exposure = "Your booth was live on the event page and the live TV scoreboard all day."
	}
	court := ""
	if sponsorCourt > 0 {
		court = fmt.Sprintf(` You also presented <b>Court %d</b> — your name rode every call and scoreboard for that court.`, sponsorCourt)
	}

	htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">BOOTH RECAP</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">%s</h1>
    </div>
    <div style="padding:24px 26px">
      <p style="margin:0 0 12px;color:#16203a;font-size:15px">Thanks for being part of <b>%s</b>, %s! Here's how your booth did:</p>
      <p style="margin:0 0 16px;color:#16203a;font-size:15px;line-height:1.6">%s%s</p>
      <a href="%s" style="display:block;margin:20px 0 4px;background:#f5c518;color:#16203a;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">Run it back — claim a booth at the next event</a>
      <p style="margin:12px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">See you on the courts.</p>
    </div>
  </div>
  <p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a></p>
</div>`,
		esc(eventName), esc(eventName), esc(vendorName),
		exposure, court, applyURL)

	var tb strings.Builder
	fmt.Fprintf(&tb, "Booth recap — %s\n\nThanks for being part of %s, %s!\n\n", eventName, eventName, vendorName)
	if clicks > 0 {
		fmt.Fprintf(&tb, "Your booth link earned %d tap-throughs from players and spectators.\n", clicks)
	} else {
		tb.WriteString("Your booth was live on the event page and the live TV scoreboard all day.\n")
	}
	if sponsorCourt > 0 {
		fmt.Fprintf(&tb, "You also presented Court %d.\n", sponsorCourt)
	}
	fmt.Fprintf(&tb, "\nRun it back — claim a booth at the next event: %s\n", applyURL)
	tb.WriteString("\n— Powered by PlanMyPickle (planmypickle.com)\n")
	return htmlBody, tb.String()
}
