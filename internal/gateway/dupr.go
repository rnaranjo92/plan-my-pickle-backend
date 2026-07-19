package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
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
// Base URLs: UAT https://uat.mydupr.com/api · prod https://prod.mydupr.com/api.
// Endpoints: POST /auth/{v}/token, GET /user/{v}/{duprId}, POST /match/{v}/create.
type RealDupr struct {
	clientKey    string
	clientSecret string
	baseURL      string // no trailing slash
	ssoBase      string // SSO iframe host, e.g. https://uat.dupr.gg
	userAPIBase  string // user-token API host, e.g. https://api.uat.dupr.gg
	version      string // path version segment, e.g. v1.0
	clubID       string // optional club to attribute submitted matches to
	http         *http.Client

	mu     sync.Mutex // guards token/expiry (held only for field reads/writes)
	token  string
	expiry time.Time
	// refreshMu serializes token refreshes so concurrent misses don't stampede
	// DUPR's auth endpoint — held across the network call, but callers holding a
	// still-valid token never contend for it (they take the fast path below).
	refreshMu sync.Mutex
}

// NewRealDupr builds the live gateway. baseURL/version/clubID may be empty to
// take the UAT defaults (so production only needs the base URL overridden).
func NewRealDupr(clientKey, clientSecret, baseURL, ssoBase, userAPIBase, version, clubID string) *RealDupr {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://uat.mydupr.com/api"
	}
	if strings.TrimSpace(ssoBase) == "" {
		ssoBase = "https://uat.dupr.gg"
	}
	// The user-token API (SSO access tokens: getBasicInfo, /subscription/active,
	// /auth/{v}/refresh). Prod: https://api.dupr.gg — set DUPR_USER_API_BASE.
	if strings.TrimSpace(userAPIBase) == "" {
		userAPIBase = "https://api.uat.dupr.gg"
	}
	if strings.TrimSpace(version) == "" {
		version = "v1.0"
	}
	return &RealDupr{
		clientKey:    strings.TrimSpace(clientKey),
		clientSecret: strings.TrimSpace(clientSecret),
		baseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		ssoBase:      strings.TrimRight(strings.TrimSpace(ssoBase), "/"),
		userAPIBase:  strings.TrimRight(strings.TrimSpace(userAPIBase), "/"),
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
//
// The blocking token exchange runs WITHOUT holding the field lock (d.mu), so a
// slow DUPR auth call can't serialize every concurrent ratings/match request
// behind it. refreshMu serializes the refresh itself so concurrent misses issue
// only one exchange (no stampede); a double-check after acquiring it means only
// the first waiter actually refreshes.
func (d *RealDupr) accessToken() (string, error) {
	// Fast path: a valid cached token, guarded briefly by the field lock.
	if tok := d.cachedToken(); tok != "" {
		return tok, nil
	}

	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	// Re-check: another goroutine may have refreshed while we waited for the lock.
	if tok := d.cachedToken(); tok != "" {
		return tok, nil
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
	expiry := time.Now().Add(50 * time.Minute) // conservative fallback
	if t, e := time.Parse(time.RFC3339, res.Expiry); e == nil {
		expiry = t
	}
	d.mu.Lock()
	d.token = res.Token
	d.expiry = expiry
	d.mu.Unlock()
	return res.Token, nil
}

// cachedToken returns the current token if it's present and not within 30s of
// expiry, else "" — holding the field lock only for the field reads.
func (d *RealDupr) cachedToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != "" && time.Now().Before(d.expiry.Add(-30*time.Second)) {
		return d.token
	}
	return ""
}

// authed makes a bearer-authenticated request and returns the raw body + status.
// permanentDuprStatus reports whether a DUPR HTTP status is a permanent failure
// (a 4xx that retrying won't fix), vs transient (5xx / 429 rate-limit / network).
func permanentDuprStatus(code int) bool {
	return code >= 400 && code < 500 && code != 429
}

// invalidateToken clears the cached bearer so the next call re-authenticates.
func (d *RealDupr) invalidateToken() {
	d.mu.Lock()
	d.token = ""
	d.mu.Unlock()
}

// authed makes a bearer-authenticated request; on a 401 (token expired/rotated
// mid-window) it force-refreshes the token and retries ONCE.
func (d *RealDupr) authed(method, path string, body any) ([]byte, int, error) {
	raw, code, err := d.authedOnce(method, path, body)
	if err == nil && code == http.StatusUnauthorized {
		d.invalidateToken()
		return d.authedOnce(method, path, body)
	}
	return raw, code, err
}

func (d *RealDupr) authedOnce(method, path string, body any) ([]byte, int, error) {
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
	}, nil
}

