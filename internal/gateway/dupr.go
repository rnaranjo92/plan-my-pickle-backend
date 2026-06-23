package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RealDupr is the live DUPR partner-API gateway: ratings lookup + match
// submission. Wired in from main only when DUPR_CLIENT_KEY/SECRET are set;
// otherwise the server keeps MockDupr. Auth is a client-key/secret exchange for
// a short-lived bearer token, cached until just before expiry.
//
// Base URLs: UAT https://uat.mydupr.com/api · prod https://api.dupr.com/api.
// Endpoints: POST /auth/{v}/token, GET /user/{v}/{duprId}, POST /match/{v}/create.
type RealDupr struct {
	clientKey    string
	clientSecret string
	baseURL      string // no trailing slash
	ssoBase      string // SSO iframe host, e.g. https://uat.dupr.gg
	version      string // path version segment, e.g. v1.0
	clubID       string // optional club to attribute submitted matches to
	http         *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewRealDupr builds the live gateway. baseURL/version/clubID may be empty to
// take the UAT defaults (so production only needs the base URL overridden).
func NewRealDupr(clientKey, clientSecret, baseURL, ssoBase, version, clubID string) *RealDupr {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://uat.mydupr.com/api"
	}
	if strings.TrimSpace(ssoBase) == "" {
		ssoBase = "https://uat.dupr.gg"
	}
	if strings.TrimSpace(version) == "" {
		version = "v1.0"
	}
	return &RealDupr{
		clientKey:    strings.TrimSpace(clientKey),
		clientSecret: strings.TrimSpace(clientSecret),
		baseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		ssoBase:      strings.TrimRight(strings.TrimSpace(ssoBase), "/"),
		version:      strings.Trim(strings.TrimSpace(version), "/"),
		clubID:       strings.TrimSpace(clubID),
		http:         &http.Client{Timeout: 8 * time.Second},
	}
}

// SsoURL returns the iframe URL a user is sent to to connect their DUPR account
// (base64(clientKey) embedded) plus the origin to validate the postMessage from.
func (d *RealDupr) SsoURL() (string, string) {
	enc := base64.StdEncoding.EncodeToString([]byte(d.clientKey))
	return d.ssoBase + "/login-external-app/" + enc, d.ssoBase
}

// RegisterWebhook registers our HTTPS URL to receive RATING webhook events.
// POST /{v}/webhook {webhookUrl, topics}. Idempotent on DUPR's side by url.
func (d *RealDupr) RegisterWebhook(webhookURL string) error {
	if strings.TrimSpace(webhookURL) == "" {
		return nil
	}
	raw, code, err := d.authed(http.MethodPost,
		fmt.Sprintf("/%s/webhook", d.version),
		map[string]any{"webhookUrl": webhookURL, "topics": []string{"RATING"}})
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("dupr register webhook http %d: %s", code, string(raw))
	}
	return nil
}

// SubscribeUserRating subscribes a connected user to RATING events. DUPR then
// immediately posts a RATING_SEED with their current rating to our webhook.
func (d *RealDupr) SubscribeUserRating(duprID string) error {
	duprID = strings.TrimSpace(duprID)
	if duprID == "" {
		return nil
	}
	raw, code, err := d.authed(http.MethodPost,
		fmt.Sprintf("/user/%s/subscribe/webhook-event", d.version),
		map[string]any{"duprIds": []string{duprID}, "topic": "RATING"})
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("dupr subscribe http %d: %s", code, string(raw))
	}
	return nil
}

// duprEnvelope is DUPR's standard response wrapper ({status, message, result}).
type duprEnvelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

