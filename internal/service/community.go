package service

import (
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/courts"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// userHomeCounty returns the caller's home county + state, or empty strings
// when we genuinely can't tell. Best-effort.
//
// Reads the profile first, then FALLS BACK to where they actually play. The
// profile column went years with nothing writing it — every account read as
// null, so anything county-scoped showed nothing to anybody. SetHomeLocation
// now fills it from the app, but that only helps people who open the app again;
// the fallback makes every existing account work immediately, because an event
// they own or are registered in already carries a county.
func (s *Service) userHomeCounty(userID string) (county, state string) {
	if userID == "" {
		return "", ""
	}
	if pr, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=county,state"); err == nil && pr != nil {
		if c := strings.TrimSpace(asStr(pr, "county")); c != "" {
			return c, asStr(pr, "state")
		}
	}
	return s.countyFromTheirEvents(userID)
}

// countyFromTheirEvents infers a home county from the events this person is
// actually involved in — one they own, else one they're registered in. Newest
// first, because where somebody plays now beats where they played a year ago.
func (s *Service) countyFromTheirEvents(userID string) (county, state string) {
	if rows, err := s.sb.Select("events",
		"owner_id=eq."+store.Q(userID)+"&county=not.is.null"+
			"&select=county,state&order=created_at.desc&limit=1"); err == nil &&
		len(rows) > 0 {
		return asStr(rows[0], "county"), asStr(rows[0], "state")
	}
	// Not an organizer — follow their player rows into the events they joined.
	pids, perr := s.playerIDsForUser(userID, "")
	if perr != nil || len(pids) == 0 {
		return "", ""
	}
	regs, err := s.sb.Select("registrations",
		"player_id="+store.In(pids)+"&select=event_id&limit=50")
	if err != nil || len(regs) == 0 {
		return "", ""
	}
	ids := make([]string, 0, len(regs))
	for _, r := range regs {
		if id := asStr(r, "event_id"); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", ""
	}
	if rows, err := s.sb.Select("events",
		"id="+store.In(ids)+"&county=not.is.null"+
			"&select=county,state&order=created_at.desc&limit=1"); err == nil &&
		len(rows) > 0 {
		return asStr(rows[0], "county"), asStr(rows[0], "state")
	}
	return "", ""
}

// SetHomeLocation records where somebody is, from coordinates the app already
// has, so county-scoped features have something to work with.
//
// Reverse-geocoded SERVER-side: the client has coordinates but no geocoder, and
// this reuses the same lookup that stamps counties onto events, so a profile and
// an event in the same place agree on what that place is called.
//
// Never overwrites a county with nothing: a failed lookup leaves what was there.
func (s *Service) SetHomeLocation(userID string, lat, lng float64) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrForbidden
	}
	if lat == 0 && lng == 0 {
		return errors.New("need a real location")
	}
	county, state := courts.ReverseCounty(lat, lng)
	if strings.TrimSpace(county) == "" {
		return nil // couldn't tell — leave whatever is stored alone
	}
	_, err := s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id": userID,
		"county":  county,
		"state":   state,
	})
	return err
}

// HomeDiscovery is what the home feed shows someone who has no activity yet.
// Place names the area when the events really are local, so the client can say
// so instead of claiming "near you" about a list it picked nationally.
type HomeDiscovery struct {
	Events []model.PublicEvent `json:"events"`
	Place  string              `json:"place,omitempty"`
}

