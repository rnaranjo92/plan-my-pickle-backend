package service

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

// A solid poster to draw on, big enough to carry a code.
func fakePoster(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Deliberately DARK and busy-ish: the whole argument for drawing an
			// opaque card is that the artwork underneath cannot be trusted.
			m.Set(x, y, color.RGBA{R: uint8(x % 40), G: 20, B: uint8(y % 40), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// The registration URL is what gets PRINTED and can never be changed afterwards,
// so its shape is pinned rather than left to a helper nobody re-reads.
func TestRegistrationURLIsThePublicSelfRegistrationForm(t *testing.T) {
	got := registrationURLFor("evt-123")
	want := "https://app.planmypickle.com/?register=evt-123"
	if got != want {
		t.Fatalf("registration url = %q, want %q", got, want)
	}
}

func TestAddPosterQRDrawsAnOpaqueQuietZone(t *testing.T) {
	src := fakePoster(t, 900, 1200)
	out, mime, err := addPosterQR(src, "image/png", registrationURLFor("e1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("QR output must be PNG (JPEG blocking eats module edges); got %q", mime)
	}
	if bytes.Equal(out, src) {
		t.Fatal("image unchanged — no QR was drawn")
	}
	m, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a decodable PNG: %v", err)
	}
	b := m.Bounds()
	if b.Dx() != 900 || b.Dy() != 1200 {
		t.Fatalf("poster was resized: %dx%d", b.Dx(), b.Dy())
	}
	// Ask the implementation where it drew, rather than re-deriving it: the card
	// is rounded DOWN to whole modules, so measuring in from posterQRFraction
	// lands on the artwork. The first version of this test did exactly that and
	// failed against correct code.
	qr, err := qrcode.New(registrationURLFor("e1"), qrcode.Highest)
	if err != nil {
		t.Fatalf("qr: %v", err)
	}
	qr.DisableBorder = true
	lay := posterQRLayout(b, len(qr.Bitmap()))

	// The card's outermost ring is the quiet zone: opaque white, no artwork.
	// Without it a geometrically perfect code still fails to scan.
	for _, p := range [][2]int{
		{lay.x + 1, lay.y + 1},
		{lay.x + lay.card - 2, lay.y + 1},
		{lay.x + 1, lay.y + lay.card - 2},
		{lay.x + lay.card - 2, lay.y + lay.card - 2},
	} {
		r, g, bl, a := m.At(p[0], p[1]).RGBA()
		if r>>8 != 255 || g>>8 != 255 || bl>>8 != 255 || a>>8 != 255 {
			t.Fatalf("quiet zone not opaque white at (%d,%d): %d,%d,%d,%d",
				p[0], p[1], r>>8, g>>8, bl>>8, a>>8)
		}
	}
	// Every pixel inside the card must be pure black or pure white. A grey one
	// means a module edge landed mid-pixel and was interpolated — the most
	// reliable way to produce a code that looks perfect and does not scan.
	for y := lay.y; y < lay.y+lay.card; y++ {
		for x := lay.x; x < lay.x+lay.card; x++ {
			r, g, bl, _ := m.At(x, y).RGBA()
			r8, g8, b8 := r>>8, g>>8, bl>>8
			if r8 != g8 || g8 != b8 {
				t.Fatalf("non-greyscale pixel inside the code at (%d,%d)", x, y)
			}
			if r8 != 0 && r8 != 255 {
				t.Fatalf("anti-aliased module at (%d,%d): value %d — modules must "+
					"be whole pixels or the code will not scan", x, y, r8)
			}
		}
	}
}

// The module grid must be exact: a QR whose cells land on fractional pixels is
// a picture of a QR. Pinned across the poster sizes the model actually returns.
func TestPosterQRLayoutKeepsModulesOnWholePixels(t *testing.T) {
	for _, size := range []image.Point{{X: 900, Y: 1200}, {X: 1024, Y: 1365},
		{X: 2048, Y: 2731}, {X: 768, Y: 1024}} {
		b := image.Rect(0, 0, size.X, size.Y)
		lay := posterQRLayout(b, 45)
		if lay.scale < 1 {
			t.Fatalf("%v: scale %d — modules would vanish", size, lay.scale)
		}
		if want := 45*lay.scale + lay.quiet*2; lay.card != want {
			t.Fatalf("%v: card %d != modules+quiet %d", size, lay.card, want)
		}
		if lay.quiet != posterQRQuietModules*lay.scale {
			t.Fatalf("%v: quiet zone %d is not %d whole modules",
				size, lay.quiet, posterQRQuietModules)
		}
		// Must sit fully inside the poster, or part of the code is cropped away.
		if lay.x < 0 || lay.y < 0 ||
			lay.x+lay.card > b.Max.X || lay.y+lay.card > b.Max.Y {
			t.Fatalf("%v: card at (%d,%d)+%d falls outside the poster",
				size, lay.x, lay.y, lay.card)
		}
	}
}

// Best-effort contract: every failure hands BACK the original image, because a
// poster with no QR is still a poster, while nothing at all is a paid
// generation the organizer never receives.
func TestAddPosterQRFailuresReturnTheOriginalImage(t *testing.T) {
	src := fakePoster(t, 900, 1200)
	cases := []struct {
		name string
		img  []byte
		url  string
	}{
		{"no url", src, "  "},
		{"no image", nil, registrationURLFor("e1")},
		{"undecodable image", []byte("not an image"), registrationURLFor("e1")},
		{"poster too small to scan", fakePoster(t, 80, 80), registrationURLFor("e1")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _, err := addPosterQR(c.img, "image/png", c.url)
			if err == nil {
				t.Fatal("expected an error the caller can log")
			}
			if !bytes.Equal(out, c.img) {
				t.Error("the original image must be handed back untouched")
			}
		})
	}
}

// The studio's link is typed by hand and ends up PRINTED, so what it accepts
// and refuses is pinned here rather than left to a regex nobody re-reads.
func TestNormalizePosterQRURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"https://planmypickle.com/e/abc", "https://planmypickle.com/e/abc"},
		{"http://example.org/x", "http://example.org/x"},
		// The common case: they type the domain and mean the web.
		{"planmypickle.com", "https://planmypickle.com"},
		{"  planmypickle.com/e/1  ", "https://planmypickle.com/e/1"},
	}
	for _, c := range ok {
		got, err := normalizePosterQRURL(c.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
	bad := []string{
		"",
		"   ",
		// A QR is unreadable to a human, so they cannot see what they are about
		// to hand out — anything that isn't a plain web link is refused.
		"javascript:alert(1)",
		"mailto:someone@example.com",
		"tel:+16195551234",
		"data:text/html,<h1>hi</h1>",
		"not a url",
		"https://has space.com/x",
		"localhost", // no dot: not a real public host
		"https://x", // ditto
	}
	for _, in := range bad {
		if got, err := normalizePosterQRURL(in); err == nil {
			t.Errorf("%q should have been refused, got %q", in, got)
		}
	}
	// Too long to scan from a wall.
	if _, err := normalizePosterQRURL("https://e.com/" + strings.Repeat("a", 600)); err == nil {
		t.Error("an over-long link should be refused")
	}
}
