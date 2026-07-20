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
	mux.HandleFunc("GET /e/{id}", s.seoEventPage)
	mux.HandleFunc("GET /pickleball-tournaments/{state}/{county}", s.seoCityHub)
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
	type url struct{ loc, lastmod string }
	var urls []url
	urls = append(urls, url{loc: seoCanonicalBase + "/"})

	seenHub := map[string]bool{}
	for _, e := range evs {
		urls = append(urls, url{loc: seoCanonicalBase + "/e/" + e.ID, lastmod: isoDate(strOr(e.StartsAt))})
		st, co := slugify(e.State), slugify(e.County)
		if st != "" && co != "" && !seenHub[st+"/"+co] {
			seenHub[st+"/"+co] = true
			urls = append(urls, url{loc: seoCanonicalBase + "/pickleball-tournaments/" + st + "/" + co})
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
	Dupr                              bool
	RegisterURL                       string
	JSONLD                            template.HTML
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

	data := seoEventData{
		Title:       ev.Name + sanct + " — Pickleball Tournament | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/e/" + ev.ID,
		Description: desc,
		H1:          ev.Name,
		DateLine:    dateLine,
		VenueLine:   venue,
		FeeLine:     feeLine,
		Dupr:        ev.DuprSanctioned,
		RegisterURL: seoAppBase + "/?event=" + ev.ID,
		JSONLD:      template.HTML(ldJSON),
	}
	s.seoRender(w, seoEventTmpl, data)
}

// --- city / metro hub ---

type seoHubCard struct {
	Name, DateLine, Venue, URL string
	Dupr                       bool
}
type seoHubData struct {
	Title, Canonical, Description, H1, Intro string
	Cards                                    []seoHubCard
	JSONLD                                   template.HTML
}

func (s *Server) seoCityHub(w http.ResponseWriter, r *http.Request) {
	stateSlug, countySlug := r.PathValue("state"), r.PathValue("county")
	evs := s.seoPublicEvents()

	var match []model.PublicEvent
	var stateName, countyName string
	for _, e := range evs {
		if slugify(e.State) == stateSlug && slugify(e.County) == countySlug {
			match = append(match, e)
			stateName, countyName = e.State, e.County
		}
	}
	if len(match) == 0 {
		s.seoNotFound(w)
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
			Venue: venue, URL: "/e/" + e.ID, Dupr: e.DuprSanctioned,
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

	data := seoHubData{
		Title:       "Pickleball Tournaments in " + place + " — 2026 Schedule | PlanMyPickle",
		Canonical:   seoCanonicalBase + "/pickleball-tournaments/" + stateSlug + "/" + countySlug,
		Description: "Find and register for pickleball tournaments in " + place + ". Upcoming events, divisions, skill brackets, and DUPR-sanctioned play on PlanMyPickle.",
		H1:          "Pickleball Tournaments in " + place,
		Intro:       plural(len(match), "upcoming pickleball tournament", "upcoming pickleball tournaments") + " in " + place + " — browse divisions, skill brackets, and fees, then register in a tap.",
		Cards:       cards,
		JSONLD:      template.HTML(ldJSON),
	}
	s.seoRender(w, seoHubTmpl, data)
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
.foot{margin-top:40px;color:var(--muted);font-size:13px}
.foot a{color:var(--green)}
</style></head><body><div class="wrap">
<header><a href="` + seoCanonicalBase + `">🥒 PlanMyPickle</a></header>`

const seoFoot = `<p class="foot">Powered by <a href="` + seoCanonicalBase + `">PlanMyPickle</a> — run pickleball tournaments, minus the chaos.</p>
</div></body></html>`

var seoEventTmpl = template.Must(template.New("ev").Parse(seoHead + `
<h1>{{.H1}}</h1>
{{if .DateLine}}<p class="meta">📅 {{.DateLine}}</p>{{end}}
{{if .VenueLine}}<p class="meta">📍 {{.VenueLine}}</p>{{end}}
<p class="meta">💵 {{.FeeLine}}</p>
{{if .Dupr}}<span class="badge">DUPR Sanctioned</span>{{end}}
<p><a class="cta" href="{{.RegisterURL}}">Register &amp; see the live bracket →</a></p>
<p class="meta">Registration, live scores, schedule, and standings run on PlanMyPickle. Tap above to open the event and sign up.</p>
` + seoFoot))

var seoHubTmpl = template.Must(template.New("hub").Parse(seoHead + `
<h1>{{.H1}}</h1>
<p class="meta">{{.Intro}}</p>
{{range .Cards}}<a class="card" href="{{.URL}}">
<h2>{{.Name}}</h2>
{{if .DateLine}}<p class="meta">📅 {{.DateLine}}</p>{{end}}
{{if .Venue}}<p class="meta">📍 {{.Venue}}</p>{{end}}
{{if .Dupr}}<span class="badge">DUPR Sanctioned</span>{{end}}
</a>{{end}}
<p><a class="cta" href="` + seoAppBase + `">Organizing? Run your tournament free →</a></p>
` + seoFoot))
