package service

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// CourtBlockICS builds a public iCalendar (RFC 5545) feed marking an event's
// courts as reserved for its scheduled window. A facility subscribes to this
// URL in their booking system (CourtReserve, Skedda, PlayByPoint, Google
// Calendar, …) as an external court-CLOSURE calendar so a tournament's courts
// aren't double-booked against open play.
//
// This is deliberately a coarse court-BLOCK (one all-day-style busy event per
// tournament day), NOT a per-match calendar: the app's per-match wall-clock is
// a client-side schedule cascade with no server-side source of truth, so a
// per-match feed would disagree with what players see. The block only needs the
// event's start/end window + court count, which are authoritative.
func (s *Service) CourtBlockICS(eventID string) (string, error) {
	ev, err := s.GetEvent(eventID)
	if err != nil {
		return "", err
	}

	start := time.Time{}
	if ev.StartsAt != nil && *ev.StartsAt != "" {
		if t, err := time.Parse(time.RFC3339, *ev.StartsAt); err == nil {
			start = t.UTC()
		}
	}
	end := time.Time{}
	if ev.EndsAt != nil && *ev.EndsAt != "" {
		if t, err := time.Parse(time.RFC3339, *ev.EndsAt); err == nil {
			end = t.UTC()
		}
	}
	// Without a start date there's nothing to block, and a VCALENDAR with zero
	// components is invalid per RFC 5545 (strict facility importers reject it).
	// 404 so a subscription simply shows nothing until the event is scheduled.
	if start.IsZero() {
		return "", ErrNotFound
	}
	// Fall back to a sensible single-day window when the end time is missing.
	if end.IsZero() || !end.After(start) {
		end = start.Add(8 * time.Hour)
	}

	where := ""
	if ev.Location != nil {
		where = *ev.Location
	}
	if ev.VenueName != nil && *ev.VenueName != "" {
		where = strings.TrimSpace(strings.TrimSuffix(*ev.VenueName+" — "+where, " — "))
	}

	courts := "courts"
	if ev.NumCourts == 1 {
		courts = "1 court"
	} else if ev.NumCourts > 1 {
		courts = fmt.Sprintf("%d courts", ev.NumCourts)
	}
	summary := fmt.Sprintf("%s — %s reserved (PlanMyPickle)", ev.Name, courts)
	desc := fmt.Sprintf("Tournament in progress — %s reserved for PlanMyPickle. Live schedule: https://app.planmypickle.com/?schedule=%s",
		courts, ev.ID)

	var b strings.Builder
	// Every content line is folded to <=75 octets (RFC 5545 §3.1) so strict
	// importers (CourtReserve/Skedda) don't truncate long names/descriptions.
	crlf := func(line string) { b.WriteString(icsFold(line) + "\r\n") }
	crlf("BEGIN:VCALENDAR")
	crlf("VERSION:2.0")
	crlf("PRODID:-//PlanMyPickle//Court Blocks//EN")
	crlf("CALSCALE:GREGORIAN")
	crlf("METHOD:PUBLISH")
	crlf("X-WR-CALNAME:" + icsEscape(ev.Name+" — court blocks"))
	crlf("BEGIN:VEVENT")
	crlf("UID:courtblock-" + ev.ID + "@planmypickle.com")
	crlf("DTSTAMP:" + icsStamp(start)) // stable stamp (no wall-clock at gen time)
	crlf("DTSTART:" + icsStamp(start))
	crlf("DTEND:" + icsStamp(end))
	crlf("SUMMARY:" + icsEscape(summary))
	if where != "" {
		crlf("LOCATION:" + icsEscape(where))
	}
	crlf("DESCRIPTION:" + icsEscape(desc))
	crlf("TRANSP:OPAQUE") // shows as BUSY on the subscriber's calendar
	crlf("END:VEVENT")
	crlf("END:VCALENDAR")
	return b.String(), nil
}

// icsFold wraps a content line to <=75 octets per RFC 5545 §3.1, inserting a
// CRLF + leading space at each fold point and never splitting a multibyte rune.
func icsFold(line string) string {
	const max = 75
	if len(line) <= max {
		return line
	}
	var b strings.Builder
	seg := 0
	for _, r := range line {
		rl := utf8.RuneLen(r)
		if seg+rl > max {
			b.WriteString("\r\n ") // folded lines start with a single space
			seg = 1                // that leading space counts as one octet
		}
		b.WriteRune(r)
		seg += rl
	}
	return b.String()
}

// icsStamp formats a UTC time as an iCal date-time (e.g. 20260711T150000Z).
func icsStamp(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// icsEscape escapes the characters iCal reserves in TEXT values (RFC 5545 §3.3.11).
func icsEscape(s string) string {
	// Normalize any CR/CRLF to a single \n first so no bare CR survives into the
	// output (a stray CR corrupts line structure for strict parsers).
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, ";", `\;`)
	s = strings.ReplaceAll(s, ",", `\,`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
