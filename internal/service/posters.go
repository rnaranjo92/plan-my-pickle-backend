package service

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// AI event posters (Nano Banana Pro).
//
// The differentiator is that the PROMPT IS DERIVED, not typed: the organizer
// picks a style, and the event's own data — name, date, venue, city, the
// club's logo — becomes the brief. Telling organizers to go prompt Gemini
// themselves is the thing this replaces.
//
// INERT until GEMINI_API_KEY is set in Railway. While clubs are in founding
// access the feature is free for anyone who can edit the event; when the Club
// fence goes up this is the natural thing to meter (see the pricing memory) —
// the gate to tighten is posterAllowed.

// posterStyles is the organizer's whole vocabulary — a picker, not a textarea.
// Keys are what the client sends; values steer the model.
var posterStyles = map[string]string{
	"clean":     "clean modern sports poster, bold flat colors, strong grid layout, generous whitespace",
	"retro":     "retro athletic poster, distressed textures, vintage varsity typography, 1970s sports-program palette",
	"neon":      "night-league poster, neon glow on deep navy, dramatic court floodlights, electric energy",
	"paper":     "hand-made community flyer, risograph print texture, two-ink palette, friendly and local",
	"epic":      "cinematic tournament poster, dramatic low-angle court photography style, gold and navy, championship gravitas",
	"tropic":    "sunny outdoor tournament poster, palm shadows, warm gradients, beach-town summer energy",
	"comic":     "pop-art comic book poster, bold halftone dots, dynamic action panels, punchy primary colors, exclamation energy",
	"street":    "urban streetball poster, spray-paint stencil texture, asphalt and chain-link motifs, bold graffiti-influenced type",
	"fiesta":    "festive fiesta poster, papel picado banners, vibrant pink orange and teal, celebratory hand-crafted feel",
	"deco":      "art-deco gala poster, black and gold geometry, elegant fan patterns, 1920s luxury typography",
	"chalk":     "gym chalkboard poster, white and pastel chalk hand-lettering on deep green board, coach's play-diagram doodles",
	"americana": "summer Americana poster, stars-and-stripes bunting, backyard-BBQ warmth, vintage state-fair typography",
	// The 2026-trend trio: abstract gradients, maximalism, dimensional type.
	"gradient": "modern abstract poster, vibrant flowing color gradients, soft glowing shapes, dreamy Y2K energy, high contrast type",
	"maximal":  "maximalist poster, richly layered collage of shapes stickers and patterns, exuberant color, dense joyful composition that still keeps the text readable",
	"depth":    "3D-inspired poster, extruded dimensional typography with dramatic perspective and soft shadows, floating elements, tactile depth",
}

// PosterStyleKeys lists the picker's options in a stable order.
func PosterStyleKeys() []string {
	return []string{
		"clean", "retro", "neon", "paper", "epic", "tropic",
		"comic", "street", "fiesta", "deco", "chalk", "americana",
		"gradient", "maximal", "depth",
	}
}

// posterLayouts is the COMPOSITION axis — where things sit on the page.
// Orthogonal to style on purpose: "retro" answers how it's drawn, "emblem"
// answers where the title goes, and conflating them forced organizers to trade
// one for the other.
var posterLayouts = map[string]string{
	"classic": "Composition: big headline across the top, event details grouped at the bottom, imagery filling the middle.",
	"emblem":  "Composition: a central circular emblem/badge holding the event title, details arranged in an arc beneath it, generous margins.",
	"split":   "Composition: bold diagonal split — imagery on one side, a clean solid-color panel carrying all text on the other.",
	"ticket":  "Composition: styled like an oversized admission ticket, title in the main field, details along a perforated stub edge.",
}

// PosterLayoutKeys in display order.
func PosterLayoutKeys() []string {
	return []string{"classic", "emblem", "split", "ticket"}
}

// posterVibes is the IMAGERY axis — what kind of picture it is at all.
var posterVibes = map[string]string{
	"illustrated": "Rendering: flat modern vector illustration, clean shapes, no photography.",
	"photo":       "Rendering: cinematic photographic imagery, shallow depth of field, real-world light.",
	"typographic": "Rendering: typography-driven design — the TEXT is the artwork; at most abstract shapes and one small paddle motif, no scenes.",
}

// PosterVibeKeys in display order.
func PosterVibeKeys() []string {
	return []string{"illustrated", "photo", "typographic"}
}

// PostersEnabled reports whether generation is configured at all.
func (s *Service) PostersEnabled() bool { return gateway.GeminiConfigured() }

// posterAllowed is the future fence, in one place.
//
// Today: anyone who may edit the event (owner, or admin of its club) may
// generate — founding access, everything free. When posters become the Club
// carrot, this is the single function that learns about credits/metering.
func (s *Service) posterAllowed(ev map[string]any, callerID string) bool {
	if asStr(ev, "owner_id") == callerID {
		return true
	}
	if clubID := strings.TrimSpace(asStr(ev, "club_id")); clubID != "" {
		return s.IsClubAdmin(clubID, callerID)
	}
	return false
}

