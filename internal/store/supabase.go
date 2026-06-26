// Supabase REST (PostgREST) client. The backend talks to the project's
// Supabase Postgres database over its REST + RPC endpoints using the SERVICE
// key, which bypasses Row Level Security — the schema enables RLS with no anon
// policies, so only the service role can read/write app data (see
// supabase/migrations/0001_initial_schema.sql).
package store

import (
	"bytes"
	"encoding/json"
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
func dbError(op, table string, status int, body []byte) error {
	log.Printf("supabase %s on %s: status=%d body=%s", op, table, status, body)
	return fmt.Errorf("supabase %s: status %d", op, status)
}

func dbDecodeError(op, table string, body []byte) error {
	log.Printf("supabase %s on %s: decode failed, body=%s", op, table, body)
	return fmt.Errorf("supabase %s: decode failed", op)
}

// Q escapes a value for use inside a PostgREST filter, e.g. "id=eq."+Q(v).
func Q(v string) string { return url.QueryEscape(v) }

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
	_ = json.Unmarshal(body, &out)
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
