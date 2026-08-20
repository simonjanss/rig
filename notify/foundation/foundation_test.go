package foundation

import (
	"slices"
	"testing"

	"github.com/simonjanss/rig/notify"
)

func TestSetIsCoherent(t *testing.T) {
	if err := Set().Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestTheShippedSetHasNotMoved is the append-only rule — see the same test in
// auth/foundation for why it is a test rather than a comment.
func TestTheShippedSetHasNotMoved(t *testing.T) {
	want := []string{"00001_notifications.sql"}

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

// TestTheModuleAndTheSetAgree ties the names the package writes SQL against to
// the migration that creates them.
func TestTheModuleAndTheSetAgree(t *testing.T) {
	tables := Set().Tables()
	for _, name := range []string{notify.Table, notify.RecipientTable, notify.DeliveryTable} {
		if !slices.Contains(tables, name) {
			t.Errorf("notify writes against %q, which no migration in the set creates (it creates %v)",
				name, tables)
		}
	}
}
