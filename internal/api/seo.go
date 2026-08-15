package api

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

// Local helpers (the api package has no shared *string / money formatters).
func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func itoa(n int) string { return strconv.Itoa(n) }

func centsToDollars(c int) string {
	if c%100 == 0 {
		return itoa(c / 100)
	}
	return fmt.Sprintf("%d.%02d", c/100, c%100)
}

// Server-rendered, crawlable public SEO pages + a dynamic sitemap.
//
// The Flutter app (app.planmypickle.com) is a client-rendered SPA — its event,
// city, and league content is invisible to search engines. These routes give
// Google real HTML with unique <title>s, an <h1>, human-readable content, and
// schema.org JSON-LD, then hand the visitor off to the app to register.
//
// Canonical URLs point at seoCanonicalBase (the apex marketing domain), so route
// /e/*, /pickleball-tournaments/*, and /sitemap.xml there via the CDN (Cloudflare
// rewrite / Vercel proxy → this backend) to land the ranking equity on the apex.
// Until routed they are live + testable on the API host itself.
const (
	seoCanonicalBase = "https://planmypickle.com"
	seoAppBase       = "https://app.planmypickle.com"
	seoMaxEvents     = 1000 // cap for the sitemap / hub scans
)

var seoNonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = seoNonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	return strings.Trim(s, "-")
}

// registerSEO wires the public (no-auth) SEO routes onto the mux.
func (s *Server) registerSEO(mux *http.ServeMux) {
	mux.HandleFunc("GET /sitemap.xml", s.seoSitemap)
	// IndexNow ownership proof: the engines fetch https://<host>/<key>.txt and
	// expect exactly the key back. Registered only when a key is configured, so
	// an unset install serves nothing rather than an empty file.
	if k := service.IndexNowKey(); k != "" {
		mux.HandleFunc("GET /"+k+".txt", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(k))
		})
	}
	mux.HandleFunc("GET /e/{id}", s.seoEventPage)
	mux.HandleFunc("GET /e/{id}/results", s.seoEventResults)
	mux.HandleFunc("GET /pickleball-tournaments/{state}/{county}", s.seoCityHub)
	mux.HandleFunc("GET /pickleball-leagues/{state}/{county}", s.seoLeagueHub)
	mux.HandleFunc("GET /l/{id}", s.seoLeaguePage)
	mux.HandleFunc("GET /class/{id}", s.seoClassPage)
}

func leagueTypeLabel(t string) string {
	switch t {
	case "ladder":
		return "Ladder league"
	case "team":
		return "Team league"
	default:
		return "Round-robin league"
	}
}

// --- data helpers ---

// seoPublicEvents returns every publicly-listed, non-demo event (the same safe
// projection + test-name filter the marketing feed uses), for the sitemap + hubs.
func (s *Server) seoPublicEvents() []model.PublicEvent {
	evs, err := s.svc.PublicEvents(seoMaxEvents, "")
	if err != nil {
		return nil
	}
	return evs
}

func fmtEventDate(rfc string) string {
	if rfc == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return ""
	}
	return t.Format("Mon, Jan 2, 2006")
}

func isoDate(rfc string) string {
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.Format("2006-01-02")
	}
	return ""
}

// --- sitemap ---

