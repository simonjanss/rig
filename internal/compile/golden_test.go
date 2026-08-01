package compile_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

var update = flag.Bool("update", false, "rewrite the golden files from current behavior")

const defaultProject = `project:
  name: demo
  module: example.com/demo
`

// TestGolden runs every fixture directory through the whole compiler.
//
// The compiler takes its schema by value, so this needs no database and no
// container. That is why the bulk of rig's suite finishes in under a second,
// and why a compiler change can be iterated on without one in the loop.
func TestGolden(t *testing.T) {
	t.Parallel()

	for _, dir := range fixtureDirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			t.Parallel()

			doc, diags := compileFixture(t, dir)

			gotIR, err := ir.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			compareGolden(t, filepath.Join(dir, "ir.golden.json"), string(gotIR))
			compareGolden(t, filepath.Join(dir, "diags.golden.txt"), renderDiagnostics(diags))
		})
	}
}

// TestCompileIsDeterministic guards the property that makes a committed
// document reviewable: the same inputs always produce the same bytes.
func TestCompileIsDeterministic(t *testing.T) {
	t.Parallel()

	for _, dir := range fixtureDirs(t) {
		first, _ := compileFixture(t, dir)
		firstBytes, err := ir.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}

		for range 3 {
			again, _ := compileFixture(t, dir)
			againBytes, err := ir.Marshal(again)
			if err != nil {
				t.Fatal(err)
			}
			if string(againBytes) != string(firstBytes) {
				t.Fatalf("%s: compiling twice produced different documents", filepath.Base(dir))
			}
		}
	}
}

// TestFixturesHashStably checks that the content hash tracks the document, so
// that a tool caching on it cannot be fooled by a reordering.
func TestFixturesHashStably(t *testing.T) {
	t.Parallel()

	for _, dir := range fixtureDirs(t) {
		doc, _ := compileFixture(t, dir)
		h1, err := doc.Hash()
		if err != nil {
			t.Fatal(err)
		}
		again, _ := compileFixture(t, dir)
		h2, err := again.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if h1 != h2 {
			t.Fatalf("%s: hash changed between identical runs", filepath.Base(dir))
		}
	}
}

func fixtureDirs(t *testing.T) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join("testdata", "*", "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no fixtures found under testdata")
	}

	dirs := make([]string, 0, len(matches))
	for _, m := range matches {
		dirs = append(dirs, filepath.Dir(m))
	}
	return dirs
}

func compileFixture(t *testing.T, dir string) (*ir.Document, []diag.Diagnostic) {
	t.Helper()

	schema := readSchema(t, filepath.Join(dir, "schema.json"))

	projectSrc := defaultProject
	if b, err := os.ReadFile(filepath.Join(dir, "rig.yaml")); err == nil {
		projectSrc = string(b)
	}
	p, pdiags := project.Parse(filepath.Join(dir, "rig.yaml"), []byte(projectSrc))
	if pdiags.HasErrors() {
		t.Fatalf("%s/rig.yaml:\n%s", dir, pdiags.String())
	}

	tablePaths, err := filepath.Glob(filepath.Join(dir, "tables", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set, tdiags := tableconf.LoadDir(tablePaths)

	doc, cdiags := compile.Compile(schema, set, compile.Options{
		Project: p,
		Tool:    "rig (test)",
	})

	var all diag.List
	all.Append(tdiags)
	all.Append(cdiags)
	return doc, all.All()
}

func readSchema(t *testing.T, path string) ir.Schema {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Unknown fields are rejected so a typo in a fixture is a failure rather
	// than a silently dropped column.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()

	var schema ir.Schema
	if err := dec.Decode(&schema); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return schema
}

// renderDiagnostics writes one diagnostic per line. Paths are normalized to the
// fixture-relative form so the golden files do not depend on where the
// repository is checked out.
func renderDiagnostics(entries []diag.Diagnostic) string {
	if len(entries) == 0 {
		return ""
	}

	var b strings.Builder
	for _, d := range entries {
		where := d.Anchor.Path
		if d.Anchor.File != "" {
			where = filepath.ToSlash(d.Anchor.File)
			if d.Anchor.Line > 0 {
				where = fmt.Sprintf("%s:%d:%d", where, d.Anchor.Line, d.Anchor.Column)
			}
		}
		fmt.Fprintf(&b, "%s[%s] %s: %s\n", d.Severity, d.Code.ID, where, d.Message)
	}
	return b.String()
}

func compareGolden(t *testing.T, path, got string) {
	t.Helper()

	if *update {
		if got == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			return
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	wantBytes, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if diff := cmp.Diff(string(wantBytes), got); diff != "" {
		t.Errorf("%s is out of date (-want +got):\n%s\nRun `go test ./internal/compile/ -update` if this change is intended.",
			path, diff)
	}
}
