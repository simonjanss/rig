package tsclient

import (
	"fmt"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/tsbuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// methodFile emits a resource's client: one method per endpoint, and one guard
// per method that can be refused field by field.
func (e *emitter) methodFile(res *ir.Resource) (gen.Artifact, error) {
	b := e.open(snake(res.Name) + "_client.gen")

	runtime := b.ImportType(e.cfg.ClientImport, "Runtime")
	b.ImportType(e.cfg.ClientImport, "CallOptions")

	b.Comment(res.Name + "Client calls the " + res.Name + " endpoints. It is " +
		"reached as `client." + clientProperty(res) + "` rather than built directly.")
	b.L("export class %sClient {", res.Name)
	b.Indent()
	b.L("readonly #rt: %s;", runtime)
	b.NL()
	b.L("constructor(rt: %s) {", runtime)
	b.Indent()
	b.L("this.#rt = rt;")
	b.Outdent()
	b.L("}")

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		b.NL()
		e.method(b, res, ep, variantFor(ep))
	}

	// Beside the JSON create rather than instead of it, so nothing breaks the
	// day somebody adds a file column to a table they already had.
	if ep := createWithFiles(res); ep != nil {
		b.NL()
		e.method(b, res, ep, variantCreateWithFiles)
	}

	b.Outdent()
	b.L("}")

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		if guardName(res, ep) == "" {
			continue
		}
		b.NL()
		e.guard(b, res, ep)
	}

	return e.close(b)
}

// method emits one call.
func (e *emitter) method(b *tsbuf.Buf, res *ir.Resource, ep *ir.Endpoint, variant methodVariant) {
	sig := e.signature(b, res, ep, variant)

	b.Comment(e.methodDoc(res, ep, variant))
	b.L("%s(%s): %s {", methodNameFor(ep, variant), sig.params, sig.result)
	b.Indent()

	if sig.query != "" {
		search := b.Import(e.cfg.ClientImport, "setParam")
		b.L("const search = new URLSearchParams();")
		for _, f := range ep.Request.QueryParams {
			set := search
			if f.IsArray() {
				set = b.Import(e.cfg.ClientImport, "setParams")
			}
			b.L("%s(search, %s, %s.%s);", set, tsbuf.Quote(f.Wire), sig.query, tsbuf.Key(f.Wire))
		}
		b.NL()
	}

	if sig.form != nil {
		e.formPreamble(b, sig.form)
	}

	b.L("return %s%s(this.#rt, {", sig.call, sig.generic)
	b.Indent()
	b.L("name: %s,", tsbuf.Quote(ep.OperationID))
	b.L("method: %s,", e.methodLiteral(b, ep))
	b.L("path: %s,", sig.path)
	if sig.query != "" {
		b.L("query: search,")
	}
	if sig.body != "" {
		b.L("body: %s,", sig.body)
	}
	if sig.form != nil {
		b.L("form,")
	}
	if sig.accept != "" {
		b.L("accept: %s,", tsbuf.Quote(sig.accept))
	}
	if fallback := e.fallbackPath(ep); fallback != "" {
		b.L("fallback: %s,", tsbuf.Quote(fallback))
	}
	b.Outdent()
	b.L("}, options);")

	b.Outdent()
	b.L("}")
}

// methodDoc is what one method says about itself.
//
// The route is in it because that is the fact a reader is most often reaching
// for — matching a line in a log, or a tab in a network panel, to a call in the
// code — and it is the one thing the method name does not say.
func (e *emitter) methodDoc(res *ir.Resource, ep *ir.Endpoint, variant methodVariant) string {
	// The multipart create is a second method on one endpoint, so the document's
	// description belongs to the other one and this says what it is instead.
	if variant == variantCreateWithFiles {
		return createWithFilesDoc(res, ep)
	}

	doc := describe(ep.Description, describe(ep.Summary, describe(ep.Title,
		ep.Name+" "+res.Name+".")))

	doc += "\n\n" + strings.ReplaceAll(ep.Pattern, "  ", " ")
	if fallback := e.fallbackPath(ep); fallback != "" {
		doc += ", falling back to POST " + e.doc.API.BasePath + fallback +
			" when something between here and the server refuses the method. " +
			"The fallback is remembered, so it is tried once."
	}

	doc += "\n\nOperation " + ep.OperationID + "."

	if name := guardName(res, ep); name != "" {
		doc += "\n\nA refusal is read back with `" + name + "`, whose `fields` say " +
			"what was wrong with each member of the body."
	}

	if variant == variantUpload {
		doc += "\n\n" + uploadDoc(ep)
	}
	return doc
}

