package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Claiming a roster row.
//
// An organizer can add players as bare names, which is fast but leaves those
// people invisible to themselves: no contact, no account, and a match history
// that belongs to nobody. Claiming is how such a row finds its owner — the
// player proves nothing about their phone or email, they simply follow a link
// the ORGANIZER handed them, which is the same trust model as being added in
// the first place.
//
// Names are never matched automatically. Two players called Mike Johnson would
// inherit each other's records, and a claimed row also opens that event's
// private feed — so identity here is always something the organizer hands over
// explicitly, never something the system guesses.

func (s *Service) claimTokensReady() bool {
	return s.columnReady("registrations", "claim_token")
}

// RegistrationClaimToken returns the claim token for a registration, minting one
// on first use. Owner-gated by the caller. Refuses rows that are already tied to
// an account — there is nothing left to claim, and handing out a live token for
// a real person's row would let anyone holding it take over their identity.
func (s *Service) RegistrationClaimToken(eventID, regID, callerID, callerEmail string) (string, error) {
	if !s.claimTokensReady() {
		return "", errors.New("claim links aren't available yet")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=owner_id")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	if asStr(ev, "owner_id") != callerID && !s.staff(callerEmail) {
		return "", ErrForbidden
	}
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(regID)+"&event_id=eq."+store.Q(eventID)+
			"&select=id,player_id,claim_token")
	if err != nil {
		return "", err
	}
	if reg == nil {
		return "", ErrNotFound
	}
	p, err := s.sb.SelectOne("players",
		"id=eq."+store.Q(asStr(reg, "player_id"))+"&select=user_id")
	if err != nil {
		return "", err
	}
	if p != nil && asStr(p, "user_id") != "" {
		return "", errors.New("that player is already on the app")
	}
	if tok := strings.TrimSpace(asStr(reg, "claim_token")); tok != "" {
		return tok, nil
	}
	tok := newID()
	if _, err := s.sb.Update("registrations", "id=eq."+store.Q(regID),
		map[string]any{"claim_token": tok}); err != nil {
		return "", err
	}
	return tok, nil
}

// ClaimRegistrationByToken binds the registration behind this token to the
// signed-in caller. Returns the event id so the app can open it.
func (s *Service) ClaimRegistrationByToken(token, userID, email string) (string, error) {
	if !s.claimTokensReady() {
		return "", errors.New("claim links aren't available yet")
	}
	token = strings.TrimSpace(token)
	if token == "" || userID == "" {
		return "", ErrForbidden
	}
	reg, err := s.sb.SelectOne("registrations",
		"claim_token=eq."+store.Q(token)+"&select=id,event_id,player_id")
	if err != nil {
		return "", err
	}
	if reg == nil {
		return "", ErrNotFound
	}
	if err := s.bindRegistrationToAccount(
		asStr(reg, "id"), asStr(reg, "player_id"), userID, email); err != nil {
		return "", err
	}
	// Burn the token: it has done its job, and a link that keeps working could
	// later be used to hand this identity to someone else.
	_, _ = s.sb.Update("registrations", "id=eq."+store.Q(asStr(reg, "id")),
		map[string]any{"claim_token": nil})
	return asStr(reg, "event_id"), nil
}

// EventClaimCode returns the event's court-side claim code, minting one on first
// use. Owner-gated: this code is what the QR carries, and holding it is what
// authorizes self-serve claiming.
func (s *Service) EventClaimCode(eventID, callerID, callerEmail string) (string, error) {
	if !s.eventClaimReady() {
		return "", errors.New("claim codes aren't available yet")
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=owner_id,league_id,claim_code")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	if asStr(ev, "owner_id") != callerID && !s.staff(callerEmail) {
		return "", ErrForbidden
	}
	if asStr(ev, "league_id") != "" {
		return "", errors.New(
			"leagues don't use court-side claiming — every player needs a contact")
	}
	if c := strings.TrimSpace(asStr(ev, "claim_code")); c != "" {
		return c, nil
	}
	code := newID()
	if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"claim_code": code}); err != nil {
		return "", err
	}
	return code, nil
}

func (s *Service) eventClaimReady() bool {
	return s.columnReady("events", "claim_code")
}

// checkEventClaimCode verifies the caller actually holds the court-side code AND
// that this event allows self-serve claiming at all.
//
// The code is the whole security model here. Anyone holding it can claim any
// unclaimed name, which is a fine trade for a casual open play — worst case
// someone takes the wrong slot and the organizer fixes it in seconds — but only
// because holding it means you were standing where the organizer posted it.
// Gating on the event id alone would NOT be that: ids travel in ordinary share
// links, so a stranger could list a roster and take a name from anywhere.
func (s *Service) checkEventClaimCode(eventID, code string) error {
	if !s.eventClaimReady() {
		return errors.New("claim codes aren't available yet")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return ErrForbidden
	}
	ev, err := s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+"&select=league_id,claim_code")
	if err != nil {
		return err
	}
	if ev == nil {
		return ErrNotFound
	}
	if asStr(ev, "league_id") != "" {
		return ErrForbidden
	}
	if stored := strings.TrimSpace(asStr(ev, "claim_code")); stored == "" || stored != code {
		return ErrForbidden
	}
	return nil
}

