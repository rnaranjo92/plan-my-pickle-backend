// Supabase REST (PostgREST) client. The backend talks to the project's
// Supabase Postgres database over its REST + RPC endpoints using the SERVICE
// key, which bypasses Row Level Security — the schema enables RLS with no anon
// policies, so only the service role can read/write app data (see
// supabase/migrations/0001_initial_schema.sql).
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// dbError logs the raw PostgREST response server-side and returns a sanitized
// error. PostgREST bodies leak table/column/constraint details, so only the
// status code is surfaced to callers.
// StatusError is a non-2xx response from PostgREST, carrying the status so
// callers can tell an ANSWER from a FAILURE. A 4xx means the database
// understood us and said no (bad filter, missing column); a 5xx or a transport
// error means we never got an answer at all. Most callers don't care — but code
// that probes the schema must not read "the request failed" as "the column
// isn't there".
type StatusError struct {
	Op, Table string
	Status    int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("supabase %s: status %d", e.Op, e.Status)
}

// StatusOf returns the HTTP status behind err, or 0 when err didn't come from a
// server response (a network failure, a decode failure, or any other error).
func StatusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

func dbError(op, table string, status int, body []byte) error {
	log.Printf("supabase %s on %s: status=%d body=%s", op, table, status, body)
	return &StatusError{Op: op, Table: table, Status: status}
}

func dbDecodeError(op, table string, body []byte) error {
	log.Printf("supabase %s on %s: decode failed, body=%s", op, table, body)
	return fmt.Errorf("supabase %s: decode failed", op)
}

// Q escapes a value for use inside a PostgREST filter, e.g. "id=eq."+Q(v).
func Q(v string) string { return url.QueryEscape(v) }

// In builds a PostgREST `in.(...)` filter value from a list of literal column
// values (typically UUIDs or ints), e.g. `"id=" + store.In(ids)`. It centralizes
// what used to be scattered `"in.("+strings.Join(ids, ",")+")"` concatenations.
//
// Values made up only of unreserved id characters — which every real id is — are
// emitted raw, byte-for-byte identical to a plain comma join, so this is a safe
// drop-in with no change to the wire format for real traffic. A value containing
// a reserved character (comma, parenthesis, quote, …) — which cannot be a
// legitimate id and today would corrupt or break out of the filter — is instead
// double-quoted and escaped so it stays a single, contained value.
func In(values []string) string {
	var b strings.Builder
	b.WriteString("in.(")
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		if isUnreservedID(v) {
			b.WriteString(v)
			continue
		}
		// PostgREST quoted-value syntax: wrap in double quotes, backslash-escape
		// embedded backslashes/quotes, then percent-encode so the quotes survive
		// the URL. The separating commas stay literal (structural).
		e := strings.ReplaceAll(v, `\`, `\\`)
		e = strings.ReplaceAll(e, `"`, `\"`)
		b.WriteString(url.QueryEscape(`"` + e + `"`))
	}
	b.WriteByte(')')
	return b.String()
}

// isUnreservedID reports whether v is safe to place raw in an in-list — only the
// characters real ids use (uuids, ints, simple slugs). Anything else takes the
// quoted/escaped path in In.
func isUnreservedID(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// Client talks to a Supabase project's PostgREST tables and RPC functions.
type Client struct {
	httpClient *http.Client
	baseURL    string
	serviceKey string
}

// NewClient reads SUPABASE_URL and SUPABASE_SERVICE_KEY from the environment.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		serviceKey: os.Getenv("SUPABASE_SERVICE_KEY"),
	}
}

// Ready reports whether the client has the URL + service key it needs.
func (c *Client) Ready() bool { return c.baseURL != "" && c.serviceKey != "" }

func (c *Client) do(method, fullURL string, body []byte, prefer string) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	return c.httpClient.Do(req)
}

// StorageUpload puts raw bytes into a Supabase Storage bucket via the service
// key (which bypasses bucket RLS) and returns the object's public URL. It
// overwrites any existing object at the same path (x-upsert), so re-uploading a
// user's avatar replaces the old file rather than orphaning it.
func (c *Client) StorageUpload(bucket, path, contentType string, data []byte) (string, error) {
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/storage/v1/object/%s/%s", c.baseURL, bucket, path),
		bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-upsert", "true")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("storage upload %s/%s: status=%d body=%s",
			bucket, path, resp.StatusCode, body)
		// Storage error bodies are NOT sensitive like DB errors (they say things
		// like "Bucket not found" / "mime type not supported") — surface a short
		// hint so the cause is visible instead of an opaque "status 400".
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("storage upload failed (%d): %s", resp.StatusCode, msg)
	}
	return fmt.Sprintf("%s/storage/v1/object/public/%s/%s",
		c.baseURL, bucket, path), nil
}

