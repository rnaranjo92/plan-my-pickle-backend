package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// SearchUsers finds followable accounts by display name. Names live on player
// rows (pmp_profiles is photo-only), so we match players.full_name for rows that
// are linked to an account, dedup by account, drop the caller, and decorate each
// with photo + whether the caller already follows them. Needs >= 2 chars.
func (s *Service) SearchUsers(callerID, q string) ([]model.UserSearchResult, error) {
	q = strings.TrimSpace(q)
	if len(q) < 2 {
		return []model.UserSearchResult{}, nil
	}
	if r := []rune(q); len(r) > 100 { // bound the ilike pattern
		q = string(r[:100])
	}
	// likeEscape neutralizes typed % / _ so they match literally; order=full_name
	// makes the 80-row window deterministic (not an arbitrary slice for "jo").
	rows, err := s.sb.Select("players",
		"full_name=ilike.*"+store.Q(likeEscape(q))+"*&user_id=not.is.null"+
			"&select=user_id,full_name,dupr_rating&order=full_name.asc&limit=80")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	names := map[string]string{}
	ratings := map[string]*float64{}
	order := make([]string, 0, 25)
	for _, r := range rows {
		uid := asStr(r, "user_id")
		if uid == "" || uid == callerID || seen[uid] {
			continue
		}
		seen[uid] = true
		names[uid] = asStr(r, "full_name")
		ratings[uid] = asFloatPtr(r, "dupr_rating")
		order = append(order, uid)
		if len(order) >= 25 {
			break
		}
	}
	return s.decorateUsers(callerID, order, names, ratings, false), nil
}

// Follow makes callerID follow targetID (idempotent). Self-follow is rejected
// here and by the follows_no_self DB constraint; a bad target trips the FK.
func (s *Service) Follow(callerID, targetID string) error {
	if callerID == "" {
		return ErrForbidden
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" || targetID == callerID {
		return errors.New("you can't follow yourself")
	}
	_, err := s.sb.Upsert("follows", "follower_id,followee_id", map[string]any{
		"follower_id": callerID,
		"followee_id": targetID,
	})
	return err
}

// Unfollow removes callerID's follow of targetID (idempotent).
func (s *Service) Unfollow(callerID, targetID string) error {
	if callerID == "" {
		return ErrForbidden
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return nil
	}
	return s.sb.Delete("follows",
		"follower_id=eq."+store.Q(callerID)+"&followee_id=eq."+store.Q(targetID))
}

// Following lists the accounts callerID follows (newest first).
func (s *Service) Following(callerID string) ([]model.UserSearchResult, error) {
	ids, err := s.followEdges(callerID, "follower_id", "followee_id")
	if err != nil {
		return nil, err
	}
	// All of these are, by definition, followed by the caller.
	return s.decorateUsers(callerID, ids, nil, nil, true), nil
}

// Followers lists the accounts that follow callerID (newest first), each tagged
// with whether the caller follows them back.
func (s *Service) Followers(callerID string) ([]model.UserSearchResult, error) {
	ids, err := s.followEdges(callerID, "followee_id", "follower_id")
	if err != nil {
		return nil, err
	}
	return s.decorateUsers(callerID, ids, nil, nil, false), nil
}

// --- helpers ---

// likeEscape neutralizes ILIKE metacharacters in user input so a name search
// matches literally — a typed % or _ (or the \ escape char itself) is treated as
// a normal character, not a wildcard. The caller still wraps the result in *...*.
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

// followEdges returns the other-side ids of a user's follow edges. matchCol is
// the column equal to userID; pickCol is the column to collect.
func (s *Service) followEdges(userID, matchCol, pickCol string) ([]string, error) {
	if userID == "" {
		return nil, ErrForbidden
	}
	// SelectAll (not Select) paginates past PostgREST's 1000-row cap, so a big
	// follow graph isn't silently truncated; it keeps our explicit order.
	rows, err := s.sb.SelectAll("follows",
		matchCol+"=eq."+store.Q(userID)+"&select="+pickCol+"&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := asStr(r, pickCol); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// followingSet reports which of ids the caller already follows.
func (s *Service) followingSet(callerID string, ids []string) map[string]bool {
	out := map[string]bool{}
	if callerID == "" || len(ids) == 0 {
		return out
	}
	rows, err := s.sb.Select("follows",
		"follower_id=eq."+store.Q(callerID)+
			"&followee_id="+store.In(ids)+"&select=followee_id")
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[asStr(r, "followee_id")] = true
	}
	return out
}

// decorateUsers builds result rows for a set of account ids: display name + DUPR
// rating from their player row, photo from pmp_profiles, isFollowing vs caller.
// Pass preName/preRating when the caller already has them (search) to skip a
// lookup; allFollowed=true short-circuits isFollowing (a "following" list).
func (s *Service) decorateUsers(callerID string, ids []string,
	preName map[string]string, preRating map[string]*float64, allFollowed bool) []model.UserSearchResult {
	if len(ids) == 0 {
		return []model.UserSearchResult{}
	}
	names, ratings := preName, preRating
	if names == nil {
		names, ratings = map[string]string{}, map[string]*float64{}
		if prows, err := s.sb.Select("players",
			"user_id="+store.In(ids)+"&select=user_id,full_name,dupr_rating"); err == nil {
			for _, p := range prows {
				uid := asStr(p, "user_id")
				if uid == "" {
					continue
				}
				if _, ok := names[uid]; !ok {
					names[uid] = asStr(p, "full_name")
					ratings[uid] = asFloatPtr(p, "dupr_rating")
				}
			}
		}
	}
	photos := s.photosByUser(ids)
	var following map[string]bool
	if !allFollowed {
		following = s.followingSet(callerID, ids)
	}
	out := make([]model.UserSearchResult, 0, len(ids))
	for _, id := range ids {
		isFollowing := allFollowed || following[id]
		out = append(out, model.UserSearchResult{
			UserID:        id,
			FullName:      names[id],
			PhotoURL:      photos[id],
			DoublesRating: ratings[id],
			IsFollowing:   isFollowing,
		})
	}
	return out
}
