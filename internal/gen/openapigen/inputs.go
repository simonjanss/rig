package openapigen

import (
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/pkg/ir"
)

// jsonPart is the multipart part carrying the row, beside the parts carrying
// bytes. It matches files/filehttp.JSONPart.
//
// A literal rather than the constant: the compiler does not import the runtime
// modules it generates code for, which is the same trade the revision header's
// default makes. The multipart test is what keeps the two honest.
const jsonPart = "json"

// requestBody renders what a client sends, or nil for an endpoint that takes
// nothing.
func (e *emitter) requestBody(res *ir.Resource, ep *ir.Endpoint) *v3.RequestBody {
	if len(ep.Request.ContentTypes) == 0 {
		return nil
	}

	content := orderedmap.New[string, *v3.MediaType]()
	for _, ct := range ep.Request.ContentTypes {
		switch ct {
		case ir.MediaMultipart:
			name := e.multipartSchema(res, ep)
			if name == "" {
				continue
			}
			mt := &v3.MediaType{Schema: base.CreateSchemaProxyRef(schemaRef(name))}
			mt.Encoding = e.encoding(ep)
			content.Set(ct, mt)
		default:
			name := e.bodySchema(res, ep)
			if name == "" {
				continue
			}
			content.Set(ct, &v3.MediaType{Schema: base.CreateSchemaProxyRef(schemaRef(name))})
		}
	}
	if content.Len() == 0 {
		return nil
	}

	return &v3.RequestBody{
		Description: requestBodyDescription(res, ep),
		Content:     content,
		// A body an endpoint declares is a body it decodes. Even a PATCH whose
		// every member is optional needs one to arrive.
		Required: boolPtr(true),
	}
}

// requestBodyDescription says what the request carries.
//
// It describes the request where the schema's own description describes the
// shape — which is the difference that earns the second sentence: an endpoint
// accepting two content types has something to say here that belongs on neither
// schema.
func requestBodyDescription(res *ir.Resource, ep *ir.Endpoint) string {
	var multipart bool
	for _, ct := range ep.Request.ContentTypes {
		if ct == ir.MediaMultipart {
			multipart = true
		}
	}

	switch {
	case multipart && len(ep.Request.ContentTypes) > 1:
		return "The " + res.Name + " to create, as JSON — or as a multipart form when " +
			"its files are being attached in the same request."
	case multipart:
		return "The file to attach, as a multipart form."
	}
	return bodyDescription(res, ep)
}

// bodySchema names and registers the JSON body of an endpoint, returning the
// component name.
//
// The compiled document does not name these. BodyObject is empty on every
// generated create, update and search; the body arrives as a bare list of
// fields. Inlining them would leave the specification without a component for
// the one shape a client most needs to construct, and would make a one-field
// change diff as a wall. So the shape is named here — with the name the reader
// already sees in the generated Go, which is why the naming lives in genutil
// rather than in this package.
func (e *emitter) bodySchema(res *ir.Resource, ep *ir.Endpoint) string {
	if obj := ep.Request.BodyObject; obj != "" {
		return obj
	}
	name := genutil.BodyShapeName(res, ep)
	if name == "" {
		return ""
	}
	if _, done := e.extra[name]; done {
		return name
	}

	props := orderedmap.New[string, *base.SchemaProxy]()
	for _, f := range ep.Request.BodyParams {
		proxy := e.fieldSchema(f)
		if s := proxy.Schema(); s != nil {
			if d := requestDefault(f, s.Type); d != nil {
				s.Default = d
			}
			if f.Immutable {
				// No OpenAPI keyword says this, and writeOnly says the
				// opposite. The paths already carry the fact — the field is
				// here and absent from the update body — so all that is left is
				// to say it in words.
				s.Description = join(s.Description,
					"Set when the "+res.Name+" is created and never changed afterwards.")
			}
		}
		props.Set(f.Wire, proxy)
	}

	schema := &base.Schema{
		Type:        []string{"object"},
		Description: bodyDescription(res, ep),
		Properties:  props,
	}
	// A PATCH leaves an absent field alone, so nothing in an update body is
	// required. Everywhere else, a field the database cannot supply is.
	if genutil.ModelInputName(ep) != ir.OpUpdate {
		schema.Required = genutil.BodyRequired(ep.Request.BodyParams)
	}

	e.extra[name] = schema
	return name
}