// StorageDelete removes one object from a bucket via the service key (bypasses
// bucket RLS). A 404 is treated as success — the goal is "the object is gone",
// and a cleanup job that already ran, or a path that was never written, both
// satisfy that. Only a real server error is reported back.
func (c *Client) StorageDelete(bucket, path string) error {
	req, err := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/storage/v1/object/%s/%s", c.baseURL, bucket, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("storage delete failed (%d): %s", resp.StatusCode, msg)
	}
	return nil
}

// SignedURLs creates time-limited signed download URLs for objects in a PRIVATE
// bucket, in one batch request, using the service key (bypasses bucket RLS).
// Returns a map of object path -> full signed URL; paths that fail to sign are
// omitted (callers fall back to the stored value).
func (c *Client) SignedURLs(bucket string, paths []string, expiresIn int) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	body, err := json.Marshal(map[string]any{"expiresIn": expiresIn, "paths": paths})
	if err != nil {
		return out, err
	}
	resp, err := c.do(http.MethodPost,
		fmt.Sprintf("%s/storage/v1/object/sign/%s", c.baseURL, bucket), body, "")
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		log.Printf("storage sign %s: status=%d body=%s", bucket, resp.StatusCode, raw)
		return out, fmt.Errorf("storage sign failed (%d)", resp.StatusCode)
	}
	var arr []struct {
		Path      string `json:"path"`
		SignedURL string `json:"signedURL"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return out, dbDecodeError("sign", bucket, raw)
	}
	for _, e := range arr {
		if e.SignedURL != "" {
			out[e.Path] = c.baseURL + "/storage/v1" + e.SignedURL
		}
	}
	return out, nil
}

// Select returns rows from a table matching a raw PostgREST query string, e.g.
// "event_id=eq.<id>&order=round_number.asc".
func (c *Client) Select(table, query string) ([]map[string]any, error) {
	resp, err := c.do(http.MethodGet, fmt.Sprintf("%s/rest/v1/%s?%s", c.baseURL, table, query), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, dbError("select", table, resp.StatusCode, body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, dbDecodeError("select", table, body)
	}
	return rows, nil
}

// selectPageSize is the row window SelectAll requests per page via the HTTP
// Range header. PostgREST silently caps a single response at its configured
// max-rows (1000 by default), so an unbounded Select can truncate a large fetch
// without erroring. SelectAll pages around that cap.
const selectPageSize = 1000

// SelectAll is Select with pagination: it fetches successive Range windows
// (0-999, 1000-1999, …) and concatenates them until a short/empty page, so large
// result sets aren't silently truncated at PostgREST's max-rows cap. Use it for
// reads whose size scales with the tournament (all matches/participants/players);
// for a known-small or already-limited query, plain Select is fine.
//
// The query must NOT carry its own limit/offset — SelectAll owns the windowing.
func (c *Client) SelectAll(table, query string) ([]map[string]any, error) {
	// Range windows are LIMIT/OFFSET applied AFTER ordering; with no ORDER BY,
	// Postgres gives no stable order across the separate paged requests, so a row
	// at a page boundary can be skipped or duplicated. Inject a deterministic
	// order on the primary key when the caller hasn't set one.
	if !strings.Contains(query, "order=") {
		if query == "" {
			query = "order=id"
		} else {
			query += "&order=id"
		}
	}
	var all []map[string]any
	for offset := 0; ; offset += selectPageSize {
		from := offset
		to := offset + selectPageSize - 1
		url := fmt.Sprintf("%s/rest/v1/%s?%s", c.baseURL, table, query)
		resp, err := c.doRange(http.MethodGet, url, fmt.Sprintf("%d-%d", from, to))
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, dbError("select", table, resp.StatusCode, body)
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, dbDecodeError("select", table, body)
		}
		all = append(all, rows...)
		// A short page (fewer than a full window) means we've reached the end.
		if len(rows) < selectPageSize {
			break
		}
	}
	return all, nil
}

// doRange issues a request with a PostgREST Range header (row window), used by
// SelectAll to page through large result sets.
func (c *Client) doRange(method, fullURL, rangeHeader string) (*http.Response, error) {
	req, err := http.NewRequest(method, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Range-Unit", "items")
	req.Header.Set("Range", rangeHeader)
	return c.httpClient.Do(req)
}

// SelectOne returns the first matching row, or (nil, nil) when none match.
func (c *Client) SelectOne(table, query string) (map[string]any, error) {
	rows, err := c.Select(table, query+"&limit=1")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// Insert writes one or many rows (struct, map, or slice) and returns the
// inserted records.
func (c *Client) Insert(table string, rows any) ([]map[string]any, error) {
	b, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(http.MethodPost, fmt.Sprintf("%s/rest/v1/%s", c.baseURL, table), b, "return=representation")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, dbError("insert", table, resp.StatusCode, body)
	}
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, dbDecodeError("insert", table, body)
	}
	return out, nil
}

// Update patches rows matching the query and returns the updated records.
func (c *Client) Update(table, query string, fields any) ([]map[string]any, error) {
	b, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(http.MethodPatch, fmt.Sprintf("%s/rest/v1/%s?%s", c.baseURL, table, query), b, "return=representation")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, dbError("update", table, resp.StatusCode, body)
	}
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, dbDecodeError("update", table, body)
	}
	return out, nil
}

// Upsert inserts or merges rows on the given conflict column(s).
func (c *Client) Upsert(table, onConflict string, rows any) ([]map[string]any, error) {
	b, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(http.MethodPost,
		fmt.Sprintf("%s/rest/v1/%s?on_conflict=%s", c.baseURL, table, onConflict),
		b, "resolution=merge-duplicates,return=representation")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		// dbError logs the full PostgREST body server-side and returns a
		// sanitized status-only error to the client.
		return nil, dbError("upsert", table, resp.StatusCode, body)
	}
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, dbDecodeError("upsert", table, body)
	}
	return out, nil
}

// Delete removes rows matching the query.
func (c *Client) Delete(table, query string) error {
	resp, err := c.do(http.MethodDelete, fmt.Sprintf("%s/rest/v1/%s?%s", c.baseURL, table, query), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return dbError("delete", table, resp.StatusCode, body)
	}
	return nil
}

// DeleteAuthUser removes a Supabase auth user via the GoTrue Admin API (this
// hits /auth/v1, not /rest/v1, and needs the service-role key the client holds).
// Used by account deletion so a user can erase their login (Apple Guideline
// 5.1.1(v)).
func (c *Client) DeleteAuthUser(uid string) error {
	resp, err := c.do(http.MethodDelete,
		fmt.Sprintf("%s/auth/v1/admin/users/%s", c.baseURL, url.PathEscape(uid)), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return dbError("auth-delete", "users", resp.StatusCode, body)
	}
	return nil
}

// GetAuthUser fetches a Supabase auth user (id, email, user_metadata, …) via the
// GoTrue Admin API. Used to resolve a display name for accounts that have no
// players row (organizers / social-only / fresh signups) so notifications show
// the person's signup name instead of a generic fallback. Best-effort.
func (c *Client) GetAuthUser(uid string) (map[string]any, error) {
	resp, err := c.do(http.MethodGet,
		fmt.Sprintf("%s/auth/v1/admin/users/%s", c.baseURL, url.PathEscape(uid)), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, dbError("auth-get", "users", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, dbDecodeError("auth-get", "users", body)
	}
	return out, nil
}

// SignInPassword signs a user in via the GoTrue password grant and returns the
// raw session JSON (access_token, refresh_token, user, …). Used by the sales-demo
// passcode flow to establish the fixed demo owner's session server-side so the
// console needs only the passcode — no separate account login. apikey (not a
// Bearer service token) is what the token endpoint gates on.
func (c *Client) SignInPassword(email, password string) ([]byte, error) {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequest(http.MethodPost,
		c.baseURL+"/auth/v1/token?grant_type=password", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, dbError("sign-in", "auth", resp.StatusCode, body)
	}
	return body, nil
}

// RPC calls a Postgres function at /rest/v1/rpc/<fn> and returns the raw JSON
// result. Aggregations (standings) and multi-step atomic writes (schedule +
// bracket generation, winner advancement) are implemented as plpgsql functions
// and invoked here so they run server-side in one transaction.
func (c *Client) RPC(fn string, payload any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(http.MethodPost, fmt.Sprintf("%s/rest/v1/rpc/%s", c.baseURL, fn), b, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, dbError("rpc", fn, resp.StatusCode, body)
	}
	return body, nil
}
