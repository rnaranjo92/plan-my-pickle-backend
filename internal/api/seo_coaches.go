package api

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strings"
)

// Public, crawlable coach pages: a /coaches directory and a /coach/{id} profile.
//
// "Find a coach" existed only INSIDE the app, behind a sign-in and a location
// permission. So the person most likely to hire a coach — someone typing
// "pickleball lessons chula vista" into Google — had no way to find one, and a
// coach who joined got a listing nobody outside the app could see. These pages
// are the front of that funnel; they hand off to the app to actually book.
//
// Only VERIFIED coaches appear. The in-app opt-in says a coach will "appear in
// players' nearby-coach search", which is a narrower promise than a page Google
// indexes — so publication follows the owner's approval, not a toggle somebody
// flipped expecting in-app discovery. Precise coordinates are dropped upstream
// in PublicCoaches; the city is what gets published.

func (s *Server) registerSEOCoaches(mux *http.ServeMux) {
	mux.HandleFunc("GET /coaches", s.seoCoachHub)
	mux.HandleFunc("GET /coach/{id}", s.seoCoachPage)
}

type seoCoachCard struct {
	Name, City, Skills, Rate, URL string
	Photo                         string
	Verified                      bool
}

type seoCoachHubData struct {
	Title, Canonical, Description, H1, Intro string
	OGImage                                  string
	Cards                                    []seoCoachCard
	JSONLD                                   template.JS
}

type seoCoachPageData struct {
	Title, Canonical, Description, H1 string
	OGImage                           string
	CityLine, RateLine, SkillsLine    string
	Photo                             string
	CertLine, ExpLine, Bio            string
	Verified                          bool
	BookURL                           string
	JSONLD                            template.JS
}

// coachRate renders an hourly rate, or "" when the coach hasn't set one.
func coachRate(cents *int) string {
	if cents == nil || *cents <= 0 {
		return ""
	}
	return "$" + centsToDollars(*cents) + "/hr"
}

func (s *Server) seoCoachHub(w http.ResponseWriter, r *http.Request) {
	coaches, err := s.svc.PublicCoaches()
	if err != nil {
		coaches = nil
	}
	// City, then name — so the page reads as a directory grouped by place rather
	// than by whenever somebody happened to sign up.
	sort.SliceStable(coaches, func(i, j int) bool {
		if ci, cj := coaches[i].City, coaches[j].City; ci != cj {
			return ci < cj
		}
		return coaches[i].Name < coaches[j].Name
	})

	var cards []seoCoachCard
	var items []any
	for i, c := range coaches {
		cards = append(cards, seoCoachCard{
			Name:     c.Name,
			City:     strings.TrimSpace(c.City),
			Skills:   strings.TrimSpace(c.Skills),
			Rate:     coachRate(c.HourlyRateCents),
			Photo:    strings.TrimSpace(c.PhotoURL),
			URL:      "/coach/" + c.UserID,
			Verified: c.Verified,
		})
		items = append(items, map[string]any{
			"@type": "ListItem", "position": i + 1,
			"url": seoCanonicalBase + "/coach/" + c.UserID, "name": c.Name,
		})
	}

	ld, _ := json.Marshal(map[string]any{
		"@context": "https://schema.org", "@type": "ItemList",
		"name": "Pickleball Coaches on PlanMyPickle", "itemListElement": items,
	})

	// An empty directory still renders a full page: the rest of it explains what
	// coaching here is and recruits coaches, and a 404 would drop the /coaches
	// URL out of the index on the exact day there is nobody listed yet.
	intro := "Every coach below applied and was reviewed by hand. Tap through for " +
		"rates, certifications and what they teach."
	if len(cards) == 0 {
		intro = "No coaches are listed yet — we're reviewing applications now."
	}

	og := ""
	for _, c := range cards {
		if c.Photo != "" {
			og = c.Photo
			break
		}
	}

	s.seoRender(w, seoCoachHubTmpl, seoCoachHubData{
		OGImage:     og,
		Title:       "Pickleball Coaches — Lessons & Video Coaching | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/coaches",
		Description: "Find a verified pickleball coach — private lessons, group clinics, and video feedback on your own match footage. Coach in person or from anywhere, on PlanMyPickle.",
		H1:          "Pickleball Coaches",
		Intro:       intro,
		Cards:       cards,
		JSONLD:      template.JS(ld),
	})
}

func (s *Server) seoCoachPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.PublicCoachByID(r.PathValue("id"))
	if err != nil || c.UserID == "" {
		s.seoNotFound(w)
		return
	}

	place := strings.TrimSpace(c.City)
	titlePlace := ""
	if place != "" {
		titlePlace = " in " + place
	}

	cert := strings.TrimSpace(c.Certifications)
	exp := ""
	if c.YearsExperience != nil && *c.YearsExperience > 0 {
		exp = plural(*c.YearsExperience, "year", "years") + " coaching"
	}

	desc := "Book a pickleball lesson with " + c.Name + titlePlace +
		". Private sessions, clinics, and video feedback on PlanMyPickle."

	ld, _ := json.Marshal(map[string]any{
		"@context": "https://schema.org", "@type": "Person",
		"name": c.Name, "jobTitle": "Pickleball Coach",
		"url":         seoCanonicalBase + "/coach/" + c.UserID,
		"description": strings.TrimSpace(c.Bio),
		"image":       strings.TrimSpace(c.PhotoURL),
		"address": map[string]any{
			"@type": "PostalAddress", "addressLocality": place,
		},
	})

	s.seoRender(w, seoCoachTmpl, seoCoachPageData{
		Title:       c.Name + " — Pickleball Coach" + titlePlace + " | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/coach/" + c.UserID,
		Description: desc,
		H1:          c.Name,
		CityLine:    place,
		Photo:       strings.TrimSpace(c.PhotoURL),
		OGImage:     strings.TrimSpace(c.PhotoURL),
		RateLine:    coachRate(c.HourlyRateCents),
		SkillsLine:  strings.TrimSpace(c.Skills),
		CertLine:    cert,
		ExpLine:     exp,
		Bio:         strings.TrimSpace(c.Bio),
		Verified:    c.Verified,
		BookURL:     seoAppBase + "/?coach=" + c.UserID,
		JSONLD:      template.JS(ld),
	})
}

// seoCoachURLs are the coach pages for the sitemap.
func (s *Server) seoCoachURLs() []string {
	coaches, err := s.svc.PublicCoaches()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(coaches)+1)
	out = append(out, "/coaches")
	for _, c := range coaches {
		out = append(out, "/coach/"+c.UserID)
	}
	return out
}
