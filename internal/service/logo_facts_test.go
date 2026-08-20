package service

import (
	"strings"
	"testing"
)

// A single sponsor logo came back TILED five times across the bottom of a real
// poster: the prompt asked for "an evenly spaced row of credits", and a row
// implies something to fill. The instructions must state the count and forbid
// repetition, or the model pads the space with copies.
func TestLogoFactsOneSponsorSaysOnce(t *testing.T) {
	got := logoFacts([4]int{0, 0, 1, 0}, nil, 0)
	for _, want := range []string{"single sponsor", "ONCE", "do not repeat"} {
		if !strings.Contains(got, want) {
			t.Errorf("one sponsor should be told to appear once (%q missing): %s",
				want, got)
		}
	}
	// The plural phrasing invites filling; it must not appear for a lone logo.
	if strings.Contains(got, "evenly spaced row") {
		t.Errorf("a lone sponsor must not be given a row to fill: %s", got)
	}
}

func TestLogoFactsManySponsorsSayEachOnce(t *testing.T) {
	got := logoFacts([4]int{0, 0, 3, 0}, nil, 0)
	if !strings.Contains(got, "3 DIFFERENT sponsor logos") {
		t.Errorf("the count should be explicit: %s", got)
	}
	if !strings.Contains(got, "exactly once") {
		t.Errorf("each logo should be placed once: %s", got)
	}
}

// The no-repeat rule applies to every logo kind, not just sponsors — the model
// tiles whenever it has space to fill.
func TestLogoFactsAlwaysForbidsTiling(t *testing.T) {
	for _, counts := range [][4]int{
		{1, 0, 0, 0}, {0, 2, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1},
	} {
		got := logoFacts(counts, []string{"Host club"}, 0)
		if !strings.Contains(got, "EXACTLY ONCE") ||
			!strings.Contains(got, "Never duplicate, tile") {
			t.Errorf("counts %v must carry the no-tiling rule: %s", counts, got)
		}
	}
}

// No logos, no instructions — an empty block would otherwise add noise to
// every poster prompt that has no uploads at all.
func TestLogoFactsEmptyWhenNoLogos(t *testing.T) {
	if got := logoFacts([4]int{}, nil, 0); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// The sponsor must sit ON the poster's own artwork. Asking for the "bottom
// margin" produced a strip that read as bolted on underneath the design.
func TestLogoFactsSponsorIsBlendedNotBanded(t *testing.T) {
	for _, counts := range [][4]int{{0, 0, 1, 0}, {0, 0, 3, 0}} {
		got := logoFacts(counts, nil, 0)
		if !strings.Contains(got, "own background as part of the design") {
			t.Errorf("counts %v: sponsor should sit on the artwork: %s", counts, got)
		}
		if !strings.Contains(got, "band, bar, footer, panel or strip") {
			t.Errorf("counts %v: a separate strip must be ruled out: %s", counts, got)
		}
		if strings.Contains(got, "bottom margin") {
			t.Errorf("counts %v: 'bottom margin' is what produced the strip: %s",
				counts, got)
		}
	}
}

// The uploaded artwork is a brand asset: only size and position may change.
// A poster style that distresses or tints everything must not touch it.
func TestLogoFactsProtectsTheArtworkItself(t *testing.T) {
	got := logoFacts([4]int{1, 0, 0, 0}, nil, 0)
	for _, want := range []string{
		"UNTOUCHABLE", "pixel-faithfully", "texture, grain, filter",
		"Only its SIZE and POSITION may change",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("logo integrity rule missing %q: %s", want, got)
		}
	}
}
