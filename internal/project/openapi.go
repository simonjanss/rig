package project

import (
	"go/token"
	"path"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// OpenAPIIR resolves where the generated document is served from, or nil for a
// project that keeps it a file.
//
// Everything here is derived, and that is the point: the routes come from
// `api.base_path`, and the package the document is embedded in is the project's
// module path joined to the openapi generator's `out_dir`. Both ends are already
// written down, there is exactly one right answer, and an option to state it
// again would be an option whose only wrong value is a typo.
//
// It is here rather than in a generator for the reason
// [Project.checkDeprecatedServerOptions] is: the question spans an `api:` key,
// `project.module` and the `generators:` list, and the only place all three are
// readable is the project configuration. A generator sees its own options and
// the frozen document, neither of which can say where another generator writes.
//
// The paths are not filled in here. They belong to the route namespace, which is
// computed once at freeze so that nothing else has to know how a base path is
// joined — or how a collision with a resource route is reported.
func (p *Project) OpenAPIIR() *ir.OpenAPI {
	if !p.Config.API.OpenAPI.Serve {
		return nil
	}

	dir, ok := p.openAPIOutDir()
	if !ok {
		// Refused by checkOpenAPIServed, which has the message. Returning nil
		// keeps a document that cannot describe its own routes from claiming to.
		return nil
	}

	imp := path.Join(p.Config.Project.Module, dir)
	return &ir.OpenAPI{Import: imp, Package: path.Base(imp)}
}

// openAPIOutDir is the openapi generator's output directory in slash form, and
// whether it is one the document can be served out of at all.
//
// Serving turns that directory into a Go package: the openapi generator writes
// the embed beside the document it produced, because an embed directive resolves
// against the directory of the file it is written in and cannot climb out of it.
// So the directory has to be one that can hold an importable package, and the
// last segment has to be a name Go will accept as a package name.
func (p *Project) openAPIOutDir() (string, bool) {
	for _, g := range p.Config.Generators {
		if g.Name != "openapi" {
			continue
		}
		dir := path.Clean(strings.ReplaceAll(g.OutDir, "\\", "/"))
		if !usableAsPackageDir(dir) {
			return "", false
		}
		return dir, true
	}
	return "", false
}

// usableAsPackageDir reports whether a directory can hold the embed package.
func usableAsPackageDir(dir string) bool {
	switch {
	case dir == "" || dir == "." || dir == "/":
		// The module root, where the project's own main package already is.
		return false
	case path.IsAbs(dir), strings.HasPrefix(dir, ".."):
		// Outside the module, so no import path names it.
		return false
	}

	base := path.Base(dir)
	switch {
	case !token.IsIdentifier(base), token.Lookup(base).IsKeyword():
		return false
	case majorVersionSegment(base):
		// Go reads a trailing v2 as a module's major version rather than as a
		// package name, so the name an importer qualifies with would be the
		// segment before it — and the package clause would disagree.
		return false
	}
	return true
}

// majorVersionSegment reports whether a path segment reads as a major version.
func majorVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkOpenAPIServed reports an `api.openapi.serve` that cannot be satisfied.
//
// An error rather than a warning, because there is no half-configured state that
// works: the router imports the package the document is embedded in, and if
// there is no such package the project does not build. Said here, once, naming
// the directory — rather than as an import of something that does not exist.
func (p *Project) checkOpenAPIServed() diag.List {
	var diags diag.List

	if !p.Config.API.OpenAPI.Serve {
		return diags
	}
	at := p.At("api", "openapi", "serve")

	var raw string
	var configured bool
	for _, g := range p.Config.Generators {
		if g.Name == "openapi" {
			raw, configured = g.OutDir, true
			break
		}
	}
	if !configured {
		diags.Add(diag.CodeOpenAPINotServable, at,
			"`api.openapi.serve` mounts the document at %s/openapi.json, and no `openapi` "+
				"generator is configured to write one. Add it — `- name: openapi` with an "+
				"`out_dir` — or drop the key", p.Config.API.BasePath)
		return diags
	}

	if dir := path.Clean(strings.ReplaceAll(raw, "\\", "/")); !usableAsPackageDir(dir) {
		diags.Add(diag.CodeOpenAPINotServable, at,
			"the openapi generator writes to %q, and serving the document makes that "+
				"directory a Go package — the embed has to sit beside the document, because "+
				"an embed directive cannot climb out of the directory of the file it is "+
				"written in. %q cannot be a package: give the generator a subdirectory of "+
				"its own whose name Go would accept, `out_dir: docs` being the usual one",
			raw, dir)
	}

	return diags
}