// methodNameFor is what one variant of an endpoint is called.
//
// The multipart create is the only one that is not the operation's own name:
// there are two methods on that endpoint and only one of them can be `create`.
func methodNameFor(ep *ir.Endpoint, variant methodVariant) string {
	if variant == variantCreateWithFiles {
		return methodName(ep) + "WithFiles"
	}
	return methodName(ep)
}

// signature is what one method takes and gives back.
type signature struct {
	params string
	result string
	// call is the runtime function this method is one line on top of.
	call string
	// generic is the type argument to that function, angle brackets included, or
	// empty for a call that decodes nothing.
	generic string
	// body is the expression to send as JSON, or empty.
	body string
	// form is the multipart body to send, or nil for a method that sends JSON or
	// nothing. The two are exclusive: an op carrying both is refused by the
	// runtime rather than resolved by a precedence somebody would have to look up.
	form *formBody
	// path is the expression that builds the route.
	path string
	// query is the name of the query parameter, or empty.
	query string
	// accept is the media type to ask for, or empty for JSON.
	accept string
}

// signature works out the shape of a method from the endpoint alone.
//
// Path parameters are positional and in route order; the query is one object;
// the body is whichever of the shared inputs, the named object, or the
// endpoint's own shape applies. Options come last, always, so that a header a
// deployment needs is never a reason to regenerate.
func (e *emitter) signature(
	b *tsbuf.Buf, res *ir.Resource, ep *ir.Endpoint, variant methodVariant,
) signature {
	var sig signature

	var params []string
	for _, f := range ep.Request.PathParams {
		params = append(params, argName(f)+": "+e.tsType(b, f))
	}

	switch {
	case variant == variantUpload:
		// One file and no row, so there is no shape to declare: the part's name
		// is the document's and the caller supplies the bytes.
		params = append(params, "file: "+b.ImportType(e.cfg.ClientImport, "Upload"))
		sig.form = uploadForm(ep)
	case variant == variantCreateWithFiles:
		// A required file column's identifier is Omit-ed from the json part:
		// the server assigns it from the bytes travelling beside the row, and
		// it is the whole reason this method exists — an input type that still
		// demanded the identifier would make the documented call refuse to
		// compile. A nullable file column stays, because a row pointing at a
		// file that already exists is an ordinary create.
		input := e.ref(b, inputTypeName(res, ep))
		if omitted := requiredFileMembers(res, ep); len(omitted) > 0 {
			input = "Omit<" + input + ", " + strings.Join(omitted, " | ") + ">"
		}
		params = append(params,
			"input: "+input,
			"files: "+e.ref(b, createFilesTypeName(res)))
		sig.form = createForm(ep)
	case ep.Name == ir.OpSearch:
		if filter, ok := genutil.SearchFilterField(ep); ok {
			params = append(params, "filter: "+e.tsType(b, filter))
			sig.body = "{ " + tsbuf.Key(filter.Wire) + ": filter }"
		}
	case inputTypeName(res, ep) != "":
		name := e.ref(b, inputTypeName(res, ep))
		params = append(params, "input: "+name)
		sig.body = "input"
	case ep.Request.BodyObject != "":
		params = append(params, "input: "+e.ref(b, ep.Request.BodyObject))
		sig.body = "input"
	case len(ep.Request.BodyParams) > 0:
		params = append(params, "input: "+e.ref(b, bodyTypeName(res, ep)))
		sig.body = "input"
	}

	if len(ep.Request.QueryParams) > 0 {
		sig.query = "query"
		// Defaulted to an empty object: every member of it is optional, so a
		// caller with nothing to say should not have to write `{}`.
		params = append(params, "query: "+e.ref(b, genutil.QueryTypeName(res, ep))+" = {}")
	}

	options := b.ImportType(e.cfg.ClientImport, "CallOptions")
	params = append(params, "options?: "+options)
	sig.params = strings.Join(params, ", ")

	sig.path = e.pathExpr(b, ep)

	switch success := e.successType(ep); {
	case e.isDownload(ep):
		// A download answers with whatever the file turned out to be, so the
		// response is handed over unread rather than decoded.
		sig.call = b.Import(e.cfg.ClientImport, "sendContent")
		sig.result = "Promise<Response>"
		sig.accept = "*/*"
	case success == "":
		sig.call = b.Import(e.cfg.ClientImport, "sendNoContent")
		sig.result = "Promise<void>"
	default:
		name := e.ref(b, success)
		sig.call = b.Import(e.cfg.ClientImport, "send")
		sig.generic = "<" + name + ">"
		sig.result = "Promise<" + name + ">"
	}

	return sig
}

