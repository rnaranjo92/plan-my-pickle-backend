package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// The weekly "what's on near you" digest.
//
// Not a newsletter: nobody writes it. It assembles itself from the events
// already in the system and is DIFFERENT for every recipient — their county,
// their week. That difference is the whole point, and it is also why it keeps
// working when the person who built it is busy for a month.
//
// SENDS NOTHING RATHER THAN NOTHING-TO-SAY. A county with no events this week
// produces no email at all. "There's nothing near you" is a worse message than
// silence: it teaches people the email is safe to ignore, and the one week
// there IS something they won't open it.

// digestDay is when it goes out. Thursday: far enough ahead of a weekend to
// plan around, late enough in the week that the weekend's events are firm.
const digestDay = time.Thursday

// digestWindowDays is how far ahead it looks. Ten days rather than seven so the
// weekend AFTER this one is visible — a tournament you learn about on Thursday
// for the following Saturday is one you can still enter.
const digestWindowDays = 10

// digestBatch bounds one run. The job re-runs hourly and picks up where it left
// off, so a big list drains over a few passes instead of one long request that
// can die halfway and lose its place.
const digestBatch = 200

// SendWeeklyDigest emails each person the events near them, once a week.
func (s *Service) SendWeeklyDigest() {
	if s.Email == nil || !s.Email.Live() {
		return // no mail gateway configured — nothing to do, quietly
	}
	now := jokeDay() // Pacific, same anchor the joke push uses
	if now.Weekday() != digestDay || now.Hour() < 8 {
		return
	}
	// NO day-level claim, deliberately.
	//
	// The first version took one, which quietly capped the whole feature at
	// digestBatch people: the batch is per RUN and the day-claim stopped every
	// later run that Thursday, so recipient 201 would have waited a week. The
	// comment said the job "picks up where it left off" and the code made that
	// impossible.
	//
	// The PER-PERSON claim below is already the concurrency guard — it is a
	// conditional update, so two containers racing on the same person means one
	// wins and one moves on. That is all the day-claim was buying, and it was
	// charging a batch limit for it.
	ready, err := s.columnReadyErr("pmp_profiles", "digest_sent_on")
	if err != nil || !ready {
		log.Printf("weekly digest: skipped — run add_weekly_digest.sql")
		return
	}

	recips, dropped := s.digestRecipients()
	// Claim the ones we can't mail at all, so they don't sit in the window
	// forever. They were never getting this week's email; leaving them
	// unclaimed only means re-resolving them every hour and, once there are
	// enough of them, crowding out everyone behind them.
	for _, uid := range dropped {
		s.claimDigest(uid, now)
	}

	sent, skipped, failed := 0, 0, 0
	for _, r := range recips {
		// CLAIM BEFORE SENDING. Stamping first means a crash costs one person
		// their digest; sending first means a re-run mails everyone twice.
		if !s.claimDigest(r.userID, now) {
			continue
		}
		evs := s.digestEventsFor(r.userID)
		if len(evs) == 0 {
			skipped++
			continue // nothing to say — say nothing
		}
		subject, htmlBody, text := digestEmail(r.name, r.place, evs, r.userID)
		if err := s.Email.SendEmail(r.email, subject, htmlBody, text); err != nil {
			log.Printf("weekly digest: send to %s failed: %v", r.email, err)
			// Give the claim back. A bad ten minutes at the mail provider would
			// otherwise burn through the whole batch — everyone stamped, nobody
			// mailed — and the hourly re-run, which exists to resume exactly
			// this, would step straight past them until next Thursday.
			//
			// The risk this reintroduces is a send that errored AFTER the
			// provider accepted it, which would double-send on the retry. That
			// is rare; a silent week of nothing is not.
			s.releaseDigest(r.userID)
			failed++
			continue
		}
		sent++
		// Stay well under Resend's default rate limit (~2 req/s), the same pace
		// the schedule blast uses. This is a background job, so the wall-clock
		// costs nobody anything — where a burst of 429s costs the whole batch.
		time.Sleep(400 * time.Millisecond)
	}
	log.Printf("weekly digest: %d sent, %d had nothing near them, "+
		"%d unplaceable, %d failed (will retry)",
		sent, skipped, len(dropped), failed)
}

type digestRecip struct {
	userID, email, name, place string
}

