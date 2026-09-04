package project

import (
	"fmt"
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
// The join is not the plain one, and the difference is the whole of #128.
// `out_dir` is a path from the directory holding rig.yaml, and an import path is
// a path from the module root; those agree only when rig.yaml sits at the module
// root. So the directory is taken relative to the go.mod above it — see
// [Project.goModuleAt] — and a project whose module begins under api/ gets the
// import its compiler would. A project with no go.mod yet is the assumption
// unchanged, because `rig init` writes no go.mod and the tutorial reaches
// `go mod init` after the first generate.
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
	doc, _ := p.resolveOpenAPI()
	return doc
}

// resolveOpenAPI is the embed package, or the reasons there is not one.
//
// One function rather than two, because [Project.OpenAPIIR] and
// [Project.checkOpenAPIServed] have to agree about every way this can fail. The
// first discards the diagnostics and the second returns them, so a case only one
// of them knew about would be either a refusal nothing explains or an import
// nothing checked — and the second of those is what #128 was.
func (p *Project) resolveOpenAPI() (*ir.OpenAPI, diag.List) {
	var diags diag.List

	if !p.Config.API.OpenAPI.Serve {
		return nil, diags
	}
	at := p.At("api", "openapi", "serve")

	raw, configured := p.generatorOutDir("openapi")
	if !configured {
		diags.Add(diag.CodeOpenAPINotServable, at,
			"`api.openapi.serve` mounts the document at %s/openapi.json, and no `openapi` "+
				"generator is configured to write one. Add it — `- name: openapi` with an "+
				"`out_dir` — or drop the key", p.Config.API.BasePath)
		return nil, diags
	}

	dir := path.Clean(strings.ReplaceAll(raw, "\\", "/"))
	if !usableAsPackageDir(dir) {
		diags.Add(diag.CodeOpenAPINotServable, at,
			"the openapi generator writes to %q, and serving the document makes that "+
				"directory a Go package — the embed has to sit beside the document, because "+
				"an embed directive cannot climb out of the directory of the file it is "+
				"written in. %q cannot be a package: give the generator a subdirectory of "+
				"its own whose name Go would accept, `out_dir: docs` being the usual one",
			raw, dir)
		return nil, diags
	}

	// Where the module begins, which `out_dir` alone cannot say.
	mod, found := p.goModuleAt(dir)

	// And whether that is the module the router is generated into, which is the
	// one that has to be able to import the embed. Asked of server-go's own
	// out_dir rather than assumed, because in a two-half layout the two
	// directories can land in different modules — or one of them in none — and
	// no import path exists that would join them.
	if router, ok := p.generatorOutDir("server-go"); ok {
		routerMod, routerFound := p.goModuleAt(path.Clean(strings.ReplaceAll(router, "\\", "/")))
		if found != routerFound || mod.File != routerMod.File {
			diags.Add(diag.CodeOpenAPINotServable, at,
				"the openapi generator writes to %q and the router is generated into %q, "+
					"and %s. Serving the document makes the first a Go package the second "+
					"imports, and no import path crosses a module boundary — put `out_dir` "+
					"under the same go.mod the router is under",
				raw, router, describeModuleSplit(p, mod, found, routerMod, routerFound))
			return nil, diags
		}
	}

	// Absent — a project that has not run `go mod init` yet — the directory
	// holding rig.yaml is assumed to be the module root, which is what this
	// always assumed. `rig init` writes no go.mod and docs/tutorial.md reaches
	// `go mod init` after the first generate, so refusing here would be a
	// diagnostic about the order of a tutorial.
	if found {
		if mod.Declared != p.Config.Project.Module {
			diags.Add(diag.CodeOpenAPINotServable, at,
				"the openapi generator writes to %q, and serving the document makes that "+
					"directory a Go package the generated router imports. %s puts it in the "+
					"module %q, and this project is %q — a package cannot be imported out of "+
					"another module. Move `out_dir` under the directory holding your go.mod, "+
					"or correct `project.module`",
				raw, p.Rel(mod.File), mod.Declared, p.Config.Project.Module)
			return nil, diags
		}
		if !usableAsPackageDir(mod.Sub) {
			diags.Add(diag.CodeOpenAPINotServable, at,
				"the openapi generator writes to %q, which %s makes the root of the module "+
					"itself — where your own main package is. Serving the document needs a "+
					"package of its own to embed it in: give the generator a subdirectory, "+
					"`out_dir: %s/docs` being the usual one",
				raw, p.Rel(mod.File), dir)
			return nil, diags
		}
		dir = mod.Sub
	}

	imp := path.Join(p.Config.Project.Module, dir)
	return &ir.OpenAPI{Import: imp, Package: path.Base(imp)}, diags
}

// generatorOutDir is one configured generator's `out_dir` as written, and
// whether that generator is configured at all.
//
// Returned raw: a diagnostic names what somebody typed, and the cleaning is the
// caller's to do once beside the checks that depend on it.
func (p *Project) generatorOutDir(name string) (string, bool) {
	for _, g := range p.Config.Generators {
		if g.Name == name {
			return g.OutDir, true
		}
	}
	return "", false
}

// describeModuleSplit says which module each of the two directories landed in,
// for the message that reports they are not the same one.
func describeModuleSplit(p *Project, doc goModule, docFound bool, router goModule, routerFound bool) string {
	switch {
	case docFound && routerFound:
		return fmt.Sprintf("%s puts the first in %q while %s puts the second in %q",
			p.Rel(doc.File), doc.Declared, p.Rel(router.File), router.Declared)
	case docFound:
		return fmt.Sprintf("%s puts the first in %q and no go.mod above the second says which module it is in",
			p.Rel(doc.File), doc.Declared)
	default:
		return fmt.Sprintf("no go.mod above the first says which module it is in, while %s puts the second in %q",
			p.Rel(router.File), router.Declared)
	}
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
//
// The one check here that reads the filesystem, and the only one in this package
// that does: whether the directory is inside the module `project.module` names
// is a question about go.mod, and asking it is the difference between a
// diagnostic and a `go build` error inside a generated file.
func (p *Project) checkOpenAPIServed() diag.List {
	_, diags := p.resolveOpenAPI()
	return diags
}
