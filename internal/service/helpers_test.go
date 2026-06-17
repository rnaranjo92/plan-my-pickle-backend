package service

import "testing"

// sameSet backs the re-score cascade (advanceTeam) — it decides whether a
// re-advanced bracket slot actually changed and downstream must reset.
func TestSameSet(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"order-agnostic", []string{"a", "b"}, []string{"b", "a"}, true},
		{"single", []string{"a"}, []string{"a"}, true},
		{"both empty", nil, nil, true},
		{"different length", []string{"a", "b"}, []string{"a"}, false},
		{"different member", []string{"a", "b"}, []string{"a", "c"}, false},
		{"multiset (dupes matter)", []string{"a", "a"}, []string{"a", "b"}, false},
	}
	for _, c := range cases {
		if got := sameSet(c.a, c.b); got != c.want {
			t.Errorf("%s: sameSet(%v,%v)=%v want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

// normPhone backs the exact-match check-in (no partial/suffix oracle).
func TestNormPhone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"+1 (555) 100-0000", "5551000000"},
		{"5551000000", "5551000000"},
		{"15551000000", "5551000000"}, // drops the NANP country code
		{"555.100.0000", "5551000000"},
		{"  555 100 0000  ", "5551000000"},
		{"1000000", "1000000"}, // short stays short (won't match a 10-digit)
		{"", ""},
	}
	for _, c := range cases {
		if got := normPhone(c.in); got != c.want {
			t.Errorf("normPhone(%q)=%q want %q", c.in, got, c.want)
		}
	}
	// Two different full numbers must not collide (the oracle fix).
	if normPhone("5551000000") == normPhone("5551000001") {
		t.Error("distinct numbers normalized equal")
	}
}

// seedSides orders bracket sides best-first by average skill (used for
// single-elim seeding + medal/playoff seeding).
func TestSeedSides(t *testing.T) {
	skill := map[string]float64{"hi": 4.5, "mid": 3.5, "lo": 2.5}
	out := seedSides([][]string{{"lo"}, {"hi"}, {"mid"}}, skill)
	if len(out) != 3 || out[0][0] != "hi" || out[1][0] != "mid" || out[2][0] != "lo" {
		t.Errorf("seedSides order = %v, want [[hi] [mid] [lo]]", out)
	}
	// Pair averages: {hi,lo}=3.5 should outrank {mid,lo}=3.0.
	pairs := seedSides([][]string{{"mid", "lo"}, {"hi", "lo"}}, skill)
	if !sameSet(pairs[0], []string{"hi", "lo"}) {
		t.Errorf("pair seed order = %v, want {hi,lo} first", pairs)
	}
}

// pairsFromRegs forms doubles teams: honor fixed partners first, then pair up
// the leftovers two-by-two.
func TestPairsFromRegs(t *testing.T) {
	regs := []reg{
		{id: "1", playerID: "a", partnerID: "b"},
		{id: "2", playerID: "b", partnerID: "a"},
		{id: "3", playerID: "c"},
		{id: "4", playerID: "d"},
	}
	pairs := pairsFromRegs(regs)
	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want 2: %v", len(pairs), pairs)
	}
	if !sameSet(pairs[0], []string{"a", "b"}) {
		t.Errorf("first pair = %v, want the fixed {a,b}", pairs[0])
	}
	if !sameSet(pairs[1], []string{"c", "d"}) {
		t.Errorf("leftover pair = %v, want {c,d}", pairs[1])
	}

	// An unmatched leftover (odd count) is dropped, not half-paired.
	odd := pairsFromRegs([]reg{{id: "1", playerID: "x"}})
	if len(odd) != 0 {
		t.Errorf("single leftover produced %v, want no pair", odd)
	}
}
