package service

import (
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// clubDisplayNames resolves display names for a batch of accounts.
//
// Two sources, because neither alone covers everybody:
//
//   - players.full_name — the name someone plays under. Absent for anyone who
//     has never been registered for an event, which includes every organizer
//     and every brand-new account.
//   - pmp_profiles.full_name — the ACCOUNT name. This is where a social sign-in
//     lands, and where a person editing "Basic info" writes.
//
// Reading only the first is why an Apple sign-in shows up as a nameless row: no
// event registration, so no player row, so no name — even though the account
// has one. Photos in this same code path were already read from pmp_profiles,
// so a club's join queue could show somebody's FACE with no name beside it.
//
// Two batched queries regardless of how many people are asked about.
func (s *Service) clubDisplayNames(userIDs []string) map[string]string {
	out := map[string]string{}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(userIDs))
	for _, u := range userIDs {
		if u = strings.TrimSpace(u); u != "" && !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	if len(uniq) == 0 {
		return out
	}
	if rows, err := s.sb.Select("players",
		"user_id="+store.In(uniq)+"&select=user_id,full_name"); err == nil {
		for _, r := range rows {
			if n := strings.TrimSpace(asStr(r, "full_name")); n != "" {
				out[asStr(r, "user_id")] = n
			}
		}
	}
	// Only ask about the ones still missing.
	gaps := make([]string, 0, len(uniq))
	for _, u := range uniq {
		if out[u] == "" {
			gaps = append(gaps, u)
		}
	}
	if len(gaps) == 0 {
		return out
	}
	if rows, err := s.sb.Select("pmp_profiles",
		"user_id="+store.In(gaps)+"&select=user_id,full_name"); err == nil {
		for _, r := range rows {
			if n := strings.TrimSpace(asStr(r, "full_name")); n != "" {
				out[asStr(r, "user_id")] = n
			}
		}
	}
	return out
}