// GeneratePoster renders a poster for an event and sets it as the event's
// poster_url. Returns the public URL.
//
// Synchronous by design: the model answers in 2-5s, which is a button with a
// spinner, not a job queue. The HTTP client allows 60s for the slow tail.
// [attach] decides whether the render becomes the event's poster.
//
// TRUE from the event's own Admin tab, where "make this event's poster" is
// literally the button's job. FALSE from the Poster Studio, which borrows an
// event only for its DETAILS (name, date, venue, divisions, club logo) — going
// there to try a style must not silently replace the poster an event is already
// advertising with. The studio shows the result and lets the organizer choose
// "Use on an event".
// [qr] adds a scannable code to the event's registration form, composited after
// generation — see poster_qr.go for why the model is never asked to draw it.
func (s *Service) GeneratePoster(
	eventID, callerID, style, layout, vibe, extra, custom string,
	logos []PosterLogo, attach, qr bool,
) (string, error) {
	if !s.PostersEnabled() {
		return "", errors.New(
			"posters aren't enabled yet — set GEMINI_API_KEY in Railway")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+
		"&select=id,name,owner_id,club_id,starts_at,venue_name,location,city,league_id,perpetual,"+
		"format,registration_fee_cents,currency,dupr_sanctioned")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	if !s.posterAllowed(ev, callerID) {
		return "", ErrForbidden
	}
	if err := s.requirePosterCredit(callerID); err != nil {
		return "", err
	}
	stylePrompt, err := composePosterDirection(style, layout, vibe, extra, custom)
	if err != nil {
		return "", err
	}

	prompt, logo := s.posterBrief(ev, stylePrompt)
	var refs [][]byte
	if len(logo) > 0 {
		refs = append(refs, logo)
	}
	if lrefs, counts, labels := fetchPosterLogos(logos); len(lrefs) > 0 {
		prompt += " " + logoFacts(counts, labels, len(refs))
		refs = append(refs, lrefs...)
	}
	// Ask for the corner BEFORE generating, so the art composes around the code
	// instead of having it punched through the middle of something.
	if qr {
		prompt += posterQRReservation
	}
	img, mime, err := gateway.GenerateImage(prompt, refs)
	if err != nil {
		log.Printf("poster: generate failed for %s: %v", eventID, err)
		return "", posterGenError(err)
	}
	if qr {
		// Best-effort: a poster without its QR still beats losing a paid
		// generation, so a failure here is logged and the image kept.
		withQR, qrMime, qerr := addPosterQR(img, mime, registrationURLFor(eventID))
		if qerr != nil {
			log.Printf("poster: QR skipped for %s: %v", eventID, qerr)
		} else {
			img, mime = withQR, qrMime
		}
	}
	ext := "png"
	if strings.Contains(mime, "jpeg") {
		ext = "jpg"
	}
	// A TIMESTAMPED path, not a fixed one: regenerating must not overwrite the
	// poster a TV or a share link is already showing until the new one is
	// actually chosen (poster_url flips atomically below), and CDN caches never
	// need invalidating for a URL that never changes content.
	path := fmt.Sprintf("%s/ai-%d.%s", eventID, time.Now().UTC().UnixNano(), ext)
	// The SAME bucket the manual poster upload uses (client kPosterBucket =
	// 'event-posters'), under an ai- prefix — it already exists, its RLS is
	// bypassed by the service key, and one bucket means one cleanup story.
	url, err := s.sb.StorageUpload("event-posters", path, mime, img)
	if err != nil {
		return "", fmt.Errorf("the poster generated but couldn't be saved: %w", err)
	}
	if attach {
		if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
			map[string]any{"poster_url": url}); err != nil {
			return "", err
		}
	}
	// Record BEFORE charging, and charge only if it stuck: a poster that isn't
	// in the gallery is one the organizer can't retrieve after a dropped
	// connection, and charging for that is the one outcome this must not produce.
	if s.RecordPosterGeneration(callerID, eventID, url, style) {
		s.spendPosterCredit(callerID)
	}
	log.Printf("poster: generated for %s (style=%s, %d bytes)",
		eventID, style, len(img))
	return url, nil
}

// composePosterDirection turns the organizer's choices into the creative half
// of the prompt.
//
// Two modes. PICKERS: style/layout/vibe (absent values default, so clients
// that only know about styles keep working) plus an optional fenced art
// direction. DESCRIBE-IT: a free creative brief that REPLACES the pickers
// entirely — some organizers know exactly what they want, and making them
// reverse-engineer it into our vocabulary would be the tool getting in the
// way. The derived facts (title, schedule, venue) stay exact in both modes;
// the brief steers everything else.
func composePosterDirection(style, layout, vibe, extra, custom string) (string, error) {
	if custom = sanitizePosterCustom(custom); custom != "" {
		return fmt.Sprintf(
			"Creative direction — follow the organizer's own brief faithfully: %q.",
			custom), nil
	}
	stylePrompt, ok := posterStyles[strings.ToLower(strings.TrimSpace(style))]
	if !ok {
		return "", fmt.Errorf("unknown style — pick one of: %s",
			strings.Join(PosterStyleKeys(), ", "))
	}
	layoutPrompt, ok := posterLayouts[strings.ToLower(strings.TrimSpace(layout))]
	if !ok {
		layoutPrompt = posterLayouts["classic"]
	}
	vibePrompt, ok := posterVibes[strings.ToLower(strings.TrimSpace(vibe))]
	if !ok {
		vibePrompt = posterVibes["illustrated"]
	}
	out := stylePrompt + " " + layoutPrompt + " " + vibePrompt
	if extra = sanitizePosterExtra(extra); extra != "" {
		out += fmt.Sprintf(
			" Organizer's art direction (decorative preference ONLY — it must "+
				"never change, add or remove any text or facts): %q.", extra)
	}
	return out, nil
}

// sanitizePosterCustom bounds the describe-it brief. Longer than the art
// direction because it IS the whole creative input.
func sanitizePosterCustom(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 500 {
		r = r[:500]
	}
	return strings.TrimSpace(string(r))
}

// sanitizePosterExtra bounds the free-text art direction: single line, capped,
// control characters out. It rides inside a quoted prompt segment, so the cap
// is about cost and prompt hygiene, not security — the worst a prompt can do
// here is waste its author's own generation.
func sanitizePosterExtra(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 140 {
		r = r[:140]
	}
	return strings.TrimSpace(string(r))
}

