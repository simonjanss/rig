package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
)

// apidocModule serves the generated document. It is imported only by a project
// that asked for the routes, so an application that keeps the document a file
// carries neither the package nor the two handlers.
const apidocModule = runtimeModule + "/apidoc"

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

// openAPIField is the Handlers field a project fills with the embedded document.
//
// The bytes are the application's because go:embed cannot reach out of the
// package it is written in: the document is written to the openapi generator's
// out_dir, and this package is server-go's. So the embed line goes where the
// project's migrations are already embedded, and the doc comment carries it —
// the one thing somebody has to write is worth having in godoc rather than only
// in the documentation.
func (e *emitter) openAPIField(b *gobuf.Buf) {
	b.Comment("OpenAPI is the document describing this API, embedded. Setting it " +
		"mounts [OpenAPIJSONPath] and [OpenAPIYAMLPath]; a nil one leaves them " +
		"unmounted, so a project that has not embedded it yet serves nothing " +
		"rather than two routes answering 404.\n\n" +
		"It is a filesystem rather than bytes because that is what go:embed " +
		"produces, and it is the application's to supply because go:embed cannot " +
		"reach out of the package it is written in — the document is written to " +
		"the openapi generator's out_dir, not beside this file. In main.go, " +
		"beside the migrations:\n\n" +
		"\t//go:embed docs/openapi.gen.json docs/openapi.gen.yaml\n" +
		"\tvar apidocs embed.FS\n\n" +
		"Only the renderings present are mounted, so `formats: [json]` gives one " +
		"route and not two. Nothing reads claims on the way in: what the document " +
		"says is what every client was generated against, and a specification " +
		"nobody may fetch is one nobody can use. A project that has to gate it " +
		"leaves this nil and mounts " +
		"[github.com/simonjanss/rig/runtime/apidoc.Handler] itself.")
	b.L("OpenAPI %s.FS", b.Import("io/fs"))
	b.NL()
}

// openAPIMount is the block registerFunc emits.
//
// The error is a panic rather than a return, for the reason the three panics
// above it are: Register has no error to give back, and the one thing that makes
// New fail is a go:embed line naming a directory with no document in it — a
// mistake in the wiring, found at startup, every time, rather than by whoever
// first fetched the route.
func (e *emitter) openAPIMount(b *gobuf.Buf) {
	apidocPkg := b.Import(apidocModule)

	b.NL()
	b.Comment("The document describing this API, on the same mux as the routes it " +
		"describes. Hand-written rather than generated, for the reason the inbox " +
		"is: serving two documents is the same in every project.")
	b.L("if h.OpenAPI != nil {")
	b.L("docs, err := %s.New(h.OpenAPI, %s.Options{", apidocPkg, apidocPkg)
	b.L("JSONPath: OpenAPIJSONPath,")
	b.L("YAMLPath: OpenAPIYAMLPath,")
	b.L("})")
	b.L("if err != nil {")
	b.L("panic(\"api.Register: \" + err.Error())")
	b.L("}")
	b.L("docs.Mount(mux)")
	b.L("}")
}
