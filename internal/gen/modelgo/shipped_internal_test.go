package modelgo

import (
	"maps"
	"slices"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
)

// Two unrelated places have to agree about which tables ship their Go: this
// generator, which writes an alias instead of a declaration, and the check that
// warns a shipped table answers camelCase whatever `naming.json_case` says. They
// were once two hand-written lists, and rig_account was on one of them — so an
// exposed account resource disagreed with the rest of a snake_case API and
// nothing said so.
//
// Equal sets rather than one containment, because both directions are the same
// bug wearing different clothes: a table listed and not shipped generates a
// declaration nobody warned about, and a table shipped and not listed is the
// original.
func TestTheShippedTablesAreTheOnesTheCompilerNames(t *testing.T) {
	t.Parallel()

	got := slices.Sorted(maps.Keys(shippedModels))
	want := slices.Sorted(slices.Values(compile.ShippedModelTables()))

	if !slices.Equal(got, want) {
		t.Errorf("shippedModels has %v; compile.ShippedModelTables says %v", got, want)
	}
}
