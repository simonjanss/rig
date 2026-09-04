package project_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

const serveOpenAPI = `api:
  openapi:
    serve: true
`

const openapiGenerator = `generators:
  - name: openapi
    out_dir: docs
`

// fixtureModule is the module path `minimal` declares.
const fixtureModule = "github.com/simonjanss/fantasyfootball"

// goMod is the smallest go.mod that names a module.
func goMod(module string) string { return "module " + module + "\n\ngo 1.26\n" }

// loadOpenAPI writes a project on disk and loads it.
//
// A real directory rather than [project.Parse] over bytes, because where go.mod
// sits relative to rig.yaml is now part of the answer and there is no way to say
// that in a string. Keys are slash paths from the tree root, and the one whose
// base is rig.yaml is the configuration loaded.
//
// A `.git` file goes at that root, because the search for go.mod stops at a
// repository boundary: without one, a temporary directory would go on climbing
// into whatever holds it and could answer with a go.mod that has nothing to do
// with the test. A file rather than a directory is what a worktree has, and it
// has to count.
func loadOpenAPI(t *testing.T, files map[string]string) (*project.Project, string) {
	t.Helper()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git"), "gitdir: elsewhere\n")

	var config string
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		writeFile(t, abs, body)
		if filepath.Base(rel) == "rig.yaml" {
			config = abs
		}
	}
	if config == "" {
		t.Fatal("the tree has no rig.yaml")
	}

	p, diags := project.LoadFile(config)
	return p, diags.String()
}

// parseOpenAPI is the flat project: go.mod beside rig.yaml, which is the layout
// every case here means unless it says otherwise.
func parseOpenAPI(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	return loadOpenAPI(t, map[string]string{
		"rig.yaml": minimal + body,
		"go.mod":   goMod(fixtureModule),
	})
}

// Off is the absence, so a project that never heard of the key gets no route
// and no field on Handlers.
func TestOpenAPIIsNotServedByDefault(t *testing.T) {
	t.Parallel()

	p, out := parseOpenAPI(t, "")
	if out != "" {
		t.Fatalf("a minimal configuration complained:\n%s", out)
	}
	if p.Config.API.OpenAPI.Serve {
		t.Error("serve is on in a project that did not ask for it")
	}
}

func TestOpenAPIServeIsRead(t *testing.T) {
	t.Parallel()

	p, out := parseOpenAPI(t, serveOpenAPI+openapiGenerator)
	if out != "" {
		t.Fatalf("a valid configuration complained:\n%s", out)
	}
	if !p.Config.API.OpenAPI.Serve {
		t.Error("serve is off in a project that asked for it")
	}
}

