package service

import (
	"bytes"
	"encoding/json"
	"errors"
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
// [subID], when the client supplies one, addresses that DEVICE directly and
// bypasses alias resolution entirely — see sendTestPushContent.
func (s *Service) SendTestPush(externalID, subID string) error {
	return s.sendTestPushRetrying(externalID, subID, "PlanMyPickle test 🥒",
		"If you can see this, push notifications are working!")
}

// sendTestPushRetrying retries once when OneSignal reports no such alias.
//
// OneSignal.login() is fire-and-forget on the client, and the readiness check
// there polls the DEVICE's subscription state — which proves a subscription
// exists, not that the external_id alias has propagated to OneSignal's servers.
// So the app can honestly report "ready" while the alias write is still in
// flight, and a send that lands in that window fails with invalid_aliases.
//
// A single retry after a pause covers the propagation gap. If it fails twice
// the alias genuinely isn't there, which is a different problem and now
// distinguishable in the logs rather than guessed at.
func (s *Service) sendTestPushRetrying(externalID, subID, heading, content string) error {
	err := s.sendTestPushContent(externalID, subID, heading, content)
	// A subscription-id send has no alias to propagate, so there is nothing a
	// retry could fix — the id either addresses a live subscription or doesn't.
	if err == nil || subID != "" || !errors.Is(err, ErrNoPushSubscription) {
		return err
	}
	log.Printf("push: alias %s not found, retrying once after propagation delay",
		externalID)
	time.Sleep(3 * time.Second)
	if err2 := s.sendTestPushContent(externalID, subID, heading, content); err2 != nil {
		log.Printf("push: alias %s still missing after retry — the account link "+
			"never completed, not a race", externalID)
		return err2
	}
	log.Printf("push: alias %s resolved on retry (propagation race)", externalID)
	return nil
}

// SendRotationTestPush sends a SAMPLE rotation-round notification to the caller,
// so an organizer can confirm delivery + see the exact format their players get.
func (s *Service) SendRotationTestPush(externalID string) error {
	return s.sendTestPushContent(externalID, "", "PlanMyPickle 🎾",
		"Round 1 — head to Court 2. (Test — your players get one like this each round.)")
}

// sendTestPushContent sends a single diagnostic push to one external id with the
// given heading/content, returning a descriptive error when push is unconfigured
// or the device isn't reachable (so the UI can guide the organizer).
// PushConfigured reports whether a OneSignal REST key is present. The boolean
// only — a health endpoint must never echo a credential.
func PushConfigured() bool {
	return strings.TrimSpace(os.Getenv("ONESIGNAL_REST_API_KEY")) != ""
}

// ErrNoPushSubscription: the send didn't fail, there is simply no device
// registered for this account. A distinct error so the API can say 409 instead
// of 500 — "we broke" and "there's nothing to send to" are different answers
// and only one of them is our fault.
var ErrNoPushSubscription = errors.New(
	"no device is signed in to notifications for this account yet")

func (s *Service) sendTestPushContent(externalID, subID, heading, content string) error {
	restKey := os.Getenv("ONESIGNAL_REST_API_KEY")
	if restKey == "" {
		return fmt.Errorf("push is not configured (no OneSignal key)")
	}
	payload := map[string]any{
		"app_id":         onesignalAppID,
		"target_channel": "push",
		"headings":       map[string]string{"en": heading},
		"contents":       map[string]string{"en": content},
		// web_url, NOT url. `url` applies to every platform, so on a phone it
		// launches the BROWSER — tapping a notification from the app to be
		// dropped into a web copy of the app is a strange thing to do to
		// someone. Web push has nowhere else to go, so it keeps the link; on
		// native, no URL means the app simply opens.
		"web_url": "https://app.planmypickle.com",
	}
	// Address the DEVICE when we know it, the person otherwise.
	//
	// An external_id has to resolve through OneSignal's identity model, and that
	// model can be wedged: a login-user 409 against a user that already holds the
	// external_id pauses the SDK's op queue, leaving a live subscription owned by
	// no addressable user. The send is then accepted and delivered to nobody —
	// silently, because from OneSignal's side the alias did resolve, to a user
	// with no subscriptions.
	//
	// A subscription id has no such indirection. When the client hands us one it
	// has just verified the subscription is opted in, so it is strictly the
	// better address for a diagnostic whose entire job is answering "can this
	// browser receive a push at all".
	if subID != "" {
		payload["include_subscription_ids"] = []string{subID}
	} else {
		payload["include_aliases"] = map[string]any{
			"external_id": []string{externalID},
		}
	}
	body, _ := json.Marshal(payload)
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
		// invalid_aliases means OneSignal has never seen a SUBSCRIPTION for this
		// account — the send didn't fail, there's simply nothing to send to.
		// Saying that plainly beats what this used to do, which was paste
		// OneSignal's raw error JSON into a snackbar: the organizer got their own
		// user id repeated half a dozen times and no idea what to do about it.
		if strings.Contains(e, "invalid_aliases") {
			return ErrNoPushSubscription
		}
		// "All included players are not subscribed" is the same situation with
		// different wording: the alias exists, the device isn't subscribed.
		if strings.Contains(e, "not subscribed") {
			return fmt.Errorf("%w (device found, but not subscribed)",
				ErrNoPushSubscription)
		}
		return fmt.Errorf("no reachable subscription: %s", e)
	}
	return nil
}

