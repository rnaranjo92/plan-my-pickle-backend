package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// The club leaderboard is sold as a system of record. Both of these were silent
// errors in it — not crashes, just a table quietly saying something untrue about
// who is winning, which is the only thing it exists to say.
func TestClubStandingKey(t *testing.T) {
	accounts := map[string]string{
		"p-jen-march": "acct-jen",
		"p-jen-june":  "acct-jen",
		"p-chris-1":   "acct-chris-one",
		"p-chris-2":   "acct-chris-two",
	}
	row := func(playerID, name string) model.Standing {
		return model.Standing{PlayerID: playerID, FullName: name}
	}

	t.Run("one member who signed up twice is ONE line", func(t *testing.T) {
		// She joined as "Jen W" in March and "Jen Whitfield" in June. Name
		// merging split her in two and halved a record she actually earned.
		a := clubStandingKey(row("p-jen-march", "Jen W"), accounts)
		b := clubStandingKey(row("p-jen-june", "Jen Whitfield"), accounts)
		if a != b {
			t.Errorf("same account split into %q and %q", a, b)
		}
	})

	t.Run("two members who share a name are TWO lines", func(t *testing.T) {
		// Name merging added two different people together, and the club had no
		// way to see it had happened.
		a := clubStandingKey(row("p-chris-1", "Chris B"), accounts)
		b := clubStandingKey(row("p-chris-2", "Chris B"), accounts)
		if a == b {
			t.Errorf("two accounts merged into one key %q", a)
		}
	})

	t.Run("walk-ups still merge by name", func(t *testing.T) {
		// No account to key on, so the old rule stands — and a duplicate here is
		// a roster problem, fixed with the ladder Merge action.
		a := clubStandingKey(row("p-walkup-1", "Sam Ortiz"), accounts)
		b := clubStandingKey(row("p-walkup-2", "  sam ortiz  "), accounts)
		if a != b {
			t.Errorf("unlinked walk-ups didn't merge: %q vs %q", a, b)
		}
	})

	t.Run("an account beats the name, both directions", func(t *testing.T) {
		// Same name, one linked and one not: they must NOT merge, because the
		// linked one is a known person and the other is an unknown.
		linked := clubStandingKey(row("p-chris-1", "Chris B"), accounts)
		unlinked := clubStandingKey(row("p-nobody", "Chris B"), accounts)
		if linked == unlinked {
			t.Error("a linked account merged with an unlinked name")
		}
	})

	t.Run("a row with no name and no account is dropped", func(t *testing.T) {
		// There is nothing to attribute the record to, and inventing a blank
		// row would put an anonymous line on the club's leaderboard.
		if got := clubStandingKey(row("", ""), accounts); got != "" {
			t.Errorf("nameless unlinked row got key %q, want dropped", got)
		}
		if got := clubStandingKey(row("", "   "), accounts); got != "" {
			t.Errorf("whitespace-only name got key %q, want dropped", got)
		}
	})

	t.Run("an empty account map is exactly the old behaviour", func(t *testing.T) {
		// The lookup is best-effort: if it fails, everything must fall back to
		// name-merging rather than the leaderboard emptying out.
		none := map[string]string{}
		a := clubStandingKey(row("p-jen-march", "Jen W"), none)
		b := clubStandingKey(row("p-other", "jen w"), none)
		if a != b {
			t.Errorf("fallback didn't merge by name: %q vs %q", a, b)
		}
		if a == "" {
			t.Error("fallback dropped a named row")
		}
	})
}