// The whole point of deriving it: there is exactly one right answer, both ends of
// it are already written down, and an option to state it again would be an option
// whose only wrong value is a typo.
//
// The layouts are the table, because the answer is not `project.module` joined to
// `out_dir` — that is only right when rig.yaml sits at the module root. `out_dir`
// is a path from rig.yaml and an import path is a path from the module root, and
// go.mod is the only thing that says how far apart those two are.
func TestTheEmbedPackageIsDerived(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		rigYAML string // where rig.yaml goes
		goMod   string // where go.mod goes, "" for a project that has none yet
		module  string
		outDir  string
		imp     string
		pkg     string
	}{{
		name:    "go.mod beside rig.yaml",
		rigYAML: "rig.yaml", goMod: "go.mod",
		module: "example.com/p", outDir: "docs",
		imp: "example.com/p/docs", pkg: "docs",
	}, {
		name:    "a subdirectory of the flat project",
		rigYAML: "rig.yaml", goMod: "go.mod",
		module: "example.com/p", outDir: "api/docs",
		imp: "example.com/p/api/docs", pkg: "docs",
	}, {
		name:    "internal is a directory like any other",
		rigYAML: "rig.yaml", goMod: "go.mod",
		module: "example.com/p", outDir: "internal/spec",
		imp: "example.com/p/internal/spec", pkg: "spec",
	}, {
		name:    "the written form is cleaned",
		rigYAML: "rig.yaml", goMod: "go.mod",
		module: "example.com/p", outDir: "./docs/",
		imp: "example.com/p/docs", pkg: "docs",
	}, {
		// #128: rig.yaml above both halves, the module under api/, and a module
		// path that ends in the same segment. Joining the raw out_dir gives
		// example.com/p/api/api/openapi, which is nothing.
		name:    "the module begins under api/, and its path says so",
		rigYAML: "rig.yaml", goMod: "api/go.mod",
		module: "example.com/p/api", outDir: "api/openapi",
		imp: "example.com/p/api/openapi", pkg: "openapi",
	}, {
		// The same layout with a module path that does not mention api/, which is
		// what examples/linearlite does. The join has to lose a segment here and
		// keep one above: both come from the same missing fact.
		name:    "the module begins under api/, and its path does not",
		rigYAML: "rig.yaml", goMod: "api/go.mod",
		module: "example.com/p", outDir: "api/docs",
		imp: "example.com/p/docs", pkg: "docs",
	}, {
		name:    "the module begins above rig.yaml",
		rigYAML: "service/rig.yaml", goMod: "go.mod",
		module: "example.com/p", outDir: "docs",
		imp: "example.com/p/service/docs", pkg: "docs",
	}, {
		// `rig init` writes no go.mod and the tutorial reaches `go mod init` after
		// the first generate, so the directory holding rig.yaml is assumed to be
		// the module root until there is something on disk that says otherwise.
		name:    "no go.mod yet",
		rigYAML: "rig.yaml", goMod: "",
		module: "example.com/p", outDir: "docs",
		imp: "example.com/p/docs", pkg: "docs",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := map[string]string{
				tc.rigYAML: "project:\n  name: p\n  module: " + tc.module + "\n" +
					serveOpenAPI +
					"generators:\n  - name: openapi\n    out_dir: " + tc.outDir + "\n",
			}
			if tc.goMod != "" {
				files[tc.goMod] = goMod(tc.module)
			}

			p, out := loadOpenAPI(t, files)
			if out != "" {
				t.Fatalf("complained:\n%s", out)
			}

			got := p.OpenAPIIR()
			if got == nil {
				t.Fatal("resolved to nothing")
			}
			if got.Import != tc.imp {
				t.Errorf("import = %q, want %q", got.Import, tc.imp)
			}
			if got.Package != tc.pkg {
				t.Errorf("package = %q, want %q", got.Package, tc.pkg)
			}
			// The paths belong to the route namespace and are filled in at
			// freeze, so that nothing outside the compiler has to know how a base
			// path is joined.
			if got.JSONPath != "" || got.YAMLPath != "" {
				t.Error("the paths were filled in here, not at freeze")
			}
		})
	}
}

// go.mod's grammar is not one line, and the module path is read out of it here
// rather than with golang.org/x/mod — a dependency the root module does not have
// and one every project that installs rig would acquire. So the forms it accepts
// are worth pinning: nothing writes the block form for this directive, but a file
// that uses it still names a module, and refusing to read it would be a
// diagnostic about nothing.
func TestTheModulePathIsReadFromEveryFormOfGoMod(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body string }{
		{"the ordinary one", "module example.com/p\n\ngo 1.26\n"},
		{"with a trailing comment", "module example.com/p // the application\n\ngo 1.26\n"},
		{"quoted", "module \"example.com/p\"\n\ngo 1.26\n"},
		{"in a block", "module (\n\texample.com/p\n)\n\ngo 1.26\n"},
		{"after a comment line", "// what this is\n\nmodule example.com/p\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, out := loadOpenAPI(t, map[string]string{
				"rig.yaml": "project:\n  name: p\n  module: example.com/p\n" + serveOpenAPI +
					"generators:\n  - name: openapi\n    out_dir: api/docs\n",
				"api/go.mod": tc.body,
			})

			if out != "" {
				t.Fatalf("complained:\n%s", out)
			}
			// api/docs read against a module beginning at api/ is docs, which the
			// plain join could not produce.
			if got := p.OpenAPIIR(); got == nil || got.Import != "example.com/p/docs" {
				t.Errorf("import = %#v, want example.com/p/docs", got)
			}
		})
	}
}

