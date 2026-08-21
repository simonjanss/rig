package goclient

import (
	"fmt"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// methodFile emits a resource's client: one method per endpoint.
func (e *emitter) methodFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)
	rig := e.client(b)

	b.Comment(res.Name + "Client calls the " + res.Name + " endpoints. It is " +
		"reached as Client." + res.Plural + " rather than built directly.")
	b.L("type %sClient struct { rt *%s.Runtime }", res.Name, rig)
	b.NL()

	if len(res.Files) > 0 {
		e.createFilesType(b, res, rig)
	}

	for i := range res.Endpoints {
		e.method(b, res, &res.Endpoints[i], rig)
	}

	// Beside Create rather than instead of it, so no existing caller breaks the
	// day somebody adds a file column to a table they already had.
	if ep := res.Endpoint(ir.OpCreate); ep != nil && len(ep.Request.FileParts) > 0 {
		e.createWithFilesMethod(b, res, ep, rig)
	}

	if ep := res.Endpoint(ir.OpList); ep != nil {
		e.allMethod(b, res, ep, rig)
	}

	return e.artifact(fileName(res, "_client"), b)
}

// method emits one endpoint.
func (e *emitter) method(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, rig string) {
	if e.fileMethod(b, res, ep, rig) {
		return
	}
	sig := e.signature(b, res, ep, rig)

	e.methodDoc(b, res, ep)
	b.L("func (c *%sClient) %s(%s) %s {", res.Name, ep.Impl.ServiceMethod, sig.params, sig.results)

	e.buildQuery(b, ep, rig)
	e.buildOp(b, ep, sig, rig)

	if sig.returns == "" {
		b.L("return %s.DoNoContent(ctx, c.rt, op, opts...)", rig)
	} else {
		b.L("return %s.Do[%s](ctx, c.rt, op, opts...)", rig, sig.returns)
	}
	b.L("}")
	b.NL()
}

// signature is what one method takes and gives back.
type signature struct {
	params  string
	results string
	// returns is the type a successful call decodes into, or empty when the
	// endpoint answers with no body.
	returns string
	// body is the expression to send, or empty for a request with no body.
	body string
	// path is the Go expression that builds the route.
	path string
	// query is the name of the query struct parameter, or empty.
	query string
}

