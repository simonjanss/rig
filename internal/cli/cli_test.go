package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/cli"
)

// run invokes the command line the way a shell does, and returns what the user
// would see plus the exit code.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Main(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// project writes a minimal but complete project and returns its root.
func newProject(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
`)
	write(t, filepath.Join(root, ".rig", "schema.json"), todoSchema)
	write(t, filepath.Join(root, "services", "todo", "todo.yaml"), `table: todo
comment: A single thing someone means to get done.
columns:
  title:
    comment: Short description of the task.
`)
	return root
}

const todoSchema = `{
  "name": "public",
  "tables": [
    {
      "name": "todo",
      "kind": "Base",
      "columns": [
        { "name": "id", "sql_type": "uuid", "udt_name": "uuid", "ordinal": 1, "is_primary_key": true },
        { "name": "tenant_id", "sql_type": "uuid", "udt_name": "uuid", "ordinal": 2 },
        { "name": "created_at", "sql_type": "timestamptz", "udt_name": "timestamptz", "ordinal": 3 },
        { "name": "title", "sql_type": "text", "udt_name": "text", "ordinal": 4 }
      ],
      "primary_key": ["id"],
      "indexes": [
        { "name": "todo_pkey", "columns": ["id"], "unique": true },
        { "name": "todo_tenant_idx", "columns": ["tenant_id"] }
      ]
    }
  ],
  "enums": []
}
`

func TestValidateCleanProject(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := run(t, "validate", "-C", root)

	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "no problems found") {
		t.Errorf("expected a summary line:\n%s", stderr)
	}
}

func TestValidateReportsOneDiagnosticForOneTypo(t *testing.T) {
	t.Parallel()

	// A mistyped key used to drop the whole file, which then made every column
	// report as unmentioned and uncommented — twenty diagnostics burying the
	// one that mattered. The typo should produce exactly one.
	root := newProject(t)
	write(t, filepath.Join(root, "services", "todo", "todo.yaml"), `table: todo
comment: A single thing someone means to get done.
columns:
  title:
    commnt: Short description of the task.
`)

	_, stderr, code := run(t, "validate", "-C", root)
	if code == 0 {
		t.Fatal("a mistyped key should fail validation")
	}
	if n := strings.Count(stderr, "error["); n != 1 {
		t.Fatalf("got %d errors, want exactly 1:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "5:5") {
		t.Errorf("the diagnostic should point at the mistyped key:\n%s", stderr)
	}
	if !strings.Contains(stderr, "'commnt' not allowed") {
		t.Errorf("the message should name the key:\n%s", stderr)
	}
}

func TestValidateStrictFailsOnWarnings(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	// Leaving the table's own configuration out entirely produces warnings —
	// an unmentioned column — but no errors.
	if err := os.Remove(filepath.Join(root, "services", "todo", "todo.yaml")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  missing_comment: off
`)

	_, stderr, code := run(t, "validate", "-C", root)
	if code != 0 {
		t.Fatalf("warnings alone should not fail:\n%s", stderr)
	}

	_, stderr, code = run(t, "validate", "-C", root, "--strict")
	if code == 0 {
		t.Fatalf("--strict should fail on warnings:\n%s", stderr)
	}
}

func TestValidateWithoutAProject(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, "validate", "-C", t.TempDir())
	if code == 0 {
		t.Fatal("there is no project here")
	}
	if !strings.Contains(stderr, "no rig.yaml") {
		t.Errorf("the message should say what is missing:\n%s", stderr)
	}
}

func TestIRWritesACanonicalDocument(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	stdout, stderr, code := run(t, "ir", "-C", root)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc["ir_version"] != float64(1) {
		t.Errorf("ir_version = %v", doc["ir_version"])
	}
	if doc["valid"] != true {
		t.Errorf("a clean project should produce a valid document")
	}

	// Running twice must produce identical bytes, or a committed document
	// becomes a source of spurious diffs.
	again, _, _ := run(t, "ir", "-C", root)
	if again != stdout {
		t.Error("two runs produced different documents")
	}
}

func TestIRWritesToAFile(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	out := filepath.Join(root, "ir.json")

	_, stderr, code := run(t, "ir", "-C", root, "-o", out)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("no file written: %v", err)
	}
}