// bodyDescription says what the shape is for.
func bodyDescription(res *ir.Resource, ep *ir.Endpoint) string {
	switch genutil.ModelInputName(ep) {
	case ir.OpCreate:
		return "The fields a new " + res.Name + " is created with. A field the database " +
			"cannot supply for itself is required."
	case ir.OpUpdate:
		return "The fields of a " + res.Name + " to change. Every member is optional: one " +
			"left out is left alone, and an explicit null clears it."
	}
	return "The request body for " + res.Name + "." + ep.Name + "."
}

// multipartSchema names and registers the form a multipart request carries.
//
// A create on a table with file columns accepts the row and its bytes together,
// because a not-null file column is otherwise unreachable: the row cannot be
// written without the file and the file cannot be attached without the row.
func (e *emitter) multipartSchema(res *ir.Resource, ep *ir.Endpoint) string {
	parts := ep.Request.FileParts
	if len(parts) == 0 {
		return ""
	}

	name := res.Name + ep.Name + "Multipart"
	if _, done := e.extra[name]; done {
		return name
	}

	props := orderedmap.New[string, *base.SchemaProxy]()
	var required []string

	// The row itself, when this endpoint also takes one.
	if body := e.bodySchema(res, ep); body != "" {
		props.Set(jsonPart, base.CreateSchemaProxyRefWithSchema(schemaRef(body),
			&base.Schema{Description: "The " + res.Name + " itself, as JSON."}))
		required = append(required, jsonPart)
	}

	for _, p := range parts {
		props.Set(p.Name, base.CreateSchemaProxy(&base.Schema{
			Type: []string{"string"},
			// The 3.1 spelling for opaque bytes. `format: binary` is 3.0's, and
			// means nothing to a 2020-12 validator.
			ContentMediaType: ir.MediaOctet,
			Description:      "The " + p.Role + ", as a file.",
		}))
		if p.Required {
			required = append(required, p.Name)
		}
	}

	e.extra[name] = &base.Schema{
		Type: []string{"object"},
		Description: "The same body " + ep.Name + " takes, in a part named `" + jsonPart +
			"`, plus one part per file column. The row and its bytes are committed " +
			"together, so a request that fails leaves neither.",
		Properties: props,
		Required:   required,
	}
	return name
}

// encoding states each part's content type.
//
// The json part is the one that matters: without it a client is free to send
// the row as text/plain, and the server's part dispatch reads the type.
func (e *emitter) encoding(ep *ir.Endpoint) *orderedmap.Map[string, *v3.Encoding] {
	out := orderedmap.New[string, *v3.Encoding]()
	if len(ep.Request.BodyParams) > 0 || ep.Request.BodyObject != "" {
		out.Set(jsonPart, &v3.Encoding{ContentType: ir.MediaJSON})
	}
	for _, p := range ep.Request.FileParts {
		out.Set(p.Name, &v3.Encoding{ContentType: ir.MediaOctet})
	}
	if out.Len() == 0 {
		return nil
	}
	return out
}

// fieldsSchema is an inline shape for a response body the document did not
// name.
//
// Nothing in the compiled document fills EndpointResponse.BodyFields today. It
// is handled rather than ignored because the alternative — emitting a response
// with a content type and no schema — describes a body as opaque when the
// document knew its shape.
func (e *emitter) fieldsSchema(fields []ir.Field, desc string) *base.Schema {
	props := orderedmap.New[string, *base.SchemaProxy]()
	for _, f := range fields {
		props.Set(f.Wire, e.fieldSchema(f))
	}
	return &base.Schema{Type: []string{"object"}, Description: desc, Properties: props}
}

// join puts a local sentence after the document's own words.
func join(existing, added string) string {
	if existing == "" {
		return added
	}
	return existing + "\n\n" + added
}
