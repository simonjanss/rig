package openapigen

import (
	"slices"
	"strconv"
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"
	"go.yaml.in/yaml/v4"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/pkg/ir"
)

// route is one method-and-path an operation answers on.
type route struct {
	method string
	path   string
	// alias says this route is not the endpoint's primary one, which decides
	// how the operation is named and what its description has to explain.
	alias bool
}

// routesOf is every route an endpoint can be described on in 3.1.
//
// The QUERY method is the whole difficulty. A search is a read that carries a
// body, so QUERY is what it should be and what rig serves — but OpenAPI 3.1 has
// no query field on a path item, and emitting one produces a document that
// fails the 3.1 meta-schema. The compiler already mounts a POST alias beside it
// for intermediaries that reject unfamiliar methods, so that alias is what gets
// documented, and the operation says so in its description.
//
// An endpoint left with no routes at all is a real case, not a defensive one: a
// project setting search_method: query gets the QUERY route and no alias, and
// there is then nothing 3.1 can say about it. It is skipped, and the resource's
// tag says why — a silently missing operation is the failure this generator
// exists to prevent.
func routesOf(ep *ir.Endpoint) []route {
	var out []route
	if ep.Method != methodQuery {
		out = append(out, route{method: ep.Method, path: genutil.RoutePath(ep.Pattern)})
	}
	for _, a := range ep.AliasPatterns {
		method, path, ok := strings.Cut(a, " ")
		if !ok {
			continue
		}
		out = append(out, route{method: method, path: path, alias: true})
	}
	return out
}

// methodQuery is the read-with-a-body method rig serves a search on.
const methodQuery = "QUERY"

// operationID names a route.
//
// The primary route keeps the endpoint's own OperationID, so the identifier in
// this document is the one the Go client's method was named from. An alias
// standing in for a primary that could not be described — the QUERY case —
// inherits it too, because from a reader's point of view it is the operation.
//
// An alias beside a primary that was described needs a name of its own, since
// OpenAPI requires operationId to be unique across the document. Nothing hits
// that branch today; it is here so the first endpoint to gain a second route
// produces a valid document rather than a duplicate identifier.
func operationID(ep *ir.Endpoint, r route, primaryEmitted bool) string {
	if !r.alias || !primaryEmitted {
		return ep.OperationID
	}
	return ep.OperationID + "Via" + strings.ToUpper(r.method[:1]) + strings.ToLower(r.method[1:])
}

// paths builds every path item in the document.
//
// Path keys are sorted once, before insertion: the rendered model is ordered
// maps all the way down, so insertion order is output order, and a generator
// has to be a pure function of its input. Lexicographic order also happens to
// read like the routing table — the collection, then its reserved segments,
// then the row and what hangs off it.
func (e *emitter) paths() *v3.Paths {
	items := map[string]*v3.PathItem{}

	add := func(r route, op *v3.Operation) {
		item, ok := items[r.path]
		if !ok {
			item = &v3.PathItem{}
			items[r.path] = item
		}
		switch r.method {
		case "GET":
			item.Get = op
		case "POST":
			item.Post = op
		case "PUT":
			item.Put = op
		case "PATCH":
			item.Patch = op
		case "DELETE":
			item.Delete = op
		case "HEAD":
			item.Head = op
		case "OPTIONS":
			item.Options = op
		}
		// Query is deliberately unreachable. The field exists on the model and
		// renders a key the 3.1 meta-schema rejects, so routesOf never yields a
		// QUERY route and this switch has no case for one.
	}

	for _, res := range e.exposed() {
		for i := range res.Endpoints {
			ep := &res.Endpoints[i]
			routes := routesOf(ep)
			primaryEmitted := false
			for _, r := range routes {
				add(r, e.operation(res, ep, r, primaryEmitted))
				if !r.alias {
					primaryEmitted = true
				}
			}
		}
		for _, r := range e.electricRoutes(res) {
			add(r.route, r.op)
		}
	}

	for _, r := range e.specRoutes() {
		add(r.route, r.op)
	}

	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := &v3.Paths{PathItems: orderedmap.New[string, *v3.PathItem]()}
	for _, k := range keys {
		out.PathItems.Set(k, items[k])
	}
	return out
}

