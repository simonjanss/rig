package gen_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// fake is a generator that emits whatever it is told to.
type fake struct {
	name      string
	version   string
	artifacts []gen.Artifact
	err       error
}

func (f *fake) Name() string        { return f.name }
func (f *fake) Description() string { return "a test generator" }
func (f *fake) Version() string {
	if f.version == "" {
		return "1"
	}
	return f.version
}

func (f *fake) Generate(context.Context, *ir.Document, gen.Options) ([]gen.Artifact, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.artifacts, nil
}

func validDoc() *ir.Document {
	return &ir.Document{IRVersion: ir.CurrentVersion, Valid: true}
}

func registryWith(gs ...gen.Generator) *gen.Registry {
	r := gen.NewRegistry()
	for _, g := range gs {
		r.MustRegister(g)
	}
	return r
}

func file(path, content string) gen.Artifact {
	return gen.Artifact{Path: path, Content: []byte(content)}
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	r := registryWith(&fake{name: "b"}, &fake{name: "a"})

	if got := r.Names(); strings.Join(got, ",") != "a,b" {
		t.Errorf("Names() = %v, want sorted", got)
	}
	if _, ok := r.Get("a"); !ok {
		t.Error("Get(a) should find it")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) should not")
	}
}

// A duplicate name is a build mistake, and finding out at startup beats
// finding out from whichever one happened to register second.
func TestDuplicateRegistrationPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("registering the same name twice should panic")
		}
	}()

	registryWith(&fake{name: "same"}, &fake{name: "same"})
}

func TestRunRefusesAnInvalidDocument(t *testing.T) {
	t.Parallel()

	doc := validDoc()
	doc.Valid = false

	_, err := gen.Run(context.Background(), registryWith(&fake{name: "a"}), doc, t.TempDir(),
		[]gen.Spec{{Name: "a"}})
	// Generating from a schema rig knows to be wrong produces code that
	// compiles and misbehaves, which is worse than producing nothing.
	if err == nil {
		t.Fatal("an invalid document should not be generated from")
	}
	if !strings.Contains(err.Error(), "did not validate") {
		t.Errorf("unhelpful message: %v", err)
	}
}

func TestRunResolvesPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	g := &fake{name: "a", artifacts: []gen.Artifact{file("sub/x.go", "x")}}

	results, err := gen.Run(context.Background(), registryWith(g), validDoc(), root,
		[]gen.Spec{{Name: "a", OutDir: "out"}})
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, "out", "sub", "x.go")
	if got := results[0].Artifacts[0].Path; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestRunRejectsAnUnknownGenerator(t *testing.T) {
	t.Parallel()

	_, err := gen.Run(context.Background(), gen.NewRegistry(), validDoc(), t.TempDir(),
		[]gen.Spec{{Name: "nope"}})
	if err == nil || !strings.Contains(err.Error(), "no generator named") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestRunRejectsDuplicatePaths(t *testing.T) {
	t.Parallel()

	g := &fake{name: "a", artifacts: []gen.Artifact{file("x.go", "one"), file("x.go", "two")}}

	// Two artifacts at one path would resolve to whichever was written last.
	_, err := gen.Run(context.Background(), registryWith(g), validDoc(), t.TempDir(),
		[]gen.Spec{{Name: "a"}})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("emitting the same path twice should be an error, got %v", err)
	}
}

func TestDiffAndWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	g := &fake{name: "a", artifacts: []gen.Artifact{file("x.go", "one")}}

	results, err := gen.Run(context.Background(), registryWith(g), validDoc(), root, []gen.Spec{{Name: "a"}})
	if err != nil {
		t.Fatal(err)
	}

	manifest := gen.NewManifest()
	deltas, err := gen.Diff(root, results, manifest, gen.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 || deltas[0].Status != gen.Added {
		t.Fatalf("first run should add: %+v", deltas)
	}
	if !gen.NeedsWork(deltas) {
		t.Error("an addition is work")
	}

	next, err := gen.Write(root, results, deltas, gen.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Save(root); err != nil {
		t.Fatal(err)
	}

	// Second run: nothing to do.
	manifest, _ = gen.LoadManifest(root)
	deltas, _ = gen.Diff(root, results, manifest, gen.DiffOptions{})
	if len(deltas) != 1 || deltas[0].Status != gen.Unchanged {
		t.Fatalf("second run should be unchanged: %+v", deltas)
	}
	if gen.NeedsWork(deltas) {
		t.Error("nothing changed, so there is nothing to do")
	}
}

// A generated file that has been edited is reported rather than silently
// overwritten. The manifest is the only way to tell that from a file rig has
// simply not caught up with.
func TestHandEditIsAConflict(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	g := &fake{name: "a", artifacts: []gen.Artifact{file("x.go", "one")}}

	results, _ := gen.Run(context.Background(), registryWith(g), validDoc(), root, []gen.Spec{{Name: "a"}})
	deltas, _ := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	next, _ := gen.Write(root, results, deltas, gen.WriteOptions{})
	_ = next.Save(root)

	// Someone edits the generated file.
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, _ := gen.LoadManifest(root)
	deltas, _ = gen.Diff(root, results, manifest, gen.DiffOptions{})
	if deltas[0].Status != gen.Conflict {
		t.Fatalf("status = %q, want conflict", deltas[0].Status)
	}

	if _, err := gen.Write(root, results, deltas, gen.WriteOptions{}); err == nil {
		t.Fatal("writing over a hand edit should be refused")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the message should say how to proceed anyway: %v", err)
	}

	// The refusal must be total: nothing is written when anything conflicts.
	got, _ := os.ReadFile(filepath.Join(root, "x.go"))
	if string(got) != "hand edited" {
		t.Errorf("the edit was overwritten despite the refusal: %q", got)
	}

	if _, err := gen.Write(root, results, deltas, gen.WriteOptions{Force: true}); err != nil {
		t.Fatalf("--force should overwrite: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(root, "x.go"))
	if string(got) != "one" {
		t.Errorf("--force did not overwrite: %q", got)
	}
}

// A create-once file belongs to the developer as soon as it exists.
func TestCreateOnce(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	scaffold := gen.Artifact{Path: "service.go", Content: []byte("generated"), Mode: gen.CreateOnce}
	g := &fake{name: "a", artifacts: []gen.Artifact{scaffold}}

	results, _ := gen.Run(context.Background(), registryWith(g), validDoc(), root, []gen.Spec{{Name: "a"}})
	deltas, _ := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	if deltas[0].Status != gen.Added {
		t.Fatalf("first run should add, got %q", deltas[0].Status)
	}
	next, _ := gen.Write(root, results, deltas, gen.WriteOptions{})
	_ = next.Save(root)

	if err := os.WriteFile(filepath.Join(root, "service.go"), []byte("my own work"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, _ := gen.LoadManifest(root)
	deltas, _ = gen.Diff(root, results, manifest, gen.DiffOptions{})
	if deltas[0].Status != gen.Kept {
		t.Fatalf("status = %q, want keep", deltas[0].Status)
	}
	if gen.NeedsWork(deltas) {
		t.Error("a kept file is not work")
	}

	if _, err := gen.Write(root, results, deltas, gen.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "service.go"))
	if string(got) != "my own work" {
		t.Errorf("a create-once file was overwritten: %q", got)
	}
}

// A file left behind by a renamed table would otherwise sit in the repository
// forever, still compiling and still wrong.
func TestStaleFileIsReportedAndPruned(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	before := &fake{name: "a", artifacts: []gen.Artifact{file("old.go", "x"), file("keep.go", "y")}}
	results, _ := gen.Run(context.Background(), registryWith(before), validDoc(), root, []gen.Spec{{Name: "a"}})
	deltas, _ := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	next, _ := gen.Write(root, results, deltas, gen.WriteOptions{})
	_ = next.Save(root)

	// The table was renamed, so one file is no longer produced.
	after := &fake{name: "a", artifacts: []gen.Artifact{file("keep.go", "y")}}
	results, _ = gen.Run(context.Background(), registryWith(after), validDoc(), root, []gen.Spec{{Name: "a"}})

	manifest, _ := gen.LoadManifest(root)
	deltas, _ = gen.Diff(root, results, manifest, gen.DiffOptions{})

	var stale *gen.Delta
	for i := range deltas {
		if deltas[i].Status == gen.Stale {
			stale = &deltas[i]
		}
	}
	if stale == nil {
		t.Fatalf("the orphan should be reported: %+v", deltas)
	}
	if filepath.Base(stale.Path) != "old.go" {
		t.Errorf("wrong file reported stale: %s", stale.Path)
	}

	// Without --prune it stays, so nothing is deleted by surprise.
	if _, err := gen.Write(root, results, deltas, gen.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.go")); err != nil {
		t.Error("the orphan should survive without --prune")
	}

	if _, err := gen.Write(root, results, deltas, gen.WriteOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.go")); !os.IsNotExist(err) {
		t.Error("--prune should remove the orphan")
	}
}

// Deleting a generator from rig.yaml is not the same as holding one back with
// --only: nothing produces its files any more, and the ones the scan cannot
// recognize — a stub carries neither `.gen.` nor the banner — are only visible
// in the record.
func TestARemovedGeneratorsOutputIsStale(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	a := &fake{name: "a", artifacts: []gen.Artifact{file("a.gen.go", "package p\n")}}
	stub := gen.Artifact{Path: "services/b/b.go", Content: []byte("package b\n"), Mode: gen.CreateOnce}
	b := &fake{name: "b", artifacts: []gen.Artifact{stub}}

	results, _ := gen.Run(context.Background(), registryWith(a, b), validDoc(), root,
		[]gen.Spec{{Name: "a"}, {Name: "b"}})
	deltas, _ := gen.Diff(root, results, gen.NewManifest(), gen.DiffOptions{})
	next, _ := gen.Write(root, results, deltas, gen.WriteOptions{})
	_ = next.Save(root)

	// Twice, because the record is what carries the exemption. A run that spared
	// the file and then recorded it as an ordinary overwrite would have spent
	// the exemption on itself, and the second `--prune` would take somebody's
	// hook — with nothing in the output of either run to say so.
	for round := 1; round <= 2; round++ {
		// `b` is gone from the configuration, so this is a full run of
		// everything that is left.
		only, _ := gen.Run(context.Background(), registryWith(a, b), validDoc(), root, []gen.Spec{{Name: "a"}})
		manifest, _ := gen.LoadManifest(root)

		deltas, err := gen.Diff(root, only, manifest, gen.DiffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		stale := staleNames(root, deltas)
		if len(stale) != 1 || stale[0] != "services/b/b.go" {
			t.Fatalf("round %d: a removed generator's output should be reported: %v", round, stale)
		}

		// Reported, and left alone: it is a create-once file, so what is inside
		// it is somebody's own code and `--prune` is not entitled to it.
		next, err := gen.Write(root, only, deltas, gen.WriteOptions{Prune: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Save(root); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "services", "b", "b.go")); err != nil {
			t.Fatalf("round %d: --prune deleted a create-once file: %v", round, err)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	m := gen.NewManifest()
	m.IRHash = "sha256:abc"
	m.Generators["a"] = "1"
	m.Files = append(m.Files, gen.Entry{Path: "x.go", Generator: "a", Mode: "overwrite", SHA256: gen.Sum([]byte("x"))})

	if err := m.Save(root); err != nil {
		t.Fatal(err)
	}

	loaded, err := gen.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IRHash != "sha256:abc" || loaded.Generators["a"] != "1" {
		t.Errorf("round trip lost data: %+v", loaded)
	}
	if _, ok := loaded.Lookup("x.go"); !ok {
		t.Error("the entry should be findable")
	}
}

// A corrupt manifest costs at most a few files reported as new, which is a far
// better outcome than refusing to run. What it does not cost is the leftover
// check — that survives a missing manifest by scanning, which
// TestStaleWithoutAManifest covers.
func TestCorruptManifestIsRecoverable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, ".rig")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "manifest.json"), []byte("{{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := gen.LoadManifest(root)
	if err != nil {
		t.Fatalf("a corrupt manifest should not fail the run: %v", err)
	}
	if len(m.Files) != 0 {
		t.Error("a corrupt manifest should be treated as empty")
	}
}

func TestDecodeOptions(t *testing.T) {
	t.Parallel()

	type opts struct {
		Package string `json:"package"`
	}

	got, err := gen.Decode[opts](gen.Options{Raw: map[string]any{"package": "store"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Package != "store" {
		t.Errorf("Package = %q", got.Package)
	}

	// A mistyped option that is silently ignored looks configured and behaves
	// as though it is not.
	if _, err := gen.Decode[opts](gen.Options{Raw: map[string]any{"packge": "store"}}); err == nil {
		t.Error("an unknown option should be rejected")
	}

	// No options at all is fine.
	if _, err := gen.Decode[opts](gen.Options{}); err != nil {
		t.Errorf("empty options should decode: %v", err)
	}
}
