package openapigen

import (
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"

	"github.com/simonjanss/rig/pkg/ir"
	"github.com/simonjanss/rig/runtime/electric"
)

// electricOperation is one live-sync route and the operation on it.
type electricOperation struct {
	route route
	op    *v3.Operation
}

// electricRoutes describes a resource's live-sync shapes.
//
// These live on ir.Resource.Electric rather than among its endpoints, so they
// need a walk of their own — and a resource can be unexposed and still stream,
// which is why they are not gated on having endpoints. Leaving them out would
// make the document quietly incomplete: the routes exist, the same mux serves
// them, and nothing else rig emits would tell a reader they are there.
//
// What the document does not do is describe the response body. It is the sync
// service's protocol, forwarded, and rig does not own it; saying so is more
// honest than inventing a schema for it.
func (e *emitter) electricRoutes(res *ir.Resource) []electricOperation {
	if !e.syncs(res) {
		return nil
	}
	el := res.Electric

	out := []electricOperation{{
		route: route{method: "GET", path: el.Path},
		op: e.shapeOperation(res, el, "stream"+res.Plural,
			"Subscribe to the live "+res.Name+" shape.",
			"Every "+res.Name+" the caller may read, kept up to date."),
	}}

	if res.Storage.IsSoftDeletable() {
		out = append(out, electricOperation{
			route: route{method: "GET", path: el.DeletedPath()},
			op: e.shapeOperation(res, el, "streamDeleted"+res.Plural,
				"Subscribe to retired "+res.Plural+".",
				"The rows a delete stamped rather than removed, kept up to date."),
		})
	}
	if res.Storage.IsSnapshotable() {
		op := e.shapeOperation(res, el, "stream"+res.Name+"Versions",
			"Subscribe to one "+res.Name+"'s history.",
			"The previous versions of a single "+res.Name+", kept up to date.")
		// The route carries {id} and the IR declares no parameter for it, so
		// the parameter is synthesised here rather than left undeclared — a
		// path template naming something the operation does not is a document
		// that fails its own validation.
		op.Parameters = append([]*v3.Parameter{{
			Name:        "id",
			In:          "path",
			Description: "Identifier of the " + res.Name + " whose history to stream.",
			Required:    boolPtr(true),
			Schema: base.CreateSchemaProxy(&base.Schema{
				Type: []string{"string"}, Format: "uuid",
			}),
		}}, op.Parameters...)

		out = append(out, electricOperation{
			route: route{method: "GET", path: el.VersionsPath()},
			op:    op,
		})
	}
	return out
}

// shapeOperation is the operation every shape route shares.
func (e *emitter) shapeOperation(
	res *ir.Resource, el *ir.ElectricEndpoint, id, summary, what string,
) *v3.Operation {
	desc := what + "\n\n" +
		"A shape is a filtered view of one table that a client subscribes to. This route " +
		"is a proxy in front of the sync service: which rows exist in the shape — the " +
		"tenant, the lifecycle predicate, and the application's own scoping — is decided " +
		"by the server and cannot be influenced from here. Where the client is in the " +
		"stream is forwarded untouched.\n\n" +
		"With `live=true` the request is a long poll: it does not answer until something " +
		"changes, so a client timeout set for an ordinary request will cut it short.\n\n" +
		"The response body is the sync protocol's own and is not described here. The " +
		"`electric-*` response headers carry the cursor: send `handle` and `offset` back " +
		"on the next request, or the subscription starts over."

	switch el.Auth {
	case ir.ElectricAuthAdmin:
		desc += "\n\nRequires an administrative session."
	case ir.ElectricAuthTenant:
		desc += "\n\nRequires a session scoped to a tenant, and the shape is filtered to it."
	}

	op := &v3.Operation{
		Tags:        []string{res.Plural},
		Summary:     summary,
		Description: desc,
		OperationId: id,
		Responses:   e.shapeResponses(),
	}

	// The protocol's own parameters, read from the proxy that forwards them
	// rather than listed again here.
	for _, name := range electric.ProtocolParams {
		op.Parameters = append(op.Parameters, &v3.Parameter{
			Name:        name,
			In:          "query",
			Description: protocolParamDoc[name],
			Required:    boolPtr(false),
			Schema:      base.CreateSchemaProxy(protocolParamSchema(name)),
		})
	}

	for _, p := range el.Params {
		f := ir.Field{
			Name: p.Field, Wire: p.Name, Type: p.Type,
			TypeKind: ir.TypeKindPrimitive, Description: p.Description,
		}
		op.Parameters = append(op.Parameters, &v3.Parameter{
			Name:        p.Name,
			In:          "query",
			Description: p.Description,
			// The one place in the document where optionality is declared
			// rather than derived.
			Required: boolPtr(!p.Optional),
			Schema:   undescribed(e.fieldSchema(f)),
		})
	}

	op.Parameters = append(op.Parameters,
		&v3.Parameter{Reference: "#/components/parameters/ApiRevision"})
	return op
}

