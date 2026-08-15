package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCoachApplyPageRenders(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.renderCoachApply(w, "", "")
	body := w.Body.String()
	for _, want := range []string{
		"Coach on PlanMyPickle", `name="email"`, `name="certifications"`,
		`action="/coaches/apply"`, "text/html",
	} {
		if want == "text/html" {
			if !strings.Contains(w.Header().Get("content-type"), want) {
				t.Fatalf("content-type = %q", w.Header().Get("content-type"))
			}
			continue
		}
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	// The error path must ESCAPE what it echoes back.
	w2 := httptest.NewRecorder()
	s.renderCoachApply(w2, "", `<script>alert(1)</script>`)
	if strings.Contains(w2.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("error message was not escaped")
	}
}
