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
