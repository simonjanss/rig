package openapigen

import (
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"

	"github.com/simonjanss/rig/pkg/ir"
)

// specTag groups the document's own two routes.
const specTag = "openapi"

// serves reports whether this API mounts the document describing it.
//
// The IR answers it, not this generator's own options, because two generators
// have to agree: server-go mounts the routes and this one describes them, and a
// specification that omitted the routes it is fetched over would be the one
// omission this generator cannot excuse.
func (e *emitter) serves() bool { return e.doc.API.OpenAPI != nil }

// specRoutes describes the document's own routes.
//
// One per format this generator emits, and that is what keeps the routes and
// their description in step without either side of the wiring knowing what the
// other was configured with: `formats: [json]` writes one file, so it describes
// one route — and the server, which mounts a route per rendering it finds
// embedded, mounts one. Neither had to be told about the other.
func (e *emitter) specRoutes() []specOperation {
	if !e.serves() {
		return nil
	}
	o := e.doc.API.OpenAPI

	var out []specOperation
	for _, f := range e.cfg.Formats {
		switch f {
		case "json":
			out = append(out, specOperation{
				route: route{method: "GET", path: o.JSONPath},
				op: e.specOperation("getOpenAPIJSON", "The OpenAPI document, as JSON.",
					ir.MediaJSON, &base.Schema{
						Type: []string{"object"},
						// The document is an OpenAPI document, and describing
						// its shape here would be restating the 3.1
						// meta-schema — badly, and in a file that would then
						// have two versions of it to keep in step.
						AdditionalProperties: &base.DynamicValue[*base.SchemaProxy, bool]{
							B: true, N: 1,
						},
					}),
			})
		case "yaml":
			out = append(out, specOperation{
				route: route{method: "GET", path: o.YAMLPath},
				op: e.specOperation("getOpenAPIYAML", "The OpenAPI document, as YAML.",
					// RFC 9512's registration. Before it there were four
					// spellings in the wild and no right answer.
					"application/yaml", &base.Schema{Type: []string{"string"}}),
			})
		}
	}
	return out
}

// specOperation is one route and the operation on it.
type specOperation struct {
	route route
	op    *v3.Operation
}

// specOperation renders one of the two.
func (e *emitter) specOperation(
	id, summary, mediaType string, schema *base.Schema,
) *v3.Operation {
	content := orderedmap.New[string, *v3.MediaType]()
	content.Set(mediaType, &v3.MediaType{Schema: base.CreateSchemaProxy(schema)})

	ok := &v3.Response{
		Description: "This document.",
		Content:     content,
		Headers:     orderedmap.New[string, *v3.Header](),
	}
	ok.Headers.Set("ETag", &v3.Header{
		Description: "The document's content hash. Send it back as If-None-Match " +
			"and an unchanged document answers 304 with no body.",
		Schema: base.CreateSchemaProxy(&base.Schema{Type: []string{"string"}}),
	})

	responses := &v3.Responses{Codes: orderedmap.New[string, *v3.Response]()}
	responses.Codes.Set("200", ok)
	responses.Codes.Set("304", &v3.Response{
		Description: "The document has not changed since the ETag sent as " +
			"If-None-Match was issued.",
	})

	op := &v3.Operation{
		Tags:    []string{specTag},
		Summary: summary,
		Description: summary + "\n\n" +
			"It is generated from the same compiled schema the routes are, so it " +
			"cannot describe an endpoint this API does not serve. What the running " +
			"server answers here is the document that build was generated with, " +
			"embedded in the binary — so a client reading it is reading this " +
			"deployment rather than whatever is on a branch.\n\n" +
			"No credential is read. What this document says is what every generated " +
			"client was built against, and a specification nobody may fetch is one " +
			"nobody can use.",
		OperationId: id,
		Responses:   responses,
	}

	// One empty requirement and nothing beside it, which is how OpenAPI spells
	// "no credential is read here". optionalCredential puts the real scheme
	// beside the empty one, and that says something different — a caller who
	// presents a credential is identified by it and may see more. Nothing on
	// this route reads one at all.
	//
	// An actually empty slice would not do: it renders as an absent key, and an
	// absent key inherits the document-level default. That default is the only
	// reason this is needed, and it exists only for a project with an auth
	// block — hence the guard.
	if e.doc.API.Auth != nil {
		op.Security = []*base.SecurityRequirement{
			{Requirements: orderedmap.New[string, []string](), ContainsEmptyRequirement: true},
		}
	}
	return op
}

// specTagDescription is the tag the two operations carry. A tag nothing defines
// is a lint finding, and one nothing references is too — so this is emitted
// exactly when the routes are.
func (e *emitter) specTagDescription() *base.Tag {
	if !e.serves() {
		return nil
	}
	return &base.Tag{
		Name: specTag,
		Description: "This document, served by the API it describes. The renderings " +
			"listed are the ones this project asked the generator to write.",
	}
}
