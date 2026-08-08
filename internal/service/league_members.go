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

// AddLeagueMember adds someone to a league's roster and invites them (owner
// only). Resolves an existing account by email/phone so a registered player
// links immediately; otherwise mints an invite token + texts/emails a join link.
func (s *Service) AddLeagueMember(leagueID, ownerID, name, email, phone string) (model.LeagueMember, error) {
	if !s.leagueMembersReady() {
		return model.LeagueMember{}, errors.New("league membership isn't available yet")
	}
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=owner_id,name")
	if err != nil {
		return model.LeagueMember{}, err
	}
	if lg == nil {
		return model.LeagueMember{}, ErrNotFound
	}
	if asStr(lg, "owner_id") != ownerID {
		return model.LeagueMember{}, ErrForbidden
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
	// Resolve to an account so a registered player links immediately.
	resolved := s.userIDByEmail(email)
	if resolved == "" && np != "" {
		resolved = s.userIDByPhone(np)
	}
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

// RemoveLeagueMember soft-removes a member (owner only) — they stop being
// auto-rostered into future sessions; past results stay on the leaderboard.
func (s *Service) RemoveLeagueMember(leagueID, memberID, ownerID string) error {
	if !s.leagueMembersReady() {
		return errors.New("league membership isn't available yet")
	}
	lg, err := s.sb.SelectOne("leagues", "id=eq."+store.Q(leagueID)+"&select=owner_id")
	if err != nil {
		return err
	}
	if lg == nil {
		return ErrNotFound
	}
	if asStr(lg, "owner_id") != ownerID {
		return ErrForbidden
	}
	// Grab the member's contact before removal so we can un-enroll them from the
	// coach roster (no-op unless the league is coach-led).
	m, _ := s.sb.SelectOne("league_members",
		"id=eq."+store.Q(memberID)+"&league_id=eq."+store.Q(leagueID)+"&select=email,phone")
	if _, err = s.sb.Update("league_members",
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
// only. Best used BEFORE the draw is generated; if a draw already exists, the
// organizer should regenerate it so the new pairing lands in the matches.
func (s *Service) SubstituteInSession(eventID, ownerID, outPlayerID, name, email, phone string) (model.Registration, error) {
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=owner_id,perpetual")
	if err != nil {
		return model.Registration{}, err
	}
	if ev == nil {
		return model.Registration{}, ErrNotFound
	}
	if asStr(ev, "owner_id") != ownerID {
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
		e := strings.ToLower(strings.TrimSpace(email))
		np := normPhone(phone)
		var pf string
		if np != "" {
			pf = "phone=eq." + store.Q(np)
		} else if e != "" {
			pf = "email=eq." + store.Q(e)
		}
		if pf != "" {
			if prows, _ := s.sb.Select("players", pf+"&select=id"); len(prows) > 0 {
				if pids := idList(prows, "id"); len(pids) > 0 {
					_ = s.sb.Delete("registrations",
						"event_id=eq."+store.Q(eventID)+
							"&player_id="+store.In(pids)+"&is_substitute=is.true")
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