func (s *Server) seoSitemap(w http.ResponseWriter, r *http.Request) {
	evs := s.seoPublicEvents()
	// lastmod must mean "when this page's content last changed". It was set to
	// the EVENT'S START DATE, so the sitemap claimed pages were modified in the
	// future — 2026-09-20 on a page served in August. Google discards a lastmod
	// it can't trust, and once discarded it stops helping schedule crawls at all,
	// which is precisely the "Discovered - currently not indexed" problem.
	//
	// Now: a real past timestamp, or nothing. An omitted lastmod is honest and
	// costs only the hint; a wrong one poisons the signal for the whole file.
	type url struct{ loc, lastmod string }
	today := time.Now().UTC().Format("2006-01-02")
	// past keeps only dates that are actually in the past.
	past := func(d string) string {
		if d == "" || d > today {
			return ""
		}
		return d
	}
	var urls []url
	urls = append(urls, url{loc: seoCanonicalBase + "/"})
	// Static free-tool + guide pages (served by the apex/Vercel, listed here so the
	// single sitemap covers them). Add new evergreen pages to this list.
	for _, p := range []string{
		"/tools",
		"/tools/pickleball-round-robin-generator",
		"/tools/pickleball-bracket-generator",
		"/tools/pickleball-double-elimination-bracket",
		"/tools/pickleball-americano-generator",
		"/tools/pickleball-tournament-time-calculator",
		"/guides",
		"/guides/how-to-run-a-dupr-sanctioned-pickleball-tournament",
		"/guides/how-to-run-a-pickleball-round-robin",
		"/guides/pickleball-tournament-formats-explained",
		"/guides/pickleball-skill-levels-explained",
		"/coaches/apply",
	} {
		urls = append(urls, url{loc: seoCanonicalBase + p})
	}

	seenHub := map[string]bool{}
	for _, e := range evs {
		urls = append(urls, url{loc: seoCanonicalBase + "/e/" + e.ID, lastmod: past(isoDate(e.CreatedAt))})
		st, co, ci := slugify(e.State), slugify(e.County), slugify(e.City)
		if st != "" && co != "" && !seenHub[st+"/"+co] {
			seenHub[st+"/"+co] = true
			urls = append(urls, url{loc: seoCanonicalBase + "/pickleball-tournaments/" + st + "/" + co})
		}
		// City pages share the county's URL shape, so they're deduped in the same
		// set — which also stops a city named like its county emitting twice.
		if st != "" && ci != "" && !seenHub[st+"/"+ci] {
			seenHub[st+"/"+ci] = true
			urls = append(urls, url{loc: seoCanonicalBase + "/pickleball-tournaments/" + st + "/" + ci})
		}
	}
	// Public coach directory + verified coach profiles.
	for _, p := range s.seoCoachURLs() {
		urls = append(urls, url{loc: seoCanonicalBase + p})
	}
	// Public upcoming coaching classes.
	if classes, _ := s.svc.PublicUpcomingClasses(seoMaxEvents); len(classes) > 0 {
		for _, c := range classes {
			if seoIsDemoName(c.Title) {
				continue
			}
			urls = append(urls, url{loc: seoCanonicalBase + "/class/" + c.ID, lastmod: past(isoDate(c.CreatedAt))})
		}
	}
	// Public leagues + their per-metro league hubs.
	if leagues, _ := s.svc.PublicLeagues(); len(leagues) > 0 {
		seenLHub := map[string]bool{}
		for _, lg := range leagues {
			urls = append(urls, url{loc: seoCanonicalBase + "/l/" + lg.ID, lastmod: past(isoDate(lg.NextDate))})
			st, co := slugify(lg.State), slugify(lg.County)
			if st != "" && co != "" && !seenLHub[st+"/"+co] {
				seenLHub[st+"/"+co] = true
				urls = append(urls, url{loc: seoCanonicalBase + "/pickleball-leagues/" + st + "/" + co})
			}
		}
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, u := range urls {
		b.WriteString("  <url><loc>" + template.HTMLEscapeString(u.loc) + "</loc>")
		if u.lastmod != "" {
			b.WriteString("<lastmod>" + u.lastmod + "</lastmod>")
		}
		b.WriteString("</url>\n")
	}
	b.WriteString("</urlset>\n")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=900")
	_, _ = w.Write([]byte(b.String()))
}

// --- event page ---

type seoEventData struct {
	Title, Canonical, Description, H1 string
	DateLine, VenueLine, FeeLine      string
	Poster, OGImage                   string
	Dupr                              bool
	RegisterURL, ResultsURL           string
	JSONLD                            template.JS
}

func (s *Server) seoEventPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev, err := s.svc.GetEvent(id)
	// Only expose publicly-listed, non-demo events — never leak a private/unlisted
	// or QA event through the crawlable surface.
	if err != nil || !ev.Listed || seoIsDemoName(ev.Name) {
		s.seoNotFound(w)
		return
	}

	venue := strings.TrimSpace(strOr(ev.VenueName))
	if a := strings.TrimSpace(strOr(ev.VenueAddress)); a != "" {
		if venue != "" {
			venue += " — " + a
		} else {
			venue = a
		}
	}
	if venue == "" {
		venue = strings.TrimSpace(strOr(ev.Location))
	}
	dateLine := fmtEventDate(strOr(ev.StartsAt))

	sanct := ""
	if ev.DuprSanctioned {
		sanct = " (DUPR Sanctioned)"
	}
	desc := "Register for " + ev.Name
	if dateLine != "" {
		desc += " on " + dateLine
	}
	if venue != "" {
		desc += " at " + venue
	}
	desc += ". Live bracket, schedule, and scores on PlanMyPickle."

	feeLine := "Free to register"
	if ev.RegistrationFeeCents > 0 {
		feeLine = "Entry fee: $" + centsToDollars(ev.RegistrationFeeCents)
	}

	// Event JSON-LD (json.Marshal HTML-escapes <,>,& → safe to inline in a script).
	ld := map[string]any{
		"@context": "https://schema.org",
		"@type":    "SportsEvent",
		"name":     ev.Name,
		"sport":    "Pickleball",
		"url":      seoCanonicalBase + "/e/" + ev.ID,
	}
	if d := isoDate(strOr(ev.StartsAt)); d != "" {
		ld["startDate"] = d
	}
	if d := isoDate(strOr(ev.EndsAt)); d != "" {
		ld["endDate"] = d
	}
	if venue != "" {
		ld["location"] = map[string]any{"@type": "Place", "name": venue}
	}
	ld["organizer"] = map[string]any{"@type": "Organization", "name": "PlanMyPickle", "url": seoCanonicalBase}
	ld["offers"] = map[string]any{
		"@type": "Offer", "url": seoAppBase + "/?event=" + ev.ID,
		"price": centsToDollars(ev.RegistrationFeeCents), "priceCurrency": "USD",
	}
	ldJSON, _ := json.Marshal(ld)

	poster := strings.TrimSpace(strOr(ev.PosterURL))
	if poster != "" {
		// Google's Event schema wants an image, and og:image is what makes a
		// shared link render as a card instead of a bare URL.
		ld["image"] = poster
		ldJSON, _ = json.Marshal(ld)
	}

	data := seoEventData{
		Title:       ev.Name + sanct + " — Pickleball Tournament | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/e/" + ev.ID,
		Description: desc,
		H1:          ev.Name,
		DateLine:    dateLine,
		VenueLine:   venue,
		FeeLine:     feeLine,
		Poster:      poster,
		OGImage:     poster,
		Dupr:        ev.DuprSanctioned,
		RegisterURL: seoAppBase + "/?event=" + ev.ID,
		ResultsURL:  seoCanonicalBase + "/e/" + ev.ID + "/results",
		JSONLD:      template.JS(ldJSON),
	}
	s.seoRender(w, seoEventTmpl, data)
}

