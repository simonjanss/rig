package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
)

// apidocModule serves the generated document. It is imported only by a project
// that asked for the routes, so an application that keeps the document a file
// carries neither the package nor the two handlers.
const apidocModule = runtimeModule + "/apidoc"

// openAPIEmbedVar is the variable the openapi generator declares beside the
// document it wrote. Both halves are here rather than spelled out twice: this
// generator writes the read and that one writes the declaration, and a rename
// that reached only one would be a project that does not compile.
const openAPIEmbedVar = "Document"

// servesOpenAPI reports whether this project mounts the document describing it.
func (e *emitter) servesOpenAPI() bool { return e.doc.API.OpenAPI != nil }

// openAPIPaths emits the two routes as constants.
//
// Exported, the way the auth foundation's BasePath is: a test, a smoke check or
// a log line at startup should be able to name the route rather than retype a
// path that came out of the configuration.
func (e *emitter) openAPIPaths(b *gobuf.Buf) {
	o := e.doc.API.OpenAPI

	b.Comment("Where the OpenAPI document answers. Both come from the compiled " +
		"document, expanded against the same base path every other route was and " +
		"checked against every other route for a collision — so these agree with " +
		"the specification that describes them by construction rather than by " +
		"somebody keeping two strings in step.")
	b.L("const (")
	b.Comment("OpenAPIJSONPath is where the JSON rendering answers.")
	b.L("OpenAPIJSONPath = %s", gobuf.Quote(o.JSONPath))
	b.Comment("OpenAPIYAMLPath is where the YAML rendering answers.")
	b.L("OpenAPIYAMLPath = %s", gobuf.Quote(o.YAMLPath))
	b.L(")")
	b.NL()
}

// openAPIDocsVar is the resolved document, shared by the mount and the line that
// says where it went.
const openAPIDocsVar = "openAPIDocs"

// openAPIResolver emits the once-resolved handler.
//
// Once rather than per call, because two callers want it: Register mounts it and
// Mount says where it went, and hashing the document twice to hand out two
// handlers over the same bytes would be work nobody asked for.
//
// The error is a panic rather than a return: there is nowhere to give one back
// from a package-level value, and the one thing that makes New fail is an embed
// directive naming a directory with no document in it. That cannot happen here —
// the directive and the document are written by the same generator run — which
// is exactly why a panic is the right shape for it. A route silently answering
// 404 forever would be found by whoever tried to use it.
func (e *emitter) openAPIResolver(b *gobuf.Buf) {
	apidocPkg := b.Import(apidocModule)
	docsPkg := b.Import(e.cfg.OpenAPIImport)
	syncPkg := b.Import("sync")

	b.Comment(openAPIDocsVar + " is the OpenAPI document describing this API, ready to " +
		"serve.\n\n" +
		"The bytes come from the package the openapi generator wrote them into, " +
		"because a go:embed directive cannot climb out of the directory of the " +
		"file it is written in and that generator's out_dir is not this one. " +
		"Which is what makes serving the specification a rig.yaml key rather " +
		"than a line in main.go.")
	b.L("var %s = %s.OnceValue(func() *%s.Handler {", openAPIDocsVar, syncPkg, apidocPkg)
	// A local name the imported package cannot also have. `docs` here would
	// shadow the import in every project whose documents live in `docs/`, which
	// is every project that took the scaffold's suggestion — legal Go, and one
	// more thing for a reader to work out.
	b.L("resolved, err := %s.New(%s.%s, %s.Options{",
		apidocPkg, docsPkg, openAPIEmbedVar, apidocPkg)
	b.L("JSONPath: OpenAPIJSONPath,")
	b.L("YAMLPath: OpenAPIYAMLPath,")
	b.L("})")
	b.L("if err != nil {")
	b.L("panic(%s + err.Error())", gobuf.Quote(e.cfg.Package+": "))
	b.L("}")
	b.L("return resolved")
	b.L("})")
	b.NL()
}

// openAPIMount is the block registerFunc emits.
//
// Nothing is passed in and nothing can be left out, which is the whole point of
// where the bytes come from: `api.openapi.serve` makes the openapi generator
// write a Go file beside the document, this package imports it, and serving the
// specification costs a rig.yaml key and no line in anybody's main.go.
func (e *emitter) openAPIMount(b *gobuf.Buf) {
	b.NL()
	b.Comment("The document describing this API, on the same mux as the routes it " +
		"describes. Hand-written rather than generated, for the reason the inbox " +
		"is: serving two documents is the same in every project.\n\n" +
		"Only the renderings this project writes are mounted, so `formats: " +
		"[json]` gives one route and not two — and the document describes one, " +
		"because the generator that wrote it knew the same thing. Nothing reads " +
		"claims here: what the document says is what every generated client was " +
		"built against, and a specification nobody may fetch is one nobody can " +
		"use. To gate it, turn `api.openapi.serve` off and mount " +
		"[github.com/simonjanss/rig/runtime/apidoc.Handler] in main.go instead.")
	b.L("%s().Mount(mux)", openAPIDocsVar)
}

// openAPIAnnounce is the line Mount writes.
//
// In Mount rather than in Register, and the difference is not cosmetic: Register
// builds a mux and is called by a test, by a batch job reaching a service through
// it, and by anything else that wants routes without a server. Mount runs once,
// as this process starts, beside the line that says which address it is
// listening on — which is where somebody looking for the document would look.
func (e *emitter) openAPIAnnounce(b *gobuf.Buf) {
	b.Comment("Said once, at info, the way the monitoring page says where it " +
		"listens. A specification is no use to somebody who cannot find it, and " +
		"where it went is a base path away from what anybody would guess.\n\n" +
		"What was actually mounted rather than the two constants above, so a " +
		"project writing only one rendering is not told about a route it does " +
		"not serve.")
	b.L("app.Logger.InfoContext(ctx, %s, %s, %s().Paths())",
		gobuf.Quote("serving the OpenAPI document"), gobuf.Quote("at"), openAPIDocsVar)
	b.NL()
}
