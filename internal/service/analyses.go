package service

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// analysisPriceCents is the à-la-carte price for one PB Vision Match Video
// Analysis. PB Vision bills us ~$8/hr of HD footage; this covers that plus margin.
const analysisPriceCents = 1299 // $12.99

// analysisAvailable reports whether the paid Match Video Analysis feature is live:
// the table exists (migration ran) and the PB Vision partner API is configured.
func (s *Service) analysisAvailable() bool {
	return s.columnReady("video_analyses", "id") && s.PBV != nil && s.PBV.Configured()
}

// StartAnalysisCheckout records a pending analysis and returns a Stripe Checkout
// URL. On payment, the checkout.session.completed webhook routes to
// markAnalysisPaid (via the analysis_id metadata) which submits the video to PB
// Vision. This is a direct platform charge — PlanMyPickle keeps the full fee.
func (s *Service) StartAnalysisCheckout(userID, email string, req model.AnalysisCheckoutRequest) (string, error) {
	if !s.analysisAvailable() {
		return "", errors.New("match video analysis isn't available yet")
	}
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	videoURL := strings.TrimSpace(req.VideoURL)
	if videoURL == "" {
		return "", errors.New("a video is required")
	}

	// PB Vision accepts up to 4 player emails for the shared report.
	emails := make([]string, 0, 4)
	for _, e := range req.PartnerEmails {
		if e = strings.TrimSpace(e); e != "" {
			emails = append(emails, e)
			if len(emails) == 4 {
				break
			}
		}
	}

	ins, err := s.sb.Insert("video_analyses", map[string]any{
		"user_id":        userID,
		"video_url":      videoURL,
		"name":           orNull(strings.TrimSpace(req.Name)),
		"court":          orNull(strings.TrimSpace(req.Court)),
		"partner_emails": emails,
		"status":         "pending_payment",
		"amount_cents":   analysisPriceCents,
		"currency":       "usd",
	})
	if err != nil {
		return "", err
	}
	if len(ins) == 0 {
		return "", errors.New("could not start the analysis")
	}
	id := asStr(ins[0], "id")

	url, err := gw.CreatePlatformCheckout(id, "analysis_id", analysisPriceCents, "usd",
		"PlanMyPickle — Match Video Analysis", email, req.SuccessURL, req.CancelURL)
	if err != nil {
		return "", err
	}
	return url, nil
}

