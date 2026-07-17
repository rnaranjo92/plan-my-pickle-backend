package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSupabase is a minimal in-memory PostgREST stand-in. Tests seed per-table
// GET responses (and RPC results); writes echo their body back as the inserted
// representation (with an injected id) so Add/Record/Set methods can read the
// "row" they just wrote. Unseeded tables return an empty array, which exercises
// each read method's empty-result path without erroring.
type fakeSupabase struct {
	mu     sync.Mutex
	get    map[string]string                  // table -> JSON array of rows (GET)
	rpc    map[string]string                  // function -> JSON body
	rpcFn  map[string]func(body []byte) string // function -> payload-aware responder
	reqs   []string                            // "METHOD /path" captured for assertions
	writes map[string][]map[string]any         // table -> captured insert/update rows
}

func newFake() *fakeSupabase {
	return &fakeSupabase{get: map[string]string{}, rpc: map[string]string{},
		rpcFn: map[string]func([]byte) string{}, writes: map[string][]map[string]any{}}
}

// written returns every row body POSTed/PATCHed to a table (for E2E assertions).
func (f *fakeSupabase) written(table string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[table]
}

// capture records an insert/update body (array or single object) under a table.
func (f *fakeSupabase) capture(table string, b []byte) {
	var arr []map[string]any
	if json.Unmarshal(b, &arr) != nil {
		var obj map[string]any
		if json.Unmarshal(b, &obj) == nil {
			arr = []map[string]any{obj}
		}
	}
	if len(arr) == 0 {
		return
	}
	f.mu.Lock()
	f.writes[table] = append(f.writes[table], arr...)
	f.mu.Unlock()
}

func (f *fakeSupabase) seed(table, json string) *fakeSupabase { f.get[table] = json; return f }
func (f *fakeSupabase) seedRPC(fn, json string) *fakeSupabase { f.rpc[fn] = json; return f }

// seedRPCFn seeds a PAYLOAD-AWARE responder — for RPCs whose result must vary
// per call (e.g. pmp_standings keyed by p_event_id in a multi-session league).
func (f *fakeSupabase) seedRPCFn(fn string, respond func(body []byte) string) *fakeSupabase {
	f.rpcFn[fn] = respond
	return f
}

func (f *fakeSupabase) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/rest/v1/rpc/"):
		fn := strings.TrimPrefix(path, "/rest/v1/rpc/")
		if respond, ok := f.rpcFn[fn]; ok {
			b, _ := io.ReadAll(r.Body)
			_, _ = w.Write([]byte(respond(b)))
			return
		}
		if body, ok := f.rpc[fn]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte("[]"))
	case strings.HasPrefix(path, "/rest/v1/"):
		table := strings.TrimPrefix(path, "/rest/v1/")
		switch r.Method {
		case http.MethodGet:
			if body, ok := f.get[table]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
			_, _ = w.Write([]byte("[]"))
		case http.MethodPost, http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			f.capture(table, b)
			_, _ = w.Write(wrapRows(b))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte("[]"))
		}
	case strings.HasPrefix(path, "/storage/v1/"):
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(path, "/auth/v1/"):
		w.WriteHeader(http.StatusOK)
	default:
		_, _ = w.Write([]byte("[]"))
	}
}

// wrapRows turns an insert/update body into a PostgREST "representation" array,
// injecting an id when missing so callers that read out[0]["id"] succeed.
func wrapRows(b []byte) []byte {
	var arr []map[string]any
	if err := json.Unmarshal(b, &arr); err != nil {
		var obj map[string]any
		if json.Unmarshal(b, &obj) == nil {
			arr = []map[string]any{obj}
		}
	}
	if arr == nil {
		arr = []map[string]any{{}}
	}
	for i := range arr {
		if arr[i] == nil {
			arr[i] = map[string]any{}
		}
		if _, ok := arr[i]["id"]; !ok {
			arr[i]["id"] = "gen-id"
		}
	}
	out, _ := json.Marshal(arr)
	return out
}

