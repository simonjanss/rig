package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Which mode a project is in, and what rig does about the two of them.
//
// The mode decides where rig's own tables come from, and the whole of it is
// checkable from a schema dump: whether a migration is vendored is a filename,
// and what the modules carry is a compile-time fact. Applying them for real is
// the Docker suite's.

// foundationProject is a project with a foundation mode and, optionally, rig's
// migrations vendored into it.
//
// The migration's contents do not matter — what is scaffolded is covered by
// internal/scaffold. What matters is the filename, because under `vendored` that
// is how rig tells its own tables from a project's.
func foundationProject(t *testing.T, mode string, vendored ...string) string {
	t.Helper()

	root := newProject(t)
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
migrations:
  foundation: `+mode+`
`)

	// The directory has to exist either way: a missing one is a project between
	// `rig init` and its first migration, which contradicts nothing and is
	// deliberately not what these cases are about.
	if err := os.MkdirAll(filepath.Join(root, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, part := range vendored {
		write(t, filepath.Join(root, "migrations",
			strings.Repeat("0", 4)+string(rune('1'+i))+"_rig_"+part+".sql"),
			"-- +goose Up\nSELECT 1;\n")
	}
	return root
}

// Switching a project that had vendored the foundation over to embedded is the
// one contradiction that has to be refused rather than discovered. The modules
// would re-apply a schema that is already there, and find out partway through a
// `rig db up` that had already run whatever came before it.
func TestEmbeddedWithVendoredMigrationsIsRefused(t *testing.T) {
	t.Parallel()

	root := foundationProject(t, "embedded", "tenancy")

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code == 0 {
		t.Fatalf("a mode contradicting the directory should fail\n%s", stderr)
	}
	if !strings.Contains(stderr, "RIG3004") {
		t.Errorf("want RIG3004, got:\n%s", stderr)
	}
	// The message has to name what is in the way, because "change the mode" is
	// not the fix — the files are.
	if !strings.Contains(stderr, "tenancy") {
		t.Errorf("the message should name the vendored part:\n%s", stderr)
	}
}

// And the ordinary case of each mode passes. Embedded with an empty directory is
// the state `setup-project` leaves behind; vendored with the migrations in it is
// what every project has today.
func TestBothModesAreAcceptedWhenTheyMatch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		mode     string
		vendored []string
	}{
		{"embedded with nothing vendored", "embedded", nil},
		{"vendored with the migrations there", "vendored", []string{"tenancy"}},
		// A vendored project with an empty directory is fine as long as nothing
		// asks for a part. The blocks that do ask are reported by their own
		// checks, which name the block rather than the mode.
		{"vendored with nothing yet", "vendored", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := foundationProject(t, tc.mode, tc.vendored...)
			_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
			if code != 0 {
				t.Fatalf("should validate\n%s", stderr)
			}
			if strings.Contains(stderr, "RIG3004") {
				t.Errorf("no mode contradiction here:\n%s", stderr)
			}
		})
	}
}

// `auth.own` and `embedded` are the same contradiction stated the other way
// round, so they are refused together rather than one of them quietly winning.
//
// Which one would it be? Believing `own` makes the mode a key rig read and threw
// away; believing `embedded` has `rig db up` apply the modules' sets over the
// migrations this project forked and stop on a table that already exists. Neither
// is a thing to do silently, and the refusal does not depend on how the forked
// migrations happen to be named — which is the only evidence the directory check
// below it has.
func TestAuthOwnWithEmbeddedIsRefused(t *testing.T) {
	t.Parallel()

	// No `_rig_*.sql` in the directory, so nothing but the two keys is in play.
	root := newProject(t)
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
auth:
  own: true
migrations:
  foundation: embedded
`)
	write(t, filepath.Join(root, "migrations", "00001_forked_identities.sql"),
		"-- +goose Up\nSELECT 1;\n")

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code == 0 {
		t.Fatalf("auth.own and embedded contradict each other\n%s", stderr)
	}
	if !strings.Contains(stderr, "RIG3004") {
		t.Errorf("want RIG3004, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "auth.own") {
		t.Errorf("the message should name the other key:\n%s", stderr)
	}
}

// And `auth.own` with the default mode is the ordinary arrangement it always was.
func TestAuthOwnIsFineWhenVendored(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
auth:
  own: true
`)
	write(t, filepath.Join(root, "migrations", "00001_rig_tenancy.sql"),
		"-- +goose Up\nSELECT 1;\n")

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code != 0 {
		t.Fatalf("auth.own owns the schema and that is allowed\n%s", stderr)
	}
	if strings.Contains(stderr, "RIG3004") {
		t.Errorf("nothing contradicts anything here:\n%s", stderr)
	}
}

// And `auth.own` keeps everything rig withholds from a table of its own.
//
// A forked foundation is the project's schema. The stub is where its rules go —
// there is nowhere else, because rig ships no configuration for a table it no
// longer considers its own — and the convention rules are advice about a schema
// somebody maintains, which this now is. Both are withheld by the same two
// readings of one list, and the list is deliberately still populated here for
// [checkReserved]'s sake, so both have to ask.
func TestAuthOwnKeepsItsStubsAndItsAdvice(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
  unmentioned_column: warn
auth:
  own: true
files:
  enabled: true
layout:
  table_dir: services/{table}
  config_file: "{table_dir}/{table}.yaml"
generators:
  - name: model-go
    out_dir: internal/model
    options:
      package: model
  - name: persist-go
    out_dir: internal/store
    options:
      package: store
      model_import: example.com/demo/internal/model
  - name: service-go
    out_dir: internal/api
    options:
      package: api
      model_import: example.com/demo/internal/model
      store_import: example.com/demo/internal/store
      api_import: example.com/demo/internal/api
      stub_dir: services/{table}
`)
	write(t, filepath.Join(root, "migrations", "00001_rig_files.sql"),
		"-- +goose Up\nSELECT 1;\n")
	addFileTable(t, filepath.Join(root, "schema.json"))

	_, stderr, code := runWithSchema(t, root, "generate", "-C", root)
	if code != 0 {
		t.Fatalf("generate failed:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "services", "rig_file", "rig_file.go")); err != nil {
		t.Errorf("a forked foundation table got no service stub: %v", err)
	}
	if !strings.Contains(stderr, "rig_file.storage_key is not mentioned") {
		t.Errorf("the columns of a forked foundation table are the project's to describe:\n%s", stderr)
	}
}