// HomeFeedEvents returns discovery events for the home feed WITHOUT the device
// ever being asked for its location.
//
// The feed used to sit behind a "Show events near me" button, because the only
// location we had came from the GPS permission prompt. But we can usually
// already tell: userHomeCounty reads the stored county, and failing that infers
// one from an event the person owns or plays in. Asking for a permission to
// learn something we know is a toll booth on an empty road.
//
// Falls back to the newest listed events nationally when there's no county to
// work with — a brand-new account with no events anywhere near it still opens on
// something real rather than an empty screen with a button on it. Place is empty
// in that case, which is how the client knows not to call it "near you".
func (s *Service) HomeFeedEvents(userID string, limit int) (HomeDiscovery, error) {
	if limit <= 0 {
		limit = 5
	}
	county, state := s.userHomeCounty(userID)
	if strings.TrimSpace(county) != "" {
		evs, err := s.PublicEvents(limit, county)
		if err != nil {
			return HomeDiscovery{}, err
		}
		// An empty county result is not an answer — a county with nothing on
		// this week would otherwise leave the screen blank for someone we could
		// still show something to.
		if len(evs) > 0 {
			place := county
			if st := strings.TrimSpace(state); st != "" {
				place = county + ", " + st
			}
			return HomeDiscovery{Events: evs, Place: place}, nil
		}
	}
	evs, err := s.PublicEventsNewest(limit)
	if err != nil {
		return HomeDiscovery{}, err
	}
	return HomeDiscovery{Events: evs}, nil
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
	// Attach the author's photo so the just-posted item shows their face right away.
	items := []model.FeedItem{fi}
	s.attachActorPhotos(items)
	return items[0], nil
}

// ensureEventPosts makes sure each given event has its single `event`-type
// feed post — the item that represents the event itself in the NewsFeed so it
// can be liked / commented like any other post. Idempotent + best-effort: it
// bulk-checks which events already have a post and only inserts the missing
// ones, so the feed self-heals for events created before event-posts existed
// (and for seeder paths that direct-insert events, bypassing CreateEvent).
func (s *Service) ensureEventPosts(eventIDs []string) {
	if len(eventIDs) == 0 {
		return
	}
	have := map[string]bool{}
	if rows, err := s.sb.Select("feed_items",
		"event_id="+store.In(eventIDs)+"&type=eq.event&select=event_id"); err == nil {
		for _, r := range rows {
			have[asStr(r, "event_id")] = true
		}
	}
	missing := make([]string, 0)
	for _, id := range eventIDs {
		if id != "" && !have[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	evs := map[string]map[string]any{}
	if rows, err := s.sb.Select("events",
		"id="+store.In(missing)+"&select=id,name,owner_id,poster_url,starts_at"); err == nil {
		for _, r := range rows {
			evs[asStr(r, "id")] = r
		}
	}
	batch := make([]map[string]any, 0, len(missing))
	for _, id := range missing {
		ev := evs[id]
		if ev == nil {
			continue
		}
		meta := map[string]any{}
		if p := asStr(ev, "poster_url"); p != "" {
			meta["poster_url"] = p
		}
		if st := asStr(ev, "starts_at"); st != "" {
			meta["starts_at"] = st
		}
		owner := asStr(ev, "owner_id")
		batch = append(batch, map[string]any{
			"type":       "event",
			"event_id":   id,
			"ref_id":     id,
			"text":       asStr(ev, "name"),
			"author_id":  owner,
			"actor_name": s.resolveDisplayName(owner, ""),
			"meta":       meta,
		})
	}
	if len(batch) > 0 {
		_, _ = s.sb.Insert("feed_items", batch)
	}
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

// SetClubManagerMode records whether this account navigates as a club manager.
//
// Stored on the ACCOUNT rather than the device. The switch reshapes the nav
// bar, and a club owner who signs out, reinstalls, or picks up a second phone
// expects to find the app arranged the way they left it — a device-local
// setting would silently revert them to the player tabs each time.
//
// Reports a real error when the column is missing rather than shrugging: the
// caller flips the nav locally either way, so a silent failure here would look
// exactly like success until they signed in somewhere else and found it gone.
func (s *Service) SetClubManagerMode(userID string, on bool) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrForbidden
	}
	ready, err := s.columnReadyErr("pmp_profiles", "club_manager_mode")
	if err != nil {
		return err
	}
	if !ready {
		return errors.New(
			"club manager mode isn't available yet — run add_club_manager_mode.sql")
	}
	_, err = s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id":           userID,
		"club_manager_mode": on,
	})
	return err
}
