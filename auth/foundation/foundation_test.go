package foundation

import (
	"strings"
	"testing"
)

func TestSetIsCoherent(t *testing.T) {
	if err := Set().Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestTheShippedSetHasNotMoved is the append-only rule, written down.
//
// A migration in this list has been applied to somebody's database. Renumbering
// or renaming one leaves that database and this directory describing different
// schemas with nothing to say so, and a project that vendored the set keeps the
// old name in its own migrations directory forever. A change to these tables is
// a new entry at the end, and updating this list is how you say you meant it.
func TestTheShippedSetHasNotMoved(t *testing.T) {
	want := []string{
		"00001_tenancy.sql",
		"00002_apikeys.sql",
		"00003_sessions.sql",
		"00004_oauth.sql",
	}

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

// TestEveryTableCarriesThePrefix guards the other half of the reservation: rig
// refuses a project any table under `rig_`, so a foundation table that forgot
// the prefix would be a name a project could also take.
func TestEveryTableCarriesThePrefix(t *testing.T) {
	for _, table := range Set().Tables() {
		if !strings.HasPrefix(table, "rig_") {
			t.Errorf("table %q does not carry the rig_ prefix", table)
		}
	}
}