// A go.mod that names no module is not a module root, and the directory above it
// may still be one.
func TestAGoModWithNoModuleDirectiveIsNotAModuleRoot(t *testing.T) {
	t.Parallel()

	p, out := loadOpenAPI(t, map[string]string{
		"rig.yaml": "project:\n  name: p\n  module: example.com/p\n" + serveOpenAPI +
			"generators:\n  - name: openapi\n    out_dir: api/docs\n",
		"go.mod":     goMod("example.com/p"),
		"api/go.mod": "go 1.26\n",
	})

	if out != "" {
		t.Fatalf("complained:\n%s", out)
	}
	if got := p.OpenAPIIR(); got == nil || got.Import != "example.com/p/api/docs" {
		t.Errorf("import = %#v, want example.com/p/api/docs", got)
	}
}

// Nil is the absence, and it is what a generator asks: does this project serve
// the document.
func TestNotServingResolvesToNothing(t *testing.T) {
	t.Parallel()

	p, out := parseOpenAPI(t, openapiGenerator)
	if out != "" {
		t.Fatalf("a project that writes the document and does not serve it complained:\n%s", out)
	}
	if got := p.OpenAPIIR(); got != nil {
		t.Errorf("want nil, got %#v", got)
	}
}

// The key mounts a route for a document rig was never asked to write. An error
// rather than a warning: the router imports the package the document is embedded
// in, so there is no half-configured state that builds.
func TestOpenAPIServeWithoutTheGeneratorIsRefused(t *testing.T) {
	t.Parallel()

	_, out := parseOpenAPI(t, serveOpenAPI)
	if !strings.Contains(out, "RIG3011") {
		t.Fatalf("want RIG3011, got:\n%s", out)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("RIG3011 was not reported as an error:\n%s", out)
	}
	// The message names the route, so what was configured is obvious.
	if !strings.Contains(out, "/api/v1/openapi.json") {
		t.Errorf("the message does not name the route:\n%s", out)
	}
}

// Serving turns the output directory into a Go package, so a directory that
// cannot be one is refused when rig.yaml is read rather than discovered by a
// build that will not compile.
func TestAnUnusableOutputDirectoryIsRefused(t *testing.T) {
	t.Parallel()

	for _, outDir := range []string{
		".",           // the module root, where main already is
		"",            // the same thing, said by omission
		"/tmp/docs",   // outside the module, so no import path names it
		"../docs",     // likewise
		"api-docs",    // not an identifier
		"docs/v2",     // Go reads a trailing v2 as a major version
		"internal/go", // a keyword
	} {
		block := "generators:\n  - name: openapi\n"
		if outDir != "" {
			block += "    out_dir: " + outDir + "\n"
		}
		p, out := parseOpenAPI(t, serveOpenAPI+block)

		if !strings.Contains(out, "RIG3011") {
			t.Errorf("out_dir %q was accepted:\n%s", outDir, out)
		}
		if got := p.OpenAPIIR(); got != nil {
			t.Errorf("out_dir %q resolved to %#v rather than nothing", outDir, got)
		}
	}
}

// A directory that is a package everywhere else is the module root in a layout
// where the module begins below rig.yaml, and the module root is where the
// project's own main package already is.
func TestTheModuleRootByWayOfANestedGoModIsRefused(t *testing.T) {
	t.Parallel()

	p, out := loadOpenAPI(t, map[string]string{
		"rig.yaml": "project:\n  name: p\n  module: example.com/p\n" + serveOpenAPI +
			"generators:\n  - name: openapi\n    out_dir: api\n",
		"api/go.mod": goMod("example.com/p"),
	})

	if !strings.Contains(out, "RIG3011") {
		t.Fatalf("out_dir naming the module root was accepted:\n%s", out)
	}
	if got := p.OpenAPIIR(); got != nil {
		t.Errorf("resolved to %#v rather than nothing", got)
	}
}