// isDownload reports whether this endpoint answers with bytes rather than JSON.
//
// From the declared content types rather than from the endpoint's name: a
// download is the one response whose media type is not known when this runs, so
// the document saying it is not JSON is the whole of the signal.
func (e *emitter) isDownload(ep *ir.Endpoint) bool {
	for _, r := range ep.Responses {
		if r.StatusCode < 200 || r.StatusCode > 299 {
			continue
		}
		for _, ct := range r.ContentTypes {
			if ct != "" && !strings.Contains(ct, "json") {
				return true
			}
		}
	}
	return false
}

// successType is what a successful call comes back with, or empty for an
// endpoint that answers with nothing.
func (e *emitter) successType(ep *ir.Endpoint) string {
	for _, r := range ep.Responses {
		if r.StatusCode < 200 || r.StatusCode > 299 {
			continue
		}
		if r.BodyObject != "" {
			return r.BodyObject
		}
	}
	return ""
}

// methodLiteral is the HTTP method as the request will carry it.
func (e *emitter) methodLiteral(b *tsbuf.Buf, ep *ir.Endpoint) string {
	if ep.Method == "QUERY" {
		return b.Import(e.cfg.ClientImport, "METHOD_QUERY")
	}
	return tsbuf.Quote(ep.Method)
}

// fallbackPath is the alias a refused QUERY is retried against, or empty.
func (e *emitter) fallbackPath(ep *ir.Endpoint) string {
	if ep.Method != "QUERY" {
		return ""
	}
	for _, alias := range ep.AliasPatterns {
		path := genutil.RoutePath(alias)
		if strings.HasPrefix(path, e.doc.API.BasePath) {
			return strings.TrimPrefix(path, e.doc.API.BasePath)
		}
	}
	return ""
}

// pathExpr builds the route, substituting the path parameters.
//
// Every value is escaped: an identifier that arrived from somewhere else can be
// anything at all, and a slash in one would otherwise address a different route
// entirely.
func (e *emitter) pathExpr(b *tsbuf.Buf, ep *ir.Endpoint) string {
	path := strings.TrimPrefix(genutil.RoutePath(ep.Pattern), e.doc.API.BasePath)

	if len(ep.Request.PathParams) == 0 {
		return tsbuf.Quote(path)
	}

	pathValue := b.Import(e.cfg.ClientImport, "pathValue")
	expr := path
	for _, f := range ep.Request.PathParams {
		expr = strings.Replace(expr, "{"+f.Wire+"}",
			fmt.Sprintf("${%s(%s)}", pathValue, argName(f)), 1)
	}
	return "`" + expr + "`"
}

// guard emits the reader for one call's failure.
//
// A type predicate rather than a type, and named for the method rather than for
// the input, so that reading a refusal is the one line asking the question:
//
//	if (isTodoCreateError(err)) { … }
//
// The alternative is the caller naming the shape — `fieldsAs` — where naming the
// wrong one is not an error at all. Every member of a field shape is optional,
// so the update shape on a failed create matches perfectly and hands back an
// empty object. Here there is one shape that compiles.
func (e *emitter) guard(b *tsbuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := guardName(res, ep)
	fields := e.ref(b, genutil.FieldsTypeName(res, ep))
	rigError := b.ImportType(e.cfg.ClientImport, "RigError")
	isInvalid := b.Import(e.cfg.ClientImport, "isInvalid")

	b.Comment(name + " reads back what " + ep.OperationID + " refused.\n\n" +
		"True only for a body the server refused field by field. Anything else — " +
		"a 404, a request that never arrived — has nothing to put beside a " +
		"control, so `fields` would be an object nobody complained about.")
	b.L("export function %s(err: unknown): err is %s<%s> {", name, rigError, fields)
	b.Indent()
	b.L("return %s(err);", isInvalid)
	b.Outdent()
	b.L("}")
}

// argName is what a path parameter is called in the method signature.
func argName(f ir.Field) string { return lowerFirst(f.Name) }

// inputTypeName is the create or update input an endpoint takes, or empty when
// its body is a shape of its own.
func inputTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	if name := genutil.ModelInputName(ep); name != "" {
		return res.Name + name + "Input"
	}
	return ""
}
