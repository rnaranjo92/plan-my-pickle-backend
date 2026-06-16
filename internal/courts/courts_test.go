package courts

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const sampleOverpass = `{
  "elements": [
    {"type":"node","id":1,"lat":40.7128,"lon":-74.0060,
      "tags":{"sport":"pickleball","name":"Riverside Courts","addr:housenumber":"10","addr:street":"Main St","addr:city":"Springfield"}},
    {"type":"way","id":2,"center":{"lat":40.7200,"lon":-74.0100},
      "tags":{"leisure":"pitch","sport":"pickleball"}},
    {"type":"node","id":3,"lat":0,"lon":0,"tags":{"sport":"pickleball"}}
  ]
}`

func TestOverpassFinderParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.FormValue("data") == "" {
			t.Errorf("expected Overpass query in 'data' form field")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleOverpass))
	}))
	defer srv.Close()

	f := &OverpassFinder{Endpoint: srv.URL, Client: srv.Client()}
	courts, err := f.Nearby(40.71, -74.0, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	// the 0,0 element is dropped; node + way(center) remain
	if len(courts) != 2 {
		t.Fatalf("want 2 courts, got %d", len(courts))
	}
	a := courts[0]
	if a.Name != "Riverside Courts" || a.Source != "osm" {
		t.Fatalf("bad first court: %+v", a)
	}
	if a.Address != "10 Main St, Springfield" {
		t.Fatalf("address not assembled: %q", a.Address)
	}
	b := courts[1]
	if b.Name != "Pickleball court" { // unnamed -> default
		t.Fatalf("unnamed court should default: %q", b.Name)
	}
	if b.Lat != 40.7200 || b.Lng != -74.0100 { // way uses center
		t.Fatalf("way center not used: %+v", b)
	}
}

func TestOverpassErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	f := &OverpassFinder{Endpoint: srv.URL, Client: srv.Client()}
	if _, err := f.Nearby(1, 1, 5); err == nil {
		t.Fatal("expected error on non-200")
	}
}
