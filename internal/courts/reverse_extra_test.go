package courts

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// roundTripFunc lets a test stand in as the reverseHTTP client's transport so
// the hardcoded api.geoapify.com calls are served canned responses.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// withGeoapify installs a fake transport + geocoder key for the duration of fn,
// passing each captured request URL back via handler. Restores globals after.
func withGeoapify(t *testing.T, key string, handler func(*http.Request) (*http.Response, error)) {
	t.Helper()
	origKey := geocoderKey
	origTransport := reverseHTTP.Transport
	geocoderKey = key
	reverseHTTP.Transport = roundTripFunc(handler)
	t.Cleanup(func() {
		geocoderKey = origKey
		reverseHTTP.Transport = origTransport
		reverseCache = sync.Map{} // reset memoization between tests
	})
}

// --- reverseGeoapify --------------------------------------------------------

func TestReverseGeoapifyName(t *testing.T) {
	withGeoapify(t, "K", func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.String(), "geocode/reverse") {
			t.Errorf("unexpected URL: %s", r.URL)
		}
		return jsonResp(200, `{"results":[{"name":"Balboa Park","street":"6th Ave","formatted":"Balboa Park, San Diego"}]}`), nil
	})
	r := reverseGeoapify(32.7, -117.1)
	if r.Name != "Balboa Park" || r.Address != "Balboa Park, San Diego" {
		t.Fatalf("bad result: %+v", r)
	}
}

func TestReverseGeoapifyFallsBackToStreet(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"results":[{"name":"","street":"6th Ave","formatted":"6th Ave, SD"}]}`), nil
	})
	r := reverseGeoapify(1, 2)
	if r.Name != "6th Ave" {
		t.Fatalf("expected street fallback, got %q", r.Name)
	}
}

func TestReverseGeoapifyEmptyResults(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"results":[]}`), nil
	})
	if r := reverseGeoapify(1, 2); r.Name != "" || r.Address != "" {
		t.Fatalf("empty results should yield empty struct, got %+v", r)
	}
}

func TestReverseGeoapifyNon200(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(500, `oops`), nil
	})
	if r := reverseGeoapify(1, 2); r != (reverseResult{}) {
		t.Fatalf("non-200 should yield empty struct, got %+v", r)
	}
}

func TestReverseGeoapifyMalformed(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{bad`), nil
	})
	if r := reverseGeoapify(1, 2); r != (reverseResult{}) {
		t.Fatalf("malformed JSON should yield empty struct, got %+v", r)
	}
}

func TestReverseGeoapifyTransportError(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	if r := reverseGeoapify(1, 2); r != (reverseResult{}) {
		t.Fatalf("transport error should yield empty struct, got %+v", r)
	}
}

// --- reverseLookup (cache) --------------------------------------------------

func TestReverseLookupCaches(t *testing.T) {
	var calls int
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"results":[{"name":"Cached Park","formatted":"addr"}]}`), nil
	})
	r1 := reverseLookup(40.123456, -74.123456)
	r2 := reverseLookup(40.123456, -74.123456) // same ~11m bucket -> cached
	if r1.Name != "Cached Park" || r2.Name != "Cached Park" {
		t.Fatalf("unexpected results: %+v %+v", r1, r2)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upstream call (cached), got %d", calls)
	}
}

// --- EnrichLabels -----------------------------------------------------------

func TestEnrichLabelsNoOpWithoutKey(t *testing.T) {
	withGeoapify(t, "", func(*http.Request) (*http.Response, error) {
		t.Fatal("should not call geocoder when key is empty")
		return nil, nil
	})
	cs := []Court{{Name: "Pickleball court", Lat: 1, Lng: 2}}
	EnrichLabels(cs)
	if cs[0].Name != "Pickleball court" {
		t.Fatalf("no-op expected, name changed to %q", cs[0].Name)
	}
}

func TestEnrichLabelsRelabelsGenericCourts(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"results":[{"name":"Sunset Park","formatted":"100 Ocean Blvd"}]}`), nil
	})
	cs := []Court{
		{Name: "Pickleball court", Lat: 10, Lng: 20},                     // relabeled, gets address
		{Name: "Named Already", Lat: 11, Lng: 21},                        // skipped
		{Name: "Pickleball court", Lat: 12, Lng: 22, Address: "keep me"}, // name set, addr kept
	}
	EnrichLabels(cs)
	if cs[0].Name != "Sunset Park" || cs[0].Address != "100 Ocean Blvd" {
		t.Fatalf("first court not enriched: %+v", cs[0])
	}
	if cs[1].Name != "Named Already" {
		t.Fatalf("named court should be untouched: %+v", cs[1])
	}
	if cs[2].Name != "Sunset Park" || cs[2].Address != "keep me" {
		t.Fatalf("existing address should be preserved: %+v", cs[2])
	}
}

func TestEnrichLabelsKeepsGenericWhenLookupEmpty(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"results":[]}`), nil
	})
	cs := []Court{{Name: "Pickleball court", Lat: 5, Lng: 6}}
	EnrichLabels(cs)
	if cs[0].Name != "Pickleball court" {
		t.Fatalf("empty lookup should keep generic label, got %q", cs[0].Name)
	}
}

