package service

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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
	"clean":  "clean modern sports poster, bold flat colors, strong grid layout, generous whitespace",
	"retro":  "retro athletic poster, distressed textures, vintage varsity typography, 1970s sports-program palette",
	"neon":   "night-league poster, neon glow on deep navy, dramatic court floodlights, electric energy",
	"paper":  "hand-made community flyer, risograph print texture, two-ink palette, friendly and local",
	"epic":   "cinematic tournament poster, dramatic low-angle court photography style, gold and navy, championship gravitas",
	"tropic": "sunny outdoor tournament poster, palm shadows, warm gradients, beach-town summer energy",
}

// PosterStyleKeys lists the picker's options in a stable order.
func PosterStyleKeys() []string {
	return []string{"clean", "retro", "neon", "paper", "epic", "tropic"}
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
func (s *Service) GeneratePoster(
	eventID, callerID, style, layout, vibe, extra string,
) (string, error) {
	if !s.PostersEnabled() {
		return "", errors.New(
			"posters aren't enabled yet — set GEMINI_API_KEY in Railway")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+
		"&select=id,name,owner_id,club_id,starts_at,venue_name,location,city,league_id,perpetual")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	if !s.posterAllowed(ev, callerID) {
		return "", ErrForbidden
	}
	stylePrompt, ok := posterStyles[strings.ToLower(strings.TrimSpace(style))]
	if !ok {
		return "", fmt.Errorf("unknown style — pick one of: %s",
			strings.Join(PosterStyleKeys(), ", "))
	}
	// Layout and vibe DEFAULT rather than error when absent: clients that only
	// know about styles (the already-patched apps) keep working unchanged.
	layoutPrompt, ok := posterLayouts[strings.ToLower(strings.TrimSpace(layout))]
	if !ok {
		layoutPrompt = posterLayouts["classic"]
	}
	vibePrompt, ok := posterVibes[strings.ToLower(strings.TrimSpace(vibe))]
	if !ok {
		vibePrompt = posterVibes["illustrated"]
	}
	stylePrompt = stylePrompt + " " + layoutPrompt + " " + vibePrompt
	if extra = sanitizePosterExtra(extra); extra != "" {
		// The organizer's own art direction, fenced: it steers decoration, and
		// is explicitly barred from changing the facts — the one thing the
		// derived brief guarantees.
		stylePrompt += fmt.Sprintf(
			" Organizer's art direction (decorative preference ONLY — it must "+
				"never change, add or remove any text or facts): %q.", extra)
	}

	prompt, logo := s.posterBrief(ev, stylePrompt)
	var refs [][]byte
	if len(logo) > 0 {
		refs = append(refs, logo)
	}
	img, mime, err := gateway.GenerateImage(prompt, refs)
	if err != nil {
		log.Printf("poster: generate failed for %s: %v", eventID, err)
		return "", errors.New("could not generate the poster — try again, " +
			"or a different style")
	}
	ext := "png"
	if strings.Contains(mime, "jpeg") {
		ext = "jpg"
	}
	// A TIMESTAMPED path, not a fixed one: regenerating must not overwrite the
	// poster a TV or a share link is already showing until the new one is
	// actually chosen (poster_url flips atomically below), and CDN caches never
	// need invalidating for a URL that never changes content.
	path := fmt.Sprintf("%s/ai-%d.%s", eventID, time.Now().UTC().Unix(), ext)
	// The SAME bucket the manual poster upload uses (client kPosterBucket =
	// 'event-posters'), under an ai- prefix — it already exists, its RLS is
	// bypassed by the service key, and one bucket means one cleanup story.
	url, err := s.sb.StorageUpload("event-posters", path, mime, img)
	if err != nil {
		return "", fmt.Errorf("the poster generated but couldn't be saved: %w", err)
	}
	if _, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"poster_url": url}); err != nil {
		return "", err
	}
	log.Printf("poster: generated for %s (style=%s, %d bytes)",
		eventID, style, len(img))
	return url, nil
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
			} else {
				facts = append(facts,
					fmt.Sprintf("The date, rendered EXACTLY: %q.", t.Format("Monday, January 2")))
			}
		}
	}
	venue := strings.TrimSpace(asStr(ev, "venue_name"))
	if venue == "" {
		venue = strings.TrimSpace(asStr(ev, "location"))
	}
	if venue != "" {
		facts = append(facts, fmt.Sprintf("The venue, rendered EXACTLY: %q.", venue))
	}
	if city := strings.TrimSpace(asStr(ev, "city")); city != "" &&
		!strings.EqualFold(city, venue) {
		facts = append(facts, fmt.Sprintf("City: %q.", city))
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
		"A portrait 3:4 pickleball event poster. Style: %s. "+
			"It must read instantly as PICKLEBALL — paddles and a pickleball, "+
			"never tennis rackets or tennis balls. %s "+
			"All text must be crisp, correctly spelled, and limited to the facts "+
			"given — no invented dates, prices or slogans. Leave breathing room; "+
			"a poster, not a collage.",
		stylePrompt, strings.Join(facts, " ")), logo
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	return data
}

var posterAssetHTTP = &http.Client{Timeout: 10 * time.Second}
