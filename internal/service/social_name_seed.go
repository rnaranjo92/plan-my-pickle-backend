package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Seeding a display name from a social sign-in.
//
// Sign in with Apple releases the person's name EXACTLY ONCE — to the app, at
// the moment they first consent. It is not in the identity token, and Apple
// will not send it again on any later sign-in; the only way back is the user
// revoking the app under Settings → Apple ID → Sign in with Apple. Combined
// with Hide My Email (which gives a @privaterelay.appleid.com alias), an
// account that misses that one moment has no human-readable identity at all —
// it turns up in a club's join queue as a blank row nobody can approve.
//
// So the client persists it the instant it arrives, through here.

// SeedMyName sets the caller's display name ONLY IF they don't have one.
//
// Never overwrites. This runs on every social sign-in, and the value it carries
// is whatever the provider said years ago — a person who has since set their
// own name must not have it silently reverted by signing in again. "Only if
// empty" is what makes it safe to call unconditionally.
//
// Deliberately NOT part of SetMyBasicInfo: that writes name AND phone together,
// so calling it here with the empty phone we have would wipe a stored number
// and un-verify it.
func (s *Service) SeedMyName(userID, fullName string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("not signed in")
	}
	fullName = strings.Join(strings.Fields(fullName), " ")
	if fullName == "" {
		return nil // nothing offered; not an error
	}
	if r := []rune(fullName); len(r) > 120 {
		fullName = string(r[:120])
	}
	// Already named on the account? Leave it alone.
	if row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=full_name"); err == nil &&
		row != nil && strings.TrimSpace(asStr(row, "full_name")) != "" {
		return nil
	}
	// Already named on a player row? Also leave it alone — that is the name
	// they play under, and it is what rosters and the feed already show.
	if rows, err := s.sb.Select("players",
		"user_id=eq."+store.Q(userID)+"&select=full_name&limit=5"); err == nil {
		for _, r := range rows {
			if strings.TrimSpace(asStr(r, "full_name")) != "" {
				return nil
			}
		}
	}
	if _, err := s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id":   userID,
		"full_name": fullName,
	}); err != nil {
		return err
	}
	// Keep any nameless player rows in sync, the same way SetMyBasicInfo does —
	// display names for the feed and rosters are read from players.
	_, _ = s.sb.Update("players", "user_id=eq."+store.Q(userID),
		map[string]any{"full_name": fullName})
	return nil
}
