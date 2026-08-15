package api

import (
	"encoding/json"
	"html/template"
	"net/http/httptest"
	"regexp"
	"testing"
)

var ldBlock = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)

// Every SSR page emits schema.org JSON-LD, and it silently shipped broken for
// months: html/template treats <script> as a JS context, so a template.HTML
// value gets STRING-escaped rather than inserted, and each page emitted
//
//	"{\"@context\":\"https://schema.org\",...}"
//
// — valid JSON, but a string, not an object. Google reported "Unparsable
// structured data" and no page was eligible for rich results. template.JS is
// the type that inserts raw.
//
// Rendering alone can't catch this; the output has to be parsed and its shape
// asserted, which is what this does for every template.
func TestJSONLDIsAnObjectNotAString(t *testing.T) {
	ld := template.JS(`{"@context":"https://schema.org","@type":"ItemList"}`)

	cases := []struct {
		name string
		tmpl *template.Template
		data any
	}{
		{"event", seoEventTmpl, seoEventData{JSONLD: ld}},
		{"hub", seoHubTmpl, seoHubData{JSONLD: ld}},
		{"town", seoTownTmpl, seoTownData{JSONLD: ld}},
		{"class", seoClassTmpl, seoClassData{JSONLD: ld}},
		{"results", seoResultsTmpl, seoResultsData{JSONLD: ld}},
		{"coachhub", seoCoachHubTmpl, seoCoachHubData{JSONLD: ld}},
		{"coach", seoCoachTmpl, seoCoachPageData{JSONLD: ld}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).seoRender(w, c.tmpl, c.data)
			m := ldBlock.FindStringSubmatch(w.Body.String())
			if m == nil {
				t.Fatal("no ld+json block rendered")
			}
			var v any
			if err := json.Unmarshal([]byte(m[1]), &v); err != nil {
				t.Fatalf("ld+json does not parse: %v\n%s", err, m[1])
			}
			// The whole point: it must decode to an OBJECT. A string here is the
			// escaping bug, and it still parses as JSON, so only the type check
			// catches it.
			obj, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("ld+json decoded to %T, want an object — it is being escaped as a string:\n%s", v, m[1])
			}
			if obj["@context"] != "https://schema.org" {
				t.Fatalf("@context = %v", obj["@context"])
			}
		})
	}
}
