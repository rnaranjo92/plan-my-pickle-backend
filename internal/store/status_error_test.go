package store

import (
	"errors"
	"fmt"
	"testing"
)

// The schema probes hang off this distinction: a 4xx is the database ANSWERING
// ("no such column"), anything else means we never got an answer. Read a blip as
// an answer and a live migration reads as un-run, which on the rotation advance
// path silently rotates a whole room by default winners.
func TestStatusOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"missing column is a 400", dbError("select", "t", 400, nil), 400},
		{"server error keeps its status", dbError("select", "t", 503, nil), 503},
		{"wrapped still reports", fmt.Errorf("probing: %w",
			dbError("select", "t", 400, nil)), 400},
		{"a plain error has no status", errors.New("dial tcp: timeout"), 0},
		{"decode failure has no status", dbDecodeError("select", "t", nil), 0},
		{"nil has no status", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StatusOf(c.err); got != c.want {
				t.Fatalf("StatusOf(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}
