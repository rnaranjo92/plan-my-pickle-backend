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
func (s *Service) GeneratePoster(eventID, callerID, style string) (string, error) {
	if !s.PostersEnabled() {
		return "", errors.New(
			"posters aren't enabled yet — set GEMINI_API_KEY in Railway")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+
		"&select=id,name,owner_id,club_id,starts_at,venue_name,location,city")
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

// posterBrief turns an event row into the model's brief, and fetches the
// club's logo bytes when there is one.
func (s *Service) posterBrief(ev map[string]any, stylePrompt string) (string, []byte) {
	name := strings.TrimSpace(asStr(ev, "name"))
	var facts []string
	if name != "" {
		facts = append(facts, fmt.Sprintf("The event title, rendered EXACTLY: %q.", name))
	}
	if raw := strings.TrimSpace(asStr(ev, "starts_at")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			facts = append(facts,
				fmt.Sprintf("The date, rendered EXACTLY: %q.", t.Format("Monday, January 2")))
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
