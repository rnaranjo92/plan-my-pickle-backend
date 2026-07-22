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
	if err == nil {
		// Notify the followed user (best-effort, off the request path). Link with
		// the follower's PLAYER id (not their user id) so the tap opens their
		// profile; empty when they have no player row (tap stays read-only).
		go func() {
			name := s.resolveDisplayName(callerID, "")
			link := ""
			if pid := s.playerIDForUser(callerID); pid != "" {
				link = "profile:" + pid
			}
			s.notifyUser(targetID, "follow", callerID, name,
				name+" started following you", link)
		}()
	}
	return err
}

// playerIDForUser returns the players.id for a Supabase user, or "" when the
// user has no player row (e.g. an organizer-only account).
func (s *Service) playerIDForUser(userID string) string {
	row, err := s.sb.SelectOne("players",
		"user_id=eq."+store.Q(userID)+"&select=id&limit=1")
	if err != nil || row == nil {
		return ""
	}
	return asStr(row, "id")
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

// distinctCap collects up to `cap` distinct non-empty strings, preserving order.
func distinctCap(vals []string, skip map[string]bool, cap int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, cap)
	for _, v := range vals {
		if v == "" || seen[v] || (skip != nil && skip[v]) {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= cap {
			break
		}
	}
	return out
}

// PlayedWithUsers suggests people the caller has shared an event with (co-
// registrants across the events they organize or play in), each tagged with the
// caller's follow state. Discovery source for the Find Players screen.
func (s *Service) PlayedWithUsers(callerID string) ([]model.UserSearchResult, error) {
	if callerID == "" {
		return []model.UserSearchResult{}, nil
	}
	// The caller's own player rows (to find their registrations + to exclude them).
	myPlayers, err := s.sb.Select("players",
		"user_id=eq."+store.Q(callerID)+"&select=id")
	if err != nil {
		return nil, err
	}
	myPids := map[string]bool{}
	pidList := make([]string, 0, len(myPlayers))
	for _, p := range myPlayers {
		if id := asStr(p, "id"); id != "" {
			myPids[id] = true
			pidList = append(pidList, id)
		}
	}
	if len(pidList) == 0 {
		return []model.UserSearchResult{}, nil
	}
	// Events the caller is registered in (most recent first).
	myRegs, err := s.sb.Select("registrations",
		"player_id="+store.In(pidList)+"&select=event_id&order=created_at.desc&limit=50")
	if err != nil {
		return nil, err
	}
	eventIDs := make([]string, 0, len(myRegs))
	for _, r := range myRegs {
		eventIDs = append(eventIDs, asStr(r, "event_id"))
	}
	eventIDs = distinctCap(eventIDs, nil, 25)
	if len(eventIDs) == 0 {
		return []model.UserSearchResult{}, nil
	}
	// Everyone else registered in those events (newest registrations first so the
	// 400-row window is deterministic + favors recent co-players).
	coRegs, err := s.sb.Select("registrations",
		"event_id="+store.In(eventIDs)+
			"&select=player_id&order=created_at.desc&limit=400")
	if err != nil {
		return nil, err
	}
	coPlayerIDs := make([]string, 0, len(coRegs))
	for _, r := range coRegs {
		coPlayerIDs = append(coPlayerIDs, asStr(r, "player_id"))
	}
	coPlayerIDs = distinctCap(coPlayerIDs, myPids, 120)
	if len(coPlayerIDs) == 0 {
		return []model.UserSearchResult{}, nil
	}
	// Resolve those players to accounts (user_id), skipping guests + the caller.
	prows, err := s.sb.Select("players",
		"id="+store.In(coPlayerIDs)+"&user_id=not.is.null&select=user_id")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(prows))
	for _, p := range prows {
		uids = append(uids, asStr(p, "user_id"))
	}
	uids = distinctCap(uids, map[string]bool{callerID: true}, 30)
	return s.decorateUsers(callerID, uids, nil, nil, false), nil
}

// NearbyUsers suggests players in the caller's city (from their account profile),
// each tagged with the caller's follow state. Empty when the caller hasn't set a
// city. Discovery source for the Find Players screen.
func (s *Service) NearbyUsers(callerID string) ([]model.UserSearchResult, error) {
	if callerID == "" {
		return []model.UserSearchResult{}, nil
	}
	me, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(callerID)+"&select=city")
	if err != nil {
		return nil, err
	}
	city := strings.TrimSpace(asStr(me, "city"))
	if city == "" {
		return []model.UserSearchResult{}, nil
	}
	rows, err := s.sb.Select("pmp_profiles",
		"city=ilike."+store.Q(likeEscape(city))+"&user_id=not.is.null"+
			"&select=user_id&limit=60")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		uids = append(uids, asStr(r, "user_id"))
	}
	uids = distinctCap(uids, map[string]bool{callerID: true}, 30)
	// Drop accounts with no player row (e.g. organizers) — decorateUsers resolves
	// names from `players`, so they'd otherwise show as blank, nameless cards.
	out := s.decorateUsers(callerID, uids, nil, nil, false)
	named := out[:0]
	for _, u := range out {
		if strings.TrimSpace(u.FullName) != "" {
			named = append(named, u)
		}
	}
	return named, nil
}

// SuggestedUsers is the general fallback discovery source: recently-active
// players who have accounts + names, so the Find Players screen is never empty
// even when the caller hasn't set a city and has no co-players yet. Excludes the
// caller; the frontend dedups anyone already shown under Near you / Played with /
// Following (those richer sources render first).
func (s *Service) SuggestedUsers(callerID string) ([]model.UserSearchResult, error) {
	if callerID == "" {
		return []model.UserSearchResult{}, nil
	}
	// Player rows tied to real accounts, newest first (favors active users). One
	// account can have many player rows; distinctCap collapses to unique users.
	rows, err := s.sb.Select("players",
		"user_id=not.is.null&select=user_id,full_name&order=created_at.desc&limit=300")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		// Skip nameless/guest-like rows up front so the cap isn't spent on blanks.
		if strings.TrimSpace(asStr(r, "full_name")) == "" {
			continue
		}
		uids = append(uids, asStr(r, "user_id"))
	}
	uids = distinctCap(uids, map[string]bool{callerID: true}, 30)
	out := s.decorateUsers(callerID, uids, nil, nil, false)
	named := out[:0]
	for _, u := range out {
		if strings.TrimSpace(u.FullName) != "" {
			named = append(named, u)
		}
	}
	return named, nil
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
