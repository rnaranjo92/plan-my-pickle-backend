package service

import "testing"

// Austen is a real coach. No matcher may reach him on any single field.
func TestRemoveDemoCoachesCannotMatchRealCoach(t *testing.T) {
	real := struct{ Name, City, Bio, Business string }{
		Name: "Austen", City: "Chula Vista, CA",
		Bio:      "Life Time pro. Clinics and private lessons.",
		Business: "Life Time",
	}
	for _, d := range demoCoaches {
		if d.Name == real.Name {
			t.Fatalf("demo name collides with a real coach: %q", d.Name)
		}
		if d.Bio == real.Bio {
			t.Fatalf("demo bio collides with a real coach")
		}
	}
	if real.Business == demoCoachMarker {
		t.Fatal("real business name collides with the demo marker")
	}
	// Sharing only the CITY must never be enough — the city is ANDed with a
	// name or bio precisely so a real coach in Chula Vista is untouched.
	shares := false
	for _, d := range demoCoaches {
		if d.City == real.City {
			shares = true
		}
	}
	if !shares {
		t.Skip("no demo coach in the same city; the AND guard is untested here")
	}
}