// --- city / metro hub ---

type seoHubCard struct {
	Name, DateLine, Venue, URL string
	// Poster is the organizer's flyer. Shown as a thumbnail, not full-bleed:
	// event flyers are usually tall portrait art, and one at card width would
	// push the second event off the screen entirely.
	Poster string
	Dupr   bool
}
type seoPlaceLink struct{ Name, URL string }

type seoHubData struct {
	Title, Canonical, Description, H1, Intro string
	OGImage                                  string
	Cities                                   []seoPlaceLink
	Cards                                    []seoHubCard
	JSONLD                                   template.JS
}

// seoCityHub serves BOTH /pickleball-tournaments/{state}/{county} and the same
// path with a city slug.
//
// One handler and one route on purpose: the two URL shapes are identical, Go's
// mux can't tell them apart, and — more usefully — the existing CDN rewrite
// already forwards this pattern, so city pages went live without touching the
// edge config.
//
// County is tried first because those URLs are already indexed and must keep
// resolving. A city page only answers when the slug isn't a county, which is
// the common case for what people actually type: "pickleball tournaments san
// diego", not "...san diego county".
func (s *Server) seoCityHub(w http.ResponseWriter, r *http.Request) {
	stateSlug, placeSlug := r.PathValue("state"), r.PathValue("county")
	evs := s.seoPublicEvents()

	var match []model.PublicEvent
	var stateName, countyName string
	for _, e := range evs {
		if slugify(e.State) == stateSlug && slugify(e.County) == placeSlug {
			match = append(match, e)
			stateName, countyName = e.State, e.County
		}
	}
	if len(match) == 0 {
		s.seoTownHub(w, stateSlug, placeSlug, evs)
		return
	}
	// Soonest first (undated last).
	sort.SliceStable(match, func(i, j int) bool {
		return strOr(match[i].StartsAt) < strOr(match[j].StartsAt)
	})

	place := countyName
	if stateName != "" {
		place += ", " + stateName
	}
	var cards []seoHubCard
	var itemList []any
	for i, e := range match {
		venue := strings.TrimSpace(strOr(e.VenueName))
		if venue == "" {
			venue = strings.TrimSpace(strOr(e.Location))
		}
		cards = append(cards, seoHubCard{
			Name: e.Name, DateLine: fmtEventDate(strOr(e.StartsAt)),
			Venue: venue, URL: "/e/" + e.ID, Poster: strOr(e.PosterURL),
			Dupr: e.DuprSanctioned,
		})
		itemList = append(itemList, map[string]any{
			"@type": "ListItem", "position": i + 1,
			"url": seoCanonicalBase + "/e/" + e.ID, "name": e.Name,
		})
	}

	ld := map[string]any{
		"@context": "https://schema.org", "@type": "ItemList",
		"name": "Pickleball Tournaments in " + place, "itemListElement": itemList,
	}
	ldJSON, _ := json.Marshal(ld)

	og := ""
	for _, c := range cards {
		if c.Poster != "" {
			og = c.Poster
			break
		}
	}
	// Link down to each city in this county: gives the city pages a crawl path
	// and lets someone on the county page narrow to where they'll actually drive.
	var cityLinks []seoPlaceLink
	seenCity := map[string]bool{}
	for _, e := range match {
		if e.City == "" || seenCity[e.City] {
			continue
		}
		seenCity[e.City] = true
		cityLinks = append(cityLinks, seoPlaceLink{
			Name: e.City,
			URL:  "/pickleball-tournaments/" + stateSlug + "/" + slugify(e.City),
		})
	}
	sort.Slice(cityLinks, func(i, j int) bool { return cityLinks[i].Name < cityLinks[j].Name })

	data := seoHubData{
		OGImage:     og,
		Cities:      cityLinks,
		Title:       "Pickleball Tournaments in " + place + " — 2026 Schedule | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/pickleball-tournaments/" + stateSlug + "/" + placeSlug,
		Description: "Find and register for pickleball tournaments in " + place + ". Upcoming events, divisions, skill brackets, and DUPR-sanctioned play on PlanMyPickle.",
		H1:          "Pickleball Tournaments in " + place,
		Intro:       plural(len(match), "upcoming pickleball tournament", "upcoming pickleball tournaments") + " in " + place + " — browse divisions, skill brackets, and fees, then register in a tap.",
		Cards:       cards,
		JSONLD:      template.JS(ldJSON),
	}
	s.seoRender(w, seoHubTmpl, data)
}

