package service

import (
	"errors"
	"sort"
	"strconv"
	"strings"

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
	// The club's OWN posts — words, photos and videos said to the club rather
	// than to one of its sessions. Guarded on the migration; visibility is the
	// same rule the join button uses: members always, everyone else only when
	// the club is public. A private club's chatter must not be readable by
	// anyone holding the link.
	if s.clubPostsReady() {
		show := s.isClubMember(clubID, viewerID) ||
			s.IsClubAdmin(clubID, viewerID)
		if !show {
			if c, cerr := s.sb.SelectOne("clubs",
				"id=eq."+store.Q(clubID)+"&select=*"); cerr == nil && c != nil {
				show = asBoolDefaultTrue(c, "is_public")
			}
		}
		if show {
			if prows, perr := s.sb.Select("feed_items",
				"club_id=eq."+store.Q(clubID)+"&select=*&order=created_at.desc"+
					"&limit="+strconv.Itoa(clubFeedLimit)); perr == nil {
				rows = append(rows, prows...)
			}
		}
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
	if len(out) > clubFeedLimit {
		out = out[:clubFeedLimit]
	}
	if len(itemIDs) > 0 {
		s.attachSocial(out, itemIDs, viewerID)
	}
	s.attachActorPhotos(out)
	return out, nil
}

// clubPostsReady reports whether add_club_posts.sql has run.
func (s *Service) clubPostsReady() bool {
	return s.columnReady("feed_items", "club_id")
}

// CreateClubPost is a member saying something TO the club — text, a photo or a
// video — as opposed to the derived feed, which only narrates its events.
//
// MEMBERS post, not just admins: a club feed only its committee can write to
// is a noticeboard, and the ask was a feed. The context cue rides in meta as
// club_name, so wherever this item travels (the author's own NewsFeed), the
// card can say which club it belongs to.
func (s *Service) CreateClubPost(
	clubID, userID, email, text, mediaURL, mediaType string,
) (model.FeedItem, error) {
	if !s.clubPostsReady() {
		return model.FeedItem{}, errors.New(
			"club posts aren't enabled yet — run add_club_posts.sql")
	}
	if !s.isClubMember(clubID, userID) && !s.IsClubAdmin(clubID, userID) {
		return model.FeedItem{}, ErrForbidden
	}
	text = strings.TrimSpace(text)
	mediaURL = strings.TrimSpace(mediaURL)
	if text == "" && mediaURL == "" {
		return model.FeedItem{}, errors.New("say something first")
	}
	if r := []rune(text); len(r) > 1000 {
		text = string(r[:1000])
	}
	meta := map[string]any{"club_name": s.clubNameOr(clubID, "")}
	if mediaURL != "" {
		if mediaType != "video" && mediaType != "image" {
			mediaType = "video"
		}
		meta["media_url"] = mediaURL
		meta["media_type"] = mediaType
	}
	rows, err := s.sb.Insert("feed_items", map[string]any{
		"type":       "post",
		"club_id":    clubID,
		"text":       text,
		"author_id":  userID,
		"actor_name": s.resolveDisplayName(userID, email),
		"meta":       meta,
	})
	if err != nil {
		return model.FeedItem{}, err
	}
	if len(rows) == 0 {
		return model.FeedItem{}, errors.New("post insert returned no row")
	}
	fi := mapFeedItem(rows[0])
	fi.ReactionCounts = map[string]int{}
	fi.MyReactions = []string{}
	items := []model.FeedItem{fi}
	s.attachActorPhotos(items)
	return items[0], nil
}