// operation renders one endpoint on one of its routes.
func (e *emitter) operation(
	res *ir.Resource, ep *ir.Endpoint, r route, primaryEmitted bool,
) *v3.Operation {
	op := &v3.Operation{
		Tags:        []string{res.Plural},
		Summary:     summaryOf(ep),
		Description: e.operationDescription(ep, r),
		OperationId: operationID(ep, r, primaryEmitted),
		Parameters:  e.parameters(ep),
		RequestBody: e.requestBody(res, ep),
		Responses:   e.responses(ep),
	}

	// Only against a scheme this document declares. A project with no auth
	// block has none, and an operation whose security names one that is not in
	// components describes a credential a reader cannot obtain — and fails
	// validation saying so.
	if ep.Public && e.doc.API.Auth != nil {
		op.Security = optionalCredential()
	}
	if ep.Permission != "" {
		// OpenAPI scope arrays mean something only for oauth2 and
		// openIdConnect; on an http bearer scheme they must be empty, so the
		// key has nowhere to go but an extension. The description says it too,
		// for the reader who will never see an x- key.
		setExtension(&op.Extensions, "x-rig-permission", scalarNode("!!str", ep.Permission))
	}
	if ep.WidePermission != "" {
		setExtension(&op.Extensions, "x-rig-wide-permission",
			scalarNode("!!str", ep.WidePermission))
	}
	return op
}

// summaryOf is the one-line heading for an endpoint.
func summaryOf(ep *ir.Endpoint) string {
	if ep.Title != "" {
		return ep.Title
	}
	return ep.Summary
}

// operationDescription is the document's own words plus what only this output
// has to explain.
func (e *emitter) operationDescription(ep *ir.Endpoint, r route) string {
	// The summary stands in when the document has nothing longer to say. It
	// repeats a line the reader has already seen, which is a small cost against
	// an operation that renders with no prose at all in a documentation viewer.
	var parts []string
	switch {
	case ep.Description != "":
		parts = append(parts, ep.Description)
		if ep.Summary != "" && ep.Title != "" && !strings.Contains(ep.Description, ep.Summary) {
			parts = append(parts, ep.Summary)
		}
	case summaryOf(ep) != "":
		parts = append(parts, summaryOf(ep))
	}

	if r.alias && ep.Method == methodQuery {
		parts = append(parts, "The primary form of this operation is `"+
			methodQuery+" "+genutil.RoutePath(ep.Pattern)+"` — a read that carries a body, and so "+
			"safe and idempotent in a way POST is not. OpenAPI 3.1 cannot describe an "+
			"operation on the QUERY method, so it is documented here as the POST alias that "+
			"exists for intermediaries which reject unfamiliar methods. One handler serves "+
			"both routes and they answer identically.")
	}

	if ep.Permission != "" {
		parts = append(parts, "Requires the `"+ep.Permission+"` permission.")
	}
	if ep.Public {
		parts = append(parts, "Answers without a credential. A caller who presents one is "+
			"still identified by it, and may be shown more than a stranger is.")
	}
	return strings.Join(parts, "\n\n")
}

// parameters renders the path and query parameters an endpoint declares, then
// the cross-cutting headers it does not.
func (e *emitter) parameters(ep *ir.Endpoint) []*v3.Parameter {
	var out []*v3.Parameter

	for _, f := range ep.Request.PathParams {
		out = append(out, &v3.Parameter{
			Name:        f.Wire,
			In:          "path",
			Description: fieldDescription(f),
			Required:    boolPtr(true),
			Schema:      undescribed(e.fieldSchema(f)),
		})
	}

	for _, f := range ep.Request.QueryParams {
		p := &v3.Parameter{
			Name:        f.Wire,
			In:          "query",
			Description: fieldDescription(f),
			Required:    boolPtr(false),
			Schema:      e.querySchema(f),
		}
		if f.IsArray() {
			p.Style = "form"
			p.Explode = boolPtr(true)
		}
		out = append(out, p)
	}

	for _, f := range ep.Request.Headers {
		out = append(out, &v3.Parameter{
			Name:        f.Wire,
			In:          "header",
			Description: fieldDescription(f),
			Required:    boolPtr(!f.IsNullable()),
			Schema:      undescribed(e.fieldSchema(f)),
		})
	}

	out = append(out, &v3.Parameter{Reference: "#/components/parameters/ApiRevision"})
	if genutil.IdempotentWrite(ep) {
		e.usedIdempotencyKey = true
		out = append(out, &v3.Parameter{Reference: "#/components/parameters/IdempotencyKey"})
	}
	return out
}