// `project.module` is the module path and go.mod is where the module begins, so
// the two have to be talking about the same module. They disagree in a project
// scaffolded before `go mod init`, because `rig init` guesses example.com/<name>.
func TestAGoModDeclaringAnotherModuleIsRefused(t *testing.T) {
	t.Parallel()

	p, out := loadOpenAPI(t, map[string]string{
		"rig.yaml": "project:\n  name: p\n  module: example.com/p\n" + serveOpenAPI + openapiGenerator,
		"go.mod":   goMod("example.com/somethingelse"),
	})

	if !strings.Contains(out, "RIG3011") {
		t.Fatalf("a go.mod naming another module was accepted:\n%s", out)
	}
	// Both ends, so the message says which two things disagree rather than that
	// something does.
	for _, want := range []string{"example.com/p", "example.com/somethingelse", "go.mod"} {
		if !strings.Contains(out, want) {
			t.Errorf("the message does not name %q:\n%s", want, out)
		}
	}
	if got := p.OpenAPIIR(); got != nil {
		t.Errorf("resolved to %#v rather than nothing", got)
	}
}

// The document and the router landing in different modules is the two-half
// layout's own mistake, and the one no go.mod above the document can catch on its
// own: examples/linearlite writes to docs/ beside rig.yaml while its module — and
// so its router — begins under api/. No import path crosses that.
func TestTheDocumentOutsideTheRoutersModuleIsRefused(t *testing.T) {
	t.Parallel()

	p, out := loadOpenAPI(t, map[string]string{
		"rig.yaml": "project:\n  name: p\n  module: example.com/p\n" + serveOpenAPI +
			"generators:\n" +
			"  - name: server-go\n    out_dir: api/internal/generated/api\n" +
			"  - name: openapi\n    out_dir: docs\n",
		"api/go.mod": goMod("example.com/p"),
	})

	if !strings.Contains(out, "RIG3011") {
		t.Fatalf("a document outside the router's module was accepted:\n%s", out)
	}
	if got := p.OpenAPIIR(); got != nil {
		t.Errorf("resolved to %#v rather than nothing", got)
	}
}

// And the same two halves inside one module is the arrangement that works, which
// is the point of the check above being about the boundary rather than the depth.
func TestTheDocumentInsideTheRoutersModuleIsQuiet(t *testing.T) {
	t.Parallel()

	p, out := loadOpenAPI(t, map[string]string{
		"rig.yaml": "project:\n  name: p\n  module: example.com/p\n" + serveOpenAPI +
			"generators:\n" +
			"  - name: server-go\n    out_dir: api/internal/generated/api\n" +
			"  - name: openapi\n    out_dir: api/docs\n",
		"api/go.mod": goMod("example.com/p"),
	})

	if out != "" {
		t.Fatalf("the two-half layout complained:\n%s", out)
	}
	if got := p.OpenAPIIR(); got == nil || got.Import != "example.com/p/docs" {
		t.Errorf("import = %#v, want example.com/p/docs", got)
	}
}

func TestOpenAPIServeWithTheGeneratorIsQuiet(t *testing.T) {
	t.Parallel()

	_, out := parseOpenAPI(t, serveOpenAPI+openapiGenerator)
	if strings.Contains(out, "RIG3011") {
		t.Fatalf("RIG3011 fired with the generator configured:\n%s", out)
	}
}

// And the check is about the key, not about the generator: a project that writes
// the document and does not serve it is the ordinary case.
func TestTheGeneratorWithoutServeIsQuiet(t *testing.T) {
	t.Parallel()

	_, out := parseOpenAPI(t, openapiGenerator)
	if strings.Contains(out, "RIG3011") {
		t.Fatalf("RIG3011 fired without the key:\n%s", out)
	}
}
