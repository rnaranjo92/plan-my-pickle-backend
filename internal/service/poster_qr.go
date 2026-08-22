package service

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	neturl "net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// A QR code on the poster that opens the event's registration form.
//
// ── THE RULE THIS FILE EXISTS TO ENFORCE ────────────────────────────────────
// The QR is COMPOSITED here, in Go, onto the finished image. It is never asked
// of the image model.
//
// A QR is an exact geometric encoding: every module has to land on its cell, or
// it is not a QR, it is a picture of one. Nano Banana Pro letters TEXT
// convincingly — which is the whole reason this feature works — and it will
// just as convincingly draw a grid of squares that no phone can read. That
// failure is invisible on screen and discovered by the organizer AFTER printing
// two hundred flyers, which makes a fake QR strictly worse than no QR.
//
// The prompt's job is only to leave the corner clear (posterQRReservation); the
// white card drawn here guarantees the code scans whether it obeyed or not.

// posterQRFraction is the QR card's width as a share of the poster's width.
//
// ~22% puts the code near 2cm on a printed A4/Letter flyer, which is about the
// floor for a phone at arm's length. Bigger starts eating the artwork.
const posterQRFraction = 0.22

// posterQRMarginFraction is the gap from the poster's edges, also relative to
// width, so the placement holds at any resolution the model returns.
const posterQRMarginFraction = 0.04

// posterQRQuietModules is the mandatory clear margin around the code, measured
// in QR modules. The spec says four; scanners genuinely fail without it, which
// is why the code is never drawn straight onto artwork.
const posterQRQuietModules = 4

// registrationURLFor is what the code encodes: the app's public
// self-registration form for this event.
//
// ⚠️ A PRINTED QR IS PERMANENT. Once a flyer is on a noticeboard we can never
// change where it points, so this must be a route we are prepared to keep
// forever. `/?register=<id>` is that route — main.dart has served it since
// self-registration shipped, it needs no session, and the id never changes.
// Do NOT swap this for a short link or a redirector that might later be re-mapped.
func registrationURLFor(eventID string) string {
	return "https://app.planmypickle.com/?register=" + eventID
}

// normalizePosterQRURL vets a link typed into the Poster Studio, where there is
// no event to derive one from.
//
// Stricter than it looks, because the result is PRINTED and permanent. A typo
// is not recoverable once the flyer is on a wall, so a bare "planmypickle.com"
// gets https:// rather than being rejected, and anything that isn't a plain
// web link is refused outright — a QR is unreadable to a human, so they cannot
// see what they are about to hand out. Non-http schemes are exactly how a code
// ends up doing something the person holding it did not expect.
func normalizePosterQRURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", errors.New("add the link the QR code should open")
	}
	if len(u) > 512 {
		// Long inputs force a denser code; past a point it stops scanning from
		// a poster on a wall, which is the only place it will ever be used.
		return "", errors.New("that link is too long to scan reliably")
	}
	if strings.ContainsAny(u, " \t\n\r") {
		return "", errors.New("a link can't contain spaces")
	}
	lower := strings.ToLower(u)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		// Refuse ANY other scheme, and note that not all of them carry "://" —
		// mailto:, tel: and javascript: don't, so a "://" test alone let them
		// through and then prepended https:// to them.
		if hasURLScheme(lower) {
			return "", errors.New("the QR code can only open a web link")
		}
		u = "https://" + u // the common case: they typed the domain
	}
	parsed, err := neturl.Parse(u)
	if err != nil || parsed.Host == "" || !strings.Contains(parsed.Host, ".") {
		return "", errors.New("that doesn't look like a web address")
	}
	return u, nil
}

// hasURLScheme reports whether the string opens with a URI scheme — "scheme:"
// per RFC 3986, where a scheme is a letter followed by letters, digits, +, -
// or . and the colon comes before any slash.
//
// Written out rather than leaning on "://" because the schemes that matter
// most here don't use the slashes: mailto:, tel:, javascript:, data:.
func hasURLScheme(s string) bool {
	for i, r := range s {
		switch {
		case r == ':':
			return i > 0
		case r == '/' || r == '?' || r == '#':
			return false // path/query started; no scheme
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '+', r == '-', r == '.':
			if i == 0 && !(r >= 'a' && r <= 'z') {
				return false // a scheme must START with a letter
			}
		default:
			return false
		}
	}
	return false
}

// posterQRReservation is appended to the model's brief when a QR is coming, so
// it composes with a hole rather than having one punched through its focal
// point. Best-effort by nature — the model may ignore it, and the opaque card
// below is what makes that survivable rather than a broken poster.
const posterQRReservation = posterQRReservationPlain +
	" Letter the words SCAN TO REGISTER in small type immediately to the LEFT " +
	"of that area."

// posterQRReservationPlain reserves the space WITHOUT captioning it — used by
// the Poster Studio, where the organizer supplies an arbitrary link and we have
// no idea whether it leads to a registration form, a venue map or a sponsor.
// Lettering "SCAN TO REGISTER" over a link that doesn't register anyone would
// be the model stating a fact we made up, which is the one thing the whole
// prompt is built to prevent.
const posterQRReservationPlain = " Leave the BOTTOM-RIGHT corner visually " +
	"quiet: a clear area about one quarter of the poster's width, free of text, " +
	"faces and busy detail, because a QR code is placed there afterwards."

