package api

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
)

// messengerVerify handles Meta's webhook verification handshake (GET). When you
// register the callback URL in the App dashboard, Meta hits this with
// hub.mode=subscribe, a hub.verify_token you chose, and a hub.challenge to echo.
// We echo the challenge only when the token matches MESSENGER_VERIFY_TOKEN.
func (s *Server) messengerVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	want := os.Getenv("MESSENGER_VERIFY_TOKEN")
	if q.Get("hub.mode") == "subscribe" && want != "" &&
		subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(want)) == 1 {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(q.Get("hub.challenge")))
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

// messengerWebhook receives Messenger events (POST): opt-ins (a player scanned
// the check-in QR / m.me?ref= link → we bind their PSID) and inbound messages
// (which refresh their 24h messaging window). FAIL-CLOSED on the app-secret
// signature when configured; always 200 fast so Meta doesn't retry-storm.
func (s *Server) messengerWebhook(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if !s.svc.Msgr.VerifySignature(body, r.Header.Get("X-Hub-Signature-256")) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	var p struct {
		Object string `json:"object"`
		Entry  []struct {
			Messaging []struct {
				Sender  struct{ ID string } `json:"sender"`
				Message *struct {
					Text string `json:"text"`
				} `json:"message"`
				Referral *struct {
					Ref string `json:"ref"`
				} `json:"referral"`
				Postback *struct {
					Payload  string `json:"payload"`
					Referral *struct {
						Ref string `json:"ref"`
					} `json:"referral"`
				} `json:"postback"`
				Optin *struct {
					Ref string `json:"ref"`
				} `json:"optin"`
			} `json:"messaging"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		shown := body
		if len(shown) > 200 {
			shown = shown[:200]
		}
		log.Printf("messenger webhook: unparseable payload: %v: %s", err, shown)
		w.WriteHeader(http.StatusOK)
		return
	}

	for _, e := range p.Entry {
		for _, m := range e.Messaging {
			psid := m.Sender.ID
			if psid == "" {
				continue
			}
			// An opt-in ref (m.me?ref=ply_<id>) can arrive via a referral event, a
			// postback's referral (the "Get Started" tap), or an optin event. First
			// non-empty wins → bind the PSID to that player.
			ref := ""
			switch {
			case m.Referral != nil && m.Referral.Ref != "":
				ref = m.Referral.Ref
			case m.Postback != nil && m.Postback.Referral != nil && m.Postback.Referral.Ref != "":
				ref = m.Postback.Referral.Ref
			case m.Optin != nil && m.Optin.Ref != "":
				ref = m.Optin.Ref
			}
			if ref != "" {
				s.svc.CaptureMessengerOptIn(ref, psid)
				continue // binding already opens the window
			}
			// Any other inbound (a message, a plain postback) refreshes the window.
			s.svc.BumpMessengerWindow(psid)
		}
	}
	w.WriteHeader(http.StatusOK)
}