// --- geocodeGeoapify --------------------------------------------------------

func TestGeocodeGeoapifySuccess(t *testing.T) {
	withGeoapify(t, "K", func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.String(), "geocode/search") {
			t.Errorf("unexpected URL: %s", r.URL)
		}
		return jsonResp(200, `{"results":[{"lat":32.7,"lon":-117.1,"formatted":"San Diego, CA"}]}`), nil
	})
	res, err := geocodeGeoapify("San Diego")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || res.Lat != 32.7 || res.Lng != -117.1 || res.Label != "San Diego, CA" {
		t.Fatalf("bad geocode result: %+v", res)
	}
}

func TestGeocodeGeoapifyNoResults(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"results":[]}`), nil
	})
	res, err := geocodeGeoapify("nowhere")
	if err != nil || res != nil {
		t.Fatalf("expected (nil,nil), got (%+v,%v)", res, err)
	}
}

func TestGeocodeGeoapifyNon200(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(403, `denied`), nil
	})
	if _, err := geocodeGeoapify("x"); err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestGeocodeGeoapifyMalformed(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{bad`), nil
	})
	if _, err := geocodeGeoapify("x"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestGeocodeGeoapifyTransportError(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	if _, err := geocodeGeoapify("x"); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestGeocodeGeoapifyCountryAny(t *testing.T) {
	t.Setenv("PMP_GEO_COUNTRY", "any")
	var gotURL string
	withGeoapify(t, "K", func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return jsonResp(200, `{"results":[{"lat":1,"lon":2,"formatted":"X"}]}`), nil
	})
	if _, err := geocodeGeoapify("x"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(gotURL, "countrycode") {
		t.Fatalf("country=any should not add filter; URL=%s", gotURL)
	}
}

// --- PlaceAutocomplete ------------------------------------------------------

func TestPlaceAutocompleteShortQueryAndNoKey(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		t.Fatal("should not call upstream for short query")
		return nil, nil
	})
	if out := PlaceAutocomplete("a", "city"); out != nil { // len 1 < 2
		t.Fatalf("short query should return nil, got %v", out)
	}
	if out := PlaceAutocomplete("   ", "place"); out != nil { // trims to empty
		t.Fatalf("whitespace query should return nil, got %v", out)
	}
	// no key -> nil even for valid query
	withGeoapify(t, "", func(*http.Request) (*http.Response, error) {
		t.Fatal("should not call upstream with no key")
		return nil, nil
	})
	if out := PlaceAutocomplete("San", "city"); out != nil {
		t.Fatalf("no key should return nil, got %v", out)
	}
}

func TestPlaceAutocompleteCity(t *testing.T) {
	var gotURL string
	withGeoapify(t, "K", func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return jsonResp(200, `{"results":[
		  {"city":"San Diego","state":"CA","formatted":"San Diego, CA, USA"},
		  {"city":"San Diego","state":"CA","formatted":"dup"},
		  {"city":"","state":"","formatted":"Fallback Label"},
		  {"city":"Austin","state":"","formatted":"Austin formatted"}
		]}`), nil
	})
	out := PlaceAutocomplete("San", "city")
	// city kind adds &type=city
	if !strings.Contains(gotURL, "type=city") {
		t.Errorf("city kind should add type=city; URL=%s", gotURL)
	}
	want := []string{"San Diego, CA", "Fallback Label", "Austin"}
	if len(out) != len(want) {
		t.Fatalf("got %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out[%d]=%q, want %q (full=%v)", i, out[i], want[i], out)
		}
	}
}

