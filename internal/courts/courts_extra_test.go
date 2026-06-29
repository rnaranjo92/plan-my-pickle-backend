package courts

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// --- pure helpers -----------------------------------------------------------

func TestFirstNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"all empty", []string{"", "", ""}, ""},
		{"no args", nil, ""},
		{"first wins", []string{"a", "b"}, "a"},
		{"skips leading empties", []string{"", "", "c"}, "c"},
		{"single empty", []string{""}, ""},
		{"single value", []string{"x"}, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstNonEmpty(tt.in...); got != tt.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStreetLabel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty -> default", "", "Pickleball court"},
		{"whitespace -> default", "   ", "Pickleball court"},
		{"comma splits to street", "8020 Regents Road, San Diego, CA", "8020 Regents Road"},
		{"no comma returns whole", "Main St", "Main St"},
		{"leading/trailing trimmed", "  123 Court Way , City ", "123 Court Way"},
		{"leading comma keeps whole", ",San Diego", ",San Diego"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streetLabel(tt.in); got != tt.want {
				t.Errorf("streetLabel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatAddr(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
		want string
	}{
		{"empty map", map[string]string{}, ""},
		{"nil map", nil, ""},
		{
			"number + street + city",
			map[string]string{"addr:housenumber": "10", "addr:street": "Main St", "addr:city": "Springfield"},
			"10 Main St, Springfield",
		},
		{
			"street + city, no number",
			map[string]string{"addr:street": "Main St", "addr:city": "Springfield"},
			"Main St, Springfield",
		},
		{
			"street only",
			map[string]string{"addr:street": "Main St"},
			"Main St",
		},
		{
			"number without street is ignored",
			map[string]string{"addr:housenumber": "10", "addr:city": "Springfield"},
			"Springfield",
		},
		{
			"city only",
			map[string]string{"addr:city": "Springfield"},
			"Springfield",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatAddr(tt.tags); got != tt.want {
				t.Errorf("formatAddr(%v) = %q, want %q", tt.tags, got, tt.want)
			}
		})
	}
}

func TestDedupe(t *testing.T) {
	t.Run("collapses same name+coords keeping richest", func(t *testing.T) {
		in := []Court{
			{Name: "Park Courts", Lat: 40.1234, Lng: -74.1234},                                   // score 4
			{Name: "Park Courts", Lat: 40.1234, Lng: -74.1234, Address: "1 Main St", Phone: "5"}, // score 7
		}
		out := dedupe(in)
		if len(out) != 1 {
			t.Fatalf("want 1 court after dedupe, got %d", len(out))
		}
		if out[0].Address != "1 Main St" || out[0].Phone != "5" {
			t.Fatalf("did not keep the richest entry: %+v", out[0])
		}
	})

	t.Run("first entry kept when later is poorer", func(t *testing.T) {
		in := []Court{
			{Name: "Park Courts", Lat: 40.1, Lng: -74.1, Address: "1 Main St", Website: "x"}, // score 7
			{Name: "Park Courts", Lat: 40.1, Lng: -74.1},                                     // score 4
		}
		out := dedupe(in)
		if len(out) != 1 {
			t.Fatalf("want 1 court, got %d", len(out))
		}
		if out[0].Address != "1 Main St" {
			t.Fatalf("should have kept richer first entry: %+v", out[0])
		}
	})

	t.Run("different coords are distinct", func(t *testing.T) {
		in := []Court{
			{Name: "Park Courts", Lat: 40.100, Lng: -74.100},
			{Name: "Park Courts", Lat: 40.900, Lng: -74.900},
		}
		out := dedupe(in)
		if len(out) != 2 {
			t.Fatalf("distinct coords should not merge: got %d", len(out))
		}
	})

	t.Run("name case-insensitive within rounding", func(t *testing.T) {
		in := []Court{
			{Name: "Park Courts", Lat: 40.12345, Lng: -74.12345},
			{Name: "PARK COURTS", Lat: 40.12340, Lng: -74.12340}, // rounds to same 3-dp key
		}
		out := dedupe(in)
		if len(out) != 1 {
			t.Fatalf("case-insensitive same-coord should merge: got %d", len(out))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if out := dedupe(nil); len(out) != 0 {
			t.Fatalf("dedupe(nil) should be empty, got %d", len(out))
		}
	})
}

// --- Overpass extra paths ---------------------------------------------------

func TestOverpassEmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"elements":[]}`))
	}))
	defer srv.Close()
	f := &OverpassFinder{Endpoint: srv.URL, Client: srv.Client()}
	out, err := f.Nearby(40, -74, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("want 0 courts, got %d", len(out))
	}
}

func TestOverpassMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()
	f := &OverpassFinder{Endpoint: srv.URL, Client: srv.Client()}
	if _, err := f.Nearby(40, -74, 10); err == nil {
		t.Fatal("expected decode error on malformed JSON")
	}
}

func TestOverpassDefaultRadius(t *testing.T) {
	var gotData string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotData = r.FormValue("data")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"elements":[]}`))
	}))
	defer srv.Close()
	f := &OverpassFinder{Endpoint: srv.URL, Client: srv.Client()}
	// radiusKm <= 0 defaults to 25km == 25000m
	if _, err := f.Nearby(40, -74, 0); err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if !strings.Contains(gotData, "around:25000") {
		t.Fatalf("default radius (25km) not applied; query=%q", gotData)
	}
}