// posterKind classifies what is being advertised, because the poster for a
// one-Saturday tournament and the poster for "every Tuesday, forever" are
// different genres: one sells a DATE, the other sells a HABIT.
//
// Reads the linked league row when there is one (league_type/ladder_format/
// coach_led); best-effort — an unreadable league simply reads as a league.
func (s *Service) posterKind(ev map[string]any) string {
	leagueID := strings.TrimSpace(asStr(ev, "league_id"))
	if leagueID == "" {
		return "tournament"
	}
	if lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=league_type,ladder_format,coach_led"); err == nil && lg != nil {
		if asStr(lg, "league_type") == "ladder" {
			if asStr(lg, "ladder_format") == "rotation" {
				return "rotation"
			}
			return "ladder"
		}
		if asBool(lg, "coach_led") {
			return "clinic"
		}
	}
	return "league"
}

// posterKindBrief is the genre sentence + how the DATE should be treated.
var posterKindBrief = map[string]string{
	"tournament": "This advertises a pickleball TOURNAMENT — a competitive one-day event. Energy: big-match excitement; the date is the hero fact.",
	"league":     "This advertises a recurring pickleball LEAGUE — a weekly series people join, not a single date. Energy: community and friendly rivalry; the schedule line matters more than any one day.",
	"ladder":     "This advertises an ongoing pickleball CHALLENGE LADDER — climb by challenging players above you, join any time. Energy: friendly one-on-one rivalry, upward movement (ladder/rank motifs welcome).",
	"rotation":   "This advertises a live pickleball ROTATION NIGHT (king of the court) — show up, get seeded, winners move up courts all evening. Energy: fast, social, drop-in friendly.",
	"clinic":     "This advertises a COACH-LED pickleball session — instruction and drills, all levels welcome. Energy: welcoming and encouraging, learning over competition.",
}

// GenerateLeaguePoster renders a poster for a LEAGUE and sets leagues.
// poster_url. Ownership is enforced by the route (same as setLeaguePoster).
//
// A league is always the recurring genre, so the brief leads with the habit:
// the league's name and its home location. Session-level posters (a single
// night) go through the EVENT path — league sessions are event rows.
func (s *Service) GenerateLeaguePoster(
	leagueID, callerID, style, layout, vibe, extra, custom string,
) (string, error) {
	if !s.PostersEnabled() {
		return "", errors.New(
			"posters aren't enabled yet — set GEMINI_API_KEY in Railway")
	}
	if err := s.requirePosterCredit(callerID); err != nil {
		return "", err
	}
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(leagueID)+"&select=id,name,location")
	if err != nil {
		return "", err
	}
	if lg == nil {
		return "", ErrNotFound
	}
	direction, err := composePosterDirection(style, layout, vibe, extra, custom)
	if err != nil {
		return "", err
	}
	var facts []string
	facts = append(facts, posterKindBrief["league"])
	if name := strings.TrimSpace(asStr(lg, "name")); name != "" {
		facts = append(facts, fmt.Sprintf("The league name, rendered EXACTLY: %q.", name))
	}
	if loc := strings.TrimSpace(asStr(lg, "location")); loc != "" {
		facts = append(facts, fmt.Sprintf("The location, rendered EXACTLY: %q.", loc))
	}
	prompt := fmt.Sprintf(
		posterPromptTemplate,
		direction, strings.Join(facts, " "))
	img, mime, err := gateway.GenerateImage(prompt, nil)
	if err != nil {
		log.Printf("poster: league generate failed for %s: %v", leagueID, err)
		return "", posterGenError(err)
	}
	ext := "png"
	if strings.Contains(mime, "jpeg") {
		ext = "jpg"
	}
	path := fmt.Sprintf("league-%s/ai-%d.%s", leagueID, time.Now().UTC().UnixNano(), ext)
	url, err := s.sb.StorageUpload("event-posters", path, mime, img)
	if err != nil {
		return "", fmt.Errorf("the poster generated but couldn't be saved: %w", err)
	}
	if _, err := s.sb.Update("leagues", "id=eq."+store.Q(leagueID),
		map[string]any{"poster_url": url}); err != nil {
		return "", err
	}
	// Metered and recorded like every other render. The league path was neither:
	// with the meter on it was unlimited free Gemini spend for anyone who made a
	// league (which is free), and its renders never appeared in the gallery the
	// UI promises — so a replaced league poster also leaked its storage object
	// forever, since the sweep only walks recorded rows.
	if s.RecordPosterGeneration(callerID, "", url, style) {
		s.spendPosterCredit(callerID)
	}
	log.Printf("poster: generated for league %s (%d bytes)", leagueID, len(img))
	return url, nil
}

// posterPromptTemplate is the frame every poster prompt shares: %s is the
// creative direction, %s the list of facts.
//
// The rules below are the ones EARNED by looking at output, not guesses:
//   - ONCE — a venue holding its own city ("Chula Vista Elite…") plus a separate
//     city fact produced posters printing the city twice. The facts are already
//     de-duplicated upstream; this stops the model re-stating them anyway.
//   - NOTHING EXTRA — models love to invent a plausible time, price or slogan to
//     fill space, and an invented fact on a printed flyer is the one failure this
//     feature must never produce.
//   - HIERARCHY — without an explicit order everything competes at the same size
//     and the result reads as a menu.
//   - ROOM — "a poster, not a collage" was already here and does real work.
const posterPromptTemplate = "A portrait 3:4 pickleball event poster. Style: %s. " +
	"It must read instantly as PICKLEBALL — paddles and a pickleball, never " +
	"tennis rackets or tennis balls. %s " +
	"TEXT RULES, follow exactly: render ONLY the facts given, each ONE TIME — " +
	"never repeat the title, date, venue or city anywhere else on the poster. " +
	"Invent NOTHING: no extra dates, times, prices, phone numbers, URLs, hashtags " +
	"or slogans, and no lorem/placeholder text. Every word must be correctly " +
	"spelled and fully legible at thumbnail size. " +
	"HIERARCHY: the title is the largest element, then the date, then everything " +
	"else small and grouped together. Leave generous breathing room — a poster, " +
	"not a collage."

