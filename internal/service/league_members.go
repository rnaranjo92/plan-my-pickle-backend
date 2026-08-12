package service

import (
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// League membership: players join a league ONCE (added by the owner + invited by
// text/email) and are auto-rostered into every session (recurring Round Robin
// nights). A substitute for a given week is just a per-session roster edit on
// that night's event — membership itself doesn't model subs. Mirrors the
// coach_students add/invite/claim flow. All gated behind columnReady so it ships
// dark before add_league_members.sql runs.

func (s *Service) leagueMembersReady() bool {
	return s.columnReady("league_members", "id")
}

// leagueInviteURL builds the claim link; the token binds on claim regardless of
// which email/phone the invitee signs up with.
func leagueInviteURL(token string) string {
	u := "https://app.planmypickle.com/?invite=league"
	if token != "" {
		u += "&lt=" + url.QueryEscape(token)
	}
	return u
}

func mapLeagueMember(m map[string]any) model.LeagueMember {
	uid := asStr(m, "user_id")
	return model.LeagueMember{
		ID:        asStr(m, "id"),
		LeagueID:  asStr(m, "league_id"),
		UserID:    uid,
		FullName:  asStr(m, "full_name"),
		Email:     asStr(m, "email"),
		Phone:     asStr(m, "phone"),
		Linked:    uid != "",
		CreatedAt: asStr(m, "created_at"),
	}
}

// staff reports whether this caller holds the support/QA super-user grant and so
// may act as the owner of any entity. See Service.IsStaffEmail.
//
// Ownership is normally enforced up in the API layer (ownerOnly/ladderOwnerOK),
// which already honors the grant. The handful of service methods that ALSO check
// owner_id themselves were invisible to it, so a super user passed the route gate
// and then hit a 403 from inside the service — which is exactly what stopped a
// support account from adding members to another organizer's league.
func (s *Service) staff(email string) bool {
	return s.IsStaffEmail != nil && s.IsStaffEmail(email)
}

// canManageLeague returns the league row when the caller may WRITE to it — its
// owner, or a super user — and ErrForbidden otherwise. ErrNotFound for a missing
// league, so callers keep their existing not-found behavior.
func (s *Service) canManageLeague(leagueID, callerID, callerEmail string) (map[string]any, error) {
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=owner_id,name")
	if err != nil {
		return nil, err
	}
	if lg == nil {
		return nil, ErrNotFound
	}
	if asStr(lg, "owner_id") != callerID && !s.staff(callerEmail) {
		return nil, ErrForbidden
	}
	return lg, nil
}

// AddLeagueMember adds someone to a league's roster and invites them (owner, or a
// super user doing field support). Resolves an existing account by email/phone so
// a registered player links immediately; otherwise mints an invite token +
// texts/emails a join link.
func (s *Service) AddLeagueMember(leagueID, ownerID, callerEmail, name, email, phone string) (model.LeagueMember, error) {
	if !s.leagueMembersReady() {
		return model.LeagueMember{}, errors.New("league membership isn't available yet")
	}
	lg, err := s.canManageLeague(leagueID, ownerID, callerEmail)
	if err != nil {
		return model.LeagueMember{}, err
	}
	leagueName := asStr(lg, "name")

	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	np := normPhone(strings.TrimSpace(phone))
	if email != "" && !strings.Contains(email, "@") {
		return model.LeagueMember{}, errors.New("enter a valid email")
	}
	if email == "" && len(np) < 10 {
		return model.LeagueMember{}, errors.New("enter the player's email or phone")
	}

	// Already an active member? (match by email or phone)
	var dup map[string]any
	if email != "" {
		dup, _ = s.sb.SelectOne("league_members",
			"league_id=eq."+store.Q(leagueID)+"&email=eq."+store.Q(email)+
				"&left_at=is.null&select=id")
	}
	if dup == nil && np != "" {
		dup, _ = s.sb.SelectOne("league_members",
			"league_id=eq."+store.Q(leagueID)+"&phone=eq."+store.Q(np)+
				"&left_at=is.null&select=id")
	}
	if dup != nil {
		return model.LeagueMember{}, errors.New("that player is already a member")
	}

	row := map[string]any{
		"league_id": leagueID,
		"full_name": orNull(name),
		"email":     orNull(email),
		"phone":     orNull(np),
	}
	// Resolve to an account so an existing player links immediately instead of
	// being texted an invite they don't need.
	//
	// Uses the shared accountForContact resolver (auth.users by email, last-10
	// digits by phone). The old pair of helpers this replaced both missed real
	// accounts: userIDByEmail searched only `players`, so anyone who had an
	// account but had never registered for an event was invisible; and
	// userIDByPhone pre-filtered with `phone=like.*<digits>` against numbers
	// stored as "(619) 889-0619", which cannot match. Members is the invite flow,
	// so a miss here is the difference between a player being linked on the spot
	// and being sent a signup link they shouldn't need.
	resolved := s.accountForContact(email, np, name)
	if resolved != "" {
		row["user_id"] = resolved
	}
	inviteToken := ""
	if resolved == "" {
		inviteToken = newID()
		row["invite_token"] = inviteToken
	}
	ins, err := s.sb.Insert("league_members", row)
	if err != nil {
		return model.LeagueMember{}, err
	}
	if len(ins) == 0 {
		return model.LeagueMember{}, errors.New("could not add that member")
	}
	// Not on PlanMyPickle yet → invite via whatever channels were given.
	if resolved == "" {
		if np != "" {
			go s.sendLeagueInviteSMS(leagueName, np, inviteToken)
		}
		if email != "" {
			go s.sendLeagueInvite(leagueName, email, name, inviteToken)
		}
	}
	return mapLeagueMember(ins[0]), nil
}

// ClaimLeagueInvite binds an invite token to the caller (sets user_id on the
// still-unlinked member row), so the membership links no matter which email/phone
// they signed up with. Returns the league id.
func (s *Service) ClaimLeagueInvite(userID, token string) (string, error) {
	if !s.leagueMembersReady() {
		return "", errors.New("league membership isn't available yet")
	}
	token = strings.TrimSpace(token)
	if token == "" || userID == "" {
		return "", ErrForbidden
	}
	row, err := s.sb.SelectOne("league_members",
		"invite_token=eq."+store.Q(token)+"&select=id,user_id,league_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	leagueID := asStr(row, "league_id")
	if uid := asStr(row, "user_id"); uid != "" {
		if uid == userID {
			return leagueID, nil // idempotent
		}
		return "", ErrForbidden // already claimed by someone else
	}
	if _, err := s.sb.Update("league_members", "id=eq."+store.Q(asStr(row, "id")),
		map[string]any{"user_id": userID, "invite_token": nil}); err != nil {
		return "", err
	}
	return leagueID, nil
}

// isActiveLeagueMember reports whether the caller is an active member of the
// league (by linked account id, else by email). Used to let members post to the
// league video feed even before they've played their first session.
func (s *Service) isActiveLeagueMember(leagueID, userID, email string) bool {
	if !s.leagueMembersReady() || leagueID == "" {
		return false
	}
	if userID != "" {
		if r, _ := s.sb.SelectOne("league_members",
			"league_id=eq."+store.Q(leagueID)+"&user_id=eq."+store.Q(userID)+
				"&left_at=is.null&select=id"); r != nil {
			return true
		}
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email != "" {
		if r, _ := s.sb.SelectOne("league_members",
			"league_id=eq."+store.Q(leagueID)+"&email=eq."+store.Q(email)+
				"&left_at=is.null&select=id"); r != nil {
			return true
		}
	}
	return false
}

// ListLeagueMembers returns a league's active roster, newest first.
func (s *Service) ListLeagueMembers(leagueID string) ([]model.LeagueMember, error) {
	if !s.leagueMembersReady() {
		return []model.LeagueMember{}, nil
	}
	rows, err := s.sb.Select("league_members",
		"league_id=eq."+store.Q(leagueID)+"&left_at=is.null&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.LeagueMember, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapLeagueMember(r))
	}
	return out, nil
}

// RemoveLeagueMember soft-removes a member (owner, or a super user doing field
// support) — they stop being auto-rostered into future sessions; past results
// stay on the leaderboard. Reversible and history-preserving, which is why it
// sits on the allowed side of the super-user delete boundary (removing the whole
// LEAGUE never is).
func (s *Service) RemoveLeagueMember(leagueID, memberID, ownerID, callerEmail string) error {
	if !s.leagueMembersReady() {
		return errors.New("league membership isn't available yet")
	}
	if _, err := s.canManageLeague(leagueID, ownerID, callerEmail); err != nil {
		return err
	}
	// Grab the member's contact before removal so we can un-enroll them from the
	// coach roster (no-op unless the league is coach-led).
	m, _ := s.sb.SelectOne("league_members",
		"id=eq."+store.Q(memberID)+"&league_id=eq."+store.Q(leagueID)+"&select=email,phone")
	if _, err := s.sb.Update("league_members",
		"id=eq."+store.Q(memberID)+"&league_id=eq."+store.Q(leagueID),
		map[string]any{"left_at": now()}); err != nil {
		return err
	}
	if m != nil {
		// The member is LEAVING this league (left_at set), but their registrations
		// in it may persist — so exclude this league from the retention check
		// (wholeLeague=true) or they'd never be unenrolled.
		go s.unenrollLeagueCoachStudent(leagueID, asStr(m, "email"), asStr(m, "phone"), true)
	}
	return nil
}

// applyLeagueSessionDefaults seeds a freshly-created session (event) from its
// league: sets its court count and auto-rosters every active member into its
// first division. Best-effort, off the request path. Called when a session is
// linked (AddEventToLeague) and when the recurrence materializer spawns one.
func (s *Service) applyLeagueSessionDefaults(leagueID, eventID string) {
	if leagueID == "" || eventID == "" {
		return
	}
	// Court count from the league default.
	if s.columnReady("leagues", "court_count") {
		if lg, _ := s.sb.SelectOne("leagues",
			"id=eq."+store.Q(leagueID)+"&select=court_count"); lg != nil {
			if cc := asInt(lg, "court_count"); cc > 0 {
				_, _ = s.sb.Update("events", "id=eq."+store.Q(eventID),
					map[string]any{"num_courts": cc})
			}
		}
	}
	// Auto-roster active members into the session's first division.
	if !s.leagueMembersReady() {
		return
	}
	members, err := s.sb.Select("league_members",
		"league_id=eq."+store.Q(leagueID)+"&left_at=is.null"+
			"&select=full_name,email,phone")
	if err != nil || len(members) == 0 {
		return
	}
	bks, err := s.GetBrackets(eventID)
	if err != nil || len(bks) == 0 {
		return
	}
	bracketID := bks[0].ID
	for _, m := range members {
		_, _ = s.RegisterPlayer(eventID, model.RegisterRequest{
			FullName:   asStr(m, "full_name"),
			Email:      asStr(m, "email"),
			Phone:      asStr(m, "phone"),
			BracketID:  bracketID,
			Self:       false,
			TrustedAdd: true,
		}, "")
	}
}

// SubstituteInSession swaps a player OUT of a single session (event) and a
// substitute IN for that night only — the sub gets their own results, and the
// absent player simply misses this session (they stay a league member). Doubles
// partner linkage is preserved (the sub inherits the out player's partner). Owner
// or super user. Best used BEFORE the draw is generated; if a draw already
// exists, the organizer should regenerate it so the new pairing lands in the
// matches.
func (s *Service) SubstituteInSession(eventID, ownerID, callerEmail, outPlayerID, name, email, phone string) (model.Registration, error) {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=owner_id,perpetual")
	if err != nil {
		return model.Registration{}, err
	}
	if ev == nil {
		return model.Registration{}, ErrNotFound
	}
	if asStr(ev, "owner_id") != ownerID && !s.staff(callerEmail) {
		return model.Registration{}, ErrForbidden
	}
	// A perpetual (recurring-league) event is ONE ongoing tournament, so the
	// absent member stays permanently rostered — we just bench them for today
	// (uncheck) rather than delete their registration, and check the sub IN so
	// today's draw picks them up. A normal session event still removes the out
	// player (that session is discardable).
	perpetual := asBool(ev, "perpetual")
	if strings.TrimSpace(name) == "" {
		return model.Registration{}, errors.New("enter the substitute's name")
	}
	reg, err := s.sb.SelectOne("registrations",
		"event_id=eq."+store.Q(eventID)+"&player_id=eq."+store.Q(outPlayerID)+
			"&select=id,bracket_id,partner_id")
	if err != nil {
		return model.Registration{}, err
	}
	if reg == nil {
		return model.Registration{}, errors.New("that player isn't in this session")
	}
	bracketID := asStr(reg, "bracket_id")
	partnerID := asStr(reg, "partner_id")
	// A prior one-night sub with this contact may still be registered (cleanup
	// only runs at the next build), which would make the RegisterPlayer below
	// fail with ErrAlreadyRegistered when the SAME person subs again. Expire any
	// TAGGED substitute registration for this contact in this event first (never
	// touches a real member who happens to share the number).
	if s.columnReady("registrations", "is_substitute") {
		// Contacts are stored RAW on players — the app sends the formatted
		// "(619) 555-0100" — so NO SQL predicate on a normalized value can match
		// them: an exact eq. on "6195550100" misses, and even a substring LIKE
		// misses because the stored text contains "555-0100" (with a hyphen), not
		// "5550100". Do what CheckInByPhone does instead: pull THIS EVENT's
		// substitute registrations (a small, scoped set) with the player's contact
		// embedded, and compare normalized values in GO.
		e := strings.ToLower(strings.TrimSpace(email))
		np := normPhone(phone)
		if e != "" || np != "" {
			regs, rerr := s.sb.Select("registrations",
				"event_id=eq."+store.Q(eventID)+"&is_substitute=is.true"+
					"&select=id,player:players!player_id(email,phone)")
			if rerr == nil {
				stale := make([]string, 0, len(regs))
				for _, r := range regs {
					p := asMap(r, "player")
					if p == nil {
						continue
					}
					if np != "" && normPhone(asStr(p, "phone")) == np {
						stale = append(stale, asStr(r, "id"))
						continue
					}
					if e != "" && strings.EqualFold(
						strings.TrimSpace(asStr(p, "email")), e) {
						stale = append(stale, asStr(r, "id"))
					}
				}
				if len(stale) > 0 {
					_ = s.sb.Delete("registrations", "id="+store.In(stale))
				}
			}
		}
	}
	sub, err := s.RegisterPlayer(eventID, model.RegisterRequest{
		FullName:   strings.TrimSpace(name),
		Email:      strings.TrimSpace(email),
		Phone:      strings.TrimSpace(phone),
		BracketID:  bracketID,
		Self:       false,
		TrustedAdd: true,
		// A one-day sub shouldn't become a permanent coaching student.
		SkipCoachEnroll: true,
	}, "")
	if err != nil {
		return model.Registration{}, err
	}
	// Tag the sub so it can be auto-expired at the next session build (its played
	// games/standings survive) and, for fixed partners, its slot restored to the
	// benched member. Column-guarded: a pre-migration DB simply skips the tag and
	// the sub stays a normal registration (the prior behaviour).
	if s.columnReady("registrations", "is_substitute") {
		_, _ = s.sb.Update("registrations", "id=eq."+store.Q(sub.ID),
			map[string]any{"is_substitute": true, "substitute_for": outPlayerID})
	}
	if perpetual {
		// Bench the member for today (stay rostered) and mark the sub present so
		// the day's schedule includes them. Clear the member's partner link since
		// the sub takes that slot below.
		_, _ = s.sb.Update("registrations", "id=eq."+store.Q(asStr(reg, "id")),
			map[string]any{"checked_in": false, "checked_in_at": nil, "partner_id": nil})
		_, _ = s.sb.Update("registrations", "id=eq."+store.Q(sub.ID),
			map[string]any{
				"checked_in":    true,
				"checked_in_at": time.Now().UTC().Format(time.RFC3339),
			})
	} else {
		// Normal session event: remove the out player's registration entirely.
		_ = s.sb.Delete("registrations", "id=eq."+store.Q(asStr(reg, "id")))
	}
	// Preserve the doubles pairing: the sub takes the out player's partner slot.
	if partnerID != "" {
		_, _ = s.sb.Update("registrations", "id=eq."+store.Q(sub.ID),
			map[string]any{"partner_id": partnerID})
		if pr, _ := s.sb.SelectOne("registrations",
			"event_id=eq."+store.Q(eventID)+"&player_id=eq."+store.Q(partnerID)+
				"&select=id"); pr != nil {
			_, _ = s.sb.Update("registrations", "id=eq."+store.Q(asStr(pr, "id")),
				map[string]any{"partner_id": sub.PlayerID})
		}
	}
	// If today's session was ALREADY built (unscored), clear it so the organizer's
	// follow-up "Build schedule" re-seeds one clean session with the sub — a plain
	// append would otherwise DOUBLE the session. If today's games are already
	// scored, this is a no-op and the sub takes effect on the next build.
	if perpetual {
		_, _ = s.clearCurrentUnscoredSession(eventID)
	}
	return sub, nil
}

func (s *Service) sendLeagueInviteSMS(leagueName, phone, token string) {
	if s.Sms == nil {
		return
	}
	name := strings.TrimSpace(leagueName)
	if name == "" {
		name = "a league"
	}
	body := fmt.Sprintf(
		"You're invited to join %s on PlanMyPickle — you'll be in every session. Join free: %s",
		name, s.ShortLink(leagueInviteURL(token)))
	if r, err := s.Sms.Send(phone, body); err != nil || !r.OK {
		log.Printf("league: invite SMS to %s failed: %v", phone, err)
	}
}

func (s *Service) sendLeagueInvite(leagueName, email, name, token string) {
	if s.Email == nil || !s.Email.Live() {
		return
	}
	league := strings.TrimSpace(leagueName)
	if league == "" {
		league = "a league"
	}
	joinURL := leagueInviteURL(token)
	esc := html.EscapeString
	hi := ""
	if strings.TrimSpace(name) != "" {
		hi = " " + esc(strings.TrimSpace(name))
	}
	subject := "You're invited to join " + league + " on PlanMyPickle"
	htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">LEAGUE INVITE</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">Join %s</h1>
    </div>
    <div style="padding:24px 26px">
      <p style="margin:0 0 10px;color:#16203a;font-size:15px">Hi%s — you're invited to join <b>%s</b> on PlanMyPickle. Members are in every session automatically.</p>
      <a href="%s" style="display:block;margin:22px 0 4px;background:#f5c518;color:#16203a;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">Join the league</a>
      <p style="margin:10px 0 0;font-size:12.5px;color:#5b6b80;text-align:center">Free to join — the link connects you to the league.</p>
    </div>
  </div>
  <p style="margin:26px 0 0;font-size:12px;color:#8a96bd;text-align:center">Powered by <a href="https://planmypickle.com" style="color:#4f8b3b;text-decoration:none;font-weight:700">PlanMyPickle</a></p>
</div>`, esc(league), hi, esc(league), joinURL)
	text := fmt.Sprintf("You're invited to join %s on PlanMyPickle. Members are in every session automatically.\n\nJoin free:\n%s",
		league, joinURL)
	if err := s.Email.SendEmail(email, subject, htmlBody, text); err != nil {
		log.Printf("league: invite email to %s failed: %v", email, err)
	}
}

// InviteRegistrant texts/emails a roster player a link to get into the app, and
// reports which channels were used.
//
// Why this exists: adding someone to an event roster sends them NOTHING. An
// organizer can fill a roster from a paper signup sheet and every one of those
// players is invisible to the app — no invite, no account, and no way for them
// to see the event they were told they're in. The only flow that ever sent an
// invite was League -> Members, which is a different screen entirely (and on a
// perpetual league, was unreachable). This closes that gap from the roster,
// where the organizer already is.
//
// For a LEAGUE event it reuses the league_members invite so the link makes them
// a member of the whole league, not just today's session — same token flow,
// so claiming binds their account no matter which email/phone they sign up with.
// A standalone tournament gets a plain link to the event.
//
// Refuses when the player is already on an account (nothing to invite them to)
// or has no contact details at all (nothing to send to) — both are states the
// caller should surface rather than silently swallow.
func (s *Service) InviteRegistrant(eventID, regID, callerID, callerEmail string) (sms bool, email bool, err error) {
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=owner_id,name,league_id")
	if err != nil {
		return false, false, err
	}
	if ev == nil {
		return false, false, ErrNotFound
	}
	if asStr(ev, "owner_id") != callerID && !s.staff(callerEmail) {
		return false, false, ErrForbidden
	}

	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(regID)+"&event_id=eq."+store.Q(eventID)+"&select=player_id")
	if err != nil {
		return false, false, err
	}
	if reg == nil {
		return false, false, ErrNotFound
	}
	p, err := s.sb.SelectOne("players",
		"id=eq."+store.Q(asStr(reg, "player_id"))+"&select=full_name,email,phone,user_id")
	if err != nil {
		return false, false, err
	}
	if p == nil {
		return false, false, ErrNotFound
	}
	if asStr(p, "user_id") != "" {
		return false, false, errors.New("they're already on the app")
	}
	name := strings.TrimSpace(asStr(p, "full_name"))
	toEmail := strings.ToLower(strings.TrimSpace(asStr(p, "email")))
	toPhone := strings.TrimSpace(asStr(p, "phone"))
	if toEmail == "" && len(normPhone(toPhone)) < 10 {
		return false, false, errors.New("add their email or phone first, then invite")
	}

	// LEAGUE event → a league membership invite, so the link joins them to every
	// session rather than this one night.
	if leagueID := asStr(ev, "league_id"); leagueID != "" && s.leagueMembersReady() {
		leagueName := ""
		if lg, _ := s.sb.SelectOne("leagues",
			"id=eq."+store.Q(leagueID)+"&select=name"); lg != nil {
			leagueName = asStr(lg, "name")
		}
		token, terr := s.leagueInviteTokenFor(leagueID, name, toEmail, toPhone)
		if terr != nil {
			return false, false, terr
		}
		if token == "" {
			// Resolved to a real account while we looked — nothing to invite.
			return false, false, errors.New("they're already on the app")
		}
		if len(normPhone(toPhone)) >= 10 && s.Sms != nil {
			s.sendLeagueInviteSMS(leagueName, toPhone, token)
			sms = true
		}
		if toEmail != "" && s.Email != nil && s.Email.Live() {
			s.sendLeagueInvite(leagueName, toEmail, name, token)
			email = true
		}
		return sms, email, nil
	}

	// Standalone event → point them at the event itself.
	eventName := strings.TrimSpace(asStr(ev, "name"))
	if eventName == "" {
		eventName = "an event"
	}
	link := "https://app.planmypickle.com/?event=" + eventID
	if len(normPhone(toPhone)) >= 10 && s.Sms != nil {
		body := fmt.Sprintf(
			"You're on the roster for %s. See the schedule and your matches on PlanMyPickle: %s",
			eventName, s.ShortLink(link))
		if r, serr := s.Sms.Send(toPhone, body); serr == nil && r.OK {
			sms = true
		}
	}
	if toEmail != "" && s.Email != nil && s.Email.Live() {
		esc := html.EscapeString
		subject := "You're on the roster for " + eventName
		htmlBody := fmt.Sprintf(`<div style="background:#f6faf1;padding:28px 16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
  <div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:16px;overflow:hidden;border:1px solid #e7eedd">
    <div style="background:#16245c;padding:22px 26px">
      <p style="margin:0;color:#8dc63f;font-size:12px;font-weight:800;letter-spacing:1.4px">YOU'RE REGISTERED</p>
      <h1 style="margin:6px 0 0;color:#ffffff;font-size:22px;line-height:1.25">%s</h1>
    </div>
    <div style="padding:24px 26px">
      <p style="margin:0 0 10px;color:#16203a;font-size:15px">You're on the roster. Open it on PlanMyPickle to see the schedule, your matches and live standings.</p>
      <a href="%s" style="display:block;margin:22px 0 4px;background:#f5c518;color:#16203a;text-decoration:none;text-align:center;font-weight:800;font-size:15px;padding:13px 18px;border-radius:999px">Open the event</a>
    </div>
  </div>
</div>`, esc(eventName), link)
		text := fmt.Sprintf("You're on the roster for %s.\n\nOpen it on PlanMyPickle:\n%s", eventName, link)
		if eerr := s.Email.SendEmail(toEmail, subject, htmlBody, text); eerr == nil {
			email = true
		}
	}
	if !sms && !email {
		return false, false, errors.New("could not send — check that SMS/email is configured")
	}
	return sms, email, nil
}

// leagueInviteTokenFor returns a claimable invite token for this person on this
// league, reusing their existing member row when there is one so repeated
// invites don't pile up duplicate rows. Returns "" when the row already resolves
// to a real account (they need no invite).
func (s *Service) leagueInviteTokenFor(leagueID, name, email, phone string) (string, error) {
	np := normPhone(phone)
	var row map[string]any
	if email != "" {
		row, _ = s.sb.SelectOne("league_members",
			"league_id=eq."+store.Q(leagueID)+"&email=eq."+store.Q(email)+
				"&left_at=is.null&select=id,user_id,invite_token")
	}
	if row == nil && np != "" {
		row, _ = s.sb.SelectOne("league_members",
			"league_id=eq."+store.Q(leagueID)+"&phone=eq."+store.Q(np)+
				"&left_at=is.null&select=id,user_id,invite_token")
	}
	if row != nil {
		if asStr(row, "user_id") != "" {
			return "", nil
		}
		if tok := asStr(row, "invite_token"); tok != "" {
			return tok, nil
		}
		// Member row exists but lost its token (claimed then unlinked) — mint one.
		tok := newID()
		if _, err := s.sb.Update("league_members",
			"id=eq."+store.Q(asStr(row, "id")),
			map[string]any{"invite_token": tok}); err != nil {
			return "", err
		}
		return tok, nil
	}
	// No membership yet — create one so the invite has something to claim.
	tok := newID()
	if _, err := s.sb.Insert("league_members", map[string]any{
		"league_id":    leagueID,
		"full_name":    orNull(name),
		"email":        orNull(email),
		"phone":        orNull(np),
		"invite_token": tok,
	}); err != nil {
		return "", err
	}
	return tok, nil
}

// AutoInviteRosterAdd sends the invite automatically when an ORGANIZER adds a
// player to a LEAGUE's roster, so the roster is the only list they need — adding
// someone there now does what League -> Members used to do by hand.
//
// Scoped to league events on purpose. A standalone tournament keeps its manual
// "Send invite" action instead: auto-texting every hand-added entrant of a
// one-off tournament is a surprise (and a per-message cost) nobody asked for,
// whereas a league member genuinely needs a way in to every future session.
//
// Best-effort and silent: InviteRegistrant already refuses when the player is
// already on an account or has no contact details, which is exactly the no-op
// we want for self-registration, placeholder fills and known players.
func (s *Service) AutoInviteRosterAdd(eventID, regID, callerID, callerEmail string) {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=league_id")
	if err != nil || ev == nil || asStr(ev, "league_id") == "" {
		return
	}
	if _, _, ierr := s.InviteRegistrant(eventID, regID, callerID, callerEmail); ierr != nil {
		log.Printf("roster: auto-invite skipped for reg %s: %v", regID, ierr)
	}
}
