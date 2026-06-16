package courts

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const samplePlaces = `{
  "places": [
    {"displayName":{"text":"Barnes Tennis Center"},
     "formattedAddress":"4490 W Point Loma Blvd, San Diego, CA 92107, USA",
     "location":{"latitude":32.7505,"longitude":-117.2456},
     "nationalPhoneNumber":"(619) 221-9000","websiteUri":"https://barnestenniscenter.com"},
    {"displayName":{"text":""},"formattedAddress":"123 Court Way",
     "location":{"latitude":32.7,"longitude":-117.1}}
  ]
}`

func TestPlacesFinderParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") == "" {
			t.Errorf("missing X-Goog-Api-Key header")
		}
		if r.Header.Get("X-Goog-FieldMask") == "" {
			t.Errorf("missing X-Goog-FieldMask header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(samplePlaces))
	}))
	defer srv.Close()

	f := &PlacesFinder{APIKey: "test", Endpoint: srv.URL, Client: srv.Client()}
	courts, err := f.Nearby(32.7, -117.2, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(courts) != 2 {
		t.Fatalf("want 2 courts, got %d", len(courts))
	}
	a := courts[0]
	if a.Name != "Barnes Tennis Center" || a.Phone == "" || a.Address == "" || a.Source != "google" {
		t.Fatalf("rich fields not parsed: %+v", a)
	}
	if courts[1].Name != "Pickleball court" { // empty displayName -> default
		t.Fatalf("unnamed should default, got %q", courts[1].Name)
	}
}

func TestNewFinderPicksSourceByEnv(t *testing.T) {
	t.Setenv("PMP_PLACES_KEY", "")
	if _, ok := NewFinder().(*OverpassFinder); !ok {
		t.Fatal("with no key, should select OverpassFinder (OSM)")
	}
	t.Setenv("PMP_PLACES_KEY", "abc123")
	if _, ok := NewFinder().(*PlacesFinder); !ok {
		t.Fatal("with a key, should select PlacesFinder (Google)")
	}
}