// newFakeSvc points a real Service (with default mock gateways) at the fake
// Supabase via the env seam New() reads.
func newFakeSvc(t *testing.T, f *fakeSupabase) *Service {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("SUPABASE_URL", srv.URL)
	t.Setenv("SUPABASE_SERVICE_KEY", "test-key")
	return New()
}

// seededFake returns a fake pre-loaded with one representative row per common
// table, using the column names the row→model mappers read. Read methods then
// exercise their mapping branch rather than just the empty path.
func seededFake() *fakeSupabase {
	return newFake().
		seed("events", `[{"id":"e1","name":"Slam","status":"live","format":"doubles","num_courts":2,"points_to_win":11,"win_by":2,"best_of":1,"registration_fee_cents":0,"owner_id":"o1","listed":true,"venue_lat":30.2,"venue_lng":-97.7}]`).
		seed("brackets", `[{"id":"b1","event_id":"e1","name":"Open","division_type":"open","sort_order":0}]`).
		seed("registrations", `[{"id":"r1","event_id":"e1","player_id":"p1","payment_status":"paid","checked_in":false,"player":{"full_name":"Al","phone":"5551234","dupr_rating":3.5}}]`).
		seed("rounds", `[{"id":"rd1","event_id":"e1","round_number":1,"status":"scheduled"}]`).
		seed("matches", `[{"id":"m1","event_id":"e1","stage":"pool","status":"scheduled","participants":[{"team":1,"player_id":"p1","player":{"full_name":"Al"}},{"team":2,"player_id":"p2","player":{"full_name":"Bo"}}]}]`).
		seed("leagues", `[{"id":"l1","owner_id":"o1","name":"Fall","league_type":"round_robin","day_type":"multi","created_at":"2026-06-20T00:00:00Z"}]`).
		seed("league_brackets", `[{"id":"lb1","league_id":"l1","name":"Open","division_type":"open","sort_order":0}]`).
		seed("ladder_entrants", `[{"id":"en1","league_bracket_id":"lb1","display_name":"Sam","position":1}]`).
		seed("ladder_matches", `[{"id":"lm1","league_bracket_id":"lb1","entrant_a_id":"en1","entrant_b_id":"en2","winner_entrant_id":"en1","score":"11-7"}]`).
		seed("teams", `[{"id":"t1","league_bracket_id":"lb1","name":"Aces"}]`).
		seed("team_fixtures", `[{"id":"tf1","league_bracket_id":"lb1","team_a_id":"t1","team_b_id":"t2","winner_team_id":"t1","score":"3-1"}]`).
		seed("flex_matchups", `[{"id":"fm1","league_bracket_id":"lb1","team_a_id":"t1","team_b_id":"t2","status":"pending"}]`).
		seed("checklist_items", `[{"id":"c1","event_id":"e1","label":"Nets","checked":false,"sort_order":0}]`).
		seed("finance_entries", `[{"id":"fe1","event_id":"e1","kind":"income","category":"fees","amount_cents":1000,"note":"x","created_at":"t"}]`).
		seed("feed_items", `[{"id":"fi1","event_id":"e1","type":"announcement","text":"Hi","created_at":"2026-06-20T00:00:00Z"}]`).
		seed("feed_comments", `[{"id":"fc1","feed_item_id":"fi1","author_name":"Lee","text":"nice","created_at":"t"}]`).
		seed("clubs", `[{"id":"cl1","name":"Club","owner_id":"o1","city":"Austin"}]`).
		seed("club_members", `[{"id":"cm1","club_id":"cl1","user_id":"u1","role":"member"}]`).
		seed("players", `[{"id":"p1","full_name":"Al","phone":"5551234","dupr_rating":3.5}]`).
		seed("profiles", `[{"id":"u1","full_name":"Al","email":"a@b.com"}]`)
}