// querySchema is a query parameter's schema, with its default.
//
// The scope parameter is the one case the generic mapping cannot handle. Its Go
// type is tenancy.Scope rather than an IR enum, so a field-driven schema would
// say `string` and leave the reader to find the two accepted values in the
// prose. The values come from the compiler's own constants, so a document
// cannot offer one the handler refuses.
func (e *emitter) querySchema(f ir.Field) *base.SchemaProxy {
	proxy := e.fieldSchema(f)
	s := proxy.Schema()
	if s == nil {
		return proxy
	}
	if f.Wire == ir.ScopeParam && f.TypeKind == ir.TypeKindPrimitive {
		s.Enum = []*yaml.Node{
			scalarNode("!!str", compile.AccessScopeOwn),
			scalarNode("!!str", compile.AccessScopeAll),
		}
	}
	if d := requestDefault(f, s.Type); d != nil {
		s.Default = d
	}
	return undescribed(proxy)
}

// undescribed strips a schema's description.
//
// A parameter carries its own, and the schema underneath it is the same field's
// — so leaving both renders the sentence twice, once directly under the other.
// The parameter's is the one that belongs, because a reader looking at a
// parameter is asking what the parameter means.
//
// A $ref proxy is left alone: there is no inline schema to strip, and the
// description on the far end belongs to the shared shape rather than to this
// use of it.
func undescribed(proxy *base.SchemaProxy) *base.SchemaProxy {
	if s := proxy.Schema(); s != nil && !proxy.IsReference() {
		s.Description = ""
	}
	return proxy
}

// responses merges what an endpoint answers with and what it can fail with.
//
// An endpoint's own Responses win over its Errors: a custom endpoint may
// describe a 409 in its own words, and the shared one would say less.
func (e *emitter) responses(ep *ir.Endpoint) *v3.Responses {
	type entry struct {
		status int
		resp   *v3.Response
	}
	var all []entry
	seen := map[int]bool{}

	for _, r := range ep.Responses {
		seen[r.StatusCode] = true
		all = append(all, entry{r.StatusCode, e.response(ep, r)})
	}
	for _, status := range ep.Errors {
		if seen[status] {
			continue
		}
		seen[status] = true
		if e.usedStatuses == nil {
			e.usedStatuses = map[int]bool{}
		}
		e.usedStatuses[status] = true
		all = append(all, entry{status, &v3.Response{
			Reference: "#/components/responses/" + e.errorResponseName(status),
		}})
	}

	slices.SortFunc(all, func(a, b entry) int { return a.status - b.status })

	out := &v3.Responses{Codes: orderedmap.New[string, *v3.Response]()}
	for _, en := range all {
		out.Codes.Set(strconv.Itoa(en.status), en.resp)
	}
	return out
}

// response renders one outcome an endpoint declares.
func (e *emitter) response(ep *ir.Endpoint, r ir.EndpointResponse) *v3.Response {
	desc := r.Description
	if desc == "" {
		// Every response must carry one, and an empty string is not a
		// description — it is a lint finding and a worse document.
		desc = defaultResponseDescription(r.StatusCode)
	}

	out := &v3.Response{Description: desc}

	if len(r.ContentTypes) > 0 {
		content := orderedmap.New[string, *v3.MediaType]()
		for _, ct := range r.ContentTypes {
			switch {
			case ct == ir.MediaJSON && r.BodyObject != "":
				content.Set(ct, &v3.MediaType{
					Schema: base.CreateSchemaProxyRef(schemaRef(r.BodyObject)),
				})
			case ct == ir.MediaJSON && len(r.BodyFields) > 0:
				content.Set(ct, &v3.MediaType{Schema: base.CreateSchemaProxy(
					e.fieldsSchema(r.BodyFields, desc))})
			default:
				// Opaque bytes. An empty media type is how 3.1 says "whatever
				// this turns out to be"; a schema would be inventing one.
				content.Set(ct, &v3.MediaType{})
			}
		}
		out.Content = content
	}

	if isSuccess(r.StatusCode) {
		headers := orderedmap.New[string, *v3.Header]()
		if e.doc.API.Revision != "" {
			headers.Set(genutil.RevisionHeader(e.doc),
				&v3.Header{Reference: "#/components/headers/ApiRevision"})
		}
		if genutil.IdempotentWrite(ep) {
			e.usedIdempotencyReplayed = true
			headers.Set("Idempotency-Replayed",
				&v3.Header{Reference: "#/components/headers/IdempotencyReplayed"})
		}
		for _, f := range r.Headers {
			headers.Set(f.Wire, &v3.Header{
				Description: fieldDescription(f),
				Schema:      e.fieldSchema(f),
			})
		}
		if headers.Len() > 0 {
			out.Headers = headers
		}
	}
	return out
}

func isSuccess(status int) bool { return status >= 200 && status < 300 }

// defaultResponseDescription is what to say about a status the document left
// undescribed.
func defaultResponseDescription(status int) string {
	switch status {
	case 204:
		return "Done. No content."
	case 304:
		return "Not modified."
	}
	return "Success."
}