// posterBrief turns an event row into the model's brief, and fetches the
// club's logo bytes when there is one.
func (s *Service) posterBrief(ev map[string]any, stylePrompt string) (string, []byte) {
	name := strings.TrimSpace(asStr(ev, "name"))
	kind := s.posterKind(ev)
	var facts []string
	if kb, ok := posterKindBrief[kind]; ok {
		facts = append(facts, kb)
	}
	if name != "" {
		facts = append(facts, fmt.Sprintf("The event title, rendered EXACTLY: %q.", name))
	}
	if raw := strings.TrimSpace(asStr(ev, "starts_at")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			// starts_at is UTC; rendered as-is, a 6 PM Pacific Tuesday becomes
			// "Wednesday 1:00 AM" ON THE POSTER — a wrong fact in print, which
			// is the one thing this feature must never produce. Anchored to
			// Pacific (same call as jokeDay) until events carry a timezone.
			if loc, lerr := time.LoadLocation("America/Los_Angeles"); lerr == nil {
				t = t.In(loc)
			}
			if asBool(ev, "perpetual") || kind == "ladder" || kind == "rotation" {
				// A recurring thing sells its CADENCE. For a perpetual league,
				// StartsAt's weekday+time IS the weekly slot (reschedule moves
				// it), so "Every Tuesday · 6:00 PM" is the true fact — a single
				// calendar date on this poster would be wrong within a week.
				facts = append(facts, fmt.Sprintf(
					"The schedule, rendered EXACTLY: %q.",
					t.Format("Every Monday · 3:04 PM")))
			} else if t.Hour() != 0 || t.Minute() != 0 {
				facts = append(facts, fmt.Sprintf(
					"The date and start time, rendered EXACTLY: %q.",
					t.Format("Monday, January 2 · 3:04 PM")))
			} else {
				facts = append(facts,
					fmt.Sprintf("The date, rendered EXACTLY: %q.", t.Format("Monday, January 2")))
			}
		}
	}
	venue := strings.TrimSpace(asStr(ev, "venue_name"))
	if venue == "" {
		// Falling back to `location` means a FULL POSTAL ADDRESS — "2800 Olympic
		// Parkway, Chula Vista, CA 91915, United States of America" is not poster
		// copy, and asking for it rendered EXACTLY forces the model to letter the
		// whole thing. Take the place name (everything before the first comma),
		// which is what a poster actually names.
		loc := strings.TrimSpace(asStr(ev, "location"))
		if i := strings.Index(loc, ","); i > 0 {
			loc = strings.TrimSpace(loc[:i])
		}
		venue = loc
	}
	if venue != "" {
		facts = append(facts, fmt.Sprintf("The venue, rendered EXACTLY: %q.", venue))
	}
	// City only when the venue doesn't ALREADY say it. The old test was
	// equality, so a venue of "Chula Vista Elite Athlete Training Center" (or a
	// full address containing the city) still added "Chula Vista" as a separate
	// fact — and the model dutifully printed the city twice.
	if city := strings.TrimSpace(asStr(ev, "city")); city != "" &&
		!strings.Contains(strings.ToLower(venue), strings.ToLower(city)) {
		facts = append(facts, fmt.Sprintf("City: %q.", city))
	}

	// Divisions: "who can play" is the first question a poster answers after
	// "when". Included even when there's only ONE — a single "3.5 & under" or
	// "Open" division is exactly the fact a player needs, and gating this on
	// len > 1 meant most events printed no division at all. Capped at 6 so a
	// mega-draw doesn't turn the design into a spreadsheet. Skipped only when
	// the sole division just repeats the event's own name.
	divisionLine := ""
	if id := strings.TrimSpace(asStr(ev, "id")); id != "" {
		if bks, err := s.GetBrackets(id); err == nil && len(bks) > 0 {
			names := make([]string, 0, 6)
			for _, b := range bks {
				if n := strings.TrimSpace(b.Name); n != "" {
					names = append(names, n)
					if len(names) == 6 {
						break
					}
				}
			}
			if len(names) == 1 && strings.EqualFold(names[0], name) {
				names = nil // "Summer Slam / Summer Slam" says nothing twice
			}
			divisionLine = strings.Join(names, " · ")
			if len(names) == 1 {
				facts = append(facts, fmt.Sprintf(
					"The division, rendered EXACTLY: %q.", names[0]))
			} else if len(names) > 1 {
				facts = append(facts, fmt.Sprintf(
					"The divisions, rendered EXACTLY as a compact list: %q.",
					divisionLine))
			}
		}
	}

	// Singles vs doubles — one word, and the first thing a player checks after
	// the date. SKIPPED when the division names already say it: "Men's Doubles
	// 3.0 · Mixed Doubles 3.5" followed by "Play format: Doubles" letters the
	// same fact twice, which is the duplicate-city bug in a new outfit. The
	// facts are de-duplicated BEFORE the model sees them — a brief that repeats
	// itself gets a poster that repeats itself.
	format := strings.ToLower(strings.TrimSpace(asStr(ev, "format")))
	if format == "singles" || format == "doubles" {
		if !strings.Contains(strings.ToLower(divisionLine), format) {
			facts = append(facts, fmt.Sprintf(
				"Play format, rendered EXACTLY: %q.",
				strings.ToUpper(format[:1])+format[1:]))
		}
	}

	// Entry fee. A poster that doesn't say the price makes everyone ask, and
	// "Free" is worth shouting when it's true.
	if cents := asInt(ev, "registration_fee_cents"); cents > 0 {
		cur := strings.ToUpper(strings.TrimSpace(asStr(ev, "currency")))
		sym := map[string]string{"USD": "$", "CAD": "$", "AUD": "$",
			"GBP": "£", "EUR": "€", "PHP": "₱"}[cur]
		amount := ""
		if cents%100 == 0 {
			amount = fmt.Sprintf("%s%d", sym, cents/100)
		} else {
			amount = fmt.Sprintf("%s%.2f", sym, float64(cents)/100)
		}
		if sym == "" {
			amount = fmt.Sprintf("%.2f %s", float64(cents)/100, cur)
		}
		facts = append(facts, fmt.Sprintf(
			"The entry fee, rendered EXACTLY: %q.", amount+" entry"))
	}

	// DUPR-sanctioned is a credential players actively look for.
	if asBool(ev, "dupr_sanctioned") {
		facts = append(facts,
			`Include a small badge or line reading EXACTLY "DUPR RATED".`)
	}

	var logo []byte
	clubName := ""
	if clubID := strings.TrimSpace(asStr(ev, "club_id")); clubID != "" {
		if c, err := s.sb.SelectOne("clubs",
			"id=eq."+store.Q(clubID)+"&select=name,logo_url"); err == nil && c != nil {
			clubName = strings.TrimSpace(asStr(c, "name"))
			if lu := strings.TrimSpace(asStr(c, "logo_url")); lu != "" {
				logo = fetchSmallImage(lu)
			}
		}
	}
	if clubName != "" {
		facts = append(facts, fmt.Sprintf("Presented by %q.", clubName))
	}
	if len(logo) > 0 {
		facts = append(facts,
			"Incorporate the attached club logo tastefully (small, e.g. a corner "+
				"badge) — do not redraw or distort it.")
	}

	return fmt.Sprintf(
		posterPromptTemplate,
		stylePrompt, strings.Join(facts, " ")), logo
}

