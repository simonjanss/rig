package servergo

import (
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// handlerFile emits one resource's routes and handlers.
func (e *emitter) handlerFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.registerResource(b, res)
	for i := range res.Endpoints {
		e.handler(b, res, &res.Endpoints[i])
	}

	return artifact(naming.Snake(res.Name)+"_routes.gen.go", b)
}

// registerResource mounts every one of a resource's routes.
func (e *emitter) registerResource(b *gobuf.Buf, res *ir.Resource) {
	httpPkg := b.Import("net/http")

	b.Comment("register" + res.Name + " mounts " + res.Name + "'s routes.")
	b.L("func register%s(mux *%s.ServeMux, s Server, svc %sService) {", res.Name, httpPkg, res.Name)

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		handler := "handle" + ep.Impl.HandlerName

		b.L("mux.HandleFunc(%s, %s(s, svc))", gobuf.Quote(ep.Pattern), handler)
		for _, alias := range ep.AliasPatterns {
			// The alias exists for intermediaries that reject QUERY. It is the
			// same handler, so the two can never answer differently.
			b.L("mux.HandleFunc(%s, %s(s, svc))", gobuf.Quote(alias), handler)
		}
	}

	b.L("}")
	b.NL()
}

// handler emits one endpoint's HTTP handler.
func (e *emitter) handler(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	httpPkg := b.Import("net/http")

	if doc := endpointDoc(ep); doc != "" {
		b.Comment("handle" + ep.Impl.HandlerName + " serves " + ep.Pattern + ".\n\n" + doc)
	} else {
		b.Comment("handle" + ep.Impl.HandlerName + " serves " + ep.Pattern + ".")
	}

	b.L("func handle%s(s Server, svc %sService) %s.HandlerFunc {", ep.Impl.HandlerName, res.Name, httpPkg)
	b.L("return func(w %s.ResponseWriter, r *%s.Request) {", httpPkg, httpPkg)

	// Before prepare, because prepare is one of the things that answers: a
	// refused caller and a client too old to serve both write a response and
	// return, and a line that missed those would be a line that only ever
	// described the requests that worked.
	b.L("rec := %s.Wrap(w)", b.Import(runtimeModule+"/reqlog"))
	b.L("w = rec")
	b.NL()

	e.span(b, ep)

	prepare := "prepare"
	if ep.Public {
		prepare = "preparePublic"
	}
	b.L("ctx, claims, rc, ok := %s(s, w, r)", prepare)
	// After prepare, so rc is the populated one: a deferred call evaluates its
	// arguments where it is written, and rc a line earlier is still empty.
	b.L("defer logRequest(s, r, rec, rc)")
	b.L("if !ok { return }")
	b.NL()

	e.requirePermission(b, ep)

	e.decodePath(b, res, ep)
	e.decodeQuery(b, res, ep)

	// A file endpoint's body is bytes rather than JSON, so the two shapes that
	// move them replace the decode and the call rather than adding to them.
	switch {
	case ep.File != nil && ep.Method == "POST":
		e.uploadHandler(b, res, ep)
	case ep.File != nil && ep.Method == "GET":
		e.downloadHandler(b, res, ep)
	default:
		e.decodeBody(b, res, ep)
		e.call(b, res, ep)
	}

	b.L("}")
	b.L("}")
	b.NL()
}

// requirePermission emits the authorization check.
//
// Before the body is read, so that a caller who may not do this at all is not a
// caller whose malformed JSON gets diagnosed first — and so a refusal costs
// nothing but the claims lookup.
//
// The check is here rather than in the repository, which is where tenancy lives.
// That is a real difference in strength and it is deliberate: tenancy cannot be
// forgotten by anything, and this cannot be forgotten by any *request*. Code that
// reaches the repository directly — a hook, a task, a seed — is trusted, and a
// permission check there would fire on internal reads that have no business
// needing a grant.
//
// It answers "may this caller do this at all" and never "to this row". The row
// rule is a hook, and the service stub says so where somebody will read it.
func (e *emitter) requirePermission(b *gobuf.Buf, ep *ir.Endpoint) {
	if ep.Permission == "" {
		// Public, or a configuration that deliberately asked for none. Both are
		// the declared case; nothing is derived silently.
		return
	}

	tenPkg := b.Import(runtimeModule + "/tenancy")
	b.L("if err := %s.Require(claims, %s); err != nil {", tenPkg, gobuf.Quote(ep.Permission))
	b.L("fail(s, w, r, rc, err)")
	b.L("return")
	b.L("}")
	b.NL()
}

