package scaffold

import (
	"slices"
	"testing"
)

// A planned name is one rig has decided on and not built. Once the part ships,
// its table configuration reserves the name on its own and the entry here
// becomes a second copy of the answer — the exact drift `ReservedResources`
// derives its set to avoid.
//
// This is the test that catches it, and it has caught it once already: the five
// notification names sat here from the day M12 was written down until the day it
// shipped.
func TestPlannedNamesAreNotBuiltYet(t *testing.T) {
	t.Parallel()

	for resource, table := range plannedResources {
		if slices.Contains(Tables(), table) {
			t.Errorf("%s is planned for %s, which the foundation now creates — "+
				"delete the entry, its configuration reserves the name", resource, table)
		}
	}
}