// --- Poster STUDIO: generate without an event ---
//
// The event path derives the whole brief from a real event row. The studio path
// is the opposite: nothing is derived, everything is typed. It exists because
// the first question the Tools entry used to ask — "which event is this for?" —
// is the wrong question. An organizer wants to make a poster, play with styles,
// and only then decide what it's for (or make one for something that isn't in
// the app at all, like a flyer for open play). So the studio takes an optional
// title/date/venue as FREE TEXT, never touches any event's poster_url, and drops
// the result in the caller's gallery. Attaching to an event is a separate,
// explicit step (SetEventPoster with the returned url).

// PosterItem is one row in a user's poster gallery.
type PosterItem struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Style     string `json:"style,omitempty"`
	EventID   string `json:"eventId,omitempty"`
	EventName string `json:"eventName,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// posterGenReady reports whether the gallery table has been migrated in. Until
// it is, generation still works — it just doesn't record a gallery row.
func (s *Service) posterGenReady() bool {
	return s.columnReady("poster_generations", "id")
}

// studioPosterPath returns the storage path a studio render is saved under.
// Namespaced by user so the gallery sweep and any future per-user quota have a
// clean prefix to work with; timestamped so a regenerate never clobbers.
func studioPosterPath(userID, ext string) string {
	return fmt.Sprintf("studio/%s/ai-%d.%s", userID, time.Now().UTC().UnixNano(), ext)
}

// GenerateStudioPoster renders a poster from typed-in details, with NO event
// behind it. Returns the public URL; the caller decides what to do with it.
// [qrURL] adds a QR to that link. There is no event here to derive one from, so
// the studio asks for it — validated BEFORE the model runs, because a rejected
// link after a paid render would be the worst possible moment to say no.
func (s *Service) GenerateStudioPoster(
	callerID, title, dateText, venueText, style, layout, vibe, extra, custom string,
	logos []PosterLogo, qrURL string,
) (string, error) {
	if !s.PostersEnabled() {
		return "", errors.New(
			"posters aren't enabled yet — set GEMINI_API_KEY in Railway")
	}
	qrLink := ""
	if strings.TrimSpace(qrURL) != "" {
		var qerr error
		if qrLink, qerr = normalizePosterQRURL(qrURL); qerr != nil {
			return "", qerr // before the credit is spent
		}
	}
	if err := s.requirePosterCredit(callerID); err != nil {
		return "", err
	}
	direction, err := composePosterDirection(style, layout, vibe, extra, custom)
	if err != nil {
		return "", err
	}
	prompt := studioBrief(title, dateText, venueText, direction)
	var refs [][]byte
	if lrefs, counts, labels := fetchPosterLogos(logos); len(lrefs) > 0 {
		prompt += " " + logoFacts(counts, labels, 0)
		refs = lrefs
	}
	if qrLink != "" {
		// No caption: the link is whatever they typed, so telling the model to
		// letter "SCAN TO REGISTER" could label a venue map as a sign-up form.
		prompt += posterQRReservationPlain
	}
	img, mime, err := gateway.GenerateImage(prompt, refs)
	if err != nil {
		log.Printf("poster: studio generate failed for %s: %v", callerID, err)
		return "", posterGenError(err)
	}
	if qrLink != "" {
		withQR, qrMime, qerr := addPosterQR(img, mime, qrLink)
		if qerr != nil {
			log.Printf("poster: studio QR skipped for %s: %v", callerID, qerr)
		} else {
			img, mime = withQR, qrMime
		}
	}
	ext := "png"
	if strings.Contains(mime, "jpeg") {
		ext = "jpg"
	}
	url, err := s.sb.StorageUpload("event-posters",
		studioPosterPath(callerID, ext), mime, img)
	if err != nil {
		return "", fmt.Errorf("the poster generated but couldn't be saved: %w", err)
	}
	if s.RecordPosterGeneration(callerID, "", url, style) {
		s.spendPosterCredit(callerID)
	}
	log.Printf("poster: studio render for %s (style=%s, %d bytes)",
		callerID, style, len(img))
	return url, nil
}

// studioBrief builds the model brief from free-typed fields. Every field is
// optional: a poster with only a title is fine, and the creative direction
// carries the rest. The RENDERED EXACTLY framing is the same discipline the
// event path uses — the model must not invent or alter the words it's given.
func studioBrief(title, dateText, venueText, direction string) string {
	var facts []string
	if t := strings.TrimSpace(oneLine(title, 90)); t != "" {
		facts = append(facts, fmt.Sprintf("The title, rendered EXACTLY: %q.", t))
	}
	if d := strings.TrimSpace(oneLine(dateText, 90)); d != "" {
		facts = append(facts, fmt.Sprintf("The date/time line, rendered EXACTLY: %q.", d))
	}
	if v := strings.TrimSpace(oneLine(venueText, 90)); v != "" {
		facts = append(facts, fmt.Sprintf("The venue/location, rendered EXACTLY: %q.", v))
	}
	joined := strings.Join(facts, " ")
	if joined == "" {
		// Nothing typed at all — still render something usable rather than erroring,
		// so playing with styles before writing any copy works.
		joined = "No fixed text was given; use tasteful placeholder-free composition " +
			"with room for a title, and add no invented specifics."
	}
	return fmt.Sprintf(
		posterPromptTemplate, direction, joined)
}

// oneLine collapses whitespace and caps length — the studio fields are single
// lines that ride inside a quoted prompt segment.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

// RecordPosterGeneration logs a render into the caller's gallery. Best-effort:
// a failed insert must never fail the generation — the poster already exists.
// eventID is "" for studio renders. Inert until the table is migrated in.
// Returns true when the render is now DISCOVERABLE — either recorded in the
// gallery, or the gallery isn't in use at all. The caller charges only on true,
// so a credit is never taken for a poster the organizer can't find afterwards.
func (s *Service) RecordPosterGeneration(userID, eventID, url, style string) bool {
	if strings.TrimSpace(url) == "" {
		return false
	}
	if !s.posterGenReady() {
		// No gallery to record into; the URL still came back in the response, so
		// this isn't a lost poster — just an unrecorded one.
		return true
	}
	row := map[string]any{
		"user_id": userID,
		"url":     url,
		"style":   strings.TrimSpace(style),
	}
	if strings.TrimSpace(eventID) != "" {
		row["event_id"] = eventID
	}
	if _, err := s.sb.Insert("poster_generations", row); err != nil {
		log.Printf("poster: gallery record FAILED for %s — not charging, the "+
			"organizer would have no way to find this poster: %v", userID, err)
		return false
	}
	return true
}

// MyPosters returns the caller's recent poster renders, newest first. Capped —
// a gallery is a recent-work strip, not an archive (and 30-day retention keeps
// it naturally short).
func (s *Service) MyPosters(callerID string) ([]PosterItem, error) {
	if !s.posterGenReady() {
		return []PosterItem{}, nil
	}
	rows, err := s.sb.Select("poster_generations",
		"user_id=eq."+store.Q(callerID)+
			"&select=id,url,style,event_id,created_at"+
			"&order=created_at.desc&limit=60")
	if err != nil {
		return nil, err
	}
	out := make([]PosterItem, 0, len(rows))
	eventIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		it := PosterItem{
			ID:        asStr(r, "id"),
			URL:       asStr(r, "url"),
			Style:     asStr(r, "style"),
			EventID:   asStr(r, "event_id"),
			CreatedAt: asStr(r, "created_at"),
		}
		if it.URL == "" {
			continue
		}
		if it.EventID != "" {
			eventIDs = append(eventIDs, it.EventID)
		}
		out = append(out, it)
	}
	// Name the events in ONE query so the gallery can say "for Summer Slam".
	if len(eventIDs) > 0 {
		if evs, err := s.sb.SelectAll("events",
			"id="+store.In(eventIDs)+"&select=id,name"); err == nil {
			names := map[string]string{}
			for _, e := range evs {
				names[asStr(e, "id")] = strings.TrimSpace(asStr(e, "name"))
			}
			for i := range out {
				out[i].EventName = names[out[i].EventID]
			}
		}
	}
	return out, nil
}

// SweepOldPosters deletes gallery renders older than posterRetention, along with
// their storage objects — EXCEPT any whose URL is still an event's or league's
// active poster. A poster you actually used is showing on a real page (TV, share
// link, public event page); sweeping it would 404 that page. The throwaway tries
// you never used get cleaned up. Idempotent and bounded per pass.
func (s *Service) SweepOldPosters() error {
	if !s.posterGenReady() {
		return nil
	}
	cutoff := time.Now().Add(-posterRetention).UTC().Format(time.RFC3339)
	nowISO := time.Now().UTC().Format(time.RFC3339)
	// A poster that is (or recently was) an event's own poster gets a rolling
	// grace period, refreshed every time the sweep sees it in use. Without it,
	// detaching a 40-day-old poster — swapping a banner to compare, say — deleted
	// it within the hour, and re-attaching from a stale gallery view left the
	// event showing a 404. Now it survives 30 more days after it stops being used.
	graceUntil := time.Now().Add(posterRetention).UTC().Format(time.RFC3339)
	var swept, kept int
	// PAGE past the rows we keep. Rows that stay (still someone's active poster)
	// are never deleted, so they'd otherwise sit at the front of an
	// order-by-oldest window forever and hide every newer expired row behind
	// them — the sweep would go quiet while storage kept growing. Skipping the
	// running count of kept rows walks the cursor past them; deleted rows are
	// gone, so nothing is skipped twice.
	for page := 0; page < posterSweepMaxPages; page++ {
		rows, err := s.sb.Select("poster_generations",
			"created_at=lt."+store.Q(cutoff)+
				"&or=(protected_until.is.null,protected_until.lt."+
				store.Q(nowISO)+")"+
				"&select=id,url,protected_until&order=created_at.asc"+
				"&limit="+strconv.Itoa(posterSweepPage)+
				"&offset="+strconv.Itoa(kept))
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		inUse, err := s.postersStillInUse(rows)
		if err != nil {
			return err
		}
		for _, r := range rows {
			id := asStr(r, "id")
			url := asStr(r, "url")
			if inUse[url] {
				kept++
				// Refresh the grace window while it's in use, so the clock starts
				// at DETACH rather than at creation.
				if s.columnReady("poster_generations", "protected_until") {
					if _, uerr := s.sb.Update("poster_generations",
						"id=eq."+store.Q(id),
						map[string]any{"protected_until": graceUntil}); uerr != nil {
						log.Printf("poster sweep: could not extend grace for %s: %v",
							id, uerr)
					}
				}
				continue // still an active poster — leave both object and row
			}
			if bucket, path, ok := storagePathFromPublicURL(url); ok {
				if derr := s.sb.StorageDelete(bucket, path); derr != nil {
					log.Printf("poster sweep: storage delete failed for %s: %v", url, derr)
					kept++ // keep the row (and its place) so a later pass retries
					continue
				}
			}
			if derr := s.sb.Delete("poster_generations", "id=eq."+store.Q(id)); derr != nil {
				log.Printf("poster sweep: row delete failed for %s: %v", id, derr)
				kept++
				continue
			}
			swept++
		}
		if len(rows) < posterSweepPage {
			break // last page
		}
	}
	if swept > 0 || kept > 0 {
		log.Printf("poster sweep: removed %d, kept %d still-in-use", swept, kept)
	}
	return nil
}

// posterSweepPage is how many expired rows one page examines, and
// posterSweepMaxPages bounds a single hourly pass so a huge backlog is drained
// over several passes instead of one very long one.
const (
	posterSweepPage     = 100
	posterSweepMaxPages = 5
)

// posterRetention is how long a generated poster survives in the gallery before
// the sweep may remove it — unless it's in use somewhere, in which case it stays
// for as long as it's referenced.
const posterRetention = 30 * 24 * time.Hour

// postersStillInUse reports which of these poster URLs are an active event or
// league poster right now, in two batched queries.
func (s *Service) postersStillInUse(rows []map[string]any) (map[string]bool, error) {
	urls := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		u := asStr(r, "url")
		if u != "" && !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	inUse := map[string]bool{}
	if len(urls) == 0 {
		return inUse, nil
	}
	// CHUNKED, because these filter values are whole URLs, not ids. Each one is
	// ~150-170 bytes once quoted and percent-encoded, so a few dozen in a single
	// `in.(...)` blows past the gateway's request-line limit — the query 414s,
	// the sweep returns early having deleted nothing, and since the backlog only
	// grows the failure repeats forever. Small batches keep every request short.
	for start := 0; start < len(urls); start += posterInUseChunk {
		end := start + posterInUseChunk
		if end > len(urls) {
			end = len(urls)
		}
		batch := urls[start:end]
		for _, table := range []string{"events", "leagues"} {
			rows, err := s.sb.SelectAll(table,
				"poster_url="+store.In(batch)+"&select=poster_url")
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				inUse[asStr(r, "poster_url")] = true
			}
		}
	}
	return inUse, nil
}

// posterInUseChunk is how many poster URLs ride in one `in.(...)` filter. Sized
// so the encoded request line stays well under an 8 KB gateway limit.
const posterInUseChunk = 20

// storagePathFromPublicURL splits a Supabase public object URL back into
// (bucket, path). Returns ok=false for anything that isn't one, so a manually
// entered or externally hosted poster_url is never mistaken for a deletable
// object.
func storagePathFromPublicURL(url string) (bucket, path string, ok bool) {
	const marker = "/storage/v1/object/public/"
	i := strings.Index(url, marker)
	if i < 0 {
		return "", "", false
	}
	rest := url[i+len(marker):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash == len(rest)-1 {
		return "", "", false
	}
	return rest[:slash], rest[slash+1:], true
}

// PosterLogo is one organizer-attached image and what it IS — because "where a
// logo belongs" depends entirely on which kind it is: the event's main mark is
// part of the identity, team logos are the cast, sponsors are the credits.
type PosterLogo struct {
	URL  string `json:"url"`
	Role string `json:"role"` // main | team | sponsor | other
	// Label is the organizer's own words for what this is ("Host club",
	// "Charity partner"). For the three canonical roles it's optional; for
	// role=other it's the whole meaning, quoted to the model verbatim.
	Label string `json:"label"`
}

// posterGenError turns a Gemini failure into something the organizer can act on.
//
// Every failure used to read "could not generate the poster — try again, or a
// different style", which is wrong advice for most of them: a different style
// doesn't help when the account is rate-limited or the model is overloaded, and
// the real cause sat only in the server log. The categories below are the ones
// that actually occur, and each says what to DO. The underlying error is still
// logged in full — this only decides what a person is told.
func posterGenError(err error) error {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "429"),
		strings.Contains(s, "resource_exhausted"),
		strings.Contains(s, "quota"),
		strings.Contains(s, "rate limit"):
		return errors.New("the poster service is at its limit right now — " +
			"wait a minute and try again")
	case strings.Contains(s, "503"),
		strings.Contains(s, "unavailable"),
		strings.Contains(s, "overloaded"),
		strings.Contains(s, "500"):
		return errors.New("the poster service is busy — try again in a moment")
	case strings.Contains(s, "deadline"),
		strings.Contains(s, "timeout"),
		strings.Contains(s, "context canceled"):
		return errors.New("that took too long to draw — try again, or a " +
			"simpler style")
	case strings.Contains(s, "safety"),
		strings.Contains(s, "blocked"),
		strings.Contains(s, "prohibited"):
		return errors.New("the image service declined that request — try " +
			"different wording or a different style")
	case strings.Contains(s, "api key"),
		strings.Contains(s, "401"), strings.Contains(s, "403"):
		return errors.New("posters aren't configured correctly — check " +
			"GEMINI_API_KEY")
	}
	return errors.New("could not generate the poster — try again, or a " +
		"different style")
}

// fetchPosterLogos downloads the attached logos (max 5 total), grouped by role
// and in a STABLE order (main, then team, then sponsor) so the prompt can name
// attachment positions. Best-effort per logo — a bad URL skips that one, never
// fails the poster. Same 5MB/PNG-JPEG discipline as the club logo.
func fetchPosterLogos(logos []PosterLogo) (refs [][]byte, counts [4]int, labels []string) {
	bucket := func(l PosterLogo) int {
		switch strings.ToLower(strings.TrimSpace(l.Role)) {
		case "main":
			return 0
		case "team":
			return 1
		case "sponsor":
			return 2
		default:
			return 3 // organizer's own kind — carried by its label
		}
	}
	for gi := 0; gi < 4; gi++ {
		for _, l := range logos {
			if bucket(l) != gi {
				continue
			}
			if len(refs) == 5 {
				return
			}
			if img := fetchSmallImage(strings.TrimSpace(l.URL)); len(img) > 0 {
				refs = append(refs, img)
				counts[gi]++
				if gi == 3 {
					labels = append(labels, oneLine(l.Label, 40))
				}
			}
		}
	}
	return
}

// logoFacts writes the prompt lines for the attached logos. [offset] is how
// many reference images precede them (the club logo, when present). Each group
// gets its OWN placement: the main logo is identity and sits with the title;
// team logos are the cast and get a mid-poster band; sponsors are credits in a
// small bottom row. All of them INSIDE the artwork — the first version said
// "along the bottom edge" and the model dutifully appended a separate white
// strip below the poster, logos floating outside the design.
func logoFacts(counts [4]int, labels []string, offset int) string {
	span := func(n int) string {
		start := offset + 1
		if n == 1 {
			return fmt.Sprintf("attached image %d", start)
		}
		return fmt.Sprintf("attached images %d–%d", start, start+n-1)
	}
	var parts []string
	if n := counts[0]; n > 0 {
		parts = append(parts, fmt.Sprintf(
			"MAIN LOGO: %s is the event's own logo — feature it prominently near "+
				"the title as part of the poster's identity.", span(n)))
		offset += n
	}
	if n := counts[1]; n > 0 {
		parts = append(parts, fmt.Sprintf(
			"TEAMS: %s are %d DIFFERENT participating team logos — place each "+
				"one exactly once, together in a medium-sized featured band "+
				"within the artwork.", span(n), n))
		offset += n
	}
	if n := counts[2]; n > 0 {
		// State the COUNT and place it accordingly. "An evenly spaced row of
		// credits" reads to the model as a row that wants filling, so a single
		// sponsor logo came back tiled five times across the bottom.
		if n == 1 {
			parts = append(parts, fmt.Sprintf(
				"SPONSOR: %s is a single sponsor logo — place it ONCE, small, in "+
					"the lower third, sitting ON the poster's own background as "+
					"part of the design. There is exactly one sponsor: do not "+
					"repeat it and do not pad the space with copies of it. Do "+
					"NOT give it a band, bar, footer, panel or strip of its "+
					"own, and do not change the poster's background behind it.",
				span(n)))
		} else {
			parts = append(parts, fmt.Sprintf(
				"SPONSORS: %s are %d DIFFERENT sponsor logos — place each one "+
					"exactly once, small, spaced across the lower third and "+
					"sitting ON the poster's own background as part of the "+
					"design. Do NOT give them a band, bar, footer, panel or "+
					"strip of their own.", span(n), n))
		}
		offset += n
	}
	// Organizer-named kinds: each gets its own line with the organizer's words,
	// and the model places it where the design suits — the label carries the
	// meaning ("Host club" reads differently from "Charity partner").
	for i := 0; i < counts[3]; i++ {
		label := ""
		if i < len(labels) {
			label = labels[i]
		}
		if label == "" {
			label = "partner"
		}
		parts = append(parts, fmt.Sprintf(
			"attached image %d is the %q logo — integrate it tastefully into the "+
				"artwork at a modest size where the design suits.",
			offset+1, label))
		offset++
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " Every attached logo must be composited " +
		"INTO the poster's own artwork and background — never on a separate " +
		"plain strip outside or below the design. Each attached logo appears " +
		"EXACTLY ONCE in the finished poster. Never duplicate, tile, mirror or " +
		"repeat a logo to fill a row or balance the layout; if a space looks " +
		"empty, leave it empty. THE LOGO ARTWORK ITSELF IS UNTOUCHABLE: " +
		"reproduce every logo pixel-faithfully and in full — never redraw, " +
		"restyle, recolor, tint, crop, stretch, rotate, add or remove text, " +
		"change its typeface, or apply the poster's texture, grain, filter or " +
		"distressing to it. Scale it proportionally and nothing more. Only its " +
		"SIZE and POSITION may change; everything inside its edges stays " +
		"exactly as supplied."
}

// fetchSmallImage downloads a small public asset (the club logo), bounded so a
// misconfigured URL can't stream something huge into memory. Best-effort: a
// missing logo means a poster without one, not a failed poster.
func fetchSmallImage(url string) []byte {
	if !strings.HasPrefix(url, "https://") {
		return nil
	}
	resp, err := posterAssetHTTP.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	// Read ONE BYTE past the cap so oversize is DETECTED, not truncated.
	// SetClubLogo accepts up to 5 MB; a silently-truncated PNG is undecodable,
	// and sending it to the model fails the WHOLE generation — for every event
	// under that club, forever, with no hint why. Best-effort means a too-big
	// or non-image logo becomes a poster without a logo, never a failed poster.
	const cap = 5 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, cap+1))
	if err != nil || len(data) > cap {
		return nil
	}
	// Only hand the model something that is actually an image: PNG or JPEG
	// magic bytes. A storage-proxy error page with a 200 status is neither.
	if len(data) < 4 {
		return nil
	}
	isPNG := data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G'
	isJPG := data[0] == 0xFF && data[1] == 0xD8
	if !isPNG && !isJPG {
		return nil
	}
	return data
}

var posterAssetHTTP = &http.Client{Timeout: 10 * time.Second}