func TestPlaceAutocompletePlace(t *testing.T) {
	var gotURL string
	withGeoapify(t, "K", func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return jsonResp(200, `{"results":[
		  {"city":"X","state":"Y","formatted":"123 Main St, Town"},
		  {"city":"X","state":"Y","formatted":""},
		  {"city":"X","state":"Y","formatted":"123 Main St, Town"}
		]}`), nil
	})
	out := PlaceAutocomplete("123", "place")
	if strings.Contains(gotURL, "type=city") {
		t.Errorf("place kind should NOT add type=city; URL=%s", gotURL)
	}
	// place uses formatted; empty dropped, duplicate dropped
	if len(out) != 1 || out[0] != "123 Main St, Town" {
		t.Fatalf("place autocomplete: got %v", out)
	}
}

func TestPlaceAutocompleteNon200(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(500, `err`), nil
	})
	if out := PlaceAutocomplete("San", "city"); out != nil {
		t.Fatalf("non-200 should return nil, got %v", out)
	}
}

func TestPlaceAutocompleteTransportError(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	if out := PlaceAutocomplete("San", "city"); out != nil {
		t.Fatalf("transport error should return nil, got %v", out)
	}
}

func TestPlaceAutocompleteMalformed(t *testing.T) {
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{bad`), nil
	})
	if out := PlaceAutocomplete("San", "city"); out != nil {
		t.Fatalf("malformed JSON should return nil, got %v", out)
	}
}

// --- Geocode (dispatcher) + geocodeNominatim --------------------------------

func TestGeocodeUsesGeoapifyWhenKeySet(t *testing.T) {
	withGeoapify(t, "K", func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.String(), "geoapify") {
			t.Errorf("expected geoapify call, got %s", r.URL)
		}
		return jsonResp(200, `{"results":[{"lat":9,"lon":8,"formatted":"Geo"}]}`), nil
	})
	res, err := Geocode("anywhere")
	if err != nil || res == nil || res.Label != "Geo" {
		t.Fatalf("expected geoapify result, got (%+v,%v)", res, err)
	}
}

func TestGeocodeFallsBackToNominatimOnGeoapifyError(t *testing.T) {
	// Geoapify returns an error, Geocode should fall through to Nominatim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"lat":"1.5","lon":"2.5","display_name":"Nomi Town"}]`))
	}))
	defer srv.Close()
	t.Setenv("PMP_NOMINATIM_URL", srv.URL)
	withGeoapify(t, "K", func(*http.Request) (*http.Response, error) {
		return jsonResp(500, `boom`), nil // geoapify errors -> fall through
	})
	res, err := Geocode("somewhere")
	if err != nil || res == nil || res.Label != "Nomi Town" {
		t.Fatalf("expected nominatim fallback, got (%+v,%v)", res, err)
	}
	if res.Lat != 1.5 || res.Lng != 2.5 {
		t.Fatalf("nominatim coords not parsed: %+v", res)
	}
}

func TestGeocodeNominatimWhenNoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json query param")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"lat":"40.0","lon":"-74.0","display_name":"Keyless City"}]`))
	}))
	defer srv.Close()
	t.Setenv("PMP_NOMINATIM_URL", srv.URL)
	withGeoapify(t, "", nil) // no key -> Geocode goes straight to Nominatim
	res, err := Geocode("city")
	if err != nil || res == nil || res.Label != "Keyless City" {
		t.Fatalf("expected nominatim result, got (%+v,%v)", res, err)
	}
}

func TestGeocodeNominatimNoResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	t.Setenv("PMP_NOMINATIM_URL", srv.URL)
	res, err := geocodeNominatim("nowhere")
	if err != nil || res != nil {
		t.Fatalf("no results should be (nil,nil), got (%+v,%v)", res, err)
	}
}

func TestGeocodeNominatimNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("PMP_NOMINATIM_URL", srv.URL)
	if _, err := geocodeNominatim("x"); err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestGeocodeNominatimMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{bad`))
	}))
	defer srv.Close()
	t.Setenv("PMP_NOMINATIM_URL", srv.URL)
	if _, err := geocodeNominatim("x"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestGeocodeNominatimCountryAny(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"lat":"1","lon":"2","display_name":"Z"}]`))
	}))
	defer srv.Close()
	t.Setenv("PMP_NOMINATIM_URL", srv.URL)
	t.Setenv("PMP_GEO_COUNTRY", "any")
	if _, err := geocodeNominatim("x"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(gotURL, "countrycodes") {
		t.Fatalf("country=any should not set countrycodes; URL=%s", gotURL)
	}
}

func TestGeocodeNominatimBadEndpoint(t *testing.T) {
	t.Setenv("PMP_NOMINATIM_URL", "http://%zz")
	if _, err := geocodeNominatim("x"); err == nil {
		t.Fatal("expected error from unparseable endpoint")
	}
}
