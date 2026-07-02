package store

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestIn(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		// Real ids (uuids/ints/slugs) are emitted raw — byte-for-byte identical
		// to the old strings.Join, so this is a safe drop-in on the wire.
		{[]string{"a1b2", "c3d4"}, `in.(a1b2,c3d4)`},
		{[]string{"550e8400-e29b-41d4-a716-446655440000"}, `in.(550e8400-e29b-41d4-a716-446655440000)`},
		{[]string{"42", "7", "1001"}, `in.(42,7,1001)`},
		{[]string{"under_score-and-dash"}, `in.(under_score-and-dash)`},
		{nil, `in.()`},
		// A value with a reserved char can't be a legit id; it's quoted + escaped
		// so it can't break out of the list.
		{[]string{"a,b)"}, `in.(%22a%2Cb%29%22)`},
		{[]string{"ok", `x"y`}, `in.(ok,%22x%5C%22y%22)`},
	}
	for _, c := range cases {
		if got := In(c.in); got != c.want {
			t.Errorf("In(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// newClient points a real Client at a mock PostgREST server via the same env
// seam NewClient reads.
func newClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("SUPABASE_URL", srv.URL)
	t.Setenv("SUPABASE_SERVICE_KEY", "svc-key")
	return NewClient(), srv
}

func TestQ(t *testing.T) {
	if got := Q("a b&c=d"); got != "a+b%26c%3Dd" {
		t.Errorf("Q escape = %q", got)
	}
}

func TestReady(t *testing.T) {
	t.Setenv("SUPABASE_URL", "https://x.supabase.co/")
	t.Setenv("SUPABASE_SERVICE_KEY", "k")
	c := NewClient()
	if !c.Ready() {
		t.Error("expected Ready with url+key")
	}
	if c.baseURL != "https://x.supabase.co" {
		t.Errorf("trailing slash not trimmed: %q", c.baseURL)
	}
	if (&Client{}).Ready() {
		t.Error("empty client should not be Ready")
	}
}

func TestSelect(t *testing.T) {
	var gotPath, gotKey, gotAuth string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotKey = r.Header.Get("apikey")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[{"id":"e1"},{"id":"e2"}]`))
	})
	rows, err := c.Select("events", "owner_id=eq.u1&order=created_at.desc")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 2 || rows[0]["id"] != "e1" {
		t.Errorf("rows = %v", rows)
	}
	if gotPath != "/rest/v1/events?owner_id=eq.u1&order=created_at.desc" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "svc-key" || gotAuth != "Bearer svc-key" {
		t.Errorf("auth headers key=%q auth=%q", gotKey, gotAuth)
	}
}

func TestSelectErrorStatus(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"bad"}`))
	})
	if _, err := c.Select("events", ""); err == nil {
		t.Error("expected error on 400")
	}
}

func TestSelectDecodeError(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	if _, err := c.Select("events", ""); err == nil {
		t.Error("expected decode error")
	}
}

func TestSelectAllPaginates(t *testing.T) {
	var ranges []string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		// First window (0-999) → a full page; second window → empty → stop.
		if strings.HasPrefix(r.Header.Get("Range"), "0-") {
			rows := make([]map[string]any, selectPageSize)
			for i := range rows {
				rows[i] = map[string]any{"id": strconv.Itoa(i)}
			}
			b, _ := json.Marshal(rows)
			_, _ = w.Write(b)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	rows, err := c.SelectAll("matches", "")
	if err != nil {
		t.Fatalf("selectall: %v", err)
	}
	if len(rows) != selectPageSize {
		t.Errorf("rows = %d, want %d", len(rows), selectPageSize)
	}
	if len(ranges) != 2 {
		t.Errorf("expected 2 paged requests, got %d", len(ranges))
	}
}

func TestSelectAllInjectsOrder(t *testing.T) {
	var gotQuery string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := c.SelectAll("matches", "event_id=eq.e1"); err != nil {
		t.Fatalf("selectall: %v", err)
	}
	if !strings.Contains(gotQuery, "order=id") {
		t.Errorf("expected injected order=id, query=%q", gotQuery)
	}
}

func TestSelectOne(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "limit=1") {
			t.Errorf("SelectOne should add limit=1, got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"id":"e1"}]`))
	})
	row, err := c.SelectOne("events", "id=eq.e1")
	if err != nil || row == nil || row["id"] != "e1" {
		t.Fatalf("got (%v, %v)", row, err)
	}
}