// pushSound names a custom notification sound to play on each platform. iOS
// wants the bundled filename WITH extension (e.g. "court_call.caf"); Android
// wants the res/raw resource name WITHOUT extension (e.g. "court_call"). Both are
// harmless when the client build doesn't bundle the file — the OS falls back to
// its default sound — so the backend can ship this ahead of the native builds.
type pushSound struct {
	iOS     string
	android string
}

// courtCallSound is the custom tone for match-start "court call" pushes. It only
// plays once the files are bundled natively (iOS: court_call.caf in the app;
// Android: res/raw/court_call); until then clients use the default sound. Web
// pushes ignore it entirely (browsers use the default).
var courtCallSound = pushSound{iOS: "court_call.caf", android: "court_call"}

// sendPush sends one bulk web/native push to the given OneSignal external_ids
// (each user's Supabase auth user id). It is intentionally best-effort: it logs
// and SWALLOWS all errors and never blocks the caller. It is a no-op when the
// REST key isn't configured or there are no recipients.
//
// url is optional — when non-empty it becomes the notification's launch URL.
// sendPushToEveryoneAt9am broadcasts to all subscribers, delivered at 9am in
// EACH subscriber's own timezone.
//
// OneSignal resolves the timezone per device (delayed_option=timezone), so
// nothing here has to know or store where anybody is. Segment-targeted rather
// than a list of external ids: this goes to everyone, and naming every user to
// say one sentence is a lot of payload for nothing.
func (s *Service) sendPushToEveryoneAt9am(heading, content string) error {
	restKey := os.Getenv("ONESIGNAL_REST_API_KEY")
	if restKey == "" {
		return nil // not configured — no-op, same as every other push path
	}
	body := map[string]any{
		"app_id":            onesignalAppID,
		"target_channel":    "push",
		"included_segments": []string{"Subscribed Users"},
		"headings":          map[string]string{"en": heading},
		"contents":          map[string]string{"en": content},
		// Per-subscriber local time. Without these two it sends immediately,
		// which for a good-morning message means 2am for a third of them.
		"delayed_option":       "timezone",
		"delivery_time_of_day": "9:00AM",
	}
	_, err := s.postPush(restKey, body)
	return err
}

func (s *Service) sendPush(externalIDs []string, heading, content, url string) error {
	return s.sendPushSound(externalIDs, heading, content, url, pushSound{})
}

