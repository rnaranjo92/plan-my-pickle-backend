package service

import "testing"

// The joke broadcast reached nobody for weeks: it targeted a OneSignal segment
// that matched no one, while every push that DOES arrive targets device ids.
// This is the list it now sends to, so its correctness is the difference
// between a broadcast and a silent no-op.
func TestAllPushSubscriptionIDsDedupesDevices(t *testing.T) {
	// One person with the phone app AND the web app has two rows; a second
	// person has one. Nobody should receive the joke twice.
	f := newFake().seed("push_subscriptions", `[
	  {"subscription_id":"sub-a"},
	  {"subscription_id":"sub-b"},
	  {"subscription_id":"sub-a"},
	  {"subscription_id":""}
	]`)
	s := newFakeSvc(t, f)

	got, err := s.allPushSubscriptionIDs()
	if err != nil {
		t.Fatalf("allPushSubscriptionIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 distinct devices, got %v", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if id == "" {
			t.Error("an empty subscription id must never be targeted")
		}
		if seen[id] {
			t.Errorf("duplicate device %q — that person gets the joke twice", id)
		}
		seen[id] = true
	}
}

// No devices recorded is a real state (pre-migration, or a fresh install), and
// it must be reported as empty so the caller can fall back rather than send to
// an empty id list.
func TestAllPushSubscriptionIDsEmpty(t *testing.T) {
	s := newFakeSvc(t, newFake().seed("push_subscriptions", `[]`))
	got, err := s.allPushSubscriptionIDs()
	if err != nil {
		t.Fatalf("allPushSubscriptionIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want none, got %v", got)
	}
}

// The query must be scoped by freshness — a row nobody has refreshed in 45 days
// is a device that has gone dark, and sending to it inflates the count while
// delivering nowhere.
func TestAllPushSubscriptionIDsFiltersByFreshness(t *testing.T) {
	f := newFake().seed("push_subscriptions", `[{"subscription_id":"sub-a"}]`)
	s := newFakeSvc(t, f)
	if _, err := s.allPushSubscriptionIDs(); err != nil {
		t.Fatalf("allPushSubscriptionIDs: %v", err)
	}
	var got string
	for _, u := range f.urls {
		if contains(u, "/push_subscriptions") {
			got = u
		}
	}
	if got == "" {
		t.Fatal("no push_subscriptions read was issued")
	}
	if !contains(got, "updated_at=gte.") {
		t.Errorf("the audience must be limited to recently-seen devices: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
