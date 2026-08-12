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

// runWithSchema is [run] against a committed schema dump.
//
// These tests exercise the command line, not the database, so they compile a
// dump rather than starting a container. Reading a real Postgres is covered by
// the docker-tagged suite; making every CLI test pay for a container would put
// a minute between a typo and finding out about it.
func runWithSchema(t *testing.T, root string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return run(t, append(args, "--schema", filepath.Join(root, "schema.json"))...)
}

// project writes a minimal but complete project and returns its root.
func newProject(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
`)
	write(t, filepath.Join(root, "schema.json"), todoSchema)
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
	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)

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

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
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

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code != 0 {
		t.Fatalf("warnings alone should not fail:\n%s", stderr)
	}

	_, stderr, code = runWithSchema(t, root, "validate", "-C", root, "--strict")
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
	stdout, stderr, code := runWithSchema(t, root, "ir", "-C", root)
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
	again, _, _ := runWithSchema(t, root, "ir", "-C", root)
	if again != stdout {
		t.Error("two runs produced different documents")
	}
}

func TestIRWritesToAFile(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	out := filepath.Join(root, "ir.json")

	_, stderr, code := runWithSchema(t, root, "ir", "-C", root, "-o", out)
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

	stdout, stderr, code := runWithSchema(t, root, "ir", "-C", root)
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
	stdout, _, code := runWithSchema(t, root, "ir", "-C", root, "--schema-only")
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

	_, stderr, _ := runWithSchema(t, root, "validate", "-C", root, "--format", "github")
	if !strings.HasPrefix(strings.TrimSpace(stderr), "::error ") {
		t.Errorf("github format should emit workflow commands:\n%s", stderr)
	}

	_, stderr, _ = runWithSchema(t, root, "validate", "-C", root, "--format", "json")
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

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root, "--format", "xml")
	if code == 0 {
		t.Error("an unknown format should be rejected")
	}
	if !strings.Contains(stderr, "unknown diagnostic format") {
		t.Errorf("unhelpful message:\n%s", stderr)
	}
}

func TestMissingSchemaDumpExplainsItself(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := run(t, "validate", "-C", root, "--schema", filepath.Join(root, "nope.json"))
	if code == 0 {
		t.Fatal("there is no such dump")
	}
	if !strings.Contains(stderr, "nope.json") {
		t.Errorf("the message should name the file it could not read:\n%s", stderr)
	}
}

func TestExplicitConfigPath(t *testing.T) {
	t.Parallel()

	root := newProject(t)
	_, stderr, code := runWithSchema(t, root, "validate", "--config", filepath.Join(root, "rig.yaml"))
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

// generatingProject is [newProject] with generators configured, so the write
// half of the tool can be driven from a schema dump.
func generatingProject(t *testing.T) string {
	t.Helper()

	root := newProject(t)
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
generators:
  - name: model-go
    out_dir: internal/model
    options: { package: model }
  - name: persist-go
    out_dir: internal/store
    options: { package: store, model_import: example.com/demo/internal/model }
`)
	return root
}

