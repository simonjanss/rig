package gentest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The harness is what every generator's suite trusts, so what it would fail to
// notice is what those suites would fail to notice. These drive it against a
// fake generator and check it catches each kind of change.

func TestGoldenWritesThenMatches(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "want")
	artifacts := []gen.Artifact{
		{Path: "a.gen.go", Content: []byte("package a\n")},
		{Path: "sub/b.gen.go", Content: []byte("package b\n")},
	}

	gentest.Golden(t, dir, artifacts, true)

	// Written where their paths say, subdirectories and all.
	if _, err := os.Stat(filepath.Join(dir, "sub", "b.gen.go")); err != nil {
		t.Fatalf("the nested artifact was not written: %v", err)
	}

	// And the same output now matches.
	gentest.Golden(t, dir, artifacts, false)
}

// A generator that stops emitting a file has changed as much as one that emits
// the wrong contents, and the second is the one a naive comparison misses.
func TestGoldenNoticesAMissingFile(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "want")
	both := []gen.Artifact{
		{Path: "a.gen.go", Content: []byte("package a\n")},
		{Path: "b.gen.go", Content: []byte("package b\n")},
	}
	gentest.Golden(t, dir, both, true)

	if !fails(func(fake *testing.T) { gentest.Golden(fake, dir, both[:1], false) }) {
		t.Error("dropping a file should fail the golden comparison")
	}
}

func TestGoldenNoticesChangedContent(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "want")
	gentest.Golden(t, dir, []gen.Artifact{{Path: "a.gen.go", Content: []byte("before\n")}}, true)

	changed := []gen.Artifact{{Path: "a.gen.go", Content: []byte("after\n")}}
	if !fails(func(fake *testing.T) { gentest.Golden(fake, dir, changed, false) }) {
		t.Error("changed content should fail the golden comparison")
	}
}

// Updating rewrites the directory rather than adding to it, or a file that
// stopped being generated would sit in the golden set forever.
func TestUpdateRemovesWhatIsNoLongerGenerated(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "want")
	gentest.Golden(t, dir, []gen.Artifact{
		{Path: "a.gen.go", Content: []byte("package a\n")},
		{Path: "gone.gen.go", Content: []byte("package gone\n")},
	}, true)

	gentest.Golden(t, dir, []gen.Artifact{{Path: "a.gen.go", Content: []byte("package a\n")}}, true)

	if _, err := os.Stat(filepath.Join(dir, "gone.gen.go")); !os.IsNotExist(err) {
		t.Error("a file that is no longer generated should not survive an update")
	}
}

// Run sorts, so a generator that iterates a map does not produce a different
// order every time and defeat the comparison before it starts.
func TestRunSortsByPath(t *testing.T) {
	t.Parallel()

	g := &fakeGenerator{artifacts: []gen.Artifact{
		{Path: "z.gen.go"}, {Path: "a.gen.go"}, {Path: "m.gen.go"},
	}}

	got := gentest.Run(t, g, &ir.Document{}, gen.Options{})

	if len(got) != 3 || got[0].Path != "a.gen.go" || got[2].Path != "z.gen.go" {
		t.Errorf("order = %v", paths(got))
	}
}

// The determinism check is the one that catches a bare map range, which is the
// mistake that produces a diff on every second run and nowhere else.
func TestDeterministicCatchesAGeneratorThatWanders(t *testing.T) {
	t.Parallel()

	steady := &fakeGenerator{artifacts: []gen.Artifact{{Path: "a.gen.go", Content: []byte("same")}}}
	gentest.Deterministic(t, steady, &ir.Document{}, gen.Options{})

	if !fails(func(fake *testing.T) {
		gentest.Deterministic(fake, &wanderingGenerator{}, &ir.Document{}, gen.Options{})
	}) {
		t.Error("output that changes between runs should fail")
	}
}

// A document that cannot be read is a broken fixture, and the message has to
// name the file: every generator's suite loads one by path.
func TestLoadDocumentNamesTheFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "broken.ir.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !fails(func(fake *testing.T) { gentest.LoadDocument(fake, path) }) {
		t.Error("an unreadable document should fail the test that asked for it")
	}
}

func TestGoldenAgainstAMissingDirectoryReportsEveryFile(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "never-written")
	artifacts := []gen.Artifact{{Path: "a.gen.go", Content: []byte("x")}}

	if !fails(func(fake *testing.T) { gentest.Golden(fake, dir, artifacts, false) }) {
		t.Error("a golden set that does not exist yet is a failure, not a pass")
	}
}

// fails runs a check against a throwaway *testing.T and reports whether it
// failed.
//
// In its own goroutine, because a Fatalf ends the goroutine it is called from
// and would otherwise take this test with it.
func fails(check func(*testing.T)) bool {
	fake := &testing.T{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		check(fake)
	}()
	<-done

	return fake.Failed()
}

type fakeGenerator struct{ artifacts []gen.Artifact }

func (*fakeGenerator) Name() string        { return "fake" }
func (*fakeGenerator) Description() string { return "a generator that does as it is told" }
func (*fakeGenerator) Version() string     { return "1" }

func (g *fakeGenerator) Generate(context.Context, *ir.Document, gen.Options) ([]gen.Artifact, error) {
	return g.artifacts, nil
}

// wanderingGenerator returns something different every time, the way one that
// ranges over a map does.
type wanderingGenerator struct{ runs int }

func (*wanderingGenerator) Name() string        { return "wandering" }
func (*wanderingGenerator) Description() string { return "different every time" }
func (*wanderingGenerator) Version() string     { return "1" }

func (g *wanderingGenerator) Generate(context.Context, *ir.Document, gen.Options) ([]gen.Artifact, error) {
	g.runs++
	return []gen.Artifact{{
		Path:    "a.gen.go",
		Content: []byte(strings.Repeat("x", g.runs)),
	}}, nil
}

func paths(artifacts []gen.Artifact) []string {
	out := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, a.Path)
	}
	return out
}
