package service

import (
	"sort"
	"strconv"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// The club feed: everything happening across a club's events, in one place.
//
// A club's activity was scattered across the events that produced it — a result
// from Tuesday's league, a new ladder posted Thursday, an announcement about
// Saturday's tournament — each visible only if you already knew to open that
// event. A club is the thing members belong to; this is the page that shows
// them it's alive.

// clubFeedLimit bounds one page of club activity. A busy club plays several
// nights a week and every score is a row, so this is a feed, not an archive.
const clubFeedLimit = 60

// ClubFeed returns recent activity across a club's events, newest first.
//
// VISIBILITY IS THE EVENT'S, not the club's. It reads the club's events through
// ClubEventsFor, which already scopes them to the viewer: a member sees the
// club's unlisted events too, an anonymous visitor sees only listed ones. Doing
// it any other way would have leaked a private league's scores onto a page that
// is open to anyone with the link.
func (s *Service) ClubFeed(clubID, viewerID string) ([]model.FeedItem, error) {
	events, err := s.ClubEventsFor(clubID, viewerID)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return []model.FeedItem{}, nil
	}
	ids := make([]string, 0, len(events))
	names := make(map[string]string, len(events))
	posters := make(map[string]string, len(events))
	for _, e := range events {
		// The same QA/demo filter the other feeds use — a club that ran a test
		// event shouldn't have it narrated to its members forever.
		if publicFeedTestName.MatchString(e.Name) {
			continue
		}
		ids = append(ids, e.ID)
		names[e.ID] = e.Name
		if e.PosterURL != nil {
			posters[e.ID] = *e.PosterURL
		}
	}
	if len(ids) == 0 {
		return []model.FeedItem{}, nil
	}

	rows, err := s.sb.Select("feed_items",
		"event_id="+store.In(ids)+"&select=*&order=created_at.desc&limit="+
			strconv.Itoa(clubFeedLimit))
	if err != nil {
		return nil, err
	}
	out := make([]model.FeedItem, 0, len(rows))
	itemIDs := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		fi := mapFeedItem(r)
		// One row, one card — the lesson from the NewsFeed, applied here before
		// it can be learned twice.
		if fi.ID == "" || seen[fi.ID] {
			continue
		}
		seen[fi.ID] = true
		fi.EventName = names[fi.EventID]
		// The event's CURRENT poster, not the one snapshotted into the post when
		// it was written — a poster added later should still show. Only for
		// `event` cards, which are the only type that renders one.
		if fi.Type == "event" {
			if p := posters[fi.EventID]; p != "" {
				fi.PosterURL = &p
			}
		}
		fi.ReactionCounts = map[string]int{}
		fi.MyReactions = []string{}
		out = append(out, fi)
		itemIDs = append(itemIDs, fi.ID)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	if len(itemIDs) > 0 {
		s.attachSocial(out, itemIDs, viewerID)
	}
	s.attachActorPhotos(out)
	return out, nil
}