// sendPushSound is sendPush with an optional custom notification sound (see
// pushSound). sendPush delegates here with an empty sound (default tone).
func (s *Service) sendPushSound(externalIDs []string, heading, content, url string, sound pushSound) error {
	restKey := os.Getenv("ONESIGNAL_REST_API_KEY")
	if restKey == "" || len(externalIDs) == 0 {
		return nil // not configured, or nobody to notify — no-op
	}

	// Build the shared half of the payload once; only the targeting differs.
	base := func() map[string]any {
		b := map[string]any{
			"app_id":         onesignalAppID,
			"target_channel": "push",
			"headings":       map[string]string{"en": heading},
			"contents":       map[string]string{"en": content},
		}
		if url != "" {
			// web_url, NOT url. `url` applies to every platform, so on a phone
			// OneSignal hands the link to the BROWSER — tapping "someone
			// registered for your tournament" bounced people out of the app and
			// into a web copy of it.
			//
			// Browsers keep the link (they have nowhere else to go). Native gets
			// no URL, so the tap simply opens the app. The link also rides in
			// `data` so a click handler can route on it once the app has a
			// runtime router — today it opens to wherever the app was.
			b["web_url"] = url
			b["data"] = map[string]any{"link": url}
		}
		// Custom per-platform sound (no-op on web; falls back to default until
		// the native build bundles the file).
		if sound.iOS != "" {
			b["ios_sound"] = sound.iOS
		}
		if sound.android != "" {
			b["android_sound"] = sound.android
		}
		return b
	}

	// Prefer device ids, fall back to aliases — and do BOTH when the recipients
	// are split, which is the normal case while devices are still recording
	// themselves.
	//
	// Two requests rather than one: OneSignal takes a single targeting method
	// per notification, so include_subscription_ids and include_aliases can't
	// ride together.
	subIDs, covered := s.pushSubscriptionsFor(externalIDs)

	var firstErr error
	if len(subIDs) > 0 {
		b := base()
		b["include_subscription_ids"] = subIDs
		invalid, err := s.postPush(restKey, b)
		// OneSignal names the ids it rejected. Dropping them here is what stops
		// dead devices being retried on every send from now on.
		s.forgetPushSubscriptions(invalid)
		firstErr = err
	}

	// Anyone with no recorded device still goes out by alias.
	rest := make([]string, 0, len(externalIDs))
	for _, id := range externalIDs {
		if !covered[id] {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 {
		b := base()
		b["include_aliases"] = map[string]any{"external_id": rest}
		if _, err := s.postPush(restKey, b); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// postPush sends a prepared OneSignal payload. Never returns a hard error: a
// missed push must not fail whatever the caller was actually doing.
//
// Returns any subscription ids OneSignal rejected as invalid, so the caller can
// forget them — see forgetPushSubscriptions.
func (s *Service) postPush(restKey string, body map[string]any) ([]string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		log.Printf("onesignal: marshal payload failed: %v", err)
		return nil, nil
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://api.onesignal.com/notifications?c=push", bytes.NewReader(payload))
	if err != nil {
		log.Printf("onesignal: build request failed: %v", err)
		return nil, nil
	}
	req.Header.Set("Authorization", "Key "+restKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := pushHTTP.Do(req)
	if err != nil {
		log.Printf("onesignal: send failed: %v", err)
		return nil, nil
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("onesignal: send rejected (http %d): %s", resp.StatusCode, raw)
	}
	return invalidSubscriptionIDs(raw), nil
}

// invalidSubscriptionIDs pulls the ids OneSignal refused out of a send response.
//
// They arrive as {"errors":{"invalid_player_ids":[...]}} — the same shape that
// named the Android FCM failure, where devices held a locally-generated id the
// server had never seen. A 200 can carry these, so it's parsed on every reply
// rather than only on failures.
func invalidSubscriptionIDs(raw []byte) []string {
	var out struct {
		Errors struct {
			InvalidPlayerIDs []string `json:"invalid_player_ids"`
		} `json:"errors"`
	}
	// Errors is polymorphic — an ARRAY of strings for some failures, an object
	// for others — so a decode failure here is expected and simply means there
	// were no invalid ids to report.
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out.Errors.InvalidPlayerIDs
}
