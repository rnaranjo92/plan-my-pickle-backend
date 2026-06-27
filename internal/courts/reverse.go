package courts

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rank sets DistanceMeters on each court (haversine from the search center),
// sorts nearest-first, and keeps at most max courts (max <= 0 keeps all).
func Rank(cs []Court, lat, lng float64, max int) []Court {
	for i := range cs {
		cs[i].DistanceMeters = int(haversineMeters(lat, lng, cs[i].Lat, cs[i].Lng))
	}
	sort.SliceStable(cs, func(i, j int) bool {
		return cs[i].DistanceMeters < cs[j].DistanceMeters
	})
	if max > 0 && len(cs) > max {
		cs = cs[:max]
	}
	return cs
}

func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthR = 6371000.0
	const rad = math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthR * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// --- reverse geocoding: label the nameless OSM courts ---

// geocoderKey enables reverse geocoding (Geoapify, OSM-based, free tier). When
// unset, EnrichLabels is a no-op and unnamed courts keep their generic label.
var geocoderKey = os.Getenv("PMP_GEOCODER_KEY")

var reverseHTTP = &http.Client{Timeout: 6 * time.Second}

// reverseCache memoizes reverse lookups by ~11m-rounded coordinate, so repeat
// searches and clustered courts never re-hit the geocoder.
var reverseCache sync.Map // string -> reverseResult

type reverseResult struct {
	Name    string
	Address string
}

// EnrichLabels reverse-geocodes courts that still have no real name (the generic
// "Pickleball court" with no address) and relabels them by their park/POI name
// or street. Lookups run concurrently (capped) and are cached. No-op when
// PMP_GEOCODER_KEY is unset.
func EnrichLabels(cs []Court) {
	if geocoderKey == "" {
		return
	}
	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i := range cs {
		if cs[i].Name != "Pickleball court" {
			continue // already has a real name or a street label
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			r := reverseLookup(cs[idx].Lat, cs[idx].Lng)
			if r.Name != "" {
				cs[idx].Name = r.Name
			}
			if cs[idx].Address == "" && r.Address != "" {
				cs[idx].Address = r.Address
			}
		}(i)
	}
	wg.Wait()
}

func reverseLookup(lat, lng float64) reverseResult {
	key := fmt.Sprintf("%.4f,%.4f", lat, lng)
	if v, ok := reverseCache.Load(key); ok {
		return v.(reverseResult)
	}
	r := reverseGeoapify(lat, lng)
	reverseCache.Store(key, r)
	return r
}

// geocodeGeoapify resolves a free-text place (city / address / zip) to a point
// via Geoapify forward geocoding. Returns nil if no match.
func geocodeGeoapify(query string) (*GeoResult, error) {
	country := os.Getenv("PMP_GEO_COUNTRY")
	if country == "" {
		country = "us"
	}
	u := fmt.Sprintf("https://api.geoapify.com/v1/geocode/search?text=%s&limit=1&format=json&apiKey=%s",
		url.QueryEscape(query), url.QueryEscape(geocoderKey))
	if country != "any" {
		u += "&filter=countrycode:" + url.QueryEscape(country)
	}
	resp, err := reverseHTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geoapify geocode %d", resp.StatusCode)
	}
	var parsed struct {
		Results []struct {
			Lat       float64 `json:"lat"`
			Lon       float64 `json:"lon"`
			Formatted string  `json:"formatted"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Results) == 0 {
		return nil, nil
	}
	r := parsed.Results[0]
	return &GeoResult{Lat: r.Lat, Lng: r.Lon, Label: r.Formatted}, nil
}

// CityAutocomplete returns up to ~6 city suggestions ("City, State") for a
// free-text query via Geoapify's autocomplete API (type=city). Returns nil when
// PMP_GEOCODER_KEY is unset (callers then fall back to a plain free-text field)
// or on any error/short query.
func CityAutocomplete(query string) []string {
	query = strings.TrimSpace(query)
	if geocoderKey == "" || len(query) < 2 {
		return nil
	}
	country := os.Getenv("PMP_GEO_COUNTRY")
	if country == "" {
		country = "us"
	}
	u := fmt.Sprintf("https://api.geoapify.com/v1/geocode/autocomplete?text=%s&type=city&limit=6&format=json&apiKey=%s",
		url.QueryEscape(query), url.QueryEscape(geocoderKey))
	if country != "any" {
		u += "&filter=countrycode:" + url.QueryEscape(country)
	}
	resp, err := reverseHTTP.Get(u)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var parsed struct {
		Results []struct {
			City      string `json:"city"`
			State     string `json:"state"`
			Formatted string `json:"formatted"`
		} `json:"results"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil {
		return nil
	}
	out := make([]string, 0, len(parsed.Results))
	seen := map[string]bool{}
	for _, r := range parsed.Results {
		label := r.City
		if label != "" && r.State != "" {
			label = r.City + ", " + r.State
		} else if label == "" {
			label = r.Formatted
		}
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	return out
}

// reverseGeoapify resolves a coordinate to a place name + address via Geoapify.
// Prefers a POI/park name, then the street. Returns an empty result on any
// error so the caller keeps the generic label.
func reverseGeoapify(lat, lng float64) reverseResult {
	u := fmt.Sprintf("https://api.geoapify.com/v1/geocode/reverse?lat=%f&lon=%f&format=json&apiKey=%s",
		lat, lng, url.QueryEscape(geocoderKey))
	resp, err := reverseHTTP.Get(u)
	if err != nil {
		return reverseResult{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reverseResult{}
	}
	var parsed struct {
		Results []struct {
			Name      string `json:"name"`
			Street    string `json:"street"`
			Formatted string `json:"formatted"`
		} `json:"results"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil || len(parsed.Results) == 0 {
		return reverseResult{}
	}
	r := parsed.Results[0]
	name := r.Name
	if name == "" {
		name = r.Street
	}
	return reverseResult{Name: name, Address: r.Formatted}
}
