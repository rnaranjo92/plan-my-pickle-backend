package service

import (
	"errors"
	"fmt"
	"hash/crc32"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// validWatermarkPositions are the placements the in-app editor offers.
var validWatermarkPositions = map[string]bool{
	"center": true, "top-left": true, "top-right": true,
	"bottom-left": true, "bottom-right": true, "tiled": true,
}

// SetSponsorWatermarkImage uploads an event's sponsor watermark image to the
// public avatars bucket and RETURNS its URL. It does NOT make it live — the URL
// is stamped onto the event only when the organizer Saves (via
// SetSponsorWatermarkSettings), so picking an image then backing out of the
// editor changes nothing on the live surfaces. Owner-only via the route. JPEG/PNG
// up to 5 MB.
func (s *Service) SetSponsorWatermarkImage(eventID, contentType string, data []byte) (string, error) {
	var ext string
	switch contentType {
	case "image/jpeg", "image/jpg":
		contentType, ext = "image/jpeg", "jpg"
	case "image/png":
		ext = "png"
	default:
		return "", errors.New("watermark must be a JPEG or PNG")
	}
	if len(data) == 0 {
		return "", errors.New("empty watermark")
	}
	if len(data) > 5*1024*1024 {
		return "", errors.New("watermark too large (max 5 MB)")
	}
	// Unique path per image (content hash) so an upload never overwrites the
	// currently-live watermark — the new image goes live only when the organizer
	// Saves. A replaced/abandoned object is left in the bucket (small; acceptable).
	path := fmt.Sprintf("event-watermark-%s-%08x.%s",
		eventID, crc32.ChecksumIEEE(data), ext)
	url, err := s.sb.StorageUpload("avatars", path, contentType, data)
	if err != nil {
		return "", err
	}
	return url, nil
}

// SetSponsorWatermarkSettings commits the watermark on Save: it stamps the chosen
// image url (making it live) atomically with its placement (opacity 0–1, scale
// 0.1–1, a validated position). An empty url leaves the existing image untouched
// (a settings-only re-save). Owner-only by route.
func (s *Service) SetSponsorWatermarkSettings(eventID, url string, opacity, scale float64, position string) error {
	opacity = clampF(opacity, 0, 1)
	scale = clampF(scale, 0.1, 1)
	position = strings.TrimSpace(position)
	if !validWatermarkPositions[position] {
		position = "center"
	}
	patch := map[string]any{
		"sponsor_watermark_opacity":  opacity,
		"sponsor_watermark_position": position,
		"sponsor_watermark_scale":    scale,
	}
	if u := strings.TrimSpace(url); u != "" {
		patch["sponsor_watermark_url"] = u
	}
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID), patch)
	return err
}

// ClearSponsorWatermark removes the watermark image (the placement config is kept,
// so re-adding an image restores the previous look). Owner-only by route.
func (s *Service) ClearSponsorWatermark(eventID string) error {
	_, err := s.sb.Update("events", "id=eq."+store.Q(eventID),
		map[string]any{"sponsor_watermark_url": nil})
	return err
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// asFloatOr reads a numeric column, falling back to d when null/absent.
func asFloatOr(m map[string]any, k string, d float64) float64 {
	if p := asFloatPtr(m, k); p != nil {
		return *p
	}
	return d
}