// A table one of rig's sets creates is rig's, whether or not the configuration
// asked for the feature that migration belongs to.
//
// A set is applied whole, so `auth:` with no provider configured has `rig db up`
// create rig_identity_oauth — and rig has to know that, or it reports RIG2005 on a
// table it made itself one command earlier, with advice (rename the migration that
// creates it) no project in this mode can follow. Same for an inbox with no
// `auth:` block, which reaches all of auth's set on its way to rig_account.
func TestEmbeddedAcceptsEveryTableItsSetsCreate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		block string
		table string
	}{
		{"auth with no provider configured", "auth:\n  enabled: true\n", "rig_identity_oauth"},
		{"an inbox with no auth block", "notifications:\n  enabled: true\n", "rig_auth_log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Nothing of the project's own, so the only table in the schema is the
			// one the modules created and the only rule in play is the prefix.
			root := t.TempDir()
			write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
migrations:
  foundation: embedded
`+tc.block)
			write(t, filepath.Join(root, "schema.json"), foundationSchema(tc.table))
			if err := os.MkdirAll(filepath.Join(root, "migrations"), 0o755); err != nil {
				t.Fatal(err)
			}

			_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
			if code != 0 {
				t.Fatalf("%s is rig's own\n%s", tc.table, stderr)
			}
			if strings.Contains(stderr, "RIG2005") {
				t.Errorf("%s is created by a set this project applies:\n%s", tc.table, stderr)
			}
		})
	}
}

// foundationSchema is a schema dump holding one of rig's own tables and nothing
// else, which is what introspection returns once `rig db up` has applied the
// modules' sets. The columns do not matter: the rule under test reads the name and
// what created it.
func foundationSchema(table string) string {
	return `{
  "name": "public",
  "tables": [
    {
      "name": "` + table + `",
      "kind": "Base",
      "columns": [
        { "name": "id", "sql_type": "uuid", "udt_name": "uuid", "ordinal": 1, "is_primary_key": true }
      ],
      "primary_key": ["id"]
    }
  ]
}
`
}

// Under embedded there is nothing to vendor, so `setup-project` writes no SQL —
// and has to say where the schema went, because "created nothing" and "the
// modules have it" look identical from the outside.
func TestSetupProjectWritesNoSQLUnderEmbedded(t *testing.T) {
	t.Parallel()

	root := foundationProject(t, "embedded")

	_, stderr, code := run(t, "-C", root, "setup-project")
	if code != 0 {
		t.Fatalf("setup-project should succeed\n%s", stderr)
	}

	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			t.Errorf("wrote %s; under embedded the modules carry the schema", e.Name())
		}
	}
	if !strings.Contains(stderr, "embedded") {
		t.Errorf("it should say why nothing was written:\n%s", stderr)
	}
	for _, module := range []string{"rig/auth", "rig/files", "rig/notify"} {
		if !strings.Contains(stderr, module) {
			t.Errorf("it should name %s as where the schema is:\n%s", module, stderr)
		}
	}
}

// Vendored is the default, so a project that never heard of the key keeps
// getting the migrations in its own directory.
func TestSetupProjectVendorsByDefault(t *testing.T) {
	t.Parallel()

	root := newProject(t)

	_, stderr, code := run(t, "-C", root, "setup-project")
	if code != 0 {
		t.Fatalf("setup-project should succeed\n%s", stderr)
	}

	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	var sql int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			sql++
		}
	}
	if sql == 0 {
		t.Error("the default should write the migrations")
	}
}

// Every block that owns rig tables refuses being turned on with nothing behind
// it, driven off the same description the check is.
//
// This was three copies of one control flow and it carried two checks. The other
// one — an exposed table with no table configuration — is gone, because rig now
// supplies that configuration itself: see addFoundationConfigs, and
// TestExposingFilesNeedsNoConfiguration for the reading that replaced it.
//
// What is left is reported for tidiness rather than safety: a project whose
// migration never ran fails on its first request either way.
func TestEveryFoundationBlockRefusesBeingTurnedOnWithNothingBehindIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		block string
		table string
	}{
		{"files", "rig_file"},
		{"notifications", "rig_notification"},
		{"presence", "rig_presence"},
	} {
		t.Run(tc.block, func(t *testing.T) {
			t.Parallel()

			// Enabled with nothing scaffolded.
			bare := newProject(t)
			write(t, filepath.Join(bare, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
`+tc.block+`:
  enabled: true
`)
			_, stderr, code := runWithSchema(t, bare, "validate", "-C", bare)
			if code == 0 {
				t.Errorf("%s.enabled with no migration should be refused", tc.block)
			}
			if !strings.Contains(stderr, tc.block+".enabled is set but this project has no "+tc.table) ||
				!strings.Contains(stderr, "run `rig setup-project`") {
				t.Errorf("the missing migration does not name the block, the table and the remedy:\n%s", stderr)
			}

		})
	}
}