// qrLayout is where the code lands and how big its parts are, in pixels.
type qrLayout struct {
	x, y  int // top-left of the white card
	card  int // card side (code + quiet zone on all four sides)
	scale int // pixels per QR module
	quiet int // quiet-zone thickness
}

// posterQRLayout computes the placement. Pure and shared with the test, because
// the arithmetic is the subtle part: the card is rounded DOWN to a whole number
// of modules, so it is always smaller than the nominal fraction, and a test that
// re-derives it from posterQRFraction probes the artwork instead of the card.
//
// The rounding is the point. A QR scaled to a fractional module size lands its
// cells on partial pixels, and the resampling blurs precisely the edges a
// scanner looks for — a code that looks flawless and reads as nothing.
func posterQRLayout(b image.Rectangle, modules int) qrLayout {
	nominal := int(float64(b.Dx()) * posterQRFraction)
	if nominal < 120 {
		nominal = 120
	}
	scale := nominal / (modules + posterQRQuietModules*2)
	if scale < 1 {
		scale = 1
	}
	quiet := posterQRQuietModules * scale
	card := modules*scale + quiet*2
	margin := int(float64(b.Dx()) * posterQRMarginFraction)
	x := b.Max.X - margin - card
	y := b.Max.Y - margin - card
	if x < b.Min.X {
		x = b.Min.X
	}
	if y < b.Min.Y {
		y = b.Min.Y
	}
	return qrLayout{x: x, y: y, card: card, scale: scale, quiet: quiet}
}

// addPosterQR draws the registration QR onto a finished poster.
//
// Returns PNG bytes regardless of what came in: the code is hard geometry and
// JPEG's blocking eats the module edges that scanners rely on, so re-encoding
// as JPEG to preserve the input format would be trading the only thing that
// matters here for a smaller file.
//
// Best-effort by contract. Every failure path returns the ORIGINAL image and an
// error for the caller to log: a poster with no QR is a poster, while no poster
// at all is a paid generation the organizer never receives.
func addPosterQR(img []byte, mime, url string) ([]byte, string, error) {
	if len(img) == 0 {
		return img, mime, errors.New("no image to draw on")
	}
	if strings.TrimSpace(url) == "" {
		return img, mime, errors.New("no registration url")
	}
	src, err := decodePoster(img, mime)
	if err != nil {
		return img, mime, err
	}
	b := src.Bounds()
	if b.Dx() < 200 || b.Dy() < 200 {
		// Too small to carry a scannable code; better untouched than defaced.
		return img, mime, errors.New("poster too small for a QR")
	}

	// Highest error correction we can afford. A poster gets printed, taped to a
	// wall, curled, and photographed at an angle in bad light; Highest survives
	// roughly 30% damage, and the cost is a denser code, not a bigger one.
	qr, err := qrcode.New(url, qrcode.Highest)
	if err != nil {
		return img, mime, err
	}
	qr.DisableBorder = true // we draw the quiet zone ourselves, sized exactly

	modules := len(qr.Bitmap()) // no border — disabled above
	if modules <= 0 {
		return img, mime, errors.New("empty qr bitmap")
	}
	lay := posterQRLayout(b, modules)

	out := image.NewRGBA(b)
	draw.Draw(out, b, src, b.Min, draw.Src)
	originX, originY, cardPx, scale, quiet :=
		lay.x, lay.y, lay.card, lay.scale, lay.quiet

	// The opaque white card IS the quiet zone. This is what makes the code scan
	// over any artwork the model produced, obedient corner or not.
	draw.Draw(out,
		image.Rect(originX, originY, originX+cardPx, originY+cardPx),
		image.NewUniform(color.White), image.Point{}, draw.Src)

	// Paint modules by hand rather than scaling qr.Image(): every dark module
	// becomes an exact scale×scale block of pixels, with no interpolation
	// anywhere near a module boundary.
	bmp := qr.Bitmap()
	for y, row := range bmp {
		for x, on := range row {
			if !on {
				continue
			}
			px := originX + quiet + x*scale
			py := originY + quiet + y*scale
			draw.Draw(out, image.Rect(px, py, px+scale, py+scale),
				image.NewUniform(color.Black), image.Point{}, draw.Src)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return img, mime, err
	}
	return buf.Bytes(), "image/png", nil
}

// decodePoster reads whatever the model returned. The mime is a hint only —
// image.Decode sniffs the real format, because a wrong hint should not cost the
// organizer their poster.
func decodePoster(img []byte, mime string) (image.Image, error) {
	if strings.Contains(mime, "png") {
		if m, err := png.Decode(bytes.NewReader(img)); err == nil {
			return m, nil
		}
	}
	if strings.Contains(mime, "jpeg") || strings.Contains(mime, "jpg") {
		if m, err := jpeg.Decode(bytes.NewReader(img)); err == nil {
			return m, nil
		}
	}
	m, _, err := image.Decode(bytes.NewReader(img))
	return m, err
}