// seoTownHub is the city-level page: /pickleball-tournaments/california/
// chula-vista. Called only when the slug didn't match a county.
//
// This exists because the county pages target a phrase nobody types. People
// search "pickleball tournaments san diego"; the county page is titled "San
// Diego County". Same events, a name real queries use.
func (s *Server) seoTownHub(
	w http.ResponseWriter, stateSlug, citySlug string, evs []model.PublicEvent) {
	var match []model.PublicEvent
	var stateName, cityName, countyName string
	for _, e := range evs {
		if slugify(e.State) == stateSlug && e.City != "" &&
			slugify(e.City) == citySlug {
			match = append(match, e)
			stateName, cityName, countyName = e.State, e.City, e.County
		}
	}
	if len(match) == 0 {
		s.seoNotFound(w)
		return
	}
	sort.SliceStable(match, func(i, j int) bool {
		return strOr(match[i].StartsAt) < strOr(match[j].StartsAt)
	})

	place := cityName
	if stateName != "" {
		place += ", " + stateName
	}

	var cards []seoHubCard
	var items []any
	og := ""
	for i, e := range match {
		venue := strings.TrimSpace(strOr(e.VenueName))
		if venue == "" {
			venue = strings.TrimSpace(strOr(e.Location))
		}
		poster := strOr(e.PosterURL)
		if og == "" {
			og = poster
		}
		cards = append(cards, seoHubCard{
			Name: e.Name, DateLine: fmtEventDate(strOr(e.StartsAt)),
			Venue: venue, URL: "/e/" + e.ID, Poster: poster,
			Dupr: e.DuprSanctioned,
		})
		items = append(items, map[string]any{
			"@type": "ListItem", "position": i + 1,
			"url": seoCanonicalBase + "/e/" + e.ID, "name": e.Name,
		})
	}
	ld, _ := json.Marshal(map[string]any{
		"@context": "https://schema.org", "@type": "ItemList",
		"name": "Pickleball Tournaments in " + place, "itemListElement": items,
	})

	// A link back up to the county page, so the city pages aren't orphans and
	// the two levels pass authority to each other.
	countyLink := ""
	if countyName != "" {
		countyLink = "/pickleball-tournaments/" + slugify(stateName) + "/" +
			slugify(countyName)
	}

	s.seoRender(w, seoTownTmpl, seoTownData{
		OGImage:     og,
		Title:       "Pickleball Tournaments in " + cityName + " — 2026 Schedule | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/pickleball-tournaments/" + stateSlug + "/" + citySlug,
		Description: "Upcoming pickleball tournaments in " + place + " — divisions, skill brackets, entry fees and DUPR-sanctioned play. Register in a tap on PlanMyPickle.",
		H1:          "Pickleball Tournaments in " + place,
		Intro:       plural(len(match), "upcoming pickleball tournament", "upcoming pickleball tournaments") + " in " + place + " — browse divisions, skill brackets and fees, then register in a tap.",
		City:        cityName,
		Place:       place,
		County:      countyName,
		CountyURL:   countyLink,
		Cards:       cards,
		JSONLD:      template.JS(ld),
	})
}

type seoTownData struct {
	Title, Canonical, Description, H1, Intro string
	City, Place, County, CountyURL, OGImage  string
	Cards                                    []seoHubCard
	JSONLD                                   template.JS
}

// --- league hub + per-league page ---

func (s *Server) seoLeagueHub(w http.ResponseWriter, r *http.Request) {
	stateSlug, countySlug := r.PathValue("state"), r.PathValue("county")
	leagues, _ := s.svc.PublicLeagues()
	var match []model.PublicLeague
	var stateName, countyName string
	for _, lg := range leagues {
		if slugify(lg.State) == stateSlug && slugify(lg.County) == countySlug {
			match = append(match, lg)
			stateName, countyName = lg.State, lg.County
		}
	}
	if len(match) == 0 {
		s.seoNotFound(w)
		return
	}
	place := countyName
	if stateName != "" {
		place += ", " + stateName
	}
	var cards []seoHubCard
	var items []any
	for i, lg := range match {
		cards = append(cards, seoHubCard{
			Name:     lg.Name,
			DateLine: leagueTypeLabel(lg.LeagueType) + " · " + plural(lg.SessionCount, "session", "sessions"),
			URL:      "/l/" + lg.ID, Dupr: lg.Sanctioned,
		})
		items = append(items, map[string]any{"@type": "ListItem", "position": i + 1,
			"url": seoCanonicalBase + "/l/" + lg.ID, "name": lg.Name})
	}
	ld, _ := json.Marshal(map[string]any{"@context": "https://schema.org", "@type": "ItemList",
		"name": "Pickleball Leagues in " + place, "itemListElement": items})
	s.seoRender(w, seoHubTmpl, seoHubData{
		Title:       "Pickleball Leagues in " + place + " — Join or Start a League | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/pickleball-leagues/" + stateSlug + "/" + countySlug,
		Description: "Find and join pickleball leagues in " + place + " — round-robin, ladder and team leagues on PlanMyPickle.",
		H1:          "Pickleball Leagues in " + place,
		Intro:       plural(len(match), "pickleball league", "pickleball leagues") + " in " + place + " — round-robin, ladder and team play. Join one or start your own.",
		Cards:       cards, JSONLD: template.JS(ld),
	})
}