// ErrDuprUserTokenExpired signals that the user's SSO access token was rejected
// (401) — the caller should refresh it via RefreshUserToken and retry once.
var ErrDuprUserTokenExpired = errors.New("dupr user access token expired")

// GetEntitlements fetches a user's entitlement codes (BASIC_L1 / PREMIUM_L1 /
// VERIFIED_L1 / ...) so DUPR+ registration gating can be enforced.
//
// Contract (confirmed against api.uat.dupr.gg/v3/api-docs/public, 2026-07-03):
// POST {userAPIBase}/subscription/active with the USER's SSO access token as
// the bearer — NOT the partner token. Response: {subscriptions:[{displayName,
// entitlements:{<operation>:[codes...]}}]}; codes are collected across every
// subscription and operation ("tournaments" today). A 401 returns
// ErrDuprUserTokenExpired so the caller can refresh + retry.
func (d *RealDupr) GetEntitlements(userToken string) ([]string, error) {
	userToken = strings.TrimSpace(userToken)
	if userToken == "" {
		return nil, errors.New("no dupr user token")
	}
	req, err := http.NewRequest(http.MethodPost,
		d.userAPIBase+"/subscription/active", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Accept", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrDuprUserTokenExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("dupr subscription/active http %d: %s", resp.StatusCode, snippet(raw))
	}
	type subsShape struct {
		Subscriptions []struct {
			Entitlements map[string][]string `json:"entitlements"`
		} `json:"subscriptions"`
	}
	collect := func(s subsShape) []string {
		var out []string
		for _, sub := range s.Subscriptions {
			for _, codes := range sub.Entitlements {
				out = append(out, codes...)
			}
		}
		return out
	}
	// Bare body per the spec; tolerate a {result:...} envelope defensively.
	var bare subsShape
	if json.Unmarshal(raw, &bare) == nil && len(bare.Subscriptions) > 0 {
		return collect(bare), nil
	}
	var env duprEnvelope
	if json.Unmarshal(raw, &env) == nil && len(env.Result) > 0 {
		var wrapped subsShape
		if json.Unmarshal(env.Result, &wrapped) == nil {
			return collect(wrapped), nil
		}
	}
	return nil, nil // parsed but no subscriptions → no entitlements
}