// signature works out the shape of a method from the endpoint alone.
//
// Path parameters are positional and in route order; the query is one struct;
// the body is whichever of the shared inputs, the named object, or the
// endpoint's own shape applies. Options come last, always, so that a header a
// deployment needs is never a reason to regenerate.
func (e *emitter) signature(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, rig string) signature {
	var sig signature

	params := []string{"ctx " + b.Import("context") + ".Context"}
	for _, f := range ep.Request.PathParams {
		params = append(params, argName(f)+" "+e.goType(b, f))
	}

	switch {
	case ep.Name == ir.OpSearch:
		if filter, ok := searchFilterField(ep); ok {
			params = append(params, "filter "+e.goType(b, filter))
			sig.body = bodyTypeName(res, ep) + "{" + filter.Name + ": filter}"
		}
	case inputTypeName(res, ep) != "":
		params = append(params, "in "+inputTypeName(res, ep))
		sig.body = "in"
	case ep.Request.BodyObject != "":
		params = append(params, "in "+ep.Request.BodyObject)
		sig.body = "in"
	case len(ep.Request.BodyParams) > 0:
		params = append(params, "in "+bodyTypeName(res, ep))
		sig.body = "in"
	}

	if len(ep.Request.QueryParams) > 0 {
		sig.query = "q"
		params = append(params, "q "+queryTypeName(res, ep))
	}
	params = append(params, "opts ..."+rig+".CallOption")

	sig.params = strings.Join(params, ", ")
	sig.returns = e.successType(ep)
	sig.results = "error"
	if sig.returns != "" {
		sig.results = "(*" + sig.returns + ", error)"
	}
	sig.path = e.pathExpr(b, ep)
	return sig
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

// pathExpr builds the route, substituting the path parameters.
//
// Every value is escaped: an identifier that arrived from somewhere else can be
// anything at all, and a slash in one would otherwise address a different route
// entirely.
func (e *emitter) pathExpr(b *gobuf.Buf, ep *ir.Endpoint) string {
	path := routePath(ep.Pattern)
	path = strings.TrimPrefix(path, e.doc.API.BasePath)

	if len(ep.Request.PathParams) == 0 {
		return gobuf.Quote(path)
	}

	rig := e.client(b)
	expr := gobuf.Quote(path)
	for _, f := range ep.Request.PathParams {
		expr = fmt.Sprintf("%s.Replace(%s, %s, %s.PathValue(%s), 1)",
			b.Import("strings"), expr, gobuf.Quote("{"+f.Wire+"}"),
			rig, stringOf(b, f, argName(f)))
	}
	return expr
}

// stringOf is how a path parameter becomes text.
//
// A uuid and an enum both have a String method that produces exactly what the
// route expects; a timestamp has one that does not, so it is formatted the way
// the server parses it.
func stringOf(b *gobuf.Buf, f ir.Field, arg string) string {
	switch f.Type {
	case ir.TypeString:
		return "string(" + arg + ")"
	case ir.TypeInt:
		return b.Import("strconv") + ".Itoa(" + arg + ")"
	case ir.TypeInt64:
		return b.Import("strconv") + ".FormatInt(" + arg + ", 10)"
	case ir.TypeTime, ir.TypeTimestamp, ir.TypeDate:
		t := b.Import("time")
		return arg + ".Format(" + t + ".RFC3339Nano)"
	default:
		return arg + ".String()"
	}
}

// buildQuery emits the query-string assembly, when there is one.
func (e *emitter) buildQuery(b *gobuf.Buf, ep *ir.Endpoint, rig string) {
	if len(ep.Request.QueryParams) == 0 {
		return
	}

	b.L("query := %s.Values{}", b.Import("net/url"))
	for _, f := range ep.Request.QueryParams {
		b.L("%s.%s(query, %s, q.%s)", rig, setterFor(f), gobuf.Quote(f.Wire), f.Name)
	}
	b.NL()
}

// setterFor is the query writer a parameter's type calls for. They all take a
// pointer and write nothing when it is nil, which is what keeps an absent
// parameter absent.
func setterFor(f ir.Field) string {
	if f.IsArray() {
		return "SetStrings"
	}
	switch f.Type {
	case ir.TypeBool:
		return "SetBool"
	case ir.TypeInt:
		return "SetInt"
	case ir.TypeInt64:
		return "SetInt64"
	case ir.TypeFloat64:
		return "SetFloat64"
	case ir.TypeUUID:
		return "SetUUID"
	case ir.TypeTime, ir.TypeTimestamp, ir.TypeDate:
		return "SetTime"
	default:
		return "SetString"
	}
}

// buildOp emits the operation the transport is handed.
func (e *emitter) buildOp(b *gobuf.Buf, ep *ir.Endpoint, sig signature, rig string) {
	b.L("op := %s.Op{", rig)
	b.L("Name: %s,", gobuf.Quote(ep.OperationID))
	b.L("Method: %s,", methodExpr(b, ep, rig))
	b.L("Path: %s,", sig.path)
	if sig.query != "" {
		b.L("Query: query,")
	}
	if sig.body != "" {
		b.L("Body: %s,", sig.body)
	}
	if fallback := e.fallbackPath(ep); fallback != "" {
		b.L("Fallback: %s,", gobuf.Quote(fallback))
	}
	b.L("}")
}

// methodExpr is the HTTP method, named rather than spelled where net/http has a
// constant for it.
func methodExpr(b *gobuf.Buf, ep *ir.Endpoint, rig string) string {
	if ep.Method == "QUERY" {
		return rig + ".MethodQuery"
	}

	http := b.Import("net/http")
	switch ep.Method {
	case "GET":
		return http + ".MethodGet"
	case "POST":
		return http + ".MethodPost"
	case "PATCH":
		return http + ".MethodPatch"
	case "PUT":
		return http + ".MethodPut"
	case "DELETE":
		return http + ".MethodDelete"
	}
	return gobuf.Quote(ep.Method)
}

// fallbackPath is the alias to POST to when QUERY is refused, or empty when the
// project mounts none.
func (e *emitter) fallbackPath(ep *ir.Endpoint) string {
	if ep.Method != "QUERY" {
		return ""
	}
	for _, alias := range ep.AliasPatterns {
		method, path, _ := strings.Cut(alias, " ")
		if method != "POST" {
			continue
		}
		return strings.TrimPrefix(path, e.doc.API.BasePath)
	}
	return ""
}

// methodDoc writes what the document says about an endpoint, and then the two
// things only a Go caller needs: the route, and what happens to a QUERY nobody
// recognizes.
func (e *emitter) methodDoc(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	doc := genutil.Describe(ep.Description, "")
	if ep.Summary != "" && !strings.Contains(doc, ep.Summary) {
		doc = strings.TrimSpace(ep.Summary + "\n\n" + doc)
	}
	if doc == "" {
		doc = ep.Impl.ServiceMethod + " calls " + res.Name + "." + ep.Name + "."
	}

	route := ep.Pattern
	if fallback := e.fallbackPath(ep); fallback != "" {
		route += ", falling back to POST " + e.doc.API.BasePath + fallback +
			" when something between here and the server refuses the method. " +
			"The fallback is remembered, so it is tried once."
	}
	doc += "\n\n" + route + "\n\nOperation " + ep.OperationID + "."

	// Named here rather than only where it is declared, because a caller reading
	// this is holding the error and not looking for the function that reads it.
	if name := errorFuncName(res, ep); name != "" {
		doc += "\n\nA refusal is read back with [" + name + "], whose Fields say " +
			"what was wrong with each member of the body."
	}

	b.Comment(doc)
}

// allMethod emits the iterator over every page of a list.
func (e *emitter) allMethod(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, rig string) {
	page := e.listResponse(res)
	if page == nil {
		return
	}
	item := listItemType(page)
	if item == "" || !hasQueryParam(ep, "offset") {
		return
	}

	query := queryTypeName(res, ep)

	b.Comment("All reads every " + res.Name + " the query matches, a page at a " +
		"time.\n\n" +
		"The query's own limit is the page size, and its offset is where the " +
		"walk starts. Iteration stops at the first failure, which arrives as the " +
		"second value of the last pair — so a loop that ignores it is a loop that " +
		"silently stops early.")
	b.L("func (c *%sClient) All(ctx %s.Context, q %s, opts ...%s.CallOption) %s.Seq2[%s, error] {",
		res.Name, b.Import("context"), query, rig, b.Import("iter"), item)
	b.L("start := 0")
	b.L("if q.Offset != nil { start = *q.Offset }")
	b.NL()
	b.L("return %s.Paginate(ctx, start, func(ctx %s.Context, offset int) (%s.Page[%s], error) {",
		rig, b.Import("context"), rig, item)
	b.L("q := q")
	b.L("q.Offset = &offset")
	b.NL()
	b.L("res, err := c.%s(ctx, q, opts...)", ep.Impl.ServiceMethod)
	b.L("if err != nil { return %s.Page[%s]{}, err }", rig, item)
	b.L("if res == nil { return %s.Page[%s]{}, nil }", rig, item)
	b.NL()
	b.L("return %s.Page[%s]{", rig, item)
	b.L("Items: res.Data,")
	b.L("Total: res.Pagination.Total,")
	b.L("Offset: res.Pagination.Offset,")
	b.L("}, nil")
	b.L("})")
	b.L("}")
	b.NL()
}

// hasQueryParam reports whether an endpoint takes a parameter by wire name.
func hasQueryParam(ep *ir.Endpoint, wire string) bool {
	for _, f := range ep.Request.QueryParams {
		if f.Wire == wire {
			return true
		}
	}
	return false
}

// listItemType is what a page holds, taken from the page shape rather than
// assumed from the resource's name.
func listItemType(page *ir.Object) string {
	for _, f := range page.Fields {
		if f.Wire == "data" {
			return strings.TrimPrefix(f.GoType, "[]")
		}
	}
	return ""
}

// routePath is the path half of a net/http pattern.
func routePath(pattern string) string {
	_, path, found := strings.Cut(pattern, " ")
	if !found {
		return pattern
	}
	return path
}

// argName is what a path parameter is called in a Go signature: the field's own
// name, unexported, since a parameter is not.
func argName(f ir.Field) string {
	return naming.New(naming.Config{}).GoUnexported(f.Name)
}