// accessToken returns a cached bearer token, refreshing via the client-key/
// secret exchange when missing or within 30s of expiry.
func (d *RealDupr) accessToken() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != "" && time.Now().Before(d.expiry.Add(-30*time.Second)) {
		return d.token, nil
	}
	endpoint := fmt.Sprintf("%s/auth/%s/token", d.baseURL, d.version)
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	cred := base64.StdEncoding.EncodeToString(
		[]byte(d.clientKey + ":" + d.clientSecret))
	req.Header.Set("x-authorization", cred)
	req.Header.Set("Accept", "application/json")

	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("dupr auth http %d: %s", resp.StatusCode, string(raw))
	}
	var env duprEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	var res struct {
		Token  string `json:"token"`
		Expiry string `json:"expiry"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil || res.Token == "" {
		return "", fmt.Errorf("dupr auth: no token in response: %s", string(raw))
	}
	d.token = res.Token
	if t, e := time.Parse(time.RFC3339, res.Expiry); e == nil {
		d.expiry = t
	} else {
		d.expiry = time.Now().Add(50 * time.Minute) // conservative fallback
	}
	return d.token, nil
}

// authed makes a bearer-authenticated request and returns the raw body + status.
func (d *RealDupr) authed(method, path string, body any) ([]byte, int, error) {
	tok, err := d.accessToken()
	if err != nil {
		return nil, 0, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, d.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

func (d *RealDupr) GetPlayerRating(duprID string) (DuprRating, error) {
	duprID = strings.TrimSpace(duprID)
	if duprID == "" {
		return DuprRating{Found: false}, nil
	}
	raw, code, err := d.authed(http.MethodGet,
		fmt.Sprintf("/user/%s/%s", d.version, url.PathEscape(duprID)), nil)
	if err != nil {
		return DuprRating{}, err
	}
	if code == http.StatusNotFound {
		return DuprRating{Found: false}, nil
	}
	if code < 200 || code >= 300 {
		return DuprRating{}, fmt.Errorf("dupr user http %d: %s", code, string(raw))
	}
	var env duprEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return DuprRating{}, err
	}
	// DUPR's /user result shape: {id, fullName, ratings:{singles, doubles,
	// isSinglesReliable, isDoublesReliable}}. Ratings are a number, the string
	// "NR", or null for an unrated player. (NOT top-level singlesRating.)
	var res struct {
		ID       string `json:"id"`
		FullName string `json:"fullName"`
		Ratings  struct {
			Singles           json.RawMessage `json:"singles"`
			Doubles           json.RawMessage `json:"doubles"`
			IsSinglesReliable *bool           `json:"isSinglesReliable"`
			IsDoublesReliable *bool           `json:"isDoublesReliable"`
		} `json:"ratings"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return DuprRating{}, err
	}
	provisional := func(reliable *bool) bool { return reliable != nil && !*reliable }
	return DuprRating{
		// A present id/fullName signals a real, consented user. Echo the id we
		// requested (DUPR returns it under `id`).
		Found:              res.ID != "" || res.FullName != "",
		DuprID:             duprID,
		FullName:           res.FullName,
		Singles:            parseDuprRating(res.Ratings.Singles),
		Doubles:            parseDuprRating(res.Ratings.Doubles),
		SinglesProvisional: provisional(res.Ratings.IsSinglesReliable),
		DoublesProvisional: provisional(res.Ratings.IsDoublesReliable),
		Raw:                string(raw),
	}, nil
}

// parseDuprRating reads a DUPR rating value that may be a JSON number, the
// string "NR"/"" (unrated), or null → 0.
func parseDuprRating(raw json.RawMessage) float64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" || strings.EqualFold(s, "NR") {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func (d *RealDupr) SubmitMatch(p DuprPayload) (DuprResult, error) {
	team := func(ids []string, games [][2]int, side int) map[string]any {
		t := map[string]any{}
		if len(ids) > 0 {
			t["player1"] = ids[0]
		}
		if len(ids) > 1 {
			t["player2"] = ids[1]
		}
		for i := 0; i < len(games) && i < 5; i++ {
			t[fmt.Sprintf("game%d", i+1)] = games[i][side]
		}
		return t
	}
	games := p.Games
	if len(games) == 0 {
		games = [][2]int{{p.Team1Score, p.Team2Score}}
	}
	format := "DOUBLES"
	if len(p.Team1DuprIDs) <= 1 && len(p.Team2DuprIDs) <= 1 {
		format = "SINGLES"
	}
	identifier := p.DuprEventID
	if identifier == "" {
		identifier = p.EventID
	}
	body := map[string]any{
		"identifier": identifier,
		"matchDate":  time.Now().Format("2006-01-02"),
		// Send both names — the partner saveMatch doc uses "format", the OpenAPI
		// spec calls it "matchFormat"; unknown fields are ignored server-side.
		"format":      format,
		"matchFormat": format,
		"matchType":   "SIDEOUT", // traditional pickleball side-out scoring
		"teamA":       team(p.Team1DuprIDs, games, 0),
		"teamB":       team(p.Team2DuprIDs, games, 1),
	}
	if p.EventName != "" {
		body["event"] = p.EventName
	}
	// Submit as a club match when a club is configured (clubId must be numeric).
	if d.clubID != "" {
		body["matchSource"] = "CLUB"
		if id, e := strconv.ParseInt(d.clubID, 10, 64); e == nil {
			body["clubId"] = id
		} else {
			body["clubId"] = d.clubID
		}
	}
	raw, code, err := d.authed(http.MethodPost,
		fmt.Sprintf("/match/%s/create", d.version), body)
	if err != nil {
		// Best-effort: a DUPR hiccup must never fail the score that triggered it.
		return DuprResult{OK: false, Error: err.Error()}, nil
	}
	if code < 200 || code >= 300 {
		log.Printf("dupr: match create http %d: %s", code, string(raw))
		return DuprResult{OK: false, Error: fmt.Sprintf("dupr http %d", code)}, nil
	}
	var env duprEnvelope
	_ = json.Unmarshal(raw, &env)
	var res struct {
		MatchCode       string `json:"matchCode"`
		HashedMatchCode string `json:"hashedMatchCode"`
		Identifier      string `json:"identifier"`
	}
	_ = json.Unmarshal(env.Result, &res)
	ref := res.MatchCode
	if ref == "" {
		ref = res.HashedMatchCode
	}
	return DuprResult{OK: true, DuprMatchID: ref}, nil
}
