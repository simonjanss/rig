// Package gentest is the shared harness for testing generators.
//
// A generator is a pure function from a document to a set of files, so testing
// one needs neither a database nor a filesystem: load a document, run the
// generator, compare the artifacts to a golden directory.
package gentest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// LoadDocument reads a compiled document from a file.
func LoadDocument(t *testing.T, path string) *ir.Document {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ir.Unmarshal(raw)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// Run executes a generator and returns its artifacts, sorted.
func Run(t *testing.T, g gen.Generator, doc *ir.Document, opts gen.Options) []gen.Artifact {
	t.Helper()

	artifacts, err := g.Generate(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("%s: %v", g.Name(), err)
	}
	slices.SortFunc(artifacts, func(a, b gen.Artifact) int { return strings.Compare(a.Path, b.Path) })
	return artifacts
}

// Golden compares artifacts against a directory of expected files.
//
// The whole set is compared, not just the files that happen to exist: a
// generator that stops emitting a file is as much a change as one that emits
// the wrong contents.
func Golden(t *testing.T, dir string, artifacts []gen.Artifact, update bool) {
	t.Helper()

	if update {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		for _, a := range artifacts {
			path := filepath.Join(dir, filepath.FromSlash(a.Path))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, a.Content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	want := readDir(t, dir)
	got := make(map[string]string, len(artifacts))
	for _, a := range artifacts {
		got[filepath.ToSlash(a.Path)] = string(a.Content)
	}

	if diff := cmp.Diff(keys(want), keys(got)); diff != "" {
		t.Errorf("the set of generated files changed (-want +got):\n%s\nRun with -update if this is intended.", diff)
	}
	for name, wantContent := range want {
		if diff := cmp.Diff(wantContent, got[name]); diff != "" {
			t.Errorf("%s changed (-want +got):\n%s", name, diff)
		}
	}
}

func readDir(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Deterministic checks that a generator produces the same bytes twice.
//
// The usual cause of failure is iterating a map without sorting it, which
// produces a generator that works until the day a run happens to hash
// differently.
func Deterministic(t *testing.T, g gen.Generator, doc *ir.Document, opts gen.Options) {
	t.Helper()

	first := Run(t, g, doc, opts)
	for range 3 {
		again := Run(t, g, doc, opts)
		if len(again) != len(first) {
			t.Fatalf("two runs produced %d and %d files", len(first), len(again))
		}
		for i := range first {
			if first[i].Path != again[i].Path {
				t.Fatalf("file %d is %s then %s", i, first[i].Path, again[i].Path)
			}
			if string(first[i].Content) != string(again[i].Content) {
				t.Fatalf("%s differs between runs", first[i].Path)
			}
		}
	}
}