// protocolParamDoc explains each of the sync protocol's parameters.
var protocolParamDoc = map[string]string{
	"offset":  "Where in the stream to resume, from the previous response's electric-offset.",
	"handle":  "The shape handle from a previous response. Omit it to start a new subscription.",
	"live":    "Long-poll until something changes, rather than answering with what is there now.",
	"cursor":  "The sync service's own cache-busting cursor, echoed back from the last response.",
	"replica": "Whether updates carry the whole row or only the columns that changed.",
}

func protocolParamSchema(name string) *base.Schema {
	if name == "live" {
		return &base.Schema{Type: []string{"boolean"}}
	}
	return &base.Schema{Type: []string{"string"}}
}

// shapeResponses is what a shape route answers with.
//
// The failure here is not rig's shared Error body. A proxy that cannot reach
// the sync service answers with plain text, so pointing at the Error schema
// would describe a JSON body that never arrives.
func (e *emitter) shapeResponses() *v3.Responses {
	ok := &v3.Response{
		Description: "A chunk of the shape, in the sync protocol's own format.",
		Content:     orderedmap.New[string, *v3.MediaType](),
		Headers:     orderedmap.New[string, *v3.Header](),
	}
	ok.Content.Set(ir.MediaJSON, &v3.MediaType{})
	for _, h := range electricHeaders {
		ok.Headers.Set(h.name, &v3.Header{
			Description: h.doc,
			Schema:      base.CreateSchemaProxy(&base.Schema{Type: []string{"string"}}),
		})
	}

	bad := &v3.Response{
		Description: "The sync service could not be reached.",
		Content:     orderedmap.New[string, *v3.MediaType](),
	}
	bad.Content.Set("text/plain", &v3.MediaType{})

	out := &v3.Responses{Codes: orderedmap.New[string, *v3.Response]()}
	out.Codes.Set("200", ok)
	out.Codes.Set("401", &v3.Response{
		Reference: "#/components/responses/" + e.errorResponseName(401)})
	out.Codes.Set("403", &v3.Response{
		Reference: "#/components/responses/" + e.errorResponseName(403)})
	out.Codes.Set("502", bad)

	if e.usedStatuses == nil {
		e.usedStatuses = map[int]bool{}
	}
	e.usedStatuses[401] = true
	e.usedStatuses[403] = true
	return out
}

// electricHeaders are the cursor the sync service returns.
//
// Named on a best-effort basis: rig forwards every electric- header the service
// sends rather than producing this list itself, so these are the ones a client
// needs and not a guarantee of the whole set.
var electricHeaders = []struct{ name, doc string }{
	{"electric-handle", "The shape handle. Send it back to continue this subscription."},
	{"electric-offset", "Where this chunk ended. Send it back to resume from here."},
	{"electric-schema", "The shape's column types."},
	{"electric-cursor", "The cursor to echo on the next request."},
	{"electric-up-to-date", "Present when the client has caught up with the shape."},
}