func TestOverpassRequestError(t *testing.T) {
	// An unparseable endpoint makes http.NewRequest / Do fail.
	f := &OverpassFinder{Endpoint: "http://%zz", Client: http.DefaultClient}
	if _, err := f.Nearby(40, -74, 10); err == nil {
		t.Fatal("expected error from bad endpoint")
	}
}

func TestOverpassPhoneWebsiteFallbacks(t *testing.T) {
	// Exercises the contact:* fallback branches of firstNonEmpty inside Nearby.
	body := `{"elements":[
	  {"type":"node","lat":40.5,"lon":-74.5,
	   "tags":{"sport":"pickleball","name":"Contact Courts",
	           "contact:phone":"555-1212","contact:website":"https://ex.com"}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	f := &OverpassFinder{Endpoint: srv.URL, Client: srv.Client()}
	out, err := f.Nearby(40.5, -74.5, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 court, got %d", len(out))
	}
	if out[0].Phone != "555-1212" || out[0].Website != "https://ex.com" {
		t.Fatalf("contact:* fallbacks not used: %+v", out[0])
	}
}

func TestOverpassUnnamedWithAddressGetsStreetLabel(t *testing.T) {
	// Unnamed court WITH an address should be labeled by its street, not the
	// generic "Pickleball court".
	body := `{"elements":[
	  {"type":"node","lat":40.6,"lon":-74.6,
	   "tags":{"sport":"pickleball","addr:housenumber":"42","addr:street":"Maple Ave","addr:city":"Town"}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	f := &OverpassFinder{Endpoint: srv.URL, Client: srv.Client()}
	out, err := f.Nearby(40.6, -74.6, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(out) != 1 || out[0].Name != "42 Maple Ave" {
		t.Fatalf("expected street label, got %+v", out)
	}
}

// --- Places extra paths -----------------------------------------------------

func TestPlacesEmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"places":[]}`))
	}))
	defer srv.Close()
	f := &PlacesFinder{APIKey: "k", Endpoint: srv.URL, Client: srv.Client()}
	out, err := f.Nearby(32, -117, 25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("want 0 courts, got %d", len(out))
	}
}

func TestPlacesErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()
	f := &PlacesFinder{APIKey: "k", Endpoint: srv.URL, Client: srv.Client()}
	if _, err := f.Nearby(32, -117, 25); err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestPlacesMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{bad`))
	}))
	defer srv.Close()
	f := &PlacesFinder{APIKey: "k", Endpoint: srv.URL, Client: srv.Client()}
	if _, err := f.Nearby(32, -117, 25); err == nil {
		t.Fatal("expected decode error on malformed JSON")
	}
}

func TestPlacesRequestError(t *testing.T) {
	f := &PlacesFinder{APIKey: "k", Endpoint: "http://%zz", Client: http.DefaultClient}
	if _, err := f.Nearby(32, -117, 25); err == nil {
		t.Fatal("expected error from bad endpoint")
	}
}

