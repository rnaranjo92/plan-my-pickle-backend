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
	return out, nil
}

// PublicClubByID returns one club plus what a visitor came to see: the next few
// sessions.
func (s *Service) PublicClubByID(
	clubID string,
) (PublicClub, []model.Event, error) {
	row, err := s.sb.SelectOne("clubs",
		"id=eq."+store.Q(clubID)+
			"&select=id,name,city,description,logo_url")
	if err != nil {
		return PublicClub{}, nil, err
	}
	if row == nil {
		return PublicClub{}, nil, ErrNotFound
	}
	name := strings.TrimSpace(asStr(row, "name"))
	if name == "" || isDemoClubName(name) {
		return PublicClub{}, nil, ErrNotFound
	}
	club := PublicClub{
		ID:          asStr(row, "id"),
		Name:        name,
		City:        strings.TrimSpace(asStr(row, "city")),
		Description: strings.TrimSpace(asStr(row, "description")),
		LogoURL:     strings.TrimSpace(asStr(row, "logo_url")),
	}

	events, _ := s.ClubEvents(clubID)
	club.EventCount = len(events)
	upcoming := upcomingClubEvents(events, time.Now(), 6)
	club.MemberCount = s.countRows("club_members",
		"club_id=eq."+clubID+"&select=user_id", "user_id")

	return club, upcoming, nil
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
