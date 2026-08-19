package service

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Clubs, from the outside.
//
// A club is the most searchable thing this product has. "Pickleball club in
// Chula Vista" is a query a real person types; "pickleball tournament id
// 8f3c…" is not. And a club page is the one page that keeps being worth
// visiting — it has something on next week, every week, which is exactly what
// a search engine and a prospective member are both looking for.
//
// What's public is deliberately the recruiting half: who the club is and what's
// coming up. NOT the roster, NOT who's drifting away, NOT contact details, and
// not every member's win-loss record — a club publishes an invitation, not its
// membership list, and the people in it joined a club rather than a website.

// PublicClub is the crawlable, no-login view of a club.
type PublicClub struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	City        string `json:"city,omitempty"`
	Description string `json:"description,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
	MemberCount int    `json:"memberCount"`
	EventCount  int    `json:"eventCount"`
	// RequiresWaiver is stated as a FACT here, with no link. The waiver URL is
	// club-supplied and this page is crawlable, so it stays on the member-facing
	// club page rather than becoming an outbound link on a public one.
	RequiresWaiver bool `json:"requiresWaiver,omitempty"`
}

// PublicClubs lists clubs for the sitemap and the city hubs, newest first.
//
// Demo and test clubs are filtered out by name — the same rule the rest of the
// crawlable surface uses. A search result that leads someone to "Test Club"
// costs more than the page was ever going to earn.
func (s *Service) PublicClubs(limit int) ([]PublicClub, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.sb.Select("clubs",
		"select=id,name,city,description,logo_url&order=created_at.desc&limit="+
			strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	out := make([]PublicClub, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(asStr(r, "name"))
		if name == "" || isDemoClubName(name) {
			continue
		}
		out = append(out, PublicClub{
			ID:          asStr(r, "id"),
			Name:        name,
			City:        strings.TrimSpace(asStr(r, "city")),
			Description: strings.TrimSpace(asStr(r, "description")),
			LogoURL:     strings.TrimSpace(asStr(r, "logo_url")),
		})
	}
	s.fillClubEventCounts(out)
	return out, nil
}

// fillClubEventCounts sets EventCount on each club, in ONE query.
//
// Needed because "has this club actually done anything" decides whether it goes
// in the sitemap, and asking per club would be a query per row on a list that
// can run to hundreds.
func (s *Service) fillClubEventCounts(clubs []PublicClub) {
	if len(clubs) == 0 {
		return
	}
	ids := make([]string, 0, len(clubs))
	for _, c := range clubs {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := s.sb.SelectAll("events", "club_id="+store.In(ids)+"&select=club_id")
	if err != nil {
		return // counts stay 0; the caller decides what that means
	}
	n := map[string]int{}
	for _, r := range rows {
		n[asStr(r, "club_id")]++
	}
	for i := range clubs {
		clubs[i].EventCount = n[clubs[i].ID]
	}
}

// WorthIndexing reports whether this club has enough behind it to be offered to
// a search engine.
//
// A club that has never run an event is a NAME and nothing else. Its page still
// works — anyone with the link can see it and join, which is what a brand-new
// club needs — but putting it in the sitemap asks Google to rank a page with
// nothing on it, and thin pages don't fail quietly: they lower the standing of
// the pages around them.
//
// Deliberately not gated on members. A club recruiting its first members has
// nothing to show yet either, and "has run something" is the honest signal.
func (c PublicClub) WorthIndexing() bool { return c.EventCount > 0 }

// PublicClubByID returns one club plus what a visitor came to see: the next few
// sessions.
func (s *Service) PublicClubByID(
	clubID string,
) (PublicClub, []model.Event, []model.Event, ClubActivity, error) {
	// Ask for the waiver column only once it exists: naming a column that
	// hasn't been migrated in fails the whole select, which would take the
	// club's public page down rather than omit one line from it.
	cols := "id,name,city,description,logo_url"
	if s.clubWaiverReady() {
		cols += ",requires_waiver"
	}
	row, err := s.sb.SelectOne("clubs",
		"id=eq."+store.Q(clubID)+"&select="+cols)
	if err != nil {
		return PublicClub{}, nil, nil, ClubActivity{}, err
	}
	if row == nil {
		return PublicClub{}, nil, nil, ClubActivity{}, ErrNotFound
	}
	name := strings.TrimSpace(asStr(row, "name"))
	if name == "" || isDemoClubName(name) {
		return PublicClub{}, nil, nil, ClubActivity{}, ErrNotFound
	}
	club := PublicClub{
		ID:          asStr(row, "id"),
		Name:        name,
		City:        strings.TrimSpace(asStr(row, "city")),
		Description: strings.TrimSpace(asStr(row, "description")),
		LogoURL:     strings.TrimSpace(asStr(row, "logo_url")),

		RequiresWaiver: asBool(row, "requires_waiver"),
	}

	events, _ := s.publicClubEvents(clubID)
	club.EventCount = len(events)
	upcoming := upcomingClubEvents(events, time.Now(), 6)
	club.MemberCount = s.countRows("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id", "user_id")

	// What the club has actually been doing, which is the part that makes
	// somebody want to come. Six recent sessions is enough to show a rhythm
	// without turning the page into an archive.
	played := pastSessions(events, time.Now(), 6)
	recent := make([]model.Event, 0, len(played))
	byID := map[string]model.Event{}
	for _, e := range events {
		byID[e.ID] = e
	}
	for _, p := range played {
		if e, ok := byID[p.id]; ok {
			recent = append(recent, e)
		}
	}
	activity := activityFrom(pastSessions(events, time.Now(), 60), time.Now())
	return club, upcoming, recent, activity, nil
}

// ClubsByCity groups public clubs under their city, for a hub page and for the
// sitemap's city list. Cities with no clubs simply don't appear.
func (s *Service) ClubsByCity() (map[string][]PublicClub, error) {
	clubs, err := s.PublicClubs(500)
	if err != nil {
		return nil, err
	}
	out := map[string][]PublicClub{}
	for _, c := range clubs {
		city := strings.TrimSpace(c.City)
		if city == "" {
			continue // a club with no city can't be found by place
		}
		if !c.WorthIndexing() {
			// The city page is a recommendation. Listing a club that has never
			// run anything spends the visitor's attention on a dead end and
			// makes the page look padded.
			continue
		}
		out[city] = append(out[city], c)
	}
	for city := range out {
		list := out[city]
		sort.SliceStable(list, func(i, j int) bool {
			return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
		})
		out[city] = list
	}
	return out, nil
}

// isDemoClubName keeps the demo and QA clubs off the crawlable surface.
//
// Matches WHOLE WORDS, not substrings. The first version used
// strings.Contains, which quietly refuses to index "Protest Park Paddlers",
// "Contest City" and anyone whose club is the "Greatest" — a filter that hides
// real clubs from search is worse than one that lets a demo through, because
// nobody ever finds out it happened.
func isDemoClubName(n string) bool {
	bad := map[string]bool{
		"test": true, "tests": true, "testing": true,
		"demo": true, "sample": true, "qa": true, "dummy": true,
	}
	for _, w := range strings.FieldsFunc(strings.ToLower(n), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if bad[w] {
			return true
		}
	}
	return false
}

// ClubActivity is the evidence that a club is alive.
//
// A prospective member is not deciding whether the club exists — they can see
// that. They are deciding whether it is WORTH TURNING UP TO, and the answer is
// carried entirely by how often it plays, when, and whether that was recent. A
// page listing one event next month and nothing else reads as a club that has
// stopped, whether or not it has.
type ClubActivity struct {
	// SessionsRecent is how many sessions were played in RecentDays.
	SessionsRecent int `json:"sessionsRecent"`
	RecentDays     int `json:"recentDays"`
	// UsualDay is the weekday the club most often plays ("Tuesday"), or "" when
	// there's no pattern to claim.
	UsualDay string `json:"usualDay,omitempty"`
	// LastPlayed is the most recent session that happened (RFC3339), "" if none.
	LastPlayed string `json:"lastPlayed,omitempty"`
}

// clubActivityWindow is how far back "recently" looks for the public page.
//
// Three months: long enough that a club playing fortnightly still looks busy,
// short enough that a club which stopped in the spring doesn't get to advertise
// last year's energy to somebody deciding about tonight.
const clubActivityWindow = 90 * 24 * time.Hour

// activityFrom summarises a club's rhythm from its sessions. Pure — what counts
// as a pattern is a judgement, and it decides whether the page tells somebody
// "plays most Tuesdays" or says nothing.
func activityFrom(sessions []clubSession, now time.Time) ClubActivity {
	out := ClubActivity{RecentDays: int(clubActivityWindow.Hours() / 24)}
	if len(sessions) == 0 {
		return out
	}
	// sessions arrive newest-first from pastSessions.
	out.LastPlayed = sessions[0].at.UTC().Format(time.RFC3339)

	byDay := map[time.Weekday]int{}
	cutoff := now.Add(-clubActivityWindow)
	for _, s := range sessions {
		if s.at.Before(cutoff) {
			continue
		}
		out.SessionsRecent++
		byDay[s.at.Weekday()]++
	}
	// A "usual day" needs to be a genuine habit, not the mode of three
	// scattered dates. Claiming "plays most Tuesdays" to someone who then turns
	// up on a Tuesday to an empty court is worse than claiming nothing.
	best, bestN := time.Weekday(0), 0
	for d, n := range byDay {
		if n > bestN || (n == bestN && d < best) {
			best, bestN = d, n
		}
	}
	if bestN >= 3 && bestN*2 > out.SessionsRecent {
		out.UsualDay = best.String()
	}
	return out
}
