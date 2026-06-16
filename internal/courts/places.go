package courts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// PlacesFinder finds courts via the Google Places API (new v1 Text Search).
// One call returns name + formatted address + phone + website — the rich data
// OSM lacks. Requires a paid API key (PMP_PLACES_KEY); implements Finder so it
// drops in for OverpassFinder via NewFinder().
type PlacesFinder struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
}

func NewPlacesFinder(key string) *PlacesFinder {
	ep := os.Getenv("PMP_PLACES_URL")
	if ep == "" {
		ep = "https://places.googleapis.com/v1/places:searchText"
	}
	return &PlacesFinder{
		APIKey:   key,
		Endpoint: ep,
		Client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (f *PlacesFinder) Nearby(lat, lng, radiusKm float64) ([]Court, error) {
	if radiusKm <= 0 {
		radiusKm = 25
	}
	radius := radiusKm * 1000
	if radius > 50000 {
		radius = 50000 // Places circle radius cap
	}
	body, _ := json.Marshal(map[string]any{
		"textQuery": "pickleball court",
		"locationBias": map[string]any{
			"circle": map[string]any{
				"center": map[string]any{"latitude": lat, "longitude": lng},
				"radius": radius,
			},
		},
	})

	req, err := http.NewRequest(http.MethodPost, f.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", f.APIKey)
	req.Header.Set("X-Goog-FieldMask",
		"places.displayName,places.formattedAddress,places.location,places.nationalPhoneNumber,places.websiteUri")

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("places %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var parsed struct {
		Places []struct {
			DisplayName struct {
				Text string `json:"text"`
			} `json:"displayName"`
			FormattedAddress string `json:"formattedAddress"`
			Location         struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"location"`
			NationalPhoneNumber string `json:"nationalPhoneNumber"`
			WebsiteURI          string `json:"websiteUri"`
		} `json:"places"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	out := make([]Court, 0, len(parsed.Places))
	for _, p := range parsed.Places {
		name := p.DisplayName.Text
		if name == "" {
			name = "Pickleball court"
		}
		out = append(out, Court{
			Name:    name,
			Lat:     p.Location.Latitude,
			Lng:     p.Location.Longitude,
			Address: p.FormattedAddress,
			Phone:   p.NationalPhoneNumber,
			Website: p.WebsiteURI,
			Source:  "google",
		})
	}
	return out, nil
}

// NewFinder picks the court data source: Google Places when PMP_PLACES_KEY is
// set (rich names/addresses/phone), otherwise the free OSM Overpass finder.
func NewFinder() Finder {
	if key := os.Getenv("PMP_PLACES_KEY"); key != "" {
		return NewPlacesFinder(key)
	}
	return NewOverpassFinder()
}
