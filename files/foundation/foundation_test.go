package foundation

import (
	"testing"

	"github.com/simonjanss/rig/files"
)

func TestSetIsCoherent(t *testing.T) {
	if err := Set().Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestTheShippedSetHasNotMoved is the append-only rule — see the same test in
// auth/foundation for why it is a test rather than a comment.
func TestTheShippedSetHasNotMoved(t *testing.T) {
	want := []string{"00001_files.sql"}

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

// TestTheModuleAndTheSetAgree ties [files.Table] to the migration that creates
// it. The package spells the name because its SQL needs it and the set spells it
// because vendoring needs it; this is what keeps the two spellings one name.
func TestTheModuleAndTheSetAgree(t *testing.T) {
	tables := Set().Tables()
	found := false
	for _, table := range tables {
		if table == files.Table {
			found = true
		}
	}
	if !found {
		t.Errorf("files.Table is %q, which no migration in the set creates (it creates %v)",
			files.Table, tables)
	}
}
