package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// onesignalAppID is the PlanMyPickle OneSignal application id. It is public
// (the same id ships in the web/native clients), so hardcoding it is fine. The
// REST API key is secret and read from the environment instead.
const onesignalAppID = "75638436-5d06-4c8b-b84f-5e065421b668"

// pushHTTP is a short-timeout client for OneSignal. Push is best-effort, so we
// never want a slow/hung request to block the caller (e.g. "Start round").
var pushHTTP = &http.Client{Timeout: 10 * time.Second}

// SendTestPush fires a single diagnostic push to one user (their external_id)
// and returns how many device subscriptions OneSignal actually targeted. A
// return of 0 means the user has NO reachable subscription (device didn't
// register, notifications denied, or — on Android — FCM isn't configured in
// OneSignal). Unlike sendPush this surfaces errors so a QA button can report them.
func (s *Service) SendTestPush(externalID string) (int, error) {
	restKey := os.Getenv("ONESIGNAL_REST_API_KEY")
	if restKey == "" {
		return 0, fmt.Errorf("push is not configured (no OneSignal key)")
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
		return 0, err
	}
	req.Header.Set("Authorization", "Key "+restKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pushHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("OneSignal HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Recipients int `json:"recipients"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Recipients, nil
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
