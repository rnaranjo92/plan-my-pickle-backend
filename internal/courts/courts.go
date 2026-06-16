// Package courts finds nearby pickleball courts. The default implementation
// queries OpenStreetMap's Overpass API (free, keyless, ODbL). A Google Places
// implementation (paid, server-side key) can satisfy the same Finder interface.
package courts

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Court struct {
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Address string  `json:"address,omitempty"`
	Phone   string  `json:"phone,omitempty"`
	Website string  `json:"website,omitempty"`
	Source  string  `json:"source"`
}

type Finder interface {
	Nearby(lat, lng, radiusKm float64) ([]Court, error)
}

// OverpassFinder queries OSM Overpass for `sport=pickleball` features.
type OverpassFinder struct {
	Endpoint string
	Client   *http.Client
}

func NewOverpassFinder() *OverpassFinder {
	// Override with PMP_OVERPASS_URL to use a mirror if the main server is busy,
	// e.g. https://overpass.kumi.systems/api/interpreter
	ep := os.Getenv("PMP_OVERPASS_URL")
	if ep == "" {
		ep = "https://overpass-api.de/api/interpreter"
	}
	return &OverpassFinder{
		Endpoint: ep,
		Client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (f *OverpassFinder) Nearby(lat, lng, radiusKm float64) ([]Court, error) {
	if radiusKm <= 0 {
		radiusKm = 25
	}
	meters := int(radiusKm * 1000)
	// nwr = nodes+ways+relations; regex (case-insensitive) catches "pickleball",
	// "Pickleball", and multi-sport values like "tennis;pickleball" — far more
	// than an exact match. "out center tags" yields a center for ways/relations.
	q := fmt.Sprintf(
		`[out:json][timeout:25];nwr["sport"~"pickleball"](around:%d,%f,%f);out center 200;`,
		meters, lat, lng)

	// Build the request explicitly: Overpass's Apache front-end returns 406 for
	// requests with no Accept header / a generic user-agent (Go's default).
	req, err := http.NewRequest(http.MethodPost, f.Endpoint,
		strings.NewReader(url.Values{"data": {q}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PlanMyPickle/1.0 (+https://planmypickle.com)")

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("overpass %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Elements []struct {
			Lat    float64 `json:"lat"`
			Lon    float64 `json:"lon"`
			Center *struct {
				Lat float64 `json:"lat"`
				Lon float64 `json:"lon"`
			} `json:"center"`
			Tags map[string]string `json:"tags"`
		} `json:"elements"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	out := make([]Court, 0, len(parsed.Elements))
	for _, e := range parsed.Elements {
		lt, ln := e.Lat, e.Lon
		if e.Center != nil {
			lt, ln = e.Center.Lat, e.Center.Lon
		}
		if lt == 0 && ln == 0 {
			continue
		}
		addr := formatAddr(e.Tags)
		name := e.Tags["name"]
		if name == "" {
			// No proper name — label it by its street instead of a generic
			// "Pickleball court".
			name = streetLabel(addr)
		}
		out = append(out, Court{
			Name:    name,
			Lat:     lt,
			Lng:     ln,
			Address: addr,
			Phone:   firstNonEmpty(e.Tags["phone"], e.Tags["contact:phone"]),
			Website: firstNonEmpty(e.Tags["website"], e.Tags["contact:website"]),
			Source:  "osm",
		})
	}
	return dedupe(out), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// dedupe collapses the many bare court polygons OSM stores at one facility into
// a single venue pin (same name + ~110m), preferring the entry with the most
// info (name/address/phone).
func dedupe(courts []Court) []Court {
	best := map[string]int{} // key -> index into out
	var out []Court
	score := func(c Court) int {
		s := 0
		if c.Name != "Pickleball court" {
			s += 4
		}
		if c.Address != "" {
			s += 2
		}
		if c.Phone != "" {
			s++
		}
		if c.Website != "" {
			s++
		}
		return s
	}
	for _, c := range courts {
		key := fmt.Sprintf("%s@%.3f,%.3f", strings.ToLower(c.Name), c.Lat, c.Lng)
		if i, ok := best[key]; ok {
			if score(c) > score(out[i]) {
				out[i] = c
			}
			continue
		}
		best[key] = len(out)
		out = append(out, c)
	}
	return out
}

// GeoResult is a geocoded point for a free-text place query.
type GeoResult struct {
	Lat   float64 `json:"lat"`
	Lng   float64 `json:"lng"`
	Label string  `json:"label"`
}

// Geocode resolves a city / address / zip to a point via OSM Nominatim (free,
// keyless; requires a User-Agent, ~1 req/s rate limit). Returns nil if not found.
func Geocode(query string) (*GeoResult, error) {
	endpoint := os.Getenv("PMP_NOMINATIM_URL")
	if endpoint == "" {
		endpoint = "https://nominatim.openstreetmap.org/search"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	country := os.Getenv("PMP_GEO_COUNTRY")
	if country == "" {
		country = "us" // bias bare zips/cities to the US (pickleball's main market)
	}
	qp := u.Query()
	qp.Set("q", query)
	qp.Set("format", "json")
	qp.Set("limit", "1")
	if country != "any" {
		qp.Set("countrycodes", country)
	}
	u.RawQuery = qp.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PlanMyPickle/1.0 (+https://planmypickle.com)")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("nominatim %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var results []struct {
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	lat, _ := strconv.ParseFloat(results[0].Lat, 64)
	lng, _ := strconv.ParseFloat(results[0].Lon, 64)
	return &GeoResult{Lat: lat, Lng: lng, Label: results[0].DisplayName}, nil
}

// streetLabel turns a formatted address into a short title — the street line
// (everything before the first comma), e.g. "8020 Regents Road, San Diego, CA"
// -> "8020 Regents Road". Used to label courts that have no proper name, so the
// list shows a street instead of a generic "Pickleball court". Falls back to
// "Pickleball court" only when there is no address at all.
func streetLabel(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "Pickleball court"
	}
	if i := strings.IndexByte(addr, ','); i > 0 {
		return strings.TrimSpace(addr[:i])
	}
	return addr
}

func formatAddr(t map[string]string) string {
	var parts []string
	num, street, city := t["addr:housenumber"], t["addr:street"], t["addr:city"]
	switch {
	case num != "" && street != "":
		parts = append(parts, num+" "+street)
	case street != "":
		parts = append(parts, street)
	}
	if city != "" {
		parts = append(parts, city)
	}
	return strings.Join(parts, ", ")
}