// TestIRIsWrittenEvenWhenInvalid is what lets a broken project be inspected,
// which is how it gets fixed.
func TestIRIsWrittenEvenWhenInvalid(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	write(t, filepath.Join(root, "services", "todo", "todo.yaml"), `table: todo
comment: A thing to do.
columns:
  title:
    comment: The title.
  gone:
    comment: This column was dropped by a migration.
`)

	stdout, stderr, code := run(t, "ir", "-C", root)
	if code == 0 {
		t.Fatal("a stale column reference should fail")
	}
	if !strings.Contains(stderr, "RIG3101") {
		t.Errorf("expected the stale-column diagnostic:\n%s", stderr)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("a document should still be written: %v", err)
	}
	if doc["valid"] != false {
		t.Error("the document should be marked invalid")
	}
}

func TestIRSchemaOnly(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	stdout, _, code := run(t, "ir", "-C", root, "--schema-only")
	if code != 0 {
		t.Fatal("schema-only should succeed")
	}

	var schema map[string]any
	if err := json.Unmarshal([]byte(stdout), &schema); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, isDocument := schema["api"]; isDocument {
		t.Error("--schema-only should print the schema, not the whole document")
	}
	if schema["name"] != "public" {
		t.Errorf("name = %v", schema["name"])
	}
}

func TestSchemaWritesBothFiles(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := run(t, "schema", "-C", root)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}

	for _, name := range []string{"table.schema.json", "rig.schema.json"} {
		path := filepath.Join(root, ".rig", name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s was not written: %v", name, err)
			continue
		}
		var s map[string]any
		if err := json.Unmarshal(b, &s); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
		if s["additionalProperties"] != false {
			t.Errorf("%s should reject unknown keys", name)
		}
	}
}

func TestSchemaSubcommandsPrint(t *testing.T) {
	t.Parallel()

	for _, sub := range []string{"table", "project"} {
		stdout, stderr, code := run(t, "schema", sub)
		if code != 0 {
			t.Fatalf("schema %s: exit %d:\n%s", sub, code, stderr)
		}
		var s map[string]any
		if err := json.Unmarshal([]byte(stdout), &s); err != nil {
			t.Errorf("schema %s is not valid JSON: %v", sub, err)
		}
	}
}

func TestCodes(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "codes")
	if code != 0 {
		t.Fatal("codes should succeed")
	}
	if !strings.Contains(stdout, "RIG3101") {
		t.Errorf("expected the code list:\n%s", stdout)
	}

	stdout, _, code = run(t, "codes", "RIG3101")
	if code != 0 {
		t.Fatal("looking up a code should succeed")
	}
	if !strings.Contains(stdout, "no longer exists") {
		t.Errorf("expected the code's summary:\n%s", stdout)
	}

	// A code in a CI log should be answerable; an invented one should not
	// silently succeed.
	if _, _, code = run(t, "codes", "RIG0000"); code == 0 {
		t.Error("an unknown code should fail")
	}
}

func TestDiagnosticFormats(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	write(t, filepath.Join(root, "services", "todo", "todo.yaml"), `table: todo
comment: A thing to do.
columns:
  title:
    commnt: oops
`)

	_, stderr, _ := run(t, "validate", "-C", root, "--format", "github")
	if !strings.HasPrefix(strings.TrimSpace(stderr), "::error ") {
		t.Errorf("github format should emit workflow commands:\n%s", stderr)
	}

	_, stderr, _ = run(t, "validate", "-C", root, "--format", "json")
	var payload struct {
		Diagnostics []map[string]any `json:"diagnostics"`
		Errors      int              `json:"errors"`
	}
	if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
		t.Fatalf("json format is not valid JSON: %v\n%s", err, stderr)
	}
	if payload.Errors != 1 || len(payload.Diagnostics) != 1 {
		t.Errorf("unexpected payload: %+v", payload)
	}

	_, stderr, code := run(t, "validate", "-C", root, "--format", "xml")
	if code == 0 {
		t.Error("an unknown format should be rejected")
	}
	if !strings.Contains(stderr, "unknown diagnostic format") {
		t.Errorf("unhelpful message:\n%s", stderr)
	}
}

func TestNoSchemaSourceExplainsItself(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
`)

	_, stderr, code := run(t, "validate", "-C", root)
	if code == 0 {
		t.Fatal("there is no schema to compile")
	}
	if !strings.Contains(stderr, "--schema") {
		t.Errorf("the message should say how to supply one:\n%s", stderr)
	}
}

func TestExplicitConfigPath(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := run(t, "validate", "--config", filepath.Join(root, "rig.yaml"))
	if code != 0 {
		t.Fatalf("an explicit config path should work from anywhere:\n%s", stderr)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
