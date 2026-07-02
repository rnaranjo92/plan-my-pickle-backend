package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// userHomeCounty returns the caller's chosen home county + state (from their
// pmp_profiles row), or empty strings if unset. Best-effort.
func (s *Service) userHomeCounty(userID string) (county, state string) {
	if userID == "" {
		return "", ""
	}
	if pr, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=county,state"); err == nil && pr != nil {
		return asStr(pr, "county"), asStr(pr, "state")
	}
	return "", ""
}

// CreateCommunityPost creates a standalone USER post (no event) tagged with the
// author's home county so it can surface in that county's NewsFeed. Signed-in
// only; the author can delete it later. Text is trimmed + capped.
func (s *Service) CreateCommunityPost(userID, email, text string) (model.FeedItem, error) {
	if userID == "" {
		return model.FeedItem{}, errors.New("sign in to post")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return model.FeedItem{}, errors.New("say something first")
	}
	if len(text) > 2000 {
		text = text[:2000]
	}
	county, state := s.userHomeCounty(userID)
	row := map[string]any{
		"type":       "community",
		"text":       text,
		"actor_name": s.resolveDisplayName(userID, email),
		"author_id":  userID,
	}
	if county != "" {
		row["county"] = county
	}
	if state != "" {
		row["state"] = state
	}
	rows, err := s.sb.Insert("feed_items", row)
	if err != nil {
		return model.FeedItem{}, err
	}
	if len(rows) == 0 {
		return model.FeedItem{}, errors.New("post insert returned no row")
	}
	fi := mapFeedItem(rows[0])
	fi.ReactionCounts = map[string]int{}
	fi.MyReactions = []string{}
	return fi, nil
}

// DeleteCommunityPost removes a user's own community post (author-only).
func (s *Service) DeleteCommunityPost(id, userID string) error {
	if userID == "" {
		return errors.New("sign in")
	}
	row, err := s.sb.SelectOne("feed_items", "id=eq."+store.Q(id)+"&select=author_id")
	if err != nil {
		return err
	}
	if row == nil || asStr(row, "author_id") != userID {
		return errors.New("you can only delete your own posts")
	}
	return s.sb.Delete("feed_items", "id=eq."+store.Q(id))
}
