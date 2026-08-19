package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// filesProject is a project with a files block and, optionally, the migration
// and table configuration that back it.
//
// The migration's contents do not matter here — what is scaffolded is covered
// by internal/scaffold, and that it applies is covered by the Docker suite.
// What matters is the file's name, because that is how rig tells a rig_file
// table it created from one a project wrote itself.
func filesProject(t *testing.T, block string, withMigration, withConfig bool) string {
	t.Helper()

	root := newProject(t)
	// missing_comment is off because a real project's comments arrive from the
	// migration's COMMENT ON through introspection, and the schema written here
	// is a stub. What is under test is the files check, not comment coverage.
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
`+block)

	if withMigration {
		write(t, filepath.Join(root, "migrations", "00001_rig_files.sql"),
			"-- +goose Up\nSELECT 1;\n")
	}
	if withConfig {
		// No restore_window_days: rig_file's window is files.restore_window, and
		// the key is refused here. TestTheScaffoldedFileConfigurationValidates
		// holds the real scaffolded file to the same shape.
		write(t, filepath.Join(root, "services", "rig_file", "rig_file.yaml"),
			"table: rig_file\nresource: File\noperations: [Get, List]\n")
		addFileTable(t, filepath.Join(root, "schema.json"))
	}
	return root
}

// addFileTable puts a rig_file into the schema dump, because a table
// configuration for a table the database does not have is its own error and
// would mask the one under test.
func addFileTable(t *testing.T, path string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}

	col := func(name, sqlType string, ordinal int, nullable bool) map[string]any {
		return map[string]any{
			"name": name, "sql_type": sqlType, "udt_name": sqlType,
			"nullable": nullable, "ordinal": ordinal,
		}
	}
	tables, _ := schema["tables"].([]any)
	schema["tables"] = append(tables, map[string]any{
		"name": "rig_file", "kind": "Base",
		"columns": []any{
			map[string]any{
				"name": "id", "sql_type": "uuid", "udt_name": "uuid",
				"nullable": false, "ordinal": 1, "is_primary_key": true,
			},
			col("tenant_id", "uuid", 2, false),
			col("created_at", "timestamptz", 3, false),
			col("deleted_at", "timestamptz", 4, true),
			col("storage_key", "text", 5, false),
			col("url", "text", 6, true),
			col("file_name", "text", 7, false),
			col("content_type", "text", 8, false),
			col("declared_content_type", "text", 9, true),
			col("size_bytes", "bigint", 10, false),
			col("checksum", "text", 11, true),
			col("uploaded_at", "timestamptz", 12, true),
		},
		"primary_key": []any{"id"},
		"uniques":     []any{[]any{"tenant_id", "id"}},
		"indexes": []any{
			map[string]any{"name": "rig_file_pkey", "columns": []any{"id"}, "unique": true, "method": "btree"},
			map[string]any{"name": "rig_file_tenant_id_key", "columns": []any{"tenant_id", "id"}, "unique": true, "method": "btree"},
		},
	})

	out, err := json.MarshalIndent(schema, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(out))
}

func TestFilesNeedsItsMigration(t *testing.T) {
	root := filesProject(t, "files:\n  enabled: true\n", false, false)

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code == 0 {
		t.Error("a files block with no rig_file table should not validate")
	}
	if !strings.Contains(stderr, "no rig_file migration") {
		t.Errorf("stderr does not say what is missing:\n%s", stderr)
	}
}

// The dangerous case, and the reason this check exists at all.
//
// files.expose is what takes rig_file out of the ignore list and makes it a
// resource. The table configuration is what makes that resource read-only and
// narrow. With the first and not the second, rig_file would arrive with full
// CRUD over its storage key — a way to point a row at any object in the bucket,
// which is exactly what the design refuses to generate.
func TestExposingFilesWithoutAConfigurationIsRefused(t *testing.T) {
	root := filesProject(t, "files:\n  enabled: true\n  expose: true\n", true, false)

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code == 0 {
		t.Error("exposing rig_file with no table configuration should not validate")
	}
	if !strings.Contains(stderr, "full CRUD over its storage key") {
		t.Errorf("stderr does not say why it matters:\n%s", stderr)
	}
}

func TestFilesValidatesWithItsMigrationAndConfiguration(t *testing.T) {
	root := filesProject(t, "files:\n  enabled: true\n  expose: true\n", true, true)

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code != 0 {
		t.Errorf("this project should validate:\n%s", stderr)
	}
}

// Not exposing it needs no configuration: the table stays managed and
// unprojected, which is right for a project whose files are only ever fetched
// through the download route.
func TestFilesWithoutExposeNeedsNoConfiguration(t *testing.T) {
	root := filesProject(t, "files:\n  enabled: true\n", true, false)

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code != 0 {
		t.Errorf("an unexposed rig_file should need no table configuration:\n%s", stderr)
	}
}

// rig_file has no restore_window_days, and the window it does have comes from
// rig.yaml. Both halves are asserted here, because a table configuration that
// declared its own would be a second number able to disagree with how long the
// bytes are actually kept.
func TestFileRestoreWindowComesFromTheProject(t *testing.T) {
	root := filesProject(t,
		"files:\n  enabled: true\n  expose: true\n  restore_window: 168h\n", true, true)

	stdout, stderr, code := runWithSchema(t, root, "ir", "-C", root)
	if code != 0 {
		t.Fatalf("this project should compile:\n%s", stderr)
	}
	if !strings.Contains(stdout, `"restore_window_days": 7`) {
		t.Error("rig_file's restore window was not read from files.restore_window")
	}

	write(t, filepath.Join(root, "services", "rig_file", "rig_file.yaml"),
		"table: rig_file\nresource: File\noperations: [Get, List]\nrestore_window_days: 30\n")

	_, stderr, code = runWithSchema(t, root, "validate", "-C", root)
	if code == 0 {
		t.Error("restore_window_days on rig_file should be refused")
	}
	if !strings.Contains(stderr, "files.restore_window") {
		t.Errorf("stderr does not say where the window lives:\n%s", stderr)
	}
}

// The flow checkFilesFoundation prescribes, run end to end.
//
// The hand-written configuration above stands in for the scaffolded one
// everywhere else in this file, and a stand-in cannot catch the scaffolded file
// being wrong — which it was: it shipped without the restore window key while
// the rule still demanded one, so following the diagnostic's own advice left
// the project failing `rig validate`.
func TestTheScaffoldedFileConfigurationValidates(t *testing.T) {
	root := t.TempDir()
	if _, stderr, code := run(t, "init", root,
		"--name", "demo", "--module", "example.com/demo"); code != 0 {
		t.Fatalf("init failed:\n%s", stderr)
	}
	if _, stderr, code := run(t, "setup-project", "-C", root,
		"--expose", "rig_file"); code != 0 {
		t.Fatalf("setup-project failed:\n%s", stderr)
	}

	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: "off"
files:
  enabled: true
  expose: true
`)
	write(t, filepath.Join(root, "schema.json"), `{"name":"public","tables":[]}`)
	addFileTable(t, filepath.Join(root, "schema.json"))

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code != 0 {
		t.Errorf("the configuration `rig setup-project --expose rig_file` writes "+
			"should validate:\n%s", stderr)
	}
}

