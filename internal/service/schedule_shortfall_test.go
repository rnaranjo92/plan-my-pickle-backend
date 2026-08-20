package service

import "strings"

import "testing"

// The organizer reads this while standing on court with players waiting, so it
// has to lead with the cause and the fix — not just "no games".
func TestNoGamesBuiltMsgLeadsWithTheFix(t *testing.T) {
	msg := noGamesBuiltMsg("doubles", []string{"Women's 3.0: 3 checked in, needs 4"})
	for _, want := range []string{"4 players checked in", "Players tab", "Women's 3.0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should mention %q; got: %s", want, msg)
		}
	}
}

func TestNoGamesBuiltMsgSinglesNeedsTwo(t *testing.T) {
	msg := noGamesBuiltMsg("singles", nil)
	if !strings.Contains(msg, "2 players") || !strings.Contains(msg, "singles") {
		t.Errorf("singles should ask for 2: %s", msg)
	}
	// With no per-division detail there should be no empty trailing parenthetical.
	if strings.Contains(msg, "()") {
		t.Errorf("empty detail should be omitted: %s", msg)
	}
}

// An unnamed division still has to read as a sentence.
func TestShortDivisionMsgHandlesBlankName(t *testing.T) {
	if got := shortDivisionMsg("  ", 1, 4); !strings.HasPrefix(got, "Your division") {
		t.Errorf("blank name should fall back: %s", got)
	}
	if got := shortDivisionMsg("Women's 3.0", 3, 4); got != "Women's 3.0: 3 checked in, needs 4" {
		t.Errorf("unexpected: %s", got)
	}
}