// digestRecipients finds people who can be emailed and haven't been this week.
//
// Also returns the ids it had to DROP — no address, or nowhere we can place
// them. Those matter to the caller: they still match the query, so unless they
// are claimed too they come back on every run and eventually fill the whole
// batch window with people who can never receive anything, and nobody behind
// them is ever fetched. "Signed up, no county, never joined an event" describes
// a lot of casual signups, so this is a matter of time, not a hypothetical.
func (s *Service) digestRecipients() (out []digestRecip, dropped []string) {
	cutoff := time.Now().UTC().AddDate(0, 0, -6).Format("2006-01-02")
	// NOT filtered on county.
	//
	// It was, and that was close to fatal: pmp_profiles.county went years with
	// nothing writing it, which is exactly why userHomeCounty grew a fallback to
	// the events somebody owns or plays in. Requiring a stored county here threw
	// that fallback away and left the digest reaching almost nobody — the same
	// null column that made every county-scoped feature look broken.
	//
	// So resolve per person below and skip the ones we genuinely can't place.
	// Ordered, so which slice of the list a run gets is defined rather than
	// whatever the planner felt like returning.
	rows, err := s.sb.Select("pmp_profiles",
		"digest_opt_out=is.false"+
			"&or=(digest_sent_on.is.null,digest_sent_on.lt."+cutoff+")"+
			"&select=user_id,full_name,county,state&order=user_id.asc"+
			"&limit="+strconv.Itoa(digestBatch))
	if err != nil {
		log.Printf("weekly digest: could not list recipients: %v", err)
		return nil, nil
	}
	out = make([]digestRecip, 0, len(rows))
	for _, r := range rows {
		uid := asStr(r, "user_id")
		if uid == "" {
			continue
		}
		// The address lives on the player row, not the profile.
		email := s.emailForUser(uid)
		if email == "" {
			dropped = append(dropped, uid)
			continue
		}
		// Blank counts as unset, the same rule userHomeCounty uses — an empty
		// string is what an empty form submit leaves behind, and treating it as
		// an answer would head the email "near ," and pull events from a county
		// it never names.
		place := strings.TrimSpace(asStr(r, "county"))
		st := strings.TrimSpace(asStr(r, "state"))
		if place == "" {
			place, st = s.userHomeCounty(uid) // the fallback: where they play
			place = strings.TrimSpace(place)
		}
		if place == "" {
			dropped = append(dropped, uid) // genuinely can't place them
			continue
		}
		if st != "" {
			place = place + ", " + st
		}
		out = append(out, digestRecip{
			userID: uid,
			email:  email,
			name:   strings.TrimSpace(asStr(r, "full_name")),
			place:  place,
		})
	}
	return out, dropped
}

// emailForUser returns an address for this account, or "".
//
// One account can own several player rows carrying different addresses, so the
// pick is ORDERED — otherwise the digest could arrive at one address this week
// and another the next, which reads as a different mailing list. Ordered on id
// rather than a date: nothing in this codebase reads created_at off players, and
// naming a column that isn't there fails the whole select, which here would
// quietly mean nobody is a recipient at all.
func (s *Service) emailForUser(userID string) string {
	row, err := s.sb.SelectOne("players",
		"user_id=eq."+store.Q(userID)+"&email=not.is.null&select=email"+
			"&order=id.asc")
	if err != nil || row == nil {
		return ""
	}
	return strings.TrimSpace(asStr(row, "email"))
}

// claimDigest marks this person as done for the week, and reports whether the
// claim was ours. Conditional update = an atomic claim, the same trick the
// daily jobs use: two containers can both try, only one wins.
func (s *Service) claimDigest(userID string, now time.Time) bool {
	cutoff := now.AddDate(0, 0, -6).Format("2006-01-02")
	rows, err := s.sb.Update("pmp_profiles",
		"user_id=eq."+store.Q(userID)+
			"&or=(digest_sent_on.is.null,digest_sent_on.lt."+cutoff+")",
		map[string]any{"digest_sent_on": now.Format("2006-01-02")})
	return err == nil && len(rows) > 0
}

// releaseDigest hands a claim back, so the next hourly tick retries this person.
// Unconditional: we only call it for a claim we just won ourselves.
func (s *Service) releaseDigest(userID string) {
	if _, err := s.sb.Update("pmp_profiles", "user_id=eq."+store.Q(userID),
		map[string]any{"digest_sent_on": nil}); err != nil {
		log.Printf("weekly digest: could not release the claim for %s: %v",
			userID, err)
	}
}