// VerifyDuprOwner reports which DUPR account an SSO user access token actually
// belongs to, so a connect can confirm the caller isn't claiming someone else's
// DUPR id. Contract (confirmed against api.dupr.gg/v3/api-docs/public, getBasicInfo):
// GET {userAPIBase}/public/user/info with the USER's SSO access token as the
// bearer → 200 {status:"SUCCESS", results:[{duprId}]}.
//
// It returns the token holder's duprId ONLY on a clean 200; on every other
// outcome (empty token, network error, 401/403/404/5xx, unparseable body) it
// returns ("", nil) — deliberately inconclusive, so the caller fails OPEN and a
// genuine connect is never blocked by a transient DUPR hiccup. The only rejection
// signal is a 200 that names a DIFFERENT id, which the caller detects by compare.
func (d *RealDupr) VerifyDuprOwner(userToken string) (string, error) {
	userToken = strings.TrimSpace(userToken)
	if userToken == "" {
		return "", nil // nothing to check → inconclusive
	}
	req, err := http.NewRequest(http.MethodGet, d.userAPIBase+"/public/user/info", nil)
	if err != nil {
		return "", nil
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Accept", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return "", nil // network error → inconclusive, never block onboarding
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil // 401/403/404/5xx → inconclusive
	}
	var env struct {
		Results []struct {
			DuprID string `json:"duprId"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Results) == 0 {
		return "", nil // unparseable / empty → inconclusive
	}
	return strings.TrimSpace(env.Results[0].DuprID), nil
}

// RefreshUserToken exchanges a user's SSO refresh token for a fresh access
// token: GET {userAPIBase}/auth/v1.0/refresh with the refresh token in the
// x-refresh-token header (the refresh token IS the credential). v1.0 returns
// the new access token as a raw string in the standard {result:...} envelope.
func (d *RealDupr) RefreshUserToken(refreshToken string) (string, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", errors.New("no dupr refresh token")
	}
	req, err := http.NewRequest(http.MethodGet,
		d.userAPIBase+"/auth/v1.0/refresh", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-refresh-token", refreshToken)
	req.Header.Set("Accept", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("dupr token refresh http %d: %s", resp.StatusCode, snippet(raw))
	}
	var env duprEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	var token string
	if err := json.Unmarshal(env.Result, &token); err != nil || token == "" {
		// v2.0 shape fallback: {result:{accessToken:...}}.
		var v2 struct {
			AccessToken string `json:"accessToken"`
		}
		if json.Unmarshal(env.Result, &v2) == nil && v2.AccessToken != "" {
			return v2.AccessToken, nil
		}
		return "", fmt.Errorf("dupr token refresh: unrecognized response: %s", snippet(raw))
	}
	return token, nil
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

// matchBody builds the shared create/update request body from a payload.
func (d *RealDupr) matchBody(p DuprPayload) map[string]any {
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
	// Idempotency identifier MUST be unique per match (not per event) AND never
	// reused across creates (DUPR rejects a reused identifier, even post-delete).
	// The caller supplies a fresh per-generation identifier; fall back to MatchID.
	identifier := p.Identifier
	if identifier == "" {
		identifier = p.MatchID
	}
	if identifier == "" {
		identifier = p.DuprEventID
	}
	if identifier == "" {
		identifier = p.EventID
	}
	matchDate := p.MatchDate
	if matchDate == "" {
		matchDate = time.Now().Format("2006-01-02")
	}
	body := map[string]any{
		"identifier": identifier,
		"matchDate":  matchDate,
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
	// Without one, submit as a PARTNER match — DUPR's doc: "For PARTNER
	// submissions, the clubId field should be omitted" — so no club is required
	// to run sanctioned events (club mode only adds club attribution/roster).
	if d.clubID != "" {
		body["matchSource"] = "CLUB"
		if id, e := strconv.ParseInt(d.clubID, 10, 64); e == nil {
			body["clubId"] = id
		} else {
			body["clubId"] = d.clubID
		}
	} else {
		body["matchSource"] = "PARTNER"
	}
	return body
}

// ClubMembers fetches a DUPR club's member roster: POST /club/{v}/members with
// {clubId}. clubID "" -> the gateway's configured club. Restricted partners get
// only connected users back.
func (d *RealDupr) ClubMembers(clubID string) ([]DuprMember, error) {
	cid := strings.TrimSpace(clubID)
	if cid == "" {
		cid = d.clubID
	}
	if cid == "" {
		return nil, fmt.Errorf("dupr: no club id configured")
	}
	id, err := strconv.ParseInt(cid, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("dupr: club id must be numeric, got %q", cid)
	}
	raw, code, err := d.authed(http.MethodPost,
		fmt.Sprintf("/club/%s/members", d.version), map[string]any{"clubId": id})
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("dupr club members %d: %s", code, snippet(raw))
	}
	var res struct {
		Results []struct {
			ID       string `json:"id"`
			FullName string `json:"fullName"`
			Singles  string `json:"singles"`
			Doubles  string `json:"doubles"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	out := make([]DuprMember, 0, len(res.Results))
	for _, m := range res.Results {
		out = append(out, DuprMember{
			DuprID:   m.ID,
			FullName: m.FullName,
			Singles:  m.Singles,
			Doubles:  m.Doubles,
		})
	}
	return out, nil
}

// snippet trims a response body to a short string for error messages (DUPR
// error bodies say things like "match already rated" — useful, not sensitive).
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// parseMatchResult pulls the match code out of a create/update response. OK is
// true only when a match code was actually found: a 2xx with an empty/unparseable
// body would otherwise yield OK:true with an empty DuprMatchID, which silently
// breaks the later Update/Delete (they'd send an empty identifier). Callers that
// require the code (create) can treat OK:false as a soft failure; update falls
// back to the code it already had.
func parseMatchResult(raw []byte) DuprResult {
	var env duprEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Printf("dupr: match result envelope parse failed: %v: %s", err, snippet(raw))
		return DuprResult{OK: false, Error: "unparseable dupr response"}
	}
	var res struct {
		MatchCode       string `json:"matchCode"`
		HashedMatchCode string `json:"hashedMatchCode"`
		Identifier      string `json:"identifier"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		log.Printf("dupr: match result body parse failed: %v: %s", err, snippet(raw))
		return DuprResult{OK: false, Error: "unparseable dupr result"}
	}
	ref := res.MatchCode
	if ref == "" {
		ref = res.HashedMatchCode
	}
	if ref == "" {
		log.Printf("dupr: match result missing matchCode: %s", snippet(raw))
		return DuprResult{OK: false, Error: "dupr response missing matchCode"}
	}
	return DuprResult{OK: true, DuprMatchID: ref}
}

func (d *RealDupr) SubmitMatch(p DuprPayload) (DuprResult, error) {
	raw, code, err := d.authed(http.MethodPost,
		fmt.Sprintf("/match/%s/create", d.version), d.matchBody(p))
	if err != nil {
		// Best-effort: a DUPR hiccup must never fail the score that triggered it.
		return DuprResult{OK: false, Error: err.Error()}, nil
	}
	if code < 200 || code >= 300 {
		log.Printf("dupr: match create http %d: %s", code, string(raw))
		return DuprResult{OK: false, Permanent: permanentDuprStatus(code),
			Error: fmt.Sprintf("dupr http %d: %s", code, snippet(raw))}, nil
	}
	return parseMatchResult(raw), nil
}

// UpdateMatch revises a previously-submitted match (e.g. a corrected score).
// matchId in the body = the stored matchCode (p.MatchCode).
func (d *RealDupr) UpdateMatch(p DuprPayload) (DuprResult, error) {
	body := d.matchBody(p)
	// updateMatch identifies the existing match by matchId, an int64 — the create
	// response's matchCode is that id as a NUMERIC STRING (e.g. "0123456789"), so
	// it must be sent as a JSON number. Sending the raw string fails the field.
	if id, err := strconv.ParseInt(strings.TrimSpace(p.MatchCode), 10, 64); err == nil {
		body["matchId"] = id
	} else {
		body["matchId"] = p.MatchCode // fallback; shouldn't happen for a real match
	}
	// KEEP the identifier on update: DUPR's update endpoint REQUIRES it (dropping it
	// → 400 "Provide a unique identifier for this match"). It's the SAME per-generation
	// identifier used at create, so DUPR matches the existing match and updates it —
	// it does NOT reuse-reject on update (verified: re-scores succeed with it present).
	raw, code, err := d.authed(http.MethodPost,
		fmt.Sprintf("/match/%s/update", d.version), body)
	if err != nil {
		return DuprResult{OK: false, Error: err.Error()}, nil
	}
	if code < 200 || code >= 300 {
		log.Printf("dupr: match update http %d: %s", code, string(raw))
		return DuprResult{OK: false, Permanent: permanentDuprStatus(code),
			Error: fmt.Sprintf("dupr http %d: %s", code, snippet(raw))}, nil
	}
	res := parseMatchResult(raw)
	if res.DuprMatchID == "" {
		// A 2xx update that echoed no code is still a success — keep the code we
		// already had for this match (parseMatchResult reports OK:false on an
		// empty code, which is the right call for create but not for update).
		res = DuprResult{OK: true, DuprMatchID: p.MatchCode}
	}
	return res, nil
}

// DeleteMatch removes a submitted match from DUPR (reverses its rating impact).
// Both the matchCode and the original identifier must match.
func (d *RealDupr) DeleteMatch(matchCode, identifier string) error {
	if strings.TrimSpace(matchCode) == "" {
		return nil
	}
	raw, code, err := d.authed(http.MethodDelete,
		fmt.Sprintf("/match/%s/delete", d.version),
		map[string]any{"matchCode": matchCode, "identifier": identifier})
	if err != nil {
		return err
	}
	// 404 = the match is already gone on DUPR — the desired end state, treat as success.
	if code == http.StatusNotFound {
		return nil
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("dupr delete http %d: %s", code, string(raw))
	}
	return nil
}