// decodePath reads the path wildcards into the endpoint's path struct.
func (e *emitter) decodePath(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	params := ep.Request.PathParams
	if len(params) == 0 {
		return
	}

	httpxPkg := b.Import(runtimeModule + "/httpx")

	b.L("var path %s", pathTypeName(res, ep))
	for _, f := range params {
		wildcard := pathWildcard(ep, f)
		switch f.Type {
		case ir.TypeUUID:
			b.L("%s, err := %s.PathUUID(r, %s)", e.localName(f), httpxPkg, gobuf.Quote(wildcard))
		case ir.TypeInt, ir.TypeInt64:
			b.L("%s, err := %s.PathInt(r, %s)", e.localName(f), httpxPkg, gobuf.Quote(wildcard))
		default:
			b.L("%s, err := %s.PathString(r, %s)", e.localName(f), httpxPkg, gobuf.Quote(wildcard))
		}
		b.L("if err != nil { fail(s, w, r, rc, err); return }")

		assign := e.localName(f)
		if f.Type == ir.TypeInt64 {
			assign = "int64(" + assign + ")"
		}
		b.L("path.%s = %s", f.Name, assign)
	}
	b.NL()
}

// decodeQuery reads the query string into the endpoint's query struct.
func (e *emitter) decodeQuery(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	params := ep.Request.QueryParams
	if len(params) == 0 {
		return
	}

	b.L("var query %s", queryTypeName(res, ep))
	for _, f := range params {
		if f.Wire == ir.ScopeParam {
			e.decodeScopeParam(b, ep, f)
			continue
		}
		e.decodeQueryParam(b, f)
	}
	b.NL()
}

// decodeScopeParam reads the reserved parameter and refuses a widening the caller
// does not hold.
//
// Both halves here rather than one in the handler and one in the service, because
// the answer to "may this caller see this much" has to be settled before anything
// runs a query. A service that trusted the field and a handler that forgot to
// check it would be one edit apart from a caller reading the whole tenant.
func (e *emitter) decodeScopeParam(b *gobuf.Buf, ep *ir.Endpoint, f ir.Field) {
	tenPkg := b.Import(runtimeModule + "/tenancy")

	b.L("scopeParam, err := %s.ParseScope(%s.QueryString(r, %s, %s))",
		tenPkg, b.Import(runtimeModule+"/httpx"), gobuf.Quote(f.Wire), gobuf.Quote(f.Default))
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.L("if err := %s.RequireScope(claims, scopeParam, %s); err != nil {",
		tenPkg, gobuf.Quote(ep.WidePermission))
	b.L("fail(s, w, r, rc, err)")
	b.L("return")
	b.L("}")
	b.L("query.%s = scopeParam", f.Name)
}

func (e *emitter) decodeQueryParam(b *gobuf.Buf, f ir.Field) {
	var (
		key      = gobuf.Quote(f.Wire)
		local    = e.localName(f)
		httpxPkg = b.Import(runtimeModule + "/httpx")
	)

	// An optional parameter keeps its zero value when the client omits it, and
	// the field is a pointer so the service can tell "not asked for" from
	// "asked for the zero value".
	assign := func(expr string) {
		if f.IsNullable() {
			b.L("if r.URL.Query().Has(%s) { v := %s; query.%s = &v }", key, expr, f.Name)
			return
		}
		b.L("query.%s = %s", f.Name, expr)
	}

	switch f.Type {
	case ir.TypeInt, ir.TypeInt64:
		b.L("%s, err := %s.QueryInt(r, %s, %s)", local, httpxPkg, key, intDefault(f))
		b.L("if err != nil { fail(s, w, r, rc, err); return }")
		if f.Type == ir.TypeInt64 {
			assign("int64(" + local + ")")
			return
		}
		assign(local)
	case ir.TypeBool:
		b.L("%s, err := %s.QueryBool(r, %s, %s)", local, httpxPkg, key, boolDefault(f))
		b.L("if err != nil { fail(s, w, r, rc, err); return }")
		assign(local)
	case ir.TypeUUID:
		b.L("%s, err := %s.QueryUUID(r, %s)", local, httpxPkg, key)
		b.L("if err != nil { fail(s, w, r, rc, err); return }")
		assign(local)
	case ir.TypeTimestamp, ir.TypeTime, ir.TypeDate:
		b.L("%s, err := %s.QueryTime(r, %s)", local, httpxPkg, key)
		b.L("if err != nil { fail(s, w, r, rc, err); return }")
		assign(local)
	default:
		// Everything else arrives as text. An enum is the common case, and its
		// underlying type is a string, so the conversion is a cast the service
		// then validates.
		expr := httpxPkg + ".QueryString(r, " + key + ", " + gobuf.Quote(f.Default) + ")"
		if goType := strings.TrimPrefix(f.GoType, "*"); goType != "string" && goType != "" {
			expr = goType + "(" + expr + ")"
		}
		assign(expr)
	}
}

// decodeBody reads and validates the request body.
func (e *emitter) decodeBody(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	if e.bodyTypeOf(b, res, ep) == "" {
		return
	}

	b.L("var body %s", e.bodyTypeOf(b, res, ep))
	if ep.Method == "GET" || ep.Method == "DELETE" {
		// A body on a method that has no defined body semantics would be
		// ignored by half the intermediaries on the way here.
		b.NL()
		return
	}

	// A create on a table with a file column honestly accepts two bodies, and
	// which one arrived is a question about this request rather than about the
	// endpoint. The JSON path below is untouched: a request without a multipart
	// content type never reaches the other branch.
	if len(ep.Request.FileParts) > 0 && ep.Name == ir.OpCreate {
		e.multipartCreate(b, res, ep)
		return
	}

	b.L("if err := decodeBody(r, &body); err != nil { fail(s, w, r, rc, err); return }")
	b.NL()
}