// markAnalysisPaid runs when the Stripe checkout for an analysis completes. It
// flips the row to processing and submits the video to PB Vision, storing the
// returned video id so the completion webhook can correlate it. Payment already
// succeeded, so a PB Vision submit failure marks the row failed but still acks
// the Stripe webhook (returns nil) — we resolve those manually (retry/refund).
func (s *Service) markAnalysisPaid(analysisID string) error {
	row, err := s.sb.SelectOne("video_analyses",
		"id=eq."+store.Q(analysisID)+"&select=id,status,video_url,name,court,partner_emails")
	if err != nil {
		return err
	}
	if row == nil {
		return nil // row deleted — nothing to do, ack the webhook
	}
	if asStr(row, "status") != "pending_payment" {
		return nil // idempotent: a retried webhook, already processed
	}

	if s.PBV == nil || !s.PBV.Configured() {
		s.failAnalysis(analysisID, "analysis service not configured")
		return nil
	}

	meta := gateway.PBVideoMeta{
		Name:       asStr(row, "name"),
		Court:      asStr(row, "court"),
		UserEmails: asStrSlice(row, "partner_emails"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vid, err := s.PBV.AddVideoByURL(ctx, asStr(row, "video_url"), meta)
	if err != nil {
		log.Printf("pbvision: submit failed for analysis %s: %v", analysisID, err)
		s.failAnalysis(analysisID, "we couldn't submit your video for analysis — our team will follow up")
		return nil
	}

	_, err = s.sb.Update("video_analyses", "id=eq."+store.Q(analysisID), map[string]any{
		"status":     "processing",
		"pb_vid":     vid,
		"updated_at": now(),
	})
	return err
}

// failAnalysis records a terminal failure on an analysis row (best effort).
func (s *Service) failAnalysis(analysisID, reason string) {
	if _, err := s.sb.Update("video_analyses", "id=eq."+store.Q(analysisID), map[string]any{
		"status":     "failed",
		"error":      reason,
		"updated_at": now(),
	}); err != nil {
		log.Printf("pbvision: failed to mark analysis %s failed: %v", analysisID, err)
	}
}

// HandlePBVisionWebhook processes a PB Vision completion callback. PB Vision
// signs nothing, so we guard with a shared secret carried in the registered URL
// (?t=<PBVISION_WEBHOOK_TOKEN>); token is passed in from the query by the handler.
func (s *Service) HandlePBVisionWebhook(token string, payload []byte) error {
	want := strings.TrimSpace(os.Getenv("PBVISION_WEBHOOK_TOKEN"))
	if want == "" || token != want {
		return errors.New("unauthorized")
	}

	var p struct {
		Vid      string          `json:"vid"`
		Webpage  string          `json:"webpage"`
		Insights json.RawMessage `json:"insights"`
		Stats    json.RawMessage `json:"stats"`
		Error    *struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	if strings.TrimSpace(p.Vid) == "" {
		return nil // nothing to correlate — ack
	}

	row, err := s.sb.SelectOne("video_analyses",
		"pb_vid=eq."+store.Q(p.Vid)+"&select=id,user_id,status")
	if err != nil {
		return err
	}
	if row == nil {
		return nil // unknown video id — ack
	}
	id := asStr(row, "id")
	userID := asStr(row, "user_id")

	upd := map[string]any{"updated_at": now()}
	ready := false
	if p.Error != nil && strings.TrimSpace(p.Error.Reason) != "" {
		upd["status"] = "failed"
		upd["error"] = p.Error.Reason
	} else {
		upd["status"] = "ready"
		ready = true
		if p.Webpage != "" {
			upd["report_url"] = p.Webpage
		}
		if len(p.Insights) > 0 {
			upd["insights"] = p.Insights
		}
		if len(p.Stats) > 0 {
			upd["stats"] = p.Stats
		}
	}
	if _, err := s.sb.Update("video_analyses", "id=eq."+store.Q(id), upd); err != nil {
		return err
	}

	if ready {
		s.notifyUser(userID, "analysis_ready", "", "",
			"Your match video analysis is ready", "analysis:"+id)
	}
	return nil
}

// ListAnalyses returns a user's analyses, newest first.
func (s *Service) ListAnalyses(userID string) ([]model.VideoAnalysis, error) {
	if !s.columnReady("video_analyses", "id") {
		return []model.VideoAnalysis{}, nil
	}
	rows, err := s.sb.Select("video_analyses",
		"user_id=eq."+store.Q(userID)+
			"&order=created_at.desc&limit=100"+
			"&select=id,status,name,court,report_url,error,amount_cents,currency,created_at")
	if err != nil {
		return nil, err
	}
	out := make([]model.VideoAnalysis, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapVideoAnalysis(r))
	}
	return out, nil
}

// GetAnalysis returns one analysis owned by the user.
func (s *Service) GetAnalysis(userID, id string) (model.VideoAnalysis, error) {
	if !s.columnReady("video_analyses", "id") {
		return model.VideoAnalysis{}, ErrNotFound
	}
	row, err := s.sb.SelectOne("video_analyses",
		"id=eq."+store.Q(id)+"&user_id=eq."+store.Q(userID)+
			"&select=id,status,name,court,report_url,error,amount_cents,currency,created_at")
	if err != nil {
		return model.VideoAnalysis{}, err
	}
	if row == nil {
		return model.VideoAnalysis{}, ErrNotFound
	}
	return mapVideoAnalysis(row), nil
}

// RegisterPBVisionWebhook points PB Vision's completion callback at our public
// endpoint, embedding the shared secret in the URL. QA-only; call once after the
// key + token are set. publicBaseURL is the API origin (e.g. https://api.planmypickle.com).
func (s *Service) RegisterPBVisionWebhook(publicBaseURL string) error {
	if s.PBV == nil || !s.PBV.Configured() {
		return errors.New("pbvision not configured")
	}
	token := strings.TrimSpace(os.Getenv("PBVISION_WEBHOOK_TOKEN"))
	if token == "" {
		return errors.New("PBVISION_WEBHOOK_TOKEN not set")
	}
	url := strings.TrimRight(publicBaseURL, "/") + "/webhooks/pbvision?t=" + token
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return s.PBV.SetWebhook(ctx, url)
}

func mapVideoAnalysis(m map[string]any) model.VideoAnalysis {
	return model.VideoAnalysis{
		ID:          asStr(m, "id"),
		Status:      asStr(m, "status"),
		Name:        asStr(m, "name"),
		Court:       asStr(m, "court"),
		ReportURL:   asStr(m, "report_url"),
		Error:       asStr(m, "error"),
		AmountCents: asInt(m, "amount_cents"),
		Currency:    asStr(m, "currency"),
		CreatedAt:   asStr(m, "created_at"),
	}
}