func (s *Server) seoLeaguePage(w http.ResponseWriter, r *http.Request) {
	lg, sessions, err := s.svc.PublicLeagueByID(r.PathValue("id"))
	if err != nil {
		s.seoNotFound(w)
		return
	}
	place := strings.TrimSpace(lg.County)
	if lg.State != "" {
		if place != "" {
			place += ", " + lg.State
		} else {
			place = lg.State
		}
	}
	var cards []seoHubCard
	var items []any
	for i, e := range sessions {
		venue := strings.TrimSpace(strOr(e.VenueName))
		if venue == "" {
			venue = strings.TrimSpace(strOr(e.Location))
		}
		cards = append(cards, seoHubCard{
			Name: e.Name, DateLine: fmtEventDate(strOr(e.StartsAt)),
			Venue: venue, URL: "/e/" + e.ID, Dupr: e.DuprSanctioned,
		})
		items = append(items, map[string]any{"@type": "ListItem", "position": i + 1,
			"url": seoCanonicalBase + "/e/" + e.ID, "name": e.Name})
	}
	ld, _ := json.Marshal(map[string]any{"@context": "https://schema.org", "@type": "ItemList",
		"name": lg.Name + " — sessions", "itemListElement": items})

	titlePlace := ""
	if place != "" {
		titlePlace = " in " + place
	}
	intro := leagueTypeLabel(lg.LeagueType)
	if place != "" {
		intro += " in " + place
	}
	intro += " · " + plural(lg.SessionCount, "session", "sessions") + "."
	if d := strings.TrimSpace(lg.Description); d != "" {
		intro += " " + d
	}
	s.seoRender(w, seoHubTmpl, seoHubData{
		Title:       lg.Name + " — Pickleball League" + titlePlace + " | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/l/" + lg.ID,
		Description: "Join " + lg.Name + ", a pickleball league" + titlePlace + ". Sessions, standings and live scores on PlanMyPickle.",
		H1:          lg.Name,
		Intro:       intro,
		Cards:       cards, JSONLD: template.JS(ld),
	})
}

// --- coaching class page ---

type seoClassData struct {
	OGImage                                            string
	Title, Canonical, Description, H1                  string
	CoachLine, DateLine, VenueLine, FeeLine, SpotsLine string
	RegisterURL                                        string
	JSONLD                                             template.JS
}

func (s *Server) seoClassPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.PublicClassByID(r.PathValue("id"))
	// Never expose a demo/test class through the crawlable surface.
	if err != nil || seoIsDemoName(c.Title) ||
		(c.CoachName != "" && seoIsDemoName(c.CoachName)) {
		s.seoNotFound(w)
		return
	}

	dateLine := fmtEventDate(c.StartsAt)
	venue := strings.TrimSpace(c.Location)
	fee := "Free"
	if c.PriceCents > 0 {
		fee = "$" + centsToDollars(c.PriceCents)
	}
	coachLine := ""
	if c.CoachName != "" {
		coachLine = "with " + c.CoachName
	}
	spots, avail := "", "https://schema.org/InStock"
	if c.Capacity > 0 {
		left := c.Capacity - c.EnrolledCount
		if left <= 0 {
			spots, avail = "Full — join the waitlist", "https://schema.org/SoldOut"
		} else {
			spots = fmt.Sprintf("%d of %d spots left", left, c.Capacity)
		}
	}

	desc := "Join " + c.Title
	if c.CoachName != "" {
		desc += " with " + c.CoachName
	}
	if dateLine != "" {
		desc += " on " + dateLine
	}
	if venue != "" {
		desc += " at " + venue
	}
	desc += ". " + fee + " · book your spot on PlanMyPickle."

	ld := map[string]any{
		"@context":            "https://schema.org",
		"@type":               "EducationEvent",
		"name":                c.Title,
		"about":               "Pickleball",
		"url":                 seoCanonicalBase + "/class/" + c.ID,
		"eventAttendanceMode": "https://schema.org/OfflineEventAttendanceMode",
	}
	if d := isoDate(c.StartsAt); d != "" {
		ld["startDate"] = d
	}
	if d := isoDate(c.EndsAt); d != "" {
		ld["endDate"] = d
	}
	if strings.TrimSpace(c.Description) != "" {
		ld["description"] = c.Description
	}
	if venue != "" {
		place := map[string]any{"@type": "Place", "name": venue}
		if c.Lat != nil && c.Lng != nil {
			place["geo"] = map[string]any{
				"@type": "GeoCoordinates", "latitude": *c.Lat, "longitude": *c.Lng}
		}
		ld["location"] = place
	}
	if c.CoachName != "" {
		ld["performer"] = map[string]any{"@type": "Person", "name": c.CoachName}
	}
	ld["organizer"] = map[string]any{
		"@type": "Organization", "name": "PlanMyPickle", "url": seoCanonicalBase}
	ld["offers"] = map[string]any{
		"@type": "Offer", "url": seoAppBase + "/?class=" + c.ID,
		"price": centsToDollars(c.PriceCents), "priceCurrency": "USD",
		"availability": avail,
	}
	ldJSON, _ := json.Marshal(ld)

	s.seoRender(w, seoClassTmpl, seoClassData{
		Title:       c.Title + " — Pickleball Class | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/class/" + c.ID,
		Description: desc,
		H1:          c.Title,
		CoachLine:   coachLine,
		DateLine:    dateLine,
		VenueLine:   venue,
		FeeLine:     fee,
		SpotsLine:   spots,
		RegisterURL: seoAppBase + "/?class=" + c.ID,
		JSONLD:      template.JS(ldJSON),
	})
}