// call invokes the service and writes the response.
func (e *emitter) call(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	httpPkg := b.Import("net/http")

	arg := func(name string, present bool) string {
		if present {
			return name
		}
		return "struct{}{}"
	}

	b.L("req := NewRequest(claims, %s, %s, %s, rc)",
		arg("path", len(ep.Request.PathParams) > 0),
		arg("query", len(ep.Request.QueryParams) > 0),
		arg("body", e.bodyTypeOf(b, res, ep) != ""),
	)
	b.NL()

	status := successStatus(ep)

	// A create on a table with a file column carries whatever the form brought
	// with it. It is nil on the JSON path, which is what makes that path the one
	// it has always been.
	extra := ""
	if len(ep.Request.FileParts) > 0 && ep.Name == ir.OpCreate {
		extra = ", pending"
	}

	// A write is recorded against the Idempotency-Key it carried, if it carried
	// one. A read goes straight to the service, because there is nothing about a
	// read that a second one could duplicate.
	if genutil.IdempotentWrite(ep) {
		e.guardedCall(b, res, ep, extra)
		return
	}

	if successBodyObject(ep) == "" {
		b.L("if err := svc.%s(ctx, req%s); err != nil { fail(s, w, r, rc, err); return }",
			ep.Impl.ServiceMethod, extra)
		b.L("w.WriteHeader(%s)", statusExpr(httpPkg, status))
		return
	}

	b.L("out, err := svc.%s(ctx, req%s)", ep.Impl.ServiceMethod, extra)
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.L("writeJSON(w, %s, out)", statusExpr(httpPkg, status))
}

// bodyTypeOf names the Go type of an endpoint's request body, or empty when it
// takes none.
//
// Create and Update decode straight into the model's input types: what a client
// sends and what the repository takes are the same fields, so there is no wire
// struct in between to copy through.
func (e *emitter) bodyTypeOf(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) string {
	if ep.Request.BodyObject != "" {
		return ep.Request.BodyObject
	}
	if name := genutil.ModelInputName(ep); name != "" {
		return e.model(b) + "." + res.Name + name + "Input"
	}
	if len(ep.Request.BodyParams) == 0 {
		return ""
	}
	return res.Name + ep.Name + "Body"
}

// hasPublicEndpoint reports whether anything in the document answers without a
// credential, so the shared helper for it is emitted only where it is called.
func (e *emitter) hasPublicEndpoint() bool {
	for i := range e.doc.API.Resources {
		for j := range e.doc.API.Resources[i].Endpoints {
			if e.doc.API.Resources[i].Endpoints[j].Public {
				return true
			}
		}
	}
	return false
}

func pathTypeName(res *ir.Resource, ep *ir.Endpoint) string  { return res.Name + ep.Name + "Path" }
func queryTypeName(res *ir.Resource, ep *ir.Endpoint) string { return res.Name + ep.Name + "Query" }

// successStatus is the status a successful call answers with.
func successStatus(ep *ir.Endpoint) int {
	for _, r := range ep.Responses {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			return r.StatusCode
		}
	}
	return 200
}

// statusExpr prefers the named constant, because "http.StatusNoContent" says
// what it means where "204" needs looking up.
func statusExpr(httpPkg string, status int) string {
	names := map[int]string{
		200: "StatusOK", 201: "StatusCreated", 202: "StatusAccepted", 204: "StatusNoContent",
	}
	if name, ok := names[status]; ok {
		return httpPkg + "." + name
	}
	return strconv.Itoa(status)
}

func successBodyObject(ep *ir.Endpoint) string {
	for _, r := range ep.Responses {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			return r.BodyObject
		}
	}
	return ""
}

func endpointDoc(ep *ir.Endpoint) string {
	doc := ep.Summary
	if ep.Description != "" {
		if doc != "" {
			doc += "\n\n"
		}
		doc += ep.Description
	}
	return doc
}

// pathWildcard is the wildcard in the route that carries this parameter.
//
// A configured endpoint writes its own path, and may spell a parameter either
// way; the compiler accepts both, so the handler has to agree with whichever
// the author chose.
func pathWildcard(ep *ir.Endpoint, f ir.Field) string {
	if strings.Contains(ep.Path, "{"+f.Wire+"}") {
		return f.Wire
	}
	return strings.ToLower(f.Name)
}

// localName is the variable a decoded parameter lands in. It is lower-cased and
// suffixed so that a parameter named Query or Path cannot shadow the structs the
// handler is filling in.
func (e *emitter) localName(f ir.Field) string { return e.namer.GoUnexported(f.Name) + "Param" }

func intDefault(f ir.Field) string {
	if f.Default == "" {
		return "0"
	}
	if _, err := strconv.Atoi(f.Default); err != nil {
		return "0"
	}
	return f.Default
}

func boolDefault(f ir.Field) string {
	if v, err := strconv.ParseBool(f.Default); err == nil && v {
		return "true"
	}
	return "false"
}