// digestEventsFor is what's on near this person in the window.
func (s *Service) digestEventsFor(userID string) []model.PublicEvent {
	county, _ := s.userHomeCounty(userID)
	if strings.TrimSpace(county) == "" {
		return nil
	}
	evs, err := s.PublicEvents(12, county)
	if err != nil {
		return nil
	}
	horizon := time.Now().UTC().AddDate(0, 0, digestWindowDays)
	out := make([]model.PublicEvent, 0, len(evs))
	for _, e := range evs {
		if e.StartsAt == nil || *e.StartsAt == "" {
			continue // undated events aren't news about THIS week
		}
		t, perr := time.Parse(time.RFC3339, *e.StartsAt)
		if perr != nil || t.After(horizon) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// digestEmail builds the message. Returns subject, HTML and a plain-text part.
func digestEmail(name, place string, evs []model.PublicEvent, userID string) (
	string, string, string) {
	who := strings.TrimSpace(name)
	if i := strings.IndexByte(who, ' '); i > 0 {
		who = who[:i] // first name only — this is a note, not a letter
	}
	// No "this week": the window is 10 days, deliberately, so the weekend AFTER
	// this one is in here. Promising a week and then listing something 9 days
	// out is a small lie in the first line the reader sees.
	subject := fmt.Sprintf("%d pickleball event%s near you",
		len(evs), plural(len(evs)))

	var b strings.Builder
	var t strings.Builder
	greet := "Here's what's on near you."
	if who != "" {
		greet = who + ", here's what's on near you."
	}
	b.WriteString(`<div style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;` +
		`max-width:560px;margin:0 auto;color:#16245C">`)
	b.WriteString(`<h1 style="font-size:20px;margin:0 0 4px">` + html.EscapeString(greet) + `</h1>`)
	if place != "" {
		b.WriteString(`<p style="margin:0 0 18px;color:#6b7280;font-size:13px">` +
			html.EscapeString(place) + `</p>`)
	}
	t.WriteString(greet + "\n\n")

	for _, e := range evs {
		when := digestWhen(e.StartsAt)
		where := ""
		if e.VenueName != nil {
			where = strings.TrimSpace(*e.VenueName)
		}
		if where == "" && e.Location != nil {
			where = strings.TrimSpace(*e.Location)
		}
		link := seoEventLink(e.ID)
		b.WriteString(`<div style="border:1px solid #e5e7eb;border-radius:12px;` +
			`padding:12px 14px;margin-bottom:10px">`)
		b.WriteString(`<a href="` + link + `" style="font-size:15px;font-weight:700;` +
			`color:#16245C;text-decoration:none">` + html.EscapeString(e.Name) + `</a>`)
		b.WriteString(`<div style="font-size:13px;color:#6b7280;margin-top:3px">` +
			html.EscapeString(strings.TrimSpace(when+" · "+where)) + `</div>`)
		b.WriteString(`</div>`)
		t.WriteString("• " + e.Name + " — " + when)
		if where != "" {
			t.WriteString(" · " + where)
		}
		t.WriteString("\n  " + link + "\n")
	}

	unsub := digestUnsubscribeLink(userID)
	b.WriteString(`<p style="font-size:11px;color:#9ca3af;margin-top:22px">` +
		`You're getting this because you play pickleball near ` +
		html.EscapeString(place) + `. ` +
		`<a href="` + unsub + `" style="color:#9ca3af">Unsubscribe</a>.</p>`)
	b.WriteString(`</div>`)
	t.WriteString("\nUnsubscribe: " + unsub + "\n")
	return subject, b.String(), t.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// digestZone is the timezone the email is WRITTEN in.
//
// Events are stored UTC, and printing that raw put a Friday 7pm league night in
// the email as "Sat · 2:00 AM" — the wrong DAY, in the one email whose entire
// job is saying when things are. A 9am Saturday tournament read as 4:00 PM.
//
// Pacific because that is where the events are; the joke push makes the same
// assumption. When that stops being true this wants to key off the EVENT's own
// locale rather than the sender's, which needs a timezone on the event row.
func digestZone() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.UTC // zoneinfo missing — UTC beats no email
}

// digestWhen writes the date the way somebody reads it, not the way it's stored.
func digestWhen(startsAt *string) string {
	if startsAt == nil {
		return "Date TBD"
	}
	t, err := time.Parse(time.RFC3339, *startsAt)
	if err != nil {
		return "Date TBD"
	}
	return t.In(digestZone()).Format("Mon, Jan 2 · 3:04 PM")
}

func seoEventLink(id string) string {
	return "https://planmypickle.com/e/" + id
}

// digestUnsubscribeLink is signed, so the link can't be used to unsubscribe
// somebody else by editing the id out of it.
func digestUnsubscribeLink(userID string) string {
	return "https://api.planmypickle.com/digest/unsubscribe?u=" + userID +
		"&t=" + digestToken(userID)
}

// digestToken is an HMAC over the user id. No new storage, and not guessable.
func digestToken(userID string) string {
	key := os.Getenv("SUPABASE_JWT_SECRET")
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("digest:" + userID))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// DigestUnsubscribe honours an unsubscribe link. Verifies the signature so the
// endpoint can't be walked to opt other people out.
func (s *Service) DigestUnsubscribe(userID, token string) error {
	if userID == "" || !hmac.Equal([]byte(token), []byte(digestToken(userID))) {
		return ErrForbidden
	}
	_, err := s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id":        userID,
		"digest_opt_out": true,
	})
	return err
}

