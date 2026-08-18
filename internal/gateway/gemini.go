package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Gemini image generation (Nano Banana Pro), for AI event posters.
//
// Small on purpose: one call in, one image out. The interesting work — deriving
// the prompt from event data — lives in the service layer where the data is;
// this file only knows how to talk to the API.
//
// INERT until GEMINI_API_KEY is set: Configured() is false and the service
// answers "posters aren't enabled" instead of erroring mid-generate.

// geminiImageModel is overridable because image models version quickly and a
// rename should be a Railway edit, not a deploy.
func geminiImageModel() string {
	if m := strings.TrimSpace(os.Getenv("GEMINI_IMAGE_MODEL")); m != "" {
		return m
	}
	return "gemini-3-pro-image"
}

// GeminiConfigured reports whether poster generation can run at all.
func GeminiConfigured() bool {
	return strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != ""
}

// geminiHTTP allows generous time: generation runs 2-5s normally, but the
// budget covers a slow tail rather than failing a poster at the 90th
// percentile.
var geminiHTTP = &http.Client{Timeout: 60 * time.Second}

// GenerateImage renders one image from a text prompt, optionally conditioned on
// reference images (the club's logo). Returns raw image bytes + a MIME type.
func GenerateImage(prompt string, refs [][]byte) ([]byte, string, error) {
	key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if key == "" {
		return nil, "", errors.New("GEMINI_API_KEY is not set")
	}
	parts := []map[string]any{{"text": prompt}}
	for _, r := range refs {
		if len(r) == 0 {
			continue
		}
		parts = append(parts, map[string]any{
			"inline_data": map[string]any{
				// The API sniffs actual content; PNG covers our only ref source
				// (club logos are stored as PNG by the crop dialog).
				"mime_type": "image/png",
				"data":      base64.StdEncoding.EncodeToString(r),
			},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"contents": []map[string]any{{"parts": parts}},
		"generationConfig": map[string]any{
			// TEXT must accompany IMAGE — Gemini's image models reject an
			// image-only modality request; the text part is simply ignored.
			"responseModalities": []string{"TEXT", "IMAGE"},
		},
	})
	if err != nil {
		return nil, "", err
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/" +
		geminiImageModel() + ":generateContent"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)

	resp, err := geminiHTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("gemini: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Error bodies name the real cause (bad key, quota, model renamed) —
		// truncated, because they can also be long.
		msg := string(raw)
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return nil, "", fmt.Errorf("gemini http %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("gemini: unreadable response: %w", err)
	}
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData != nil && p.InlineData.Data != "" {
				img, derr := base64.StdEncoding.DecodeString(p.InlineData.Data)
				if derr != nil {
					return nil, "", fmt.Errorf("gemini: bad image payload: %w", derr)
				}
				mime := p.InlineData.MimeType
				if mime == "" {
					mime = "image/png"
				}
				return img, mime, nil
			}
		}
	}
	// A 200 with no image usually means the prompt tripped a safety filter.
	return nil, "", errors.New(
		"the model returned no image — try a different style")
}
