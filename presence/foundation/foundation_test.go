package foundation

import (
	"slices"
	"testing"

	"github.com/simonjanss/rig/presence"
)

func TestSetIsCoherent(t *testing.T) {
	if err := Set().Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestTheShippedSetHasNotMoved is the append-only rule — see the same test in
// auth/foundation for why it is a test rather than a comment.
func TestTheShippedSetHasNotMoved(t *testing.T) {
	want := []string{"00001_presence.sql"}

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

// TestTheModuleAndTheSetAgree ties the name the package writes SQL against to
// the migration that creates it.
func TestTheModuleAndTheSetAgree(t *testing.T) {
	tables := Set().Tables()
	if !slices.Contains(tables, presence.Table) {
		t.Errorf("presence writes against %q, which no migration in the set creates (it creates %v)",
			presence.Table, tables)
	}
}

// TestTheBookkeepingTableIsItsOwn is the reason each set has a private one: two
// modules sharing one would share a numbering sequence, and two of them adding a
// migration in the same release would collide on a version goose refuses to
// resolve.
func TestTheBookkeepingTableIsItsOwn(t *testing.T) {
	if Table == "rig_migrations" {
		t.Fatal("the set records itself in the project's own bookkeeping table")
	}
	if got := Set().Table; got != Table {
		t.Errorf("the set records itself in %q and the constant says %q", got, Table)
	}
}