// --- event results / standings page ---

type seoResultsData struct {
	OGImage                                          string
	Title, Canonical, Description, H1, Sub, EventURL string
	Body                                             template.HTML
	JSONLD                                           template.JS
}

func (s *Server) seoEventResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev, err := s.svc.GetEvent(id)
	if err != nil || !ev.Listed || seoIsDemoName(ev.Name) {
		s.seoNotFound(w)
		return
	}
	brackets, _ := s.svc.GetBrackets(id)
	var body strings.Builder
	champ := ""
	hasResults := false
	for _, b := range brackets {
		st, e2 := s.svc.Standings(id, b.ID, true)
		if e2 != nil || len(st) == 0 {
			continue
		}
		hasResults = true
		name := strings.TrimSpace(b.Name)
		if name == "" {
			name = "Division"
		}
		body.WriteString(`<div class="round"><h3>` + template.HTMLEscapeString(name) + `</h3>`)
		body.WriteString(`<table style="width:100%;border-collapse:collapse;font-size:14px">` +
			`<tr style="color:#5b6b80;text-align:left"><th style="padding:6px 8px 6px 0">#</th>` +
			`<th style="padding:6px 8px">Player</th><th style="padding:6px 8px;text-align:center">W</th>` +
			`<th style="padding:6px 8px;text-align:center">L</th><th style="padding:6px 8px;text-align:center">Diff</th></tr>`)
		for i, row := range st {
			medal := ""
			if i == 0 {
				medal = " 🏆"
				if champ == "" {
					champ = row.FullName
				}
			}
			ds := strconv.Itoa(row.PointDiff)
			if row.PointDiff > 0 {
				ds = "+" + ds
			}
			body.WriteString(fmt.Sprintf(
				`<tr style="border-top:1px solid #eef2e6"><td style="padding:7px 8px 7px 0;font-weight:800;color:#4f8b3b">%d</td>`+
					`<td style="padding:7px 8px;font-weight:600">%s%s</td><td style="padding:7px 8px;text-align:center">%d</td>`+
					`<td style="padding:7px 8px;text-align:center">%d</td><td style="padding:7px 8px;text-align:center">%s</td></tr>`,
				i+1, template.HTMLEscapeString(row.FullName), medal, row.Wins, row.Losses, ds))
		}
		body.WriteString(`</table></div>`)
	}
	eventURL := seoCanonicalBase + "/e/" + id
	if !hasResults {
		// No results posted yet — link back, noindex (don't index a thin page).
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8">` +
			`<meta name="robots" content="noindex"><title>Results — ` +
			template.HTMLEscapeString(ev.Name) + `</title><p>Results for ` +
			template.HTMLEscapeString(ev.Name) + ` aren't posted yet. <a href="` +
			eventURL + `">See the event</a>.</p>`))
		return
	}
	sub := "Live standings & results"
	if champ != "" {
		sub = "🏆 Champion: " + champ
	}
	ld, _ := json.Marshal(map[string]any{
		"@context": "https://schema.org", "@type": "SportsEvent",
		"name": ev.Name + " — Results", "sport": "Pickleball",
		"url": seoCanonicalBase + "/e/" + id + "/results",
	})
	s.seoRender(w, seoResultsTmpl, seoResultsData{
		Title:       ev.Name + " — Results & Standings | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/e/" + id + "/results",
		Description: "Final standings and results for " + ev.Name + " — division standings, wins and point differentials on PlanMyPickle.",
		H1:          ev.Name + " — Results",
		Sub:         sub, Body: template.HTML(body.String()),
		EventURL: eventURL, JSONLD: template.JS(ld),
	})
}

// --- rendering ---

func (s *Server) seoRender(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = t.Execute(w, data)
}

func (s *Server) seoNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Not found | PlanMyPickle</title>` +
		`<meta name="robots" content="noindex"><p>This page isn't available. ` +
		`<a href="` + seoCanonicalBase + `">Go to PlanMyPickle</a>.</p>`))
}

var seoIsDemoRe = regexp.MustCompile(`(?i)\b(test|demo|dbg|debug|authcheck)\b`)

func seoIsDemoName(n string) bool { return seoIsDemoRe.MatchString(n) }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return itoa(n) + " " + many
}

