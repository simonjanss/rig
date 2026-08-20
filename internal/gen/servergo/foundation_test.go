package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/internal/gen/servicego"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// embeddedDoc is a document whose modules carry their own schema.
//
// The flag is set here rather than in a fixture because it is the only thing that
// differs: which sets a project needs follows from its auth, files and
// notification blocks, which the fixtures already have. A second copy of one of
// them would be a few hundred lines that differ in one boolean.
func embeddedDoc(t *testing.T, fixture string) *ir.Document {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.API.EmbeddedFoundation = true
	return doc
}

// emitted reports whether a generator wrote a file by this name. [find] fails the
// test when there is none, which is the opposite of what the vendored case needs
// to assert.
func emitted(artifacts []gen.Artifact, name string) bool {
	for _, a := range artifacts {
		if filepath.Base(a.Path) == name {
			return true
		}
	}
	return false
}

// A project that vendored its foundation gets no file at all — the same rule
// auth.gen.go follows, and for the same reason: this is the only thing in the API
// package that would name rig/migrate, so not writing it is what keeps goose out
// of the module.
func TestNoFoundationFileWhenVendored(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	if doc.API.EmbeddedFoundation {
		t.Fatal("the fixture should be vendored, which is the default")
	}

	artifacts := gentest.Run(t, servergo.New(), doc, opts())
	if emitted(artifacts, "foundation.gen.go") {
		t.Error("a vendored project should get no migration wiring")
	}
	for _, a := range artifacts {
		if strings.Contains(string(a.Content), "rig/migrate") {
			t.Errorf("%s names rig/migrate; goose should not reach a vendored project's "+
				"API package", a.Path)
		}
	}
}

// And an embedded one gets the sets it needs, in the order they must be applied,
// with its own last.
func TestFoundationFileNamesEverySetInOrder(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, servergo.New(), embeddedDoc(t, "notify.ir.json"), opts())

	if !emitted(artifacts, "foundation.gen.go") {
		t.Fatal("an embedded project needs the migration wiring")
	}
	src := find(t, artifacts, "foundation.gen.go")

	// This fixture has an auth block and notifications, so it needs auth's set
	// and notify's — and not files', which nothing here asks for.
	for _, want := range []string{
		"github.com/simonjanss/rig/auth/foundation",
		"github.com/simonjanss/rig/notify/foundation",
		"github.com/simonjanss/rig/migrate",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("should import %s:\n%s", want, src)
		}
	}
	if strings.Contains(src, "rig/files/foundation") {
		t.Errorf("nothing here accepts uploads, so files' set should be absent:\n%s", src)
	}

	// Order is the whole point of generating this. auth creates rig_account, which
	// notify's tables reference, so auth cannot come second.
	authAt := strings.Index(src, `Name: "rig/auth"`)
	notifyAt := strings.Index(src, `Name: "rig/notify"`)
	projectAt := strings.Index(src, "project,")
	if authAt < 0 || notifyAt < 0 || projectAt < 0 {
		t.Fatalf("want all three sources:\n%s", src)
	}
	if !(authAt < notifyAt && notifyAt < projectAt) {
		t.Errorf("want rig/auth, then rig/notify, then the project:\n%s", src)
	}

	// Each set records itself in its own table, which is what lets the modules be
	// upgraded independently.
	for _, want := range []string{"foundation.Table"} {
		if !strings.Contains(src, want) {
			t.Errorf("each source should name its own bookkeeping table (%s):\n%s", want, src)
		}
	}
}

// A project that only accepts uploads needs one set, and must not be handed the
// auth module for it: rig_file references nothing, which is what makes uploads
// work in a project with no authentication.
func TestFilesAloneNeedsOnlyItsOwnSet(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, servergo.New(), embeddedDoc(t, "files.ir.json"), opts())

	if !emitted(artifacts, "foundation.gen.go") {
		t.Fatal("an embedded project needs the migration wiring")
	}
	src := find(t, artifacts, "foundation.gen.go")

	if !strings.Contains(src, "github.com/simonjanss/rig/files/foundation") {
		t.Errorf("should import files' set:\n%s", src)
	}
	for _, absent := range []string{"rig/auth/foundation", "rig/notify/foundation"} {
		if strings.Contains(src, absent) {
			t.Errorf("a project with only uploads should not depend on %s:\n%s", absent, src)
		}
	}
}

// And it compiles, which is what catches a set named with the wrong import alias
// or a field that has since been renamed.
func TestFoundationFileCompiles(t *testing.T) {
	t.Parallel()

	doc := embeddedDoc(t, "notify.ir.json")

	api := gentest.Run(t, servicego.New(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "store_import": "rigtest/store",
	}})
	api = append(api, gentest.Run(t, servergo.New(), doc, opts())...)

	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{
			Dir: "store",
			Artifacts: gentest.Run(t, persistgo.New(), doc, gen.Options{Raw: map[string]any{
				"package": "store", "model_import": "rigtest/model",
			}}),
		},
		gentest.Package{Dir: "api", Artifacts: api},
	)
}
