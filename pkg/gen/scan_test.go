package gen_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/pkg/gen"
)

// write puts a file on disk, creating its directory.
func write(t *testing.T, root, rel, content string) string {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// staleNames is the set of paths a comparison called leftovers.
func staleNames(root string, deltas []gen.Delta) []string {
	var out []string
	for _, d := range deltas {
		if d.Status == gen.Stale {
			rel, _ := filepath.Rel(root, d.Path)
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

func TestOrphansRecognizesRigsOwnOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Named as rig names its output.
	write(t, root, "internal/model/ghost.gen.go", "package model\n")
	write(t, root, "docs/openapi.gen.yaml", "openapi: 3.1.0\n")
	// Banner but no `.gen.` in the name, which is the TypeScript entry point.
	write(t, root, "web/src/api/index.ts", gen.Banner+"\n\nexport {};\n")

	// Nothing rig wrote.
	write(t, root, "main.go", "package main\n")
	write(t, root, "README.md", "# hello\n")
	write(t, root, "services/article/article.go", "package article\n")
	// A file whose name mentions generation without being generated.
	write(t, root, "internal/model/generated.go", "package model\n")

	deltas, err := gen.Orphans(root, nil)
	if err != nil {
		t.Fatal(err)
	}

	// WalkDir is lexical, so the order is the tree's own.
	got := strings.Join(staleNames(root, deltas), ",")
	want := "docs/openapi.gen.yaml,internal/model/ghost.gen.go,web/src/api/index.ts"
	if got != want {
		t.Errorf("Orphans() = %s, want %s", got, want)
	}
}

func TestOrphansSkipsClaimedFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, root, "internal/model/article.gen.go", "package model\n")
	write(t, root, "internal/model/ghost.gen.go", "package model\n")

	claimed := map[string]bool{"internal/model/article.gen.go": true}

	deltas, err := gen.Orphans(root, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if got := staleNames(root, deltas); len(got) != 1 || got[0] != "internal/model/ghost.gen.go" {
		t.Errorf("a claimed file should not be a leftover: %v", got)
	}
}

func TestOrphansStopsAtDirectoriesItDoesNotOwn(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// A nested project checks itself, with the generators it configures.
	write(t, root, "packages/inner/rig.yaml", "project:\n  name: inner\n")
	write(t, root, "packages/inner/internal/model/model.gen.go", "package model\n")

	write(t, root, "node_modules/thing/dist/thing.gen.js", "// vendored\n")
	write(t, root, "vendor/dep/dep.gen.go", "package dep\n")
	write(t, root, ".rig/manifest.json", "{}\n")
	write(t, root, "internal/gen/testdata/golden/model.gen.go", "package model\n")

	deltas, err := gen.Orphans(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := staleNames(root, deltas); len(got) != 0 {
		t.Errorf("nothing here is this project's to remove: %v", got)
	}
}

// The case that matters in CI: the manifest is gitignored, so a checkout that
// never generated anything still has to notice a file nobody produces.
func TestStaleWithoutAManifest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	g := &fake{name: "a", artifacts: []gen.Artifact{file("keep.gen.go", "package a\n")}}
	results, err := gen.Run(context.Background(), registryWith(g), validDoc(), root, []gen.Spec{{Name: "a"}})
	if err != nil {
		t.Fatal(err)
	}

	deltas, _ := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	if _, err := gen.Write(root, results, deltas, gen.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	// A renamed table left this behind, and the manifest that would have
	// remembered it was never committed.
	write(t, root, "old.gen.go", "package a\n")

	deltas, err = gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := staleNames(root, deltas); len(got) != 1 || got[0] != "old.gen.go" {
		t.Fatalf("the leftover should be reported: %v", got)
	}
	if !gen.NeedsWork(deltas) {
		t.Error("a leftover is work, which is what makes `rig check` fail")
	}

	// It survives a run without --prune, and is not adopted into the manifest:
	// a scan cannot say which generator wrote it, and an entry naming none
	// would claim rig wrote it on the strength of its name.
	next, err := gen.Write(root, results, deltas, gen.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.gen.go")); err != nil {
		t.Error("the leftover should survive without --prune")
	}
	for _, e := range next.Files {
		if e.Path == "old.gen.go" {
			t.Errorf("a scanned leftover should not be recorded: %+v", e)
		}
	}

	if _, err := gen.Write(root, results, deltas, gen.WriteOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.gen.go")); !os.IsNotExist(err) {
		t.Error("--prune should remove the leftover")
	}
}

// A hand-owned stub carries neither signal, which is what keeps `--prune` from
// deleting the code somebody wrote.
func TestAStubIsNeverALeftover(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stub := gen.Artifact{Path: "services/article/article.go", Content: []byte("package article\n"), Mode: gen.CreateOnce}
	g := &fake{name: "a", artifacts: []gen.Artifact{stub}}

	results, _ := gen.Run(context.Background(), registryWith(g), validDoc(), root, []gen.Spec{{Name: "a"}})
	deltas, _ := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	next, _ := gen.Write(root, results, deltas, gen.WriteOptions{})
	_ = next.Save(root)

	// The table is gone, so the generator no longer produces the stub. Only the
	// manifest can report it, and only with the manifest present.
	none := &fake{name: "a"}
	results, _ = gen.Run(context.Background(), registryWith(none), validDoc(), root, []gen.Spec{{Name: "a"}})

	deltas, err := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := staleNames(root, deltas); len(got) != 0 {
		t.Errorf("a scan must not claim hand-written code: %v", got)
	}
}

// --only runs a subset, and the generators that did not run still own their
// files.
func TestAPartialRunClaimsNothing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	a := &fake{name: "a", artifacts: []gen.Artifact{file("a.gen.go", "package p\n")}}
	b := &fake{name: "b", artifacts: []gen.Artifact{file("b.gen.go", "package p\n")}}

	results, _ := gen.Run(context.Background(), registryWith(a, b), validDoc(), root,
		[]gen.Spec{{Name: "a"}, {Name: "b"}})
	deltas, _ := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	next, _ := gen.Write(root, results, deltas, gen.WriteOptions{})
	_ = next.Save(root)

	// Now only `a` runs. Both the manifest's record of b.gen.go and the file
	// itself have to be left alone.
	only, _ := gen.Run(context.Background(), registryWith(a, b), validDoc(), root, []gen.Spec{{Name: "a"}})
	manifest, _ := gen.LoadManifest(root)

	deltas, err := gen.Diff(root, only, manifest, gen.DiffOptions{Partial: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := staleNames(root, deltas); len(got) != 0 {
		t.Errorf("--only should not propose deleting another generator's work: %v", got)
	}
	if gen.NeedsWork(deltas) {
		t.Error("nothing changed, so a partial run has nothing to do")
	}

	// The record has to survive too. It is rebuilt from this run's deltas, and
	// this run had nothing to say about b.gen.go — forgetting it would cost the
	// next run the only thing that tells a hand edit from stale output.
	next, err = gen.Write(root, only, deltas, gen.WriteOptions{Previous: manifest})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.Lookup("b.gen.go"); !ok {
		t.Errorf("--only should not forget another generator's files: %+v", next.Files)
	}
	if next.Generators["b"] == "" {
		t.Errorf("nor which version of it last ran: %v", next.Generators)
	}
}