// The loop the tool exists for: write the code, say what changed, and be quiet
// the second time.
func TestGenerateWritesTheCodeAndThenHasNothingToDo(t *testing.T) {
	t.Parallel()

	root := generatingProject(t)

	_, stderr, code := runWithSchema(t, root, "generate", "-C", root)
	if code != 0 {
		t.Fatalf("generate failed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "add") {
		t.Errorf("the first run should report what it added:\n%s", stderr)
	}

	// The manifest is what makes the second run quiet and the stale check
	// possible.
	if _, err := os.Stat(filepath.Join(root, ".rig", "manifest.json")); err != nil {
		t.Errorf("no manifest: %v", err)
	}

	_, stderr, code = runWithSchema(t, root, "generate", "-C", root)
	if code != 0 {
		t.Fatalf("the second run failed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "up to date") {
		t.Errorf("a run that changes nothing should say so:\n%s", stderr)
	}

	_, stderr, code = runWithSchema(t, root, "check", "-C", root)
	if code != 0 {
		t.Fatalf("check should pass on freshly generated code:\n%s", stderr)
	}
}

// check is the CI gate. Committed generated code nobody regenerated is how a
// schema change quietly stops matching the code that reads it.
func TestCheckFailsWhenTheCodeIsBehind(t *testing.T) {
	t.Parallel()

	root := generatingProject(t)
	if _, stderr, code := runWithSchema(t, root, "generate", "-C", root); code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}

	if err := os.Remove(filepath.Join(root, "internal", "model", "todo.gen.go")); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runWithSchema(t, root, "check", "-C", root)
	if code == 0 {
		t.Fatalf("check should have failed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "rig generate") {
		t.Errorf("the failure should say what to do about it:\n%s", stderr)
	}
	// And it writes nothing: it is a report, and a CI gate that fixes the thing
	// it is gating is not a gate.
	if _, err := os.Stat(filepath.Join(root, "internal", "model", "todo.gen.go")); !os.IsNotExist(err) {
		t.Error("check should not have written the file back")
	}
}

// A generated file somebody edited is not one to silently overwrite: the edit
// is either a mistake worth knowing about or work worth keeping.
func TestAHandEditedGeneratedFileNeedsForce(t *testing.T) {
	t.Parallel()

	root := generatingProject(t)
	if _, stderr, code := runWithSchema(t, root, "generate", "-C", root); code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}

	edited := filepath.Join(root, "internal", "model", "todo.gen.go")
	raw, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edited, append(raw, []byte("\n// mine\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runWithSchema(t, root, "generate", "-C", root)
	if code == 0 {
		t.Fatalf("generate should have refused:\n%s", stderr)
	}

	after, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "// mine") {
		t.Error("the edit should still be there after a refusal")
	}

	if _, stderr, code := runWithSchema(t, root, "generate", "-C", root, "--force"); code != 0 {
		t.Fatalf("--force:\n%s", stderr)
	}
	after, err = os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "// mine") {
		t.Error("--force is what says to overwrite it")
	}
}

// A file no generator claims any more is reported rather than deleted, because
// deleting a file the tool no longer understands is not a decision to make on
// somebody's behalf without saying so.
func TestAFileNoGeneratorProducesIsReportedThenPruned(t *testing.T) {
	t.Parallel()

	root := generatingProject(t)
	if _, stderr, code := runWithSchema(t, root, "generate", "-C", root); code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}

	orphan := filepath.Join(root, "internal", "store", "todo_repository.gen.go")
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("the file this test is about was never written: %v", err)
	}

	// Drop the generator that produced it.
	write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
generators:
  - name: model-go
    options: { package: model }
