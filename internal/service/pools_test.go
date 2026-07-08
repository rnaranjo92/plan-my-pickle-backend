package service

import "testing"

func unitIDs(n int) [][]string {
	out := make([][]string, n)
	for i := range out {
		out[i] = []string{string(rune('A' + i))}
	}
	return out
}

// splitPools: <8 units = one pool (unchanged legacy behavior); 8+ split into
// ceil(n/5) pools with sizes within 1, snake-seeded by rating so pool strength
// is balanced, and every unit lands in exactly one pool.
func TestSplitPools(t *testing.T) {
	skill := map[string]float64{}
	for i := 0; i < 26; i++ {
		// 'A' is the strongest, descending — so units are already rating-ordered.
		skill[string(rune('A'+i))] = float64(100 - i)
	}

	cases := []struct {
		n         int
		wantPools int
	}{
		{4, 1}, {7, 1}, // small fields: one pool, exactly the old behavior
		{8, 2}, {10, 2}, {12, 3}, {13, 3}, {16, 4},
	}
	for _, c := range cases {
		pools := splitPools(unitIDs(c.n), skill)
		if len(pools) != c.wantPools {
			t.Fatalf("n=%d: pools=%d want %d", c.n, len(pools), c.wantPools)
		}
		// Every unit appears exactly once; sizes within 1 of each other.
		seen := map[string]bool{}
		minSz, maxSz := 1<<30, 0
		for _, p := range pools {
			if len(p) < minSz {
				minSz = len(p)
			}
			if len(p) > maxSz {
				maxSz = len(p)
			}
			for _, u := range p {
				if seen[u[0]] {
					t.Fatalf("n=%d: unit %s in two pools", c.n, u[0])
				}
				seen[u[0]] = true
			}
		}
		if len(seen) != c.n {
			t.Fatalf("n=%d: %d units placed, want %d", c.n, len(seen), c.n)
		}
		if c.wantPools > 1 && maxSz-minSz > 1 {
			t.Fatalf("n=%d: pool sizes %d..%d differ by >1", c.n, minSz, maxSz)
		}
	}

	// Snake seeding at n=8/2 pools: 1→A, 2→B, 3→B, 4→A, 5→A, 6→B, 7→B, 8→A.
	pools := splitPools(unitIDs(8), skill)
	wantA := "ADEH" // seeds 1,4,5,8
	gotA := ""
	for _, u := range pools[0] {
		gotA += u[0]
	}
	if gotA != wantA {
		t.Fatalf("snake pool A = %q, want %q", gotA, wantA)
	}
}

func TestPoolGroupName(t *testing.T) {
	if poolGroupName(0) != "pool_a" || poolGroupName(1) != "pool_b" || poolGroupName(25) != "pool_z" {
		t.Fatalf("letter names wrong: %s %s %s", poolGroupName(0), poolGroupName(1), poolGroupName(25))
	}
	if poolGroupName(26) != "pool_27" {
		t.Fatalf("overflow name = %s, want pool_27", poolGroupName(26))
	}
}
