package service

import "testing"

// entrantRefs is the whole correctness argument for a merge: every column that
// points at the entrant being folded away has to be repointed BEFORE it's
// deleted, or the history it was supposed to preserve goes with it.
//
// This test is a tripwire. If a new table starts referencing ladder_entrants
// and isn't added here, a merge silently drops that data — and the failure only
// shows up weeks later as a record that doesn't add up.
func TestEntrantRefsCoverEveryReferencingColumn(t *testing.T) {
	// What the schema references today, verified against the query layer.
	want := map[string][]string{
		"ladder_matches": {
			"entrant_a_id", "entrant_b_id", "winner_entrant_id",
		},
		"ladder_challenges": {
			"challenger_entrant_id", "challenged_entrant_id",
		},
		"rotation_players": {"entrant_id"},
	}

	got := map[string]map[string]bool{}
	for _, ref := range entrantRefs {
		if got[ref.table] == nil {
			got[ref.table] = map[string]bool{}
		}
		if got[ref.table][ref.column] {
			t.Errorf("%s.%s listed twice — the merge would repoint it twice",
				ref.table, ref.column)
		}
		got[ref.table][ref.column] = true
	}

	for table, cols := range want {
		for _, col := range cols {
			if !got[table][col] {
				t.Errorf("%s.%s is not in entrantRefs — a merge would leave it "+
					"pointing at a deleted entrant", table, col)
			}
		}
	}
	for table, cols := range got {
		for col := range cols {
			found := false
			for _, w := range want[table] {
				if w == col {
					found = true
				}
			}
			if !found {
				t.Errorf("entrantRefs lists %s.%s, which this test doesn't know "+
					"about — update the test or drop the ref", table, col)
			}
		}
	}
}

// Every ref must name a real table and column, since these strings are
// interpolated straight into the query.
func TestEntrantRefsAreWellFormed(t *testing.T) {
	for _, ref := range entrantRefs {
		if ref.table == "" || ref.column == "" {
			t.Fatalf("empty ref: %+v", ref)
		}
		for _, s := range []string{ref.table, ref.column} {
			for _, c := range s {
				ok := (c >= 'a' && c <= 'z') || c == '_' ||
					(c >= '0' && c <= '9')
				if !ok {
					t.Errorf("%q is not a bare identifier — it goes into a "+
						"query unquoted", s)
					break
				}
			}
		}
	}
}
