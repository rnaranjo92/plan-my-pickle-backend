package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// onesignalAppID is the PlanMyPickle OneSignal application id. It is public
// (the same id ships in the web/native clients), so hardcoding it is fine. The
// REST API key is secret and read from the environment instead.
const onesignalAppID = "75638436-5d06-4c8b-b84f-5e065421b668"

// pushHTTP is a short-timeout client for OneSignal. Push is best-effort, so we
// never want a slow/hung request to block the caller (e.g. "Start round").
var pushHTTP = &http.Client{Timeout: 10 * time.Second}

// SendTestPush fires a single diagnostic push to one user (their external_id).
// It returns an error ONLY when the notification couldn't be created or OneSignal
// reports no reachable subscription. NOTE: OneSignal's `recipients` count in the
// create response is unreliable for external_id/alias sends (aliases resolve to
// subscriptions asynchronously), so we do NOT gate on it — the reliable signal is
// whether OneSignal accepted the notification (an id, no `errors`).
func (s *Service) SendTestPush(externalID string) error {
	restKey := os.Getenv("ONESIGNAL_REST_API_KEY")
	if restKey == "" {
		return fmt.Errorf("push is not configured (no OneSignal key)")
	}
	body, _ := json.Marshal(map[string]any{
		"app_id":          onesignalAppID,
		"target_channel":  "push",
		"include_aliases": map[string]any{"external_id": []string{externalID}},
		"headings":        map[string]string{"en": "PlanMyPickle test 🥒"},
		"contents":        map[string]string{"en": "If you can see this, push notifications are working!"},
		"url":             "https://app.planmypickle.com",
	})
	req, err := http.NewRequest(http.MethodPost,
		"https://api.onesignal.com/notifications?c=push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Key "+restKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pushHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OneSignal HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		ID     string          `json:"id"`
		Errors json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(raw, &out)
	// A populated `errors` (e.g. "All included players are not subscribed") is
	// the genuine "no reachable device" signal.
	if e := strings.TrimSpace(string(out.Errors)); e != "" && e != "null" && e != "[]" && e != "{}" {
		return fmt.Errorf("no reachable subscription: %s", e)
	}
	return nil
}

// sendPush sends one bulk web/native push to the given OneSignal external_ids
// (each user's Supabase auth user id). It is intentionally best-effort: it logs
// and SWALLOWS all errors and never blocks the caller. It is a no-op when the
// REST key isn't configured or there are no recipients.
//
// url is optional — when non-empty it becomes the notification's launch URL.
func (s *Service) sendPush(externalIDs []string, heading, content, url string) error {
	restKey := os.Getenv("ONESIGNAL_REST_API_KEY")
	if restKey == "" || len(externalIDs) == 0 {
		return nil // not configured, or nobody to notify — no-op
	}

	body := map[string]any{
		"app_id":         onesignalAppID,
		"target_channel": "push",
		"include_aliases": map[string]any{
			"external_id": externalIDs,
		},
		"headings": map[string]string{"en": heading},
		"contents": map[string]string{"en": content},
	}
	if url != "" {
		body["url"] = url
	}

	payload, err := json.Marshal(body)
	if err != nil {
		log.Printf("onesignal: marshal payload failed: %v", err)
		return nil
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://api.onesignal.com/notifications?c=push", bytes.NewReader(payload))
	if err != nil {
		log.Printf("onesignal: build request failed: %v", err)
		return nil
	}
	req.Header.Set("Authorization", "Key "+restKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := pushHTTP.Do(req)
	if err != nil {
		log.Printf("onesignal: send failed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		log.Printf("onesignal: send rejected (http %d): %s", resp.StatusCode, raw)
	}
	return nil
}
