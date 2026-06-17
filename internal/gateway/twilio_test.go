package gateway

import "testing"

func TestToE164(t *testing.T) {
	cases := map[string]string{
		"5125551234":        "+15125551234", // bare US 10-digit
		"15125551234":       "+15125551234", // 1 + 10
		"(512) 555-1234":    "+15125551234", // formatted US
		"512.555.1234":      "+15125551234",
		"+1 512 555 1234":   "+15125551234", // already E.164, spaced
		"+15125551234":      "+15125551234",
		"+44 20 7946 0958":  "+442079460958", // intl kept as-is
		"  +1-512-555-1234": "+15125551234",  // leading space + dashes
	}
	for in, want := range cases {
		if got := toE164(in); got != want {
			t.Errorf("toE164(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMockSmsRecords(t *testing.T) {
	m := NewMockSms()
	r, err := m.Send("+15125551234", "up next on Court 3")
	if err != nil || !r.OK || len(m.Sent) != 1 {
		t.Fatalf("mock send: ok=%v err=%v sent=%d", r.OK, err, len(m.Sent))
	}
	if m.Sent[0].To != "+15125551234" || m.Sent[0].Body != "up next on Court 3" {
		t.Errorf("mock recorded wrong payload: %+v", m.Sent[0])
	}
}
