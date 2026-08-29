package gen_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The report is what `rig generate` and `rig check` print, so it is the only
// part of this package most people ever see.
func TestReportSaysWhatChangedAndCountsIt(t *testing.T) {
	t.Parallel()

	root := filepath.FromSlash("/project")
	deltas := []gen.Delta{
		{Status: gen.Added, Path: filepath.Join(root, "internal", "store", "a.gen.go")},
		{Status: gen.Changed, Path: filepath.Join(root, "internal", "store", "b.gen.go")},
		{Status: gen.Unchanged, Path: filepath.Join(root, "internal", "store", "c.gen.go")},
		{Status: gen.Stale, Path: filepath.Join(root, "internal", "store", "d.gen.go")},
	}

	var quiet bytes.Buffer
	gen.Report(&quiet, root, deltas, false)

	// Paths are relative to the project, because an absolute one is mostly the
	// reader's home directory.
	if !strings.Contains(quiet.String(), "internal/store/a.gen.go") {
		t.Errorf("paths should be relative:\n%s", quiet.String())
	}
	// What did not change is noise on every run.
	if strings.Contains(quiet.String(), "c.gen.go") {
		t.Errorf("unchanged files should be quiet by default:\n%s", quiet.String())
	}
	if !strings.Contains(quiet.String(), "1 add, 1 change, 1 ok, 1 stale") {
		t.Errorf("the summary should count every status:\n%s", quiet.String())
	}

	var loud bytes.Buffer
	gen.Report(&loud, root, deltas, true)

	if !strings.Contains(loud.String(), "c.gen.go") {
		t.Errorf("verbose should list what did not change:\n%s", loud.String())
	}
}

// A path outside the project keeps its absolute form: a relative path with
// enough ".." in it to leave the tree tells the reader nothing.
func TestReportKeepsForeignPathsAbsolute(t *testing.T) {
	t.Parallel()

	outside := filepath.FromSlash("/elsewhere/x.gen.go")

	var out bytes.Buffer
	gen.Report(&out, filepath.FromSlash("/project"), []gen.Delta{
		{Status: gen.Added, Path: outside},
	}, false)

	if !strings.Contains(out.String(), outside) {
		t.Errorf("got %q, want the absolute path", out.String())
	}
}

func TestReportOfNothingSaysNothing(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	gen.Report(&out, "/project", nil, false)

	if out.Len() != 0 {
		t.Errorf("got %q, want silence", out.String())
	}
}

// Two generators answering to one name is a build mistake, and the second
// registration is the one that would silently win.
func TestRegisteringTwiceUnderOneNamePanics(t *testing.T) {
	t.Parallel()

	r := gen.NewRegistry()
	r.MustRegister(&namedGenerator{name: "persist-go"})

	defer func() {
		if recover() == nil {
			t.Error("a duplicate name should panic at registration, not resolve later")
		}
	}()
	r.MustRegister(&namedGenerator{name: "persist-go"})
}

func TestRegistryOrdersByName(t *testing.T) {
	t.Parallel()

	r := gen.NewRegistry()
	for _, name := range []string{"server-go", "model-go", "persist-go"} {
		r.MustRegister(&namedGenerator{name: name})
	}

	var got []string
	for _, g := range r.All() {
		got = append(got, g.Name())
	}

	want := []string{"model-go", "persist-go", "server-go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("All = %v, want %v", got, want)
	}
	if names := r.Names(); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("Names = %v, want %v", names, want)
	}

	if _, ok := r.Get("model-go"); !ok {
		t.Error("Get should find a registered generator")
	}
	if _, ok := r.Get("nothing"); ok {
		t.Error("Get should miss an unregistered one")
	}
}

// The manifest is bookkeeping, not a source of truth. Losing it costs a few
// files reported as new; refusing to start over it would cost the project.
func TestACorruptManifestIsForgottenRatherThanFatal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(gen.ManifestName))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := gen.LoadManifest(root)
	if err != nil {
		t.Fatalf("a corrupt manifest should not stop the run: %v", err)
	}
	if len(m.Files) != 0 {
		t.Errorf("it should have been forgotten, got %d entries", len(m.Files))
	}
}

func TestAMissingManifestIsAFirstRun(t *testing.T) {
	t.Parallel()

	m, err := gen.LoadManifest(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || len(m.Files) != 0 {
		t.Errorf("got %+v, want an empty manifest", m)
	}
	if _, found := m.Lookup("internal/store/a.gen.go"); found {
		t.Error("nothing is recorded yet")
	}
}

// Saved sorted, because an unsorted manifest produces a diff on every run from
// map ordering alone and nobody can see the real change in it.
func TestTheManifestIsSavedSorted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	m := gen.NewManifest()
	m.Files = []gen.Entry{
		{Path: "z.gen.go", Generator: "persist-go"},
		{Path: "a.gen.go", Generator: "persist-go"},
		{Path: "m.gen.go", Generator: "persist-go"},
	}

	if err := m.Save(root); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(gen.ManifestName)))
	if err != nil {
		t.Fatal(err)
	}

	var reloaded gen.Manifest
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Files) != 3 ||
		reloaded.Files[0].Path != "a.gen.go" || reloaded.Files[2].Path != "z.gen.go" {
		t.Errorf("files = %+v, want them sorted", reloaded.Files)
	}
	if reloaded.Version == 0 {
		t.Error("the manifest should record its own version")
	}

	// And what was written comes back.
	loaded, err := gen.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if e, found := loaded.Lookup("m.gen.go"); !found || e.Generator != "persist-go" {
		t.Errorf("Lookup = %+v, %v", e, found)
	}
}

// Decode is how every generator reads its own options, so a typo in rig.yaml
// has to stop the run rather than be ignored into a default.
func TestDecodeRefusesAnOptionNobodyDeclared(t *testing.T) {
	t.Parallel()

	type options struct {
		Package string `json:"package"`
	}

	good, err := gen.Decode[options](gen.Options{Raw: map[string]any{"package": "store"}})
	if err != nil || good.Package != "store" {
		t.Fatalf("Decode = %+v, %v", good, err)
	}

	if _, err := gen.Decode[options](gen.Options{Raw: map[string]any{"pakage": "store"}}); err == nil {
		t.Error("a misspelled option should be an error, not a silent default")
	}

	// No options at all is the ordinary case for a generator that needs none.
	if _, err := gen.Decode[options](gen.Options{}); err != nil {
		t.Errorf("an absent options block is not a failure: %v", err)
	}
}

type namedGenerator struct{ name string }

func (g *namedGenerator) Name() string        { return g.name }
func (g *namedGenerator) Description() string { return "for a test" }
func (g *namedGenerator) Version() string     { return "1" }

func (g *namedGenerator) Generate(context.Context, *ir.Document, gen.Options) ([]gen.Artifact, error) {
	return nil, nil
}