func TestSelectOneNone(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	row, err := c.SelectOne("events", "id=eq.none")
	if err != nil || row != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", row, err)
	}
}

func TestInsert(t *testing.T) {
	var gotBody, gotPrefer string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotPrefer = r.Header.Get("Prefer")
		_, _ = w.Write([]byte(`[{"id":"new"}]`))
	})
	out, err := c.Insert("players", map[string]any{"name": "Al"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if out[0]["id"] != "new" {
		t.Errorf("out = %v", out)
	}
	if !strings.Contains(gotBody, `"name":"Al"`) {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.Contains(gotPrefer, "return=representation") {
		t.Errorf("prefer = %q", gotPrefer)
	}
}

func TestInsertError(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"code":"23505"}`))
	})
	if _, err := c.Insert("players", map[string]any{}); err == nil {
		t.Error("expected error on 409")
	}
}

func TestUpdate(t *testing.T) {
	var method string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_, _ = w.Write([]byte(`[{"id":"e1","name":"New"}]`))
	})
	out, err := c.Update("events", "id=eq.e1", map[string]any{"name": "New"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", method)
	}
	if out[0]["name"] != "New" {
		t.Errorf("out = %v", out)
	}
}

func TestUpsert(t *testing.T) {
	var gotURL, gotPrefer string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotPrefer = r.Header.Get("Prefer")
		_, _ = w.Write([]byte(`[{"id":"u1"}]`))
	})
	out, err := c.Upsert("reactions", "feed_item_id,user_id,type",
		map[string]any{"type": "like"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("out = %v", out)
	}
	if !strings.Contains(gotURL, "on_conflict=feed_item_id,user_id,type") {
		t.Errorf("url = %q", gotURL)
	}
	if !strings.Contains(gotPrefer, "merge-duplicates") {
		t.Errorf("prefer = %q", gotPrefer)
	}
}

func TestDelete(t *testing.T) {
	var method string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.WriteHeader(204)
	})
	if err := c.Delete("registrations", "id=eq.r1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %s", method)
	}
}

func TestDeleteError(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	})
	if err := c.Delete("registrations", "id=eq.r1"); err == nil {
		t.Error("expected error on 403")
	}
}

func TestRPC(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/rpc/standings") {
			t.Errorf("rpc path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"wins":3}]`))
	})
	body, err := c.RPC("standings", map[string]any{"event_id": "e1"})
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	if !strings.Contains(string(body), `"wins":3`) {
		t.Errorf("body = %s", body)
	}
}

func TestRPCError(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	if _, err := c.RPC("boom", nil); err == nil {
		t.Error("expected error on 500")
	}
}

func TestStorageUpload(t *testing.T) {
	var gotUpsert, gotCT string
	c, srv := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotUpsert = r.Header.Get("x-upsert")
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	})
	url, err := c.StorageUpload("avatars", "u1/photo.jpg", "image/jpeg", []byte{0xff, 0xd8})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	want := srv.URL + "/storage/v1/object/public/avatars/u1/photo.jpg"
	if url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
	if gotUpsert != "true" || gotCT != "image/jpeg" {
		t.Errorf("upsert=%q ct=%q", gotUpsert, gotCT)
	}
}

func TestStorageUploadError(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte("Bucket not found"))
	})
	_, err := c.StorageUpload("nope", "x", "image/png", []byte{1})
	if err == nil || !strings.Contains(err.Error(), "Bucket not found") {
		t.Errorf("err = %v, want hint surfaced", err)
	}
}

func TestDeleteAuthUser(t *testing.T) {
	var gotPath string
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	})
	if err := c.DeleteAuthUser("uid-1"); err != nil {
		t.Fatalf("delete auth: %v", err)
	}
	if gotPath != "/auth/v1/admin/users/uid-1" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestDeleteAuthUserError(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	if err := c.DeleteAuthUser("missing"); err == nil {
		t.Error("expected error on 404")
	}
}

// Sanity: dbError/dbDecodeError produce sanitized messages.
func TestDBErrorSanitizes(t *testing.T) {
	err := dbError("select", "secret_table", 500, []byte("sensitive column details"))
	if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret_table") {
		t.Errorf("error leaks details: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should carry status: %v", err)
	}
	_ = fmt.Sprint(dbDecodeError("select", "t", []byte("x")))
}
