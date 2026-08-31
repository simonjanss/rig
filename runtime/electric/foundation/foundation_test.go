package foundation

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/runtime/electric"
)

func TestSetIsCoherent(t *testing.T) {
	if err := Set().Validate(); err != nil {
		t.Fatal(err)
	}
}

// The SQL names the role as a literal and Go names it as a constant, because SQL has
// nowhere to read a Go constant from. This is what keeps the two from drifting: rename
// [electric.Role] without editing the migration and the role rig creates is not the role
// rig sets a password on, which fails at the far end as an authentication error against a
// role nobody looked for.
func TestTheMigrationNamesTheRoleGoNames(t *testing.T) {
	body, err := Set().Read("electric_role")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "CREATE ROLE "+electric.Role+" LOGIN") {
		t.Errorf("the migration does not create the role %q that electric.Role names", electric.Role)
	}
}