// --- templates (parsed once) ---

const seoHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
<link rel="canonical" href="{{.Canonical}}">
<meta property="og:title" content="{{.Title}}">
<meta property="og:description" content="{{.Description}}">
<meta property="og:url" content="{{.Canonical}}">
<meta property="og:type" content="website">
{{if .OGImage}}<meta property="og:image" content="{{.OGImage}}">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:image" content="{{.OGImage}}">{{end}}
<script type="application/ld+json">{{.JSONLD}}</script>
<style>
:root{--navy:#16245c;--green:#4f8b3b;--ink:#16203a;--muted:#5b6b80}
*{box-sizing:border-box}body{margin:0;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:var(--ink);background:#f6faf1;line-height:1.5}
.wrap{max-width:760px;margin:0 auto;padding:24px 18px 60px}
header a{color:var(--green);font-weight:800;text-decoration:none;font-size:18px}
h1{color:var(--navy);font-size:26px;line-height:1.2;margin:18px 0 6px}
.meta{color:var(--muted);font-size:15px;margin:2px 0}
.badge{display:inline-block;background:#e9f2df;color:var(--green);font-weight:800;font-size:12px;padding:3px 9px;border-radius:999px;margin-top:8px}
.cta{display:inline-block;margin:22px 0 6px;background:#f5c518;color:var(--ink);text-decoration:none;font-weight:800;padding:13px 22px;border-radius:999px}
.card{display:block;background:#fff;border:1px solid #e7eedd;border-radius:14px;padding:16px 18px;margin:12px 0;text-decoration:none;color:var(--ink)}
.card h2{margin:0 0 4px;color:var(--navy);font-size:18px}
.card.has-img{display:flex;gap:14px;align-items:flex-start}
.thumb{width:104px;height:104px;flex:none;border-radius:10px;object-fit:cover;background:#eef4e6}
.cbody{min-width:0}
.avatar{border-radius:50%}
.portrait{width:120px;height:120px;border-radius:50%;object-fit:cover;background:#eef4e6;border:1px solid #e7eedd;margin:16px 0 0;display:block}
.hero{width:100%;max-height:420px;object-fit:cover;border-radius:14px;border:1px solid #e7eedd;background:#eef4e6;margin:14px 0 4px;display:block}
.foot{margin-top:40px;color:var(--muted);font-size:13px}
.foot a{color:var(--green)}
</style></head><body><div class="wrap">
<header><a href="` + seoCanonicalBase + `">🥒 PlanMyPickle</a></header>`

const seoFoot = `<p class="foot"><a href="/coaches">Find a pickleball coach</a> · <a href="/coaches/apply">Coach with us</a></p>
<p class="foot">Powered by <a href="` + seoCanonicalBase + `">PlanMyPickle</a> — run pickleball tournaments, minus the chaos.</p>
</div></body></html>`

var seoEventTmpl = template.Must(template.New("ev").Parse(seoHead + `
<h1>{{.H1}}</h1>
{{if .Poster}}<img class="hero" src="{{.Poster}}" alt="{{.H1}} event poster">{{end}}
{{if .DateLine}}<p class="meta">📅 {{.DateLine}}</p>{{end}}
{{if .VenueLine}}<p class="meta">📍 {{.VenueLine}}</p>{{end}}
<p class="meta">💵 {{.FeeLine}}</p>
{{if .Dupr}}<span class="badge">DUPR Sanctioned</span>{{end}}
<p><a class="cta" href="{{.RegisterURL}}">Register &amp; see the live bracket →</a></p>
<p class="meta">Registration, live scores, schedule, and standings run on PlanMyPickle. Tap above to open the event and sign up.</p>
{{if .ResultsURL}}<p class="meta">🏆 <a href="{{.ResultsURL}}">Results &amp; standings</a></p>{{end}}
` + seoFoot))

var seoClassTmpl = template.Must(template.New("cls").Parse(seoHead + `
<h1>{{.H1}}</h1>
{{if .CoachLine}}<p class="meta">🎾 {{.CoachLine}}</p>{{end}}
{{if .DateLine}}<p class="meta">📅 {{.DateLine}}</p>{{end}}
{{if .VenueLine}}<p class="meta">📍 {{.VenueLine}}</p>{{end}}
<p class="meta">💵 {{.FeeLine}}</p>
{{if .SpotsLine}}<p class="meta">👥 {{.SpotsLine}}</p>{{end}}
<p><a class="cta" href="{{.RegisterURL}}">Book your spot →</a></p>
<p class="meta">Class booking runs on PlanMyPickle. Tap above to reserve your spot and get reminders.</p>
` + seoFoot))

var seoResultsTmpl = template.Must(template.New("res").Parse(seoHead + `
<h1>{{.H1}}</h1>
<p class="meta">{{.Sub}}</p>
{{.Body}}
<p><a class="cta" href="{{.EventURL}}">Event details &amp; registration →</a></p>
` + seoFoot))

// The city page carries real editorial copy, not just a list.
//
// The county pages were ~265 words of listing chrome, competing against
// directories with thousands of events and years of history. A page that thin
// gives Google nothing to rank on beyond the event names themselves. What
// follows is the substance a player searching the city actually wants — formats,
// brackets, what sanctioning means, what it costs — and it's the same on every
// city page by design: it's context, and it's true everywhere.
var seoTownTmpl = template.Must(template.New("town").Parse(seoHead + `
<h1>{{.H1}}</h1>
<p class="meta">{{.Intro}}</p>
{{range .Cards}}<a class="card{{if .Poster}} has-img{{end}}" href="{{.URL}}">
{{if .Poster}}<img class="thumb" src="{{.Poster}}" alt="{{.Name}} event poster" loading="lazy" width="104" height="104">{{end}}
<div class="cbody">
<h2>{{.Name}}</h2>
{{if .DateLine}}<p class="meta">📅 {{.DateLine}}</p>{{end}}
{{if .Venue}}<p class="meta">📍 {{.Venue}}</p>{{end}}
{{if .Dupr}}<span class="badge">DUPR Sanctioned</span>{{end}}
</div>
</a>{{end}}
<p><a class="cta" href="` + seoAppBase + `">Organizing? Run your tournament free →</a></p>

<h2>Playing a pickleball tournament in {{.City}}</h2>
<p>Every event above runs on PlanMyPickle, so the same things are true of all of
them: you register online, you get your schedule and court assignment on your
phone, and scores go up live as they're entered — no crowding round a paper
draw sheet taped to a fence.</p>
<p>Most {{.City}} events are doubles, and you enter with a partner in a skill
bracket. Some also run singles or mixed. If an event is listed as
<b>DUPR&nbsp;Sanctioned</b>, your results are submitted to DUPR and count toward
your rating — worth knowing before you enter, because sanctioned play is how you
build a rating that other tournaments will seed you on.</p>

<h2>Picking the right bracket</h2>
<p>Brackets are set by skill, usually in half-point steps — 3.0, 3.5, 4.0 and up
— and often split by age. Entering a bracket well above your level isn't
ambitious, it's a short day. If you don't have a DUPR rating yet, enter where you
honestly play; organizers can move you before the draw is built, and most would
rather do that than have a mismatched first round.</p>

<h2>What the format tells you about your day</h2>
<p><b>Round robin</b> — you play everyone in your pool, so a bad first game
doesn't end your tournament. Best value if you want court time.</p>
<p><b>Single elimination</b> — lose once and you're done. Shortest day, highest
stakes.</p>
<p><b>Double elimination</b> — a losers' bracket gives you a second life, so one
bad match doesn't decide it.</p>
<p><b>Pools to playoff</b> — round-robin pools seed a knockout. The most common
shape for a full-day event, and the one that rewards consistency.</p>

<h2>Entry fees and what to bring</h2>
<p>Entry fees are shown on each event page before you commit, and vary with the
format and how many events you enter. Bring more water than you think, indoor
and outdoor balls if the listing doesn't say which is used, and tape or a spare
grip. Arrive early enough to warm up — first-round no-shows are usually people
who budgeted for the drive but not the parking.</p>

<h2>Running one in {{.City}}?</h2>
<p>PlanMyPickle is free to run a tournament on: draws, scheduling, live scoring,
standings and check-in. If you organize in {{.City}}, your event appears on this
page automatically once it's published and listed publicly.</p>
<p><a class="cta" href="` + seoAppBase + `">Create your event free →</a></p>
{{if .CountyURL}}<p class="meta">Looking wider? See all
<a href="{{.CountyURL}}">pickleball tournaments in {{.County}}</a>.</p>{{end}}
<p class="meta">Also on PlanMyPickle:
<a href="/coaches">pickleball coaches and lessons</a> ·
<a href="` + seoCanonicalBase + `/guides/pickleball-tournament-formats-explained">tournament formats explained</a> ·
<a href="` + seoCanonicalBase + `/guides/pickleball-skill-levels-explained">skill levels explained</a></p>
` + seoFoot))

var seoHubTmpl = template.Must(template.New("hub").Parse(seoHead + `
<h1>{{.H1}}</h1>
<p class="meta">{{.Intro}}</p>
{{range .Cards}}<a class="card{{if .Poster}} has-img{{end}}" href="{{.URL}}">
{{if .Poster}}<img class="thumb" src="{{.Poster}}" alt="{{.Name}} event poster" loading="lazy" width="104" height="104">{{end}}
<div class="cbody">
<h2>{{.Name}}</h2>
{{if .DateLine}}<p class="meta">📅 {{.DateLine}}</p>{{end}}
{{if .Venue}}<p class="meta">📍 {{.Venue}}</p>{{end}}
{{if .Dupr}}<span class="badge">DUPR Sanctioned</span>{{end}}
</div>
</a>{{end}}
<p><a class="cta" href="` + seoAppBase + `">Organizing? Run your tournament free →</a></p>
{{if .Cities}}<p class="meta">By city:
{{range $i, $c := .Cities}}{{if $i}} · {{end}}<a href="{{$c.URL}}">{{$c.Name}}</a>{{end}}</p>{{end}}
` + seoFoot))
