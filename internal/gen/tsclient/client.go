package tsclient

import (
	"sort"
	"strings"

	"github.com/simonjanss/rig/internal/gen/tsbuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// clientFile emits the entry point: what the server is, and one property per
// resource.
func (e *emitter) clientFile() (gen.Artifact, error) {
	b := e.open("client.gen")

	b.Doc("The generated client for the " + e.doc.API.Name + " API, version " +
		e.doc.API.Version + ".\n\n" +
		"One method per endpoint, grouped by resource: `createClient(config)` and " +
		"then `client.<resource>.<operation>(…)`. The types are this directory's; " +
		"everything underneath a method — the request, the credential, the failure " +
		"— is " + e.cfg.ClientImport + "'s, and a failure arrives as a `RigError` " +
		"whose `code` says which of them it was.")

	runtime := b.Import(e.cfg.ClientImport, "Runtime")
	config := b.ImportType(e.cfg.ClientImport, "Config")

	b.Comment("The prefix every route sits under.\n\n" +
		"The document's, not a setting: a client that could be pointed at a " +
		"different one would be a client for a different API.")
	b.L("export const basePath = %s;", tsbuf.Quote(e.doc.API.BasePath))
	b.NL()

	b.Comment("The date the API surface this client was generated from last " +
		"changed.\n\n" +
		"Every request says it, so the server's logs can answer the question you " +
		"have to answer before removing anything: how old is the oldest client " +
		"still calling. Regenerating against an unchanged API leaves it alone — it " +
		"is not a build stamp.")
	b.L("export const revision = %s;", tsbuf.Quote(e.revision()))
	b.NL()

	b.Comment("Where the revision is sent: the same header the server generated " +
		"from this document reads.")
	b.L("export const revisionHeader = %s;", tsbuf.Quote(e.revisionHeader()))
	b.NL()

	if e.cfg.DefaultBaseURL != "" {
		b.Comment("Where this API runs in development, so a tool that only ever " +
			"talks to that one can leave `baseUrl` out.")
		b.L("export const defaultBaseUrl = %s;", tsbuf.Quote(e.cfg.DefaultBaseURL))
		b.NL()
	}

	resources := e.exposed()

	b.Comment("The API. One property per resource, so what a client can do is what " +
		"the schema says it can, and reaching for a resource that does not exist " +
		"is a type error rather than a 404.")
	b.L("export type Client = {")
	b.Indent()
	b.Comment("The transport underneath, for a request this client has no method " +
		"for — and for the live-sync collections, which take it.")
	b.L("readonly runtime: %s;", runtime)
	for _, res := range resources {
		b.Comment(describe(e.resourceDoc(res), res.Name+"."))
		b.L("readonly %s: %s;", clientProperty(res), e.refValue(b, res.Name+"Client"))
	}
	b.Outdent()
	b.L("};")
	b.NL()

	b.Comment("Builds a client.\n\n" +
		"The only required setting is `baseUrl`, and in a browser served from the " +
		"same origin as the API that is the empty string. A credential can be " +
		"given here or installed later with `client.runtime.use(…)`.")
	b.L("export function createClient(config: %s): Client {", config)
	b.Indent()
	b.L("const runtime = new %s(config, {", runtime)
	b.Indent()
	b.L("basePath,")
	b.L("revision,")
	b.L("revisionHeader,")
	if profile := e.doc.API.Auth; profile != nil {
		b.L("auth: {")
		b.Indent()
		b.L("basePath: %s,", tsbuf.Quote(profile.BasePath))
		b.L("accessTtlMs: %d,", profile.Session.AccessTTL.Duration().Milliseconds())
		b.L("refreshTtlMs: %d,", profile.Session.RefreshTTL.Duration().Milliseconds())
		b.L("rotationLeewayMs: %d,", profile.Session.RotationLeeway.Duration().Milliseconds())
		b.Outdent()
		b.L("},")
	}
	b.Outdent()
	b.L("});")
	b.NL()
	b.L("return {")
	b.Indent()
	b.L("runtime,")
	for _, res := range resources {
		b.L("%s: new %s(runtime),", clientProperty(res), e.refValue(b, res.Name+"Client"))
	}
	b.Outdent()
	b.L("};")
	b.Outdent()
	b.L("}")

	return e.close(b)
}

// revision is the date the API surface last changed, or the document's version
// when it carries none.
func (e *emitter) revision() string {
	if e.doc.API.Revision != "" {
		return e.doc.API.Revision
	}
	return e.doc.API.Version
}

// revisionHeader is where the revision is sent.
func (e *emitter) revisionHeader() string {
	if e.doc.API.RevisionHeader != "" {
		return e.doc.API.RevisionHeader
	}
	return defaultRevisionHeader
}

// resourceDoc is what a resource says about itself, taken from its own object so
// the sentence beside `client.todos` is the sentence beside `type Todo`.
func (e *emitter) resourceDoc(res *ir.Resource) string {
	if obj := e.object(res.Name); obj != nil {
		return obj.Description
	}
	return res.Description
}

// indexFile emits the barrel.
//
// One import specifier for a whole generated client, which is what a front end
// wants: `import { createClient, type Todo } from "./api"`. The alternative is
// somebody having to know that `Todo` is in `todo.gen.ts` and `TodoCreateInput`
// is in `todo_input.gen.ts`, which is a fact about this generator rather than
// about the API.
//
// Written from the artifacts already produced rather than from the document a
// second time, so a file that stops being emitted stops being exported in the
// same run.
func (e *emitter) indexFile(artifacts []gen.Artifact) (gen.Artifact, error) {
	b := tsbuf.New()
	e.cur = ""

	b.Doc("Everything the generated client exports.\n\n" +
		"The runtime it is built on is a separate package and is not re-exported: " +
		"a caller who needs `RigError`, `Session` or `paginate` imports them from " +
		e.cfg.ClientImport + " directly, which is where their documentation is.")

	modules := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		modules = append(modules, "./"+strings.TrimSuffix(a.Path, ".ts")+".js")
	}
	sort.Strings(modules)

	for _, module := range modules {
		b.L("export * from %s;", tsbuf.Quote(module))
	}

	return e.artifact("index.ts", b)
}
