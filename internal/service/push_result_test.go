package service

import "testing"

// A send that reaches nobody is the failure mode this whole file exists for: it
// comes back HTTP 200, with an id, and no error. The only thing that tells it
// apart from a working send is the recipient count, so the count has to survive
// every response shape OneSignal actually returns.
func TestParsePushResult(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		recipients int
		id         string
		invalid    int
	}{
		{
			name:       "a normal delivery",
			raw:        `{"id":"abc-123","recipients":4,"external_id":null}`,
			recipients: 4,
			id:         "abc-123",
		},
		{
			// The silent failure. Accepted, addressed, delivered to nobody —
			// a wedged alias or a subscription that is no longer opted in.
			name:       "accepted and delivered to nobody",
			raw:        `{"id":"abc-123","recipients":0}`,
			recipients: 0,
			id:         "abc-123",
		},
		{
			name:       "invalid ids ride along with a 200",
			raw:        `{"id":"x","recipients":2,"errors":{"invalid_player_ids":["a","b"]}}`,
			recipients: 2,
			id:         "x",
			invalid:    2,
		},
		{
			// `errors` is polymorphic — an ARRAY here, an object above. Decoding
			// must not throw away the rest of the response when it is the array
			// form, and must never panic.
			name: "errors as an array",
			raw:  `{"errors":["All included players are not subscribed"]}`,
		},
		{
			name: "garbage",
			raw:  `not json at all`,
		},
		{
			name: "empty body",
			raw:  ``,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePushResult([]byte(c.raw))
			if got.recipients != c.recipients {
				t.Errorf("recipients = %d, want %d", got.recipients, c.recipients)
			}
			if got.id != c.id {
				t.Errorf("id = %q, want %q", got.id, c.id)
			}
			if len(got.invalid) != c.invalid {
				t.Errorf("invalid = %v, want %d of them", got.invalid, c.invalid)
			}
		})
	}
}

// logPush truncates long headings rather than printing a paragraph per send —
// and must not slice a heading shorter than the limit.
func TestLogPushHeadingLengths(t *testing.T) {
	// Nothing to assert beyond "does not panic": the value is the log line.
	logPush("devices", 3, pushResult{recipients: 3, id: "x"}, "short")
	logPush("aliases", 1, pushResult{}, "")
	logPush("segment", 0, pushResult{recipients: 91},
		"a heading that runs well past forty characters and keeps going")
}
