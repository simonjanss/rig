package compile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// compileCache runs the rowcache fixture's schema against a configuration
// written here, so that each combination of "is there a block" and "does a table
// ask" can be checked without a fixture directory of its own.
func compileCache(t *testing.T, projectSrc, tableSrc string) (*ir.Document, string) {
	t.Helper()

	dir := filepath.Join("testdata", "rowcache")
	schema := readSchema(t, filepath.Join(dir, "schema.json"))

	p, pdiags := project.Parse("rig.yaml", []byte(projectSrc))
	if pdiags.HasErrors() {
		t.Fatalf("rig.yaml:\n%s", pdiags.String())
	}

	// The fixture's own table configuration with the cache key replaced, so the
	// case is about that key and nothing else.
	base, err := os.ReadFile(filepath.Join(dir, "tables", "lesson.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(base), "cache: true", "") + "\n" + tableSrc

	path := filepath.Join(t.TempDir(), "lesson.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	set, tdiags := tableconf.LoadDir([]string{path})

	doc, cdiags := compile.Compile(schema, set, compile.Options{Project: p, Tool: "rig (test)"})

	var all diag.List
	all.Append(tdiags)
	all.Append(cdiags)
	return doc, all.String()
}

const (
	cacheOn  = "project:\n  name: demo\n  module: example.com/demo\ncache:\n  enabled: true\n"
	cacheOff = "project:\n  name: demo\n  module: example.com/demo\n"
	authOn   = "auth:\n  enabled: true\n"
)

// A table asking to be held needs the block that owns the channel. Without one
// there is nothing to publish a withdrawal on, so an entry could be held and
// never withdrawn — which is the single failure this whole design is arranged to
// make impossible, and the reason it is refused rather than ignored.
func TestATableCannotCacheWithoutTheBlock(t *testing.T) {
	t.Parallel()

	doc, out := compileCache(t, cacheOff, "cache: true\n")
	if !strings.Contains(out, "RIG3007") {
		t.Errorf("a table cached with no block was accepted:\n%s", out)
	}
	if res := doc.Resource("Lesson"); res != nil && res.Cached {
		t.Error("the flag was carried into the document anyway")
	}
}

// And the other direction: a block nothing reads. It is the rule every other
// block carries — numbers somebody set and believed in, silently unread — and it
// is here rather than in internal/project because there are two ways to satisfy
// it and that package can only see one.
func TestACacheNobodyReadsIsRefused(t *testing.T) {
	t.Parallel()

	if _, out := compileCache(t, cacheOn, ""); !strings.Contains(out, "RIG3002") {
		t.Errorf("a cache block with no reader at all was accepted:\n%s", out)
	}

	// An `auth:` block is one reader, and it was the only one until tables could
	// ask.
	if _, out := compileCache(t, cacheOn+authOn, ""); strings.Contains(out, "RIG3002") {
		t.Errorf("authentication is a reader:\n%s", out)
	}

	// And a table is the other, with no authentication anywhere.
	doc, out := compileCache(t, cacheOn, "cache: true\n")
	if strings.Contains(out, "RIG3002") {
		t.Errorf("a cached table is a reader:\n%s", out)
	}
	if res := doc.Resource("Lesson"); res == nil || !res.Cached {
		t.Error("the table asked to be cached and the document does not say so")
	}
}

// A table rig created is not a table a project can promise about.
//
// `cache: true` says every write to this table goes through the generated
// repository, and rig's own tables are written by the module that owns them —
// auth, files, notify, presence — in that module's own SQL. Those are exactly the
// writes nothing could publish a withdrawal from, so this is refused for the
// reason a missing block is: an entry that could be held and never withdrawn.
//
// The files fixture is the one that has both a foundation table with a
// configuration file of its own and ordinary tables beside it, which is what lets
// the second half of this be a control rather than an assumption.
func TestRigsOwnTablesCannotBeCached(t *testing.T) {
	t.Parallel()

	// rig_file is the files part's, and the diagnostic says so by naming the part
	// rather than the module, because the part is what a project scaffolded.
	_, out := compileFilesFixture(t, "rig_file.yaml")
	if !strings.Contains(out, "RIG3008") {
		t.Errorf("one of rig's own tables was allowed to cache:\n%s", out)
	}
	if !strings.Contains(out, "files") {
		t.Errorf("the refusal does not name the part the table belongs to:\n%s", out)
	}

	// The control: the same fixture, the same block, a table the project owns.
	doc, out := compileFilesFixture(t, "profile.yaml")
	if strings.Contains(out, "RIG3008") {
		t.Errorf("a project's own table was refused:\n%s", out)
	}
	if res := doc.Resource("Profile"); res == nil || !res.Cached {
		t.Error("the project's own table asked to be cached and the document does not say so")
	}
}

// compileFilesFixture compiles testdata/files with the cache block turned on and
// `cache: true` added to one of its table configurations.
func compileFilesFixture(t *testing.T, cacheOnTable string) (*ir.Document, string) {
	t.Helper()

	dir := filepath.Join("testdata", "files")
	schema := readSchema(t, filepath.Join(dir, "schema.json"))

	rigYAML, err := os.ReadFile(filepath.Join(dir, "rig.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, pdiags := project.Parse("rig.yaml", append(rigYAML, []byte("\ncache:\n  enabled: true\n")...))
	if pdiags.HasErrors() {
		t.Fatalf("rig.yaml:\n%s", pdiags.String())
	}

	paths, err := filepath.Glob(filepath.Join(dir, "tables", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// The one named file is rewritten into a directory of its own and the rest are
	// read where they lie, so the case is the key and nothing else.
	tmp := t.TempDir()
	for i, path := range paths {
		if filepath.Base(path) != cacheOnTable {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := filepath.Join(tmp, cacheOnTable)
		if err := os.WriteFile(edited, append(body, []byte("\ncache: true\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[i] = edited
	}

	set, tdiags := tableconf.LoadDir(paths)
	doc, cdiags := compile.Compile(schema, set, compile.Options{
		Project:    p,
		Tool:       "rig (test)",
		Foundation: readFoundation(t, dir),
	})

	var all diag.List
	all.Append(tdiags)
	all.Append(cdiags)
	return doc, all.String()
}

// Turning the cache on must not move a client's revision, and the per-table flag
// is the half of the switch that could have.
//
// Whether a replica answered out of memory or out of the database is not
// something a caller can observe, so a project that enabled it and spent a
// revision would be telling every client it was built against something older
// than the server over a change none of them can see. API.Cache has been cleared
// from the hash since the block existed; Resource.Cached has to be too, or the
// two halves of one switch would disagree about whether it is visible.
func TestCachingDoesNotMoveTheRevision(t *testing.T) {
	t.Parallel()

	plain, _ := compileCache(t, cacheOff, "")
	held, _ := compileCache(t, cacheOn, "cache: true\n")

	if res := held.Resource("Lesson"); res == nil || !res.Cached {
		t.Fatal("the second document is not the cached one")
	}

	want, err := plain.Hash()
	if err != nil {
		t.Fatal(err)
	}
	got, err := held.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("enabling the cache moved the revision:\n uncached %s\n   cached %s", want, got)
	}
}

// Hash takes a document by value and clears fields on the copy. Resources are a
// slice, so clearing one reaches through the shallow copy into the caller's
// document unless it takes one of its own — and a document quietly stripped of
// its opt-ins by having its revision computed is a generator emitting no cache
// at all.
func TestHashingDoesNotStripTheDocumentItWasGiven(t *testing.T) {
	t.Parallel()

	doc, _ := compileCache(t, cacheOn, "cache: true\n")
	if _, err := doc.Hash(); err != nil {
		t.Fatal(err)
	}

	if res := doc.Resource("Lesson"); res == nil || !res.Cached {
		t.Error("hashing the document cleared the flag it was asked about")
	}
}
