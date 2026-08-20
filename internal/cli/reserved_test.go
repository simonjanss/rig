// The reserved names, met from the command line.
//
// The rule itself is the compiler's, and internal/compile tests it. What is here
// is the earlier door: `rig migration new --table` refuses before the file
// exists, so the fix is a different word on a command line rather than a
// migration, a rename in every client, and a deprecation window.
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationNewRefusesAReservedTableName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		table string
		want  string
	}{
		// Projects to Account, which rig_account takes.
		{"account", "rig_account"},
		// Under the prefix, by a name rig does not use.
		{"rig_leaderboard", "rig_ prefix"},
		// Under the prefix, by a name rig does use. Different advice.
		{"rig_api_key", "rig setup-project"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			t.Parallel()

			root := newProject(t)
			_, stderr, code := run(t, "-C", root, "migration", "new", "create_thing", "--table", tc.table)
			if code == 0 {
				t.Fatalf("--table %s was accepted", tc.table)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}

			// Nothing written. The check runs before the directory is made, so a
			// refused name leaves no trace to clean up.
			if entries, err := os.ReadDir(filepath.Join(root, "migrations")); err == nil && len(entries) > 0 {
				t.Errorf("a refused name still wrote %d file(s)", len(entries))
			}
		})
	}
}

// A name rig does not reserve still works, which is most of them. Without this
// the test above would pass on a command that refused everything.
func TestMigrationNewAcceptsAnOrdinaryTableName(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := run(t, "-C", root, "migration", "new", "create_bookmark", "--table", "bookmark")
	if code != 0 {
		t.Fatalf("bookmark was refused: %s", stderr)
	}
}

// `auth.own` is the off switch here too. A project that has forked the schema
// owns those tables, so refusing it a migration for one would be refusing it its
// own foundation.
func TestMigrationNewUnderAuthOwn(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
auth:
  own: true
`)

	_, stderr, code := run(t, "-C", root, "migration", "new", "create_account", "--table", "account")
	if code != 0 {
		t.Fatalf("auth.own did not turn the rule off: %s", stderr)
	}
}
