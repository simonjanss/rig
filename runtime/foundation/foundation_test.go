package foundation

import "testing"

func TestSetIsCoherent(t *testing.T) {
	if err := Set().Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestTheShippedSetHasNotMoved is the append-only rule — see the same test in
// auth/foundation for why it is a test rather than a comment.
func TestTheShippedSetHasNotMoved(t *testing.T) {
	want := []string{"00001_idempotency.sql"}

	got := Set().Migrations
	if len(got) < len(want) {
		t.Fatalf("the set lost migrations: have %d, shipped %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].File() != name {
			t.Errorf("migration %d is %s, shipped as %s", i+1, got[i].File(), name)
		}
	}
}