`)

	_, stderr, code := runWithSchema(t, root, "generate", "-C", root)
	if code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--prune") {
		t.Errorf("a stale file should be reported with the way to remove it:\n%s", stderr)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("it should not have been deleted without being asked")
	}

	if _, stderr, code := runWithSchema(t, root, "generate", "-C", root, "--prune"); code != 0 {
		t.Fatalf("--prune:\n%s", stderr)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("--prune should have removed it")
	}
}

// A name in --only that matches nothing is a typo, and running a subset of what
// was asked for is worse than saying so.
func TestOnlyNamesAGeneratorOrSaysWhatThereIs(t *testing.T) {
	t.Parallel()

	root := generatingProject(t)

	_, stderr, code := runWithSchema(t, root, "generate", "-C", root, "--only", "model-go")
	if code != 0 {
		t.Fatalf("generate --only:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "model", "todo.gen.go")); err != nil {
		t.Errorf("the named generator should have run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "store")); !os.IsNotExist(err) {
		t.Error("the one that was not named should not have")
	}

	_, stderr, code = runWithSchema(t, root, "generate", "-C", root, "--only", "modelgo")
	if code == 0 {
		t.Fatal("a name that matches nothing should be an error")
	}
	if !strings.Contains(stderr, "model-go") {
		t.Errorf("the error should list what is configured:\n%s", stderr)
	}
}

// A project with nothing configured gets told how to configure something,
// rather than "wrote 0 files".
func TestAProjectWithNoGeneratorsIsToldSo(t *testing.T) {
	t.Parallel()

	root := newProject(t)

	_, stderr, code := runWithSchema(t, root, "generate", "-C", root)
	if code == 0 {
		t.Fatal("generating nothing should not be a success")
	}
	if !strings.Contains(stderr, "generators:") || !strings.Contains(stderr, "rig generators") {
		t.Errorf("the error should say what to add and how to see the options:\n%s", stderr)
	}
}

func TestGeneratorsListsWhatRigKnowsAbout(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := run(t, "generators")
	if code != 0 {
		t.Fatalf("generators:\n%s", stderr)
	}
	for _, name := range []string{"model-go", "persist-go", "server-go", "service-go", "electric"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("%s is missing from the list:\n%s", name, stdout)
		}
	}
}

// init is the first thing anybody runs, so what it leaves behind has to be a
// project the next command accepts.
func TestInitWritesAProjectTheRestOfTheToolCanRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	_, stderr, code := run(t, "init", root, "--module", "example.com/app")
	if code != 0 {
		t.Fatalf("init:\n%s", stderr)
	}

	for _, name := range []string{"rig.yaml", "AGENTS.md", ".rig/table.schema.json", ".rig/rig.schema.json"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(root, "rig.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "example.com/app") {
		t.Errorf("the module should be the one that was asked for:\n%s", raw)
	}

	// Nothing that already exists is overwritten: running init twice is what
	// somebody does when they are not sure whether they ran it.
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = run(t, "init", root)
	if code != 0 {
		t.Fatalf("the second init:\n%s", stderr)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("it should say what it kept:\n%s", stderr)
	}
	kept, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "mine\n" {
		t.Error("it kept nothing")
	}
}

// The module path is a guess when nobody gives one, because a project that will
// not initialize without it is a worse first impression than a line to edit.
func TestInitGuessesAModuleFromTheDirectory(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "fantasyfootball")

	if _, stderr, code := run(t, "init", root); code != 0 {
		t.Fatalf("init:\n%s", stderr)
	}
	raw, err := os.ReadFile(filepath.Join(root, "rig.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "fantasyfootball") {
		t.Errorf("the directory name should be the project name:\n%s", raw)
	}
}

// goose applies migrations in name order, so the number is the ordering and a
// duplicate one is two migrations that may run either way round.
func TestMigrationNewNumbersFromWhatIsAlreadyThere(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if _, stderr, code := run(t, "init", root); code != 0 {
		t.Fatalf("init:\n%s", stderr)
	}

	if _, stderr, code := run(t, "-C", root, "migration", "new", "create_todo", "--table", "todo",
		"--soft-delete", "--snapshot"); code != 0 {
		t.Fatalf("migration new:\n%s", stderr)
	}
	if _, stderr, code := run(t, "-C", root, "migration", "new", "add_notes"); code != 0 {
		t.Fatalf("the second migration:\n%s", stderr)
	}

	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) != 2 {
		t.Fatalf("migrations = %v, want two", names)
	}
	if !strings.HasPrefix(names[0], "00001_") || !strings.HasPrefix(names[1], "00002_") {
		t.Errorf("migrations = %v, want them numbered in order", names)
	}

	first, err := os.ReadFile(filepath.Join(root, "migrations", names[0]))
	if err != nil {
		t.Fatal(err)
	}
	body := string(first)
	for _, want := range []string{"-- +goose Up", "-- +goose Down", "CREATE TABLE todo",
		"deleted_at", "version_type", "snapshot_from_todo_id"} {
		if !strings.Contains(body, want) {
			t.Errorf("the scaffolded migration is missing %q:\n%s", want, body)
		}
	}
}

// setup-project scaffolds real tables following the same conventions as
// anybody's own, which is what makes `rig sync` and `rig generate` able to read
// them at all.
func TestSetupProjectScaffoldsTheFoundation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if _, stderr, code := run(t, "init", root); code != 0 {
		t.Fatalf("init:\n%s", stderr)
	}

	// --dry-run writes nothing, which is the only way to see what it would do
	// before letting it.
	_, stderr, code := run(t, "-C", root, "setup-project", "--dry-run")
	if code != 0 {
		t.Fatalf("--dry-run:\n%s", stderr)
	}
	if !strings.Contains(stderr, "would create") {
		t.Errorf("--dry-run should list the files:\n%s", stderr)
	}
	for _, e := range readDir(t, filepath.Join(root, "migrations")) {
		if strings.HasSuffix(e, ".sql") {
			t.Errorf("--dry-run wrote %s", e)
		}
	}

	if _, stderr, code := run(t, "-C", root, "setup-project"); code != 0 {
		t.Fatalf("setup-project:\n%s", stderr)
	}
	entries := readDir(t, filepath.Join(root, "migrations"))
	if len(entries) == 0 {
		t.Fatal("no migrations were written")
	}

	// Idempotent: it is run again by anybody who is not sure whether they ran it.
	_, stderr, code = run(t, "-C", root, "setup-project")
	if code != 0 {
		t.Fatalf("the second run:\n%s", stderr)
	}
	if !strings.Contains(stderr, "already in place") {
		t.Errorf("the second run should say there is nothing to do:\n%s", stderr)
	}
	again := readDir(t, filepath.Join(root, "migrations"))
	if len(again) != len(entries) {
		t.Errorf("%d migrations became %d", len(entries), len(again))
	}
}

// Skipping a part something else depends on produces SQL that fails halfway
// through `rig db up`, which is a worse way to find out than a message here.
func TestSetupProjectRefusesASkipThatWouldNotApply(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if _, stderr, code := run(t, "init", root); code != 0 {
		t.Fatalf("init:\n%s", stderr)
	}

	_, stderr, code := run(t, "-C", root, "setup-project", "--skip", "tenancy")
	if code == 0 {
		t.Fatal("skipping what everything references should be refused")
	}
	if !strings.Contains(stderr, "tenancy") {
		t.Errorf("the error should name it:\n%s", stderr)
	}

	if _, stderr, code := run(t, "-C", root, "setup-project", "--skip", "nonsense"); code == 0 {
		t.Errorf("an unknown part should be refused:\n%s", stderr)
	}

	// A leaf part is fine to leave out.
	if _, stderr, code := run(t, "-C", root, "setup-project", "--skip", "oauth", "--dry-run"); code != 0 {
		t.Errorf("skipping a leaf:\n%s", stderr)
	} else if strings.Contains(stderr, "oauth") {
		t.Errorf("the skipped part should not be listed:\n%s", stderr)
	}
}

// Every command that needs a project has to say the same thing when there is
// not one, because "open /dev/null/rig.yaml: not a directory" helps nobody.
func TestTheCommandsThatNeedAProjectSayWhenThereIsNone(t *testing.T) {
	t.Parallel()

	empty := t.TempDir()

	for _, args := range [][]string{
		{"generate"},
		{"check"},
		{"setup-project"},
		{"migration", "new", "x"},
	} {
		_, stderr, code := run(t, append([]string{"-C", empty}, args...)...)
		if code == 0 {
			t.Errorf("%v: should have failed outside a project", args)
			continue
		}
		if !strings.Contains(stderr, "rig.yaml") {
			t.Errorf("%v: the error should name what is missing:\n%s", args, stderr)
		}
	}
}

// readDir lists the file names in a directory.
func readDir(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
