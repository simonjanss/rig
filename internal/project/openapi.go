package project

import "github.com/simonjanss/rig/internal/diag"

// checkOpenAPIServed reports `api.openapi.serve` with nothing to serve.
//
// It is here rather than in a generator for the reason
// [Project.checkDeprecatedServerOptions] is: the question spans two blocks — an
// `api:` key and the `generators:` list — and the only place both are readable
// is the project configuration. A generator sees its own options and the frozen
// document, neither of which can say whether some other generator was
// configured.
//
// A warning and not a refusal. A project wiring this up in the other order — key
// first, generator second — is not broken, just unfinished, and what it costs to
// leave it wrong is loud enough on its own: server-go refuses to emit a router
// with no `openapi_import`, and an import naming a package no generator writes
// is a build that fails. This is the earliest and cheapest of the three, not the
// only one.
func (p *Project) checkOpenAPIServed() diag.List {
	var diags diag.List

	if !p.Config.API.OpenAPI.Serve {
		return diags
	}
	for _, g := range p.Config.Generators {
		if g.Name == "openapi" {
			return diags
		}
	}

	diags.Add(diag.CodeOpenAPINotServable, p.At("api", "openapi", "serve"),
		"`api.openapi.serve` mounts the document at %s/openapi.json, and no `openapi` "+
			"generator is configured to write one. Add it — `- name: openapi` with an "+
			"`out_dir` — or drop the key", p.Config.API.BasePath)

	return diags
}