// ClaimableEntries lists the unclaimed roster names behind the court-side QR.
// Requires the event's claim code — see checkEventClaimCode.
func (s *Service) ClaimableEntries(eventID, code string) ([]map[string]any, error) {
	if !s.claimTokensReady() {
		return nil, errors.New("claim links aren't available yet")
	}
	if err := s.checkEventClaimCode(eventID, code); err != nil {
		return nil, err
	}
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+
			"&select=id,player_id,player:players!player_id(full_name,user_id,phone)")
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, r := range rows {
		p := asMap(r, "player")
		if p == nil || asStr(p, "user_id") != "" {
			continue // already someone's
		}
		if isPlaceholderPhone(asStr(p, "phone")) {
			continue // demo filler, not a person
		}
		out = append(out, map[string]any{
			"registrationId": asStr(r, "id"),
			"fullName":       asStr(p, "full_name"),
		})
	}
	return out, nil
}

// ClaimRegistrationByID is the event-QR path: the caller picked their own name
// from ClaimableEntries. Re-checks the code (not just on the list call, or a
// stale list could be replayed) and that the row is still unclaimed.
func (s *Service) ClaimRegistrationByID(eventID, regID, code, userID, email string) error {
	if !s.claimTokensReady() {
		return errors.New("claim links aren't available yet")
	}
	if userID == "" {
		return ErrForbidden
	}
	if err := s.checkEventClaimCode(eventID, code); err != nil {
		return err
	}
	reg, err := s.sb.SelectOne("registrations",
		"id=eq."+store.Q(regID)+"&event_id=eq."+store.Q(eventID)+"&select=id,player_id")
	if err != nil {
		return err
	}
	if reg == nil {
		return ErrNotFound
	}
	return s.bindRegistrationToAccount(
		asStr(reg, "id"), asStr(reg, "player_id"), userID, email)
}

// bindRegistrationToAccount ties a roster row to an account, whichever way the
// claim arrived.
//
// Two shapes, because players.user_id is unique per account:
//   - the account has NO player row yet → stamp user_id onto this one, so their
//     history stays exactly where it is;
//   - the account already HAS a canonical player row → re-point the registration
//     at that row instead. Stamping would violate the unique index, and merging
//     is what the caller actually wants: one person, one player row, all their
//     events on it.
func (s *Service) bindRegistrationToAccount(regID, playerID, userID, email string) error {
	if playerID == "" {
		return ErrNotFound
	}
	p, err := s.sb.SelectOne("players",
		"id=eq."+store.Q(playerID)+"&select=user_id,phone")
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound
	}
	if uid := asStr(p, "user_id"); uid != "" {
		if uid == userID {
			return nil // already theirs — claiming twice is a no-op, not an error
		}
		return errors.New("someone has already claimed that spot")
	}
	if isPlaceholderPhone(asStr(p, "phone")) {
		return errors.New("that's a placeholder player, not a real entry")
	}
	canonical, err := s.sb.SelectOne("players",
		"user_id=eq."+store.Q(userID)+"&select=id")
	if err != nil {
		return err
	}
	if canonical != nil {
		cid := asStr(canonical, "id")
		// Merging moves this registration onto the account's existing player row —
		// but if that row is ALREADY entered in this event, the move would leave
		// them registered twice: two roster lines, two draw slots, one person.
		// Refuse instead, since the caller is already in the event they're trying
		// to claim into.
		reg, rerr := s.sb.SelectOne("registrations", "id=eq."+store.Q(regID)+"&select=event_id")
		if rerr != nil {
			return rerr
		}
		if reg != nil {
			dup, derr := s.sb.SelectOne("registrations",
				"event_id=eq."+store.Q(asStr(reg, "event_id"))+
					"&player_id=eq."+store.Q(cid)+"&select=id")
			if derr != nil {
				return derr
			}
			if dup != nil {
				return errors.New("you're already on this roster")
			}
		}
		_, uerr := s.sb.Update("registrations", "id=eq."+store.Q(regID),
			map[string]any{"player_id": cid})
		return uerr
	}
	upd := map[string]any{"user_id": userID}
	if e := strings.ToLower(strings.TrimSpace(email)); e != "" {
		upd["email"] = e
	}
	_, err = s.sb.Update("players",
		"id=eq."+store.Q(playerID)+"&user_id=is.null", upd)
	return err
}
