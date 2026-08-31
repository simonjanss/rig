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

// The key mounts a route for a document rig was never asked to write. It is a
// warning rather than a refusal — the generator can be added second — but it has
// to be said, because the failure otherwise arrives as a go:embed line naming a
// file nobody wrote.
func TestOpenAPIServeWithoutTheGeneratorWarns(t *testing.T) {
	t.Parallel()

	_, out := parseOpenAPI(t, serveOpenAPI)
	if !strings.Contains(out, "RIG3011") {
		t.Fatalf("want RIG3011, got:\n%s", out)
	}
	// A warning: nothing here should stop a generate.
	if strings.Contains(out, "error") {
		t.Errorf("RIG3011 was reported as an error:\n%s", out)
	}
	// The message names the route, so it is obvious what was configured.
	if !strings.Contains(out, "/api/v1/openapi.json") {
		t.Errorf("the message does not name the route:\n%s", out)
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