// What `--expose` tells you to write has to be the key that reads it.
//
// rig_file's is files.expose, and auth.expose does not reach it, so printing
// the auth block for it would be advice that leaves the project exactly where
// it was.
func TestExposeAdviceNamesTheRightKey(t *testing.T) {
	root := t.TempDir()
	if _, stderr, code := run(t, "init", root,
		"--name", "demo", "--module", "example.com/demo"); code != 0 {
		t.Fatalf("init failed:\n%s", stderr)
	}

	_, stderr, code := run(t, "setup-project", "-C", root,
		"--expose", "rig_account,rig_file")
	if code != 0 {
		t.Fatalf("setup-project failed:\n%s", stderr)
	}

	if !strings.Contains(stderr, "expose: [rig_account]") {
		t.Errorf("an auth table is not sent to auth.expose:\n%s", stderr)
	}
	if strings.Contains(stderr, "rig_account, rig_file") {
		t.Errorf("rig_file is sent to auth.expose, which does not read it:\n%s", stderr)
	}
	if !strings.Contains(stderr, "files:\n    enabled: true\n    expose: true") {
		t.Errorf("rig_file is not sent to files.expose:\n%s", stderr)
	}
}

// The other half of rig_file having a switch of its own: `auth.expose` must not
// reach it, or the key the rest of rig reads says one thing while the table is
// projected anyway — with no table configuration required and full CRUD over
// the storage key.
func TestAuthExposeDoesNotReachRigFile(t *testing.T) {
	root := filesProject(t, "auth:\n  expose: [rig_file]\n", true, true)

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code == 0 {
		t.Error("auth.expose should not project rig_file")
	}
	if !strings.Contains(stderr, "files.expose") {
		t.Errorf("stderr does not name the switch that does:\n%s", stderr)
	}
}
