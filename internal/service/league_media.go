package service

import (
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// LeagueMediaItem is one photo or video posted anywhere in a league.
type LeagueMediaItem struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Kind      string `json:"kind"` // "photo" | "video"
	Caption   string `json:"caption,omitempty"`
	AuthorID  string `json:"authorId,omitempty"`
	Author    string `json:"author,omitempty"`
	EventID   string `json:"eventId,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// LeagueMedia gathers every photo and video posted to a league, newest first.
//
// A league's pictures are currently scattered: each session's feed holds its
// own, so the photos from six weeks of Wednesdays live in six places and the
// only way to find one is to remember which night it was. This reads them back
// as a single gallery.
//
// Sourced from the feed rather than a new upload path on purpose — people
// already post to the feed, and a media tab nobody has to remember to post to
// separately is one that actually fills up.
func (s *Service) LeagueMedia(leagueID string) ([]LeagueMediaItem, error) {
	events, err := s.sb.Select("events",
		"league_id=eq."+store.Q(leagueID)+"&select=id")
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return []LeagueMediaItem{}, nil
	}
	ids := make([]string, 0, len(events))
	for _, e := range events {
		if id := asStr(e, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []LeagueMediaItem{}, nil
	}
	// SelectAll: a long-running league's feed passes the row cap easily, and a
	// gallery that silently stops at last month is worse than no gallery.
	rows, err := s.sb.SelectAll("feed_items",
		"event_id="+store.In(ids)+"&select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]LeagueMediaItem, 0, len(rows))
	for _, r := range rows {
		fi := mapFeedItem(r)
		if fi.MediaURL == nil || strings.TrimSpace(*fi.MediaURL) == "" {
			continue
		}
		kind := "photo"
		if fi.MediaType != nil && strings.EqualFold(*fi.MediaType, "video") {
			kind = "video"
		}
		out = append(out, LeagueMediaItem{
			ID:        fi.ID,
			URL:       *fi.MediaURL,
			Kind:      kind,
			Caption:   fi.Text,
			AuthorID:  fi.AuthorID,
			Author:    strOr(fi.ActorName),
			EventID:   fi.EventID,
			CreatedAt: fi.CreatedAt,
		})
	}
	// Newest first across every session, not per session — the gallery is one
	// timeline, which is the whole point of collecting them.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}

var _ = model.FeedItem{}
