package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// PBVision is the live PB Vision partner-API gateway: submit a match video for
// AI analysis and register a completion webhook. Wired in from main ONLY when
// PBVISION_API_KEY is set; otherwise the whole "Match Video Analysis" feature
// stays dormant (nil gateway).
//
// Auth: x-api-key header. Prod base: https://api-2o2klzx4pa-uc.a.run.app
// (the SDK's useProdServer:true). Endpoints:
//   - POST /partner/add_video_by_url  {url, userEmails[], name, court, ...} -> {vid}
//   - POST /partner/webhook/set       {url}
// On completion PB Vision POSTs {from_url, webpage, insights, stats, vid, error}
// to the registered webhook — with NO signature, so our receiver is guarded by a
// secret token embedded in the registered URL (PBVISION_WEBHOOK_TOKEN).
type PBVision struct {
	apiKey  string
	baseURL string // no trailing slash
	http    *http.Client
}

const pbvProdBase = "https://api-2o2klzx4pa-uc.a.run.app"

// NewPBVision builds the gateway. baseURL empty -> prod default.
func NewPBVision(apiKey, baseURL string) *PBVision {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = pbvProdBase
	}
	return &PBVision{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Configured reports whether a real API key is set.
func (p *PBVision) Configured() bool { return p != nil && p.apiKey != "" }

// PBVideoMeta is the optional metadata attached when submitting a video.
type PBVideoMeta struct {
	UserEmails     []string
	Name           string
	Desc           string
	Facility       string
	Court          string
	GameStartEpoch int64
}

// AddVideoByURL submits a video (PB Vision downloads it from videoURL) and
// returns the PB Vision video id (vid), which correlates the completion webhook
// back to our analysis record.
func (p *PBVision) AddVideoByURL(ctx context.Context, videoURL string, meta PBVideoMeta) (string, error) {
	if !p.Configured() {
		return "", errors.New("pbvision: not configured")
	}
	body := map[string]any{"url": videoURL}
	if len(meta.UserEmails) > 0 {
		body["userEmails"] = meta.UserEmails
	}
	if meta.Name != "" {
		body["name"] = meta.Name
	}
	if meta.Desc != "" {
		body["desc"] = meta.Desc
	}
	if meta.Facility != "" {
		body["facility"] = meta.Facility
	}
	if meta.Court != "" {
		body["court"] = meta.Court
	}
	if meta.GameStartEpoch > 0 {
		body["gameStartEpoch"] = meta.GameStartEpoch
	}
	var out struct {
		Vid string `json:"vid"`
	}
	if err := p.post(ctx, "/partner/add_video_by_url", body, &out); err != nil {
		return "", err
	}
	if out.Vid == "" {
		return "", errors.New("pbvision: add_video_by_url returned no vid")
	}
	return out.Vid, nil
}

// GetVideoResult is a best-effort snapshot of a submitted video's analysis,
// mirroring the completion-webhook payload.
type GetVideoResult struct {
	Webpage  string
	Insights json.RawMessage
	Stats    json.RawMessage
	Error    string
}

// GetVideo fetches a video's current analysis by vid — used to resolve an
// already-processed (duplicate) submission immediately instead of waiting for a
// completion webhook that won't fire a second time. STRICTLY best-effort: the
// partner API is primarily webhook-driven, so this path may not exist. Override
// it with PBVISION_GET_VIDEO_PATH; any error / still-processing / unknown shape
// returns ok=false and the caller falls back to the webhook.
func (p *PBVision) GetVideo(ctx context.Context, vid string) (GetVideoResult, bool) {
	if !p.Configured() || strings.TrimSpace(vid) == "" {
		return GetVideoResult{}, false
	}
	path := strings.TrimSpace(os.Getenv("PBVISION_GET_VIDEO_PATH"))
	if path == "" {
		path = "/partner/get_video"
	}
	var out struct {
		Status   string          `json:"status"`
		Webpage  string          `json:"webpage"`
		Insights json.RawMessage `json:"insights"`
		Stats    json.RawMessage `json:"stats"`
		Error    string          `json:"error"`
	}
	if err := p.post(ctx, path, map[string]any{"vid": vid}, &out); err != nil {
		return GetVideoResult{}, false // endpoint absent or call failed
	}
	st := strings.ToLower(strings.TrimSpace(out.Status))
	done := out.Error != "" || len(out.Insights) > 0 || out.Webpage != "" ||
		st == "completed" || st == "complete" || st == "ready" ||
		st == "done" || st == "failed"
	if !done {
		return GetVideoResult{}, false // still processing — wait for the webhook
	}
	return GetVideoResult{
		Webpage:  out.Webpage,
		Insights: out.Insights,
		Stats:    out.Stats,
		Error:    out.Error,
	}, true
}

// SetWebhook registers the completion-webhook URL with PB Vision (one-time; the
// URL should carry the shared secret token so our receiver can verify callers).
func (p *PBVision) SetWebhook(ctx context.Context, webhookURL string) error {
	if !p.Configured() {
		return errors.New("pbvision: not configured")
	}
	return p.post(ctx, "/partner/webhook/set", map[string]any{"url": webhookURL}, nil)
}

func (p *PBVision) post(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pbvision %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	if out != nil && len(rb) > 0 {
		if err := json.Unmarshal(rb, out); err != nil {
			return err
		}
	}
	return nil
}
