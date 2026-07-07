package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Find-a-partner: players opt into a public directory (pmp_profiles.These
// seeking_partner) filterable by gender / city / DUPR band. Profiles carry
// gender + city account-level (pmp_profiles, migration 0059) because a user's
// player rows are per-event denormalizations.

// SetMyProfileDetails saves the caller's partner-finder fields. Gender is
// M / F / "" (prefer not to say); city is free text (capped).
func (s *Service) SetMyProfileDetails(userID, gender, city string, seeking bool) error {
	if userID == "" {
		return errors.New("not signed in")
	}
	gender = strings.ToUpper(strings.TrimSpace(gender))
	if gender != "" && gender != "M" && gender != "F" {
		return errors.New("gender must be M, F, or blank")
	}
	city = strings.TrimSpace(city)
	if r := []rune(city); len(r) > 80 {
		city = string(r[:80])
	}
	_, err := s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id":         userID,
		"gender":          gender,
		"city":            city,
		"seeking_partner": seeking,
	})
	return err
}

// PartnerDirectory lists players who flagged "looking for a partner", filtered
// by gender / city substring / DUPR band. Signed-in only (reduces scraping of
// the directory); the caller never appears in their own results.
func (s *Service) PartnerDirectory(callerID, gender, city string,
	minRating, maxRating *float64) ([]model.PartnerResult, error) {
	if callerID == "" {
		return nil, ErrForbidden
	}
	q := "seeking_partner=is.true&select=user_id,gender,city,photo_url&limit=200"
	gender = strings.ToUpper(strings.TrimSpace(gender))
	if gender == "M" || gender == "F" {
		q += "&gender=eq." + gender
	}
	if city = strings.TrimSpace(city); city != "" {
		q += "&city=ilike.*" + store.Q(likeEscape(city)) + "*"
	}
	rows, err := s.sb.Select("pmp_profiles", q)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	meta := map[string]map[string]any{}
	for _, r := range rows {
		uid := asStr(r, "user_id")
		if uid == "" || uid == callerID {
			continue
		}
		ids = append(ids, uid)
		meta[uid] = r
	}
	users := s.decorateUsers(callerID, ids, nil, nil, false)
	out := make([]model.PartnerResult, 0, len(users))
	for _, u := range users {
		// A directory row with no player identity yet (never registered) has no
		// name/rating — skip it rather than show an anonymous card.
		if u.FullName == "" {
			continue
		}
		if minRating != nil && (u.DoublesRating == nil || *u.DoublesRating < *minRating) {
			continue
		}
		if maxRating != nil && (u.DoublesRating == nil || *u.DoublesRating > *maxRating) {
			continue
		}
		m := meta[u.UserID]
		out = append(out, model.PartnerResult{
			UserSearchResult: u,
			Gender:           asStr(m, "gender"),
			City:             asStr(m, "city"),
		})
	}
	return out, nil
}
