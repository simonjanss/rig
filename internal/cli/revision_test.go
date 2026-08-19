package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/internal/cli"
)

// generateAt runs `rig generate` on a chosen day.
func generateAt(t *testing.T, root string, day time.Time, args ...string) (stderr string, code int) {
	t.Helper()

	full := append([]string{"generate", "-C", root, "--schema", filepath.Join(root, "schema.json")}, args...)

	var out, errOut bytes.Buffer
	code = cli.MainAt(full, &out, &errOut, day)
	return errOut.String(), code
}

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// servingProject configures the generators that put the revision into code,
// which the model and repository layers have no reason to.
func servingProject(t *testing.T) string {
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
  - name: service-go
    out_dir: internal/api
    options:
      package: api
      model_import: example.com/demo/internal/model
      store_import: example.com/demo/internal/store
  - name: server-go
    out_dir: internal/api
    options: { package: api, model_import: example.com/demo/internal/model }
`)
	return root
}

func readRevisionFile(t *testing.T, root string) (rev, hash string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, ".rig", "revision.json"))
	if err != nil {
		t.Fatalf("read revision.json: %v", err)
	}
	var f struct {
		Revision string `json:"revision"`
		Hash     string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("revision.json: %v", err)
	}
	return f.Revision, f.Hash
}

// The point of the whole feature: a client built a month after another one, from
// an API nobody changed in between, reports the same revision — because it is
// the same API. A build stamp would make them look a month apart.
func TestTheRevisionHoldsStillWhileTheSurfaceDoes(t *testing.T) {
	t.Parallel()

	root := servingProject(t)

	if stderr, code := generateAt(t, root, day("2026-03-01")); code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}
	first, firstHash := readRevisionFile(t, root)
	if first != "2026-03-01" {
		t.Fatalf("revision = %q, want the day it was generated", first)
	}

	api := filepath.Join(root, "internal", "api", "api.gen.go")
	before, err := os.ReadFile(api)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), `const Revision = "2026-03-01"`) {
		t.Error("the generated API does not carry the revision")
	}

	// A month later, the same schema.
	if stderr, code := generateAt(t, root, day("2026-04-01")); code != 0 {
		t.Fatalf("the second generate failed:\n%s", stderr)
	}

	second, secondHash := readRevisionFile(t, root)
	if second != first || secondHash != firstHash {
		t.Errorf("the record moved without the surface: %s/%s then %s/%s",
			first, firstHash, second, secondHash)
	}
	after, err := os.ReadFile(api)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("regenerating on a later day rewrote the generated code")
	}
}

// And when the surface does move, the date moves with it, and says so.
func TestTheRevisionMovesWithTheSurface(t *testing.T) {
	t.Parallel()

	root := servingProject(t)

	if stderr, code := generateAt(t, root, day("2026-03-01")); code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}
	_, firstHash := readRevisionFile(t, root)

	// A new column is a new field, which is a change to what a client sees.
	write(t, filepath.Join(root, "services", "todo", "todo.yaml"), `table: todo
comment: A single thing someone means to get done.
columns:
  title:
    comment: Short description of the task.
  notes:
    comment: Anything else worth remembering about it.
`)
	write(t, filepath.Join(root, "schema.json"),
		strings.Replace(todoSchema,
			`{ "name": "title", "sql_type": "text", "udt_name": "text", "ordinal": 4 }`,
			`{ "name": "title", "sql_type": "text", "udt_name": "text", "ordinal": 4 },`+
				`{ "name": "notes", "sql_type": "text", "udt_name": "text", "ordinal": 5, "nullable": true }`,
			1))

	stderr, code := generateAt(t, root, day("2026-04-01"))
	if code != 0 {
		t.Fatalf("the second generate failed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "the revision is now 2026-04-01") {
		t.Errorf("a moved revision should be reported:\n%s", stderr)
	}

	rev, hash := readRevisionFile(t, root)
	if rev != "2026-04-01" {
		t.Errorf("revision = %q, want the day the surface changed", rev)
	}
	if hash == firstHash {
		t.Error("the recorded hash did not move with the surface")
	}

	api, err := os.ReadFile(filepath.Join(root, "internal", "api", "api.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(api), `const Revision = "2026-04-01"`) {
		t.Error("the generated API is still carrying the old revision")
	}
}

// `rig check` runs in CI, every day, against code nobody regenerated. If it
// dated the document itself it would either fail every morning or quietly
// accept the change it is there to catch.
func TestCheckNeitherMovesNorWritesTheRevision(t *testing.T) {
	t.Parallel()

	root := servingProject(t)

	if stderr, code := generateAt(t, root, day("2026-03-01")); code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}

	// Months later, with no clock of its own to appeal to.
	_, stderr, code := runWithSchema(t, root, "check", "-C", root)
	if code != 0 {
		t.Fatalf("check should pass on freshly generated code:\n%s", stderr)
	}

	rev, _ := readRevisionFile(t, root)
	if rev != "2026-03-01" {
		t.Errorf("check moved the revision to %q", rev)
	}
}

// A project that has never generated has no revision, and every command that
// only reads says so rather than inventing today.
func TestReadingCommandsDoNotStampAProjectThatNeverGenerated(t *testing.T) {
	t.Parallel()

	root := servingProject(t)

	stdout, stderr, code := runWithSchema(t, root, "ir", "-C", root)
	if code != 0 {
		t.Fatalf("ir:\n%s", stderr)
	}
	if strings.Contains(stdout, `"revision"`) {
		t.Error("ir invented a revision for a project that has never generated")
	}
	if _, err := os.Stat(filepath.Join(root, ".rig", "revision.json")); !os.IsNotExist(err) {
		t.Errorf("a reading command wrote revision.json: %v", err)
	}
}

// `rig ir` reports what was recorded, so the document somebody is looking at is
// the document their clients were built from.
func TestIRReportsTheRecordedRevision(t *testing.T) {
	t.Parallel()

	root := servingProject(t)

	if stderr, code := generateAt(t, root, day("2026-03-01")); code != 0 {
		t.Fatalf("generate:\n%s", stderr)
	}

	stdout, stderr, code := runWithSchema(t, root, "ir", "-C", root)
	if code != 0 {
		t.Fatalf("ir:\n%s", stderr)
	}
	if !strings.Contains(stdout, `"revision": "2026-03-01"`) {
		t.Error("ir does not report the recorded revision")
	}
}
