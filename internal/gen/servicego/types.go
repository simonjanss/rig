package servicego

import (
	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// typesFile emits one resource's wire types and the conversions to and from the
// persistence layer.
func (e *emitter) typesFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.listResponseType(b, res)
	e.endpointTypes(b, res)

	return artifact(naming.Snake(res.Name)+".gen.go", b, gen.Overwrite)
}

func (e *emitter) listResponseType(b *gobuf.Buf, res *ir.Resource) {
	name := res.Name + "ListResponse"
	if e.object(name) == nil {
		return
	}

	obj := e.object(name)

	b.Comment(obj.Description)
	b.L("type %s struct {", name)

	// The document's words first, then whatever is true only of Go. A reader of
	// the OpenAPI schema does not need to know what the repository returns, and
	// a reader of this struct does.
	b.Comment(e.fieldDoc(obj, "Data") + "\n\n" +
		"Pointers, because that is what the repository returns and copying every " +
		"row to change that would be work nobody asked for.")
	b.L("Data []*%s `json:%s`", e.entity(b, res), gobuf.Quote(e.namer.JSON("Data")))
	b.Comment(e.fieldDoc(obj, "Pagination"))
	b.L("Pagination Pagination `json:%s`", gobuf.Quote(e.namer.JSON("Pagination")))
	b.L("}")
	b.NL()
}

// fieldDoc is what the document says about one of an object's fields.
func (e *emitter) fieldDoc(obj *ir.Object, name string) string {
	for _, f := range obj.Fields {
		if f.Name == name {
			return f.Description
		}
	}
	return ""
}

// endpointTypes emit the path, query, and body structs for every endpoint.
//
// A type per slot per endpoint is verbose, and it is what makes the service
// interface say exactly what each operation takes.
func (e *emitter) endpointTypes(b *gobuf.Buf, res *ir.Resource) {
	for i := range res.Endpoints {
		ep := &res.Endpoints[i]

		e.paramStruct(b, pathTypeName(res, ep), "Path parameters for "+res.Name+"."+ep.Name+".",
			ep.Request.PathParams)
		e.paramStruct(b, queryTypeName(res, ep), "Query parameters for "+res.Name+"."+ep.Name+".",
			ep.Request.QueryParams)

		// Create and Update take the model's input types directly. They are
		// the same fields with the same tags, and a wire type beside them
		// would be a third copy of the entity to keep in step.
		if ep.Request.BodyObject == "" && !genutil.UsesModelInput(ep) {
			e.paramStruct(b, bodyTypeName(res, ep), bodyDoc(res, ep), ep.Request.BodyParams)
			e.bodyError(b, res, ep)
		}
	}
}

// bodyError emits the typed failure for a custom endpoint's body.
//
// Nothing generated fills one in — a custom endpoint's body means whatever the
// endpoint means, and no generator knows that — so this is the one input error
// rig emits without also emitting the validator that returns it. It is here
// because the alternative is every project hand-writing the same struct to say
// the same thing, and the generated client already declares the shape it decodes
// into: two hand-written halves would agree until the day somebody renamed a
// field.
//
// A search is left out. Its body is a filter, nothing validates one, and every
// resource has a search — so the rule that covered it emitted fifty lines per
// resource for a failure no code can produce.
func (e *emitter) bodyError(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	if len(ep.Request.BodyParams) == 0 || ep.Name == ir.OpSearch {
		return
	}

	var (
		name = bodyTypeName(res, ep) + "Error"
		body = bodyTypeName(res, ep)
	)

	genutil.InputError{
		Name: name,
		Doc: name + " says what was wrong with each field of a " + body + ".\n\n" +
			"Its shape is the body's shape, so a client can attach every message to " +
			"the field it is about without matching on strings. A member is nil when " +
			"that field was fine, and the whole value is nil when the body was. It " +
			"is what the 422 carries, and returning one from " + res.Name + "." +
			ep.Name + " is how that endpoint refuses a field rather than a request.",
		Subject: "the request",
		Entity: "Entity is a problem with the body as a whole rather than with one " +
			"field.",
		Fields: ep.Request.BodyParams,
	}.Emit(b)
}

func bodyDoc(res *ir.Resource, ep *ir.Endpoint) string {
	return "The request body for " + res.Name + "." + ep.Name + "."
}

// paramStruct emits a struct for one slot, or nothing when the slot is empty.
func (e *emitter) paramStruct(b *gobuf.Buf, name, doc string, fields []ir.Field) {
	if len(fields) == 0 {
		return
	}

	b.Comment(doc)
	b.L("type %s struct {", name)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name, e.paramGoType(b, f), jsonTag(f))
	}
	b.L("}")
	b.NL()
}

// paramGoType renders a request parameter's type.
//
// An update body is the one place a patch appears: leaving a field out and
// clearing it are different requests, and a pointer cannot say which.
func (e *emitter) paramGoType(b *gobuf.Buf, f ir.Field) string {
	if f.Type == "" {
		return "any"
	}
	if kind, ok := e.doc.TypeKindOf(f.Type); ok && kind == ir.TypeKindObject {
		if f.IsArray() {
			return "[]" + e.objectRef(b, f.Type)
		}
		return e.objectRef(b, f.Type)
	}
	return e.goType(b, f)
}

func pathTypeName(res *ir.Resource, ep *ir.Endpoint) string  { return res.Name + ep.Name + "Path" }
func queryTypeName(res *ir.Resource, ep *ir.Endpoint) string { return res.Name + ep.Name + "Query" }
func bodyTypeName(res *ir.Resource, ep *ir.Endpoint) string  { return res.Name + ep.Name + "Body" }

// slotType is the type argument for one Request slot, or struct{} when the slot
// is empty.
func (e *emitter) slotType(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, slot string) string {
	switch slot {
	case "path":
		if len(ep.Request.PathParams) == 0 {
			return "struct{}"
		}
		return pathTypeName(res, ep)
	case "query":
		if len(ep.Request.QueryParams) == 0 {
			return "struct{}"
		}
		return queryTypeName(res, ep)
	default:
		// An upload's body is the bytes on their way past. It is not decoded
		// into anything and it is not buffered: the service streams it to the
		// store, so a file larger than memory is an ordinary upload.
		if ep.File != nil {
			if ep.Method == "POST" {
				return b.Import(filesModule) + ".Upload"
			}
			return "struct{}"
		}
		if ep.Request.BodyObject != "" {
			return e.objectRef(b, ep.Request.BodyObject)
		}
		if genutil.UsesModelInput(ep) {
			return e.entity(b, res) + genutil.ModelInputName(ep) + "Input"
		}
		if len(ep.Request.BodyParams) == 0 {
			return "struct{}"
		}
		return bodyTypeName(res, ep)
	}
}
