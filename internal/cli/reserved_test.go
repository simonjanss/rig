// The reserved names, met from the command line.
//
// The rule itself is the compiler's, and internal/compile tests it. What is here
// is the earlier door: `rig migration new --table` asks before the file exists,
// so the fix is a different word on a command line rather than a migration, a
// rename in every client, and a deprecation window.
//
// It asks, rather than refuses, because the two halves of the rule are not
// equally final. Nothing moves a table off the `rig_` prefix, so that one is a
// refusal. A reserved resource name is only what the table projects to by
// default and a `resource:` key moves it, so that one is a warning naming the
// key — refusing it would make `table: file` with `resource: Document`, which the
// compiler allows, a table rig cannot scaffold.
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
		// Under the prefix, by a name rig does not use.
		{"rig_leaderboard", "rig_ prefix"},
		// Under the prefix, by a name rig does use. Different advice: the table
		// is rig's, so what is missing is the migration filename that says so.
		{"rig_api_key", "_rig_apikeys.sql"},
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

// A reserved resource name is a warning and the migration is still written,
// because a `resource:` key answers it and the table itself is allowed. The
// warning has to name that key: a message that only said the name was taken
// would read as a refusal that failed to refuse.
func TestMigrationNewWarnsAboutAReservedResourceName(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := run(t, "-C", root, "migration", "new", "create_account", "--table", "account")
	if code != 0 {
		t.Fatalf("--table account was refused: %s", stderr)
	}
	for _, want := range []string{"rig_account", "resource:"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to mention %q", stderr, want)
		}
	}

	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("wrote %d file(s), want the migration", len(entries))
	}
}

// A name rig does not reserve still works, which is most of them, and it says
// nothing at all. Without this the two tests above would pass on a command that
// refused or warned about everything.
func TestMigrationNewAcceptsAnOrdinaryTableName(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := run(t, "-C", root, "migration", "new", "create_bookmark", "--table", "bookmark")
	if code != 0 {
		t.Fatalf("bookmark was refused: %s", stderr)
	}
	if strings.Contains(stderr, "warning") {
		t.Errorf("stderr = %q, want no warning for an ordinary name", stderr)
	}
}

// `auth.own` is the off switch here too. A project that has forked the schema
// owns those tables, so refusing it a migration for one would be refusing it its
// own foundation.
//
// rig_account is what makes this test mean something: the prefix is the half
// that is a refusal, so without the switch this command fails.
func TestMigrationNewUnderAuthOwn(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
auth:
  own: true
`)

	_, stderr, code := run(t, "-C", root, "migration", "new", "create_account", "--table", "rig_account")
	if code != 0 {
		t.Fatalf("auth.own did not turn the rule off: %s", stderr)
	}
	if strings.Contains(stderr, "warning") {
		t.Errorf("stderr = %q, want no warning under auth.own", stderr)
	}
}