func TestPlacesRichFieldsAndRadiusCap(t *testing.T) {
	// Verifies rating/category parse AND the >50km radius is capped to 50000.
	var gotBody string
	body := `{"places":[
	  {"displayName":{"text":"Ace Park"},"formattedAddress":"1 A St",
	   "location":{"latitude":1.0,"longitude":2.0},
	   "nationalPhoneNumber":"111","websiteUri":"https://a.example",
	   "rating":4.5,"userRatingCount":120,
	   "primaryTypeDisplayName":{"text":"Park"}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	f := &PlacesFinder{APIKey: "k", Endpoint: srv.URL, Client: srv.Client()}
	out, err := f.Nearby(1, 2, 100) // 100km -> capped at 50000m
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 court, got %d", len(out))
	}
	c := out[0]
	if c.Rating != 4.5 || c.RatingCount != 120 || c.Category != "Park" {
		t.Fatalf("rich google fields not parsed: %+v", c)
	}
	if !strings.Contains(gotBody, "50000") {
		t.Fatalf("radius not capped at 50000; body=%q", gotBody)
	}
}

func TestPlacesDefaultRadius(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"places":[]}`))
	}))
	defer srv.Close()
	f := &PlacesFinder{APIKey: "k", Endpoint: srv.URL, Client: srv.Client()}
	if _, err := f.Nearby(1, 2, 0); err != nil { // 0 -> default 25km == 25000
		t.Fatalf("nearby: %v", err)
	}
	if !strings.Contains(gotBody, "25000") {
		t.Fatalf("default radius (25000) not applied; body=%q", gotBody)
	}
}

// --- haversine / Rank edge cases --------------------------------------------

func TestHaversineKnownDistance(t *testing.T) {
	// NYC (40.7128,-74.0060) -> LA (34.0522,-118.2437) ~= 3,936 km.
	d := haversineMeters(40.7128, -74.0060, 34.0522, -118.2437)
	const wantKm = 3936.0
	if diff := d/1000 - wantKm; diff < -40 || diff > 40 {
		t.Fatalf("haversine NYC->LA = %.1f km, want ~%.0f km", d/1000, wantKm)
	}
}

func TestHaversineZeroForSamePoint(t *testing.T) {
	if d := haversineMeters(10, 20, 10, 20); d != 0 {
		t.Fatalf("same point should be 0m, got %v", d)
	}
}

func TestRankNoCapKeepsAll(t *testing.T) {
	cs := []Court{
		{Name: "a", Lat: 40.80, Lng: -74.0},
		{Name: "b", Lat: 40.71, Lng: -74.0},
		{Name: "c", Lat: 40.75, Lng: -74.0},
	}
	out := Rank(cs, 40.71, -74.0, 0) // max <= 0 keeps all
	if len(out) != 3 {
		t.Fatalf("max<=0 should keep all, got %d", len(out))
	}
	if out[0].Name != "b" {
		t.Fatalf("nearest should be first, got %s", out[0].Name)
	}
}

func TestRankCapLargerThanLen(t *testing.T) {
	cs := []Court{{Name: "a", Lat: 1, Lng: 1}}
	out := Rank(cs, 0, 0, 10)
	if len(out) != 1 {
		t.Fatalf("cap larger than len should keep all, got %d", len(out))
	}
}

func TestRankEmpty(t *testing.T) {
	if out := Rank(nil, 0, 0, 5); len(out) != 0 {
		t.Fatalf("Rank(nil) should be empty, got %d", len(out))
	}
}

func TestRankSetsDistance(t *testing.T) {
	cs := []Court{{Name: "x", Lat: 40.72, Lng: -74.0}}
	out := Rank(cs, 40.71, -74.0, 0)
	if out[0].DistanceMeters <= 0 {
		t.Fatalf("DistanceMeters should be set positive, got %d", out[0].DistanceMeters)
	}
}

// --- ensure Court JSON tags marshal as the API expects (light sanity) -------

func TestCourtStructFields(t *testing.T) {
	c := Court{Name: "N", Lat: 1, Lng: 2, Source: "osm"}
	rt := reflect.TypeOf(c)
	if f, _ := rt.FieldByName("Source"); f.Tag.Get("json") != "source" {
		t.Fatalf("Source json tag = %q, want source", f.Tag.Get("json"))
	}
}
