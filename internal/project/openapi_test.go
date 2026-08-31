package project_test

import (
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

func parseOpenAPI(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	p, diags := project.Parse("rig.yaml", []byte(minimal+body))
	return p, diags.String()
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
// it are already in rig.yaml, and an option to state it again would be an option
// whose only wrong value is a typo.
func TestTheEmbedPackageIsDerived(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ outDir, imp, pkg string }{
		{"docs", "github.com/simonjanss/fantasyfootball/docs", "docs"},
		{"api/docs", "github.com/simonjanss/fantasyfootball/api/docs", "docs"},
		{"internal/spec", "github.com/simonjanss/fantasyfootball/internal/spec", "spec"},
		{"./docs/", "github.com/simonjanss/fantasyfootball/docs", "docs"},
	} {
		p, out := parseOpenAPI(t, serveOpenAPI+
			"generators:\n  - name: openapi\n    out_dir: "+tc.outDir+"\n")
		if out != "" {
			t.Errorf("out_dir %q complained:\n%s", tc.outDir, out)
			continue
		}

		got := p.OpenAPIIR()
		if got == nil {
			t.Errorf("out_dir %q resolved to nothing", tc.outDir)
			continue
		}
		if got.Import != tc.imp {
			t.Errorf("out_dir %q: import = %q, want %q", tc.outDir, got.Import, tc.imp)
		}
		if got.Package != tc.pkg {
			t.Errorf("out_dir %q: package = %q, want %q", tc.outDir, got.Package, tc.pkg)
		}
		// The paths belong to the route namespace and are filled in at freeze,
		// so that nothing outside the compiler has to know how a base path is
		// joined.
		if got.JSONPath != "" || got.YAMLPath != "" {
			t.Errorf("out_dir %q: the paths were filled in here, not at freeze", tc.outDir)
		}
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
