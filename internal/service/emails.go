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

	go func() {
		for _, rc := range recips {
			htmlBody, textBody := scheduleEmailBody(
				rc.name, eventName, when, where, rc.lines, scheduleURL, premium)
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
func scheduleEmailBody(fullName, eventName, when, where string, lines []string,
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

	footer := ""
	if !ownerPremium {
		footer = `<p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">
  Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a>
  — tournaments, minus the chaos.</p>`
	}

	htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">YOUR GAME SCHEDULE</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">%s</h1>
    </div>
    <div style="padding:24px 26px">
      <p style="margin:0 0 6px;color:#16203a;font-size:15px">Hi %s — here are your matches:</p>
      %s
      %s
      <a href="%s" style="display:block;margin:22px 0 4px;background:#f5c518;color:#16203a;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">Open the live schedule</a>
      <p style="margin:12px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">Times &amp; courts can shift on game day — the live schedule always has the latest.</p>
    </div>
  </div>%s
</div>`,
		esc(eventName), esc(firstName), meta, items.String(), scheduleURL, footer)

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
