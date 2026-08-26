package genutil

import (
	"net/http"
	"strings"

	"github.com/simonjanss/rig/pkg/ir"
)

// DefaultRevisionHeader carries the API revision when a document predates the
// setting.
//
// It matches rigclient.DefaultRevisionHeader and rig.yaml's
// api.revision_header. A literal rather than the constant, because the
// compiler does not depend on the modules it generates code for, and every
// project that has the setting sends its own value through the document.
const DefaultRevisionHeader = "API-Revision"

// RevisionHeader is the header the API revision travels in, both ways.
//
// One fallback rather than one per generator: the client that sends the header,
// the server that reads it and the specification that documents it have to name
// the same thing, and a document written before the setting existed says
// nothing at all.
func RevisionHeader(doc *ir.Document) string {
	if h := doc.API.RevisionHeader; h != "" {
		return h
	}
	return DefaultRevisionHeader
}

// BodyShapeName is what an endpoint's request body is called, for every output
// that has to name it: the Go client's struct, the service layer's parameter
// shape, the OpenAPI component.
//
// Empty when there is nothing to name — no body at all, or a body the document
// already names as an object, in which case that name is the one to use.
//
// A create or update is named whether or not it turned out to have any fields.
// The model declares that input type from the operation rather than from the
// column list, so a table whose every column is server-managed still has a
// ProfileCreateInput; a caller that answered "nothing to name" here would leave
// the client emitting a parameter with no type.
//
// It lives here because three generators name this shape and a fourth is
// arriving. A specification calling the create body ProfileCreateInput while
// the client declares ProfileCreateBody describes a type the SDK does not have,
// and the reader has no way to tell which of the two is the mistake.
func BodyShapeName(res *ir.Resource, ep *ir.Endpoint) string {
	if ep.Request.BodyObject != "" {
		return ""
	}
	if name := ModelInputName(ep); name != "" {
		return res.Name + name + "Input"
	}
	if len(ep.Request.BodyParams) == 0 {
		return ""
	}
	return res.Name + ep.Name + "Body"
}

// BodyRequired names the fields of a request body a caller cannot leave out.
//
// A field is required when the database cannot supply it: not nullable, not an
// array, and with no column default to fall back on. That is checkable rather
// than stylistic — omit one and the insert genuinely fails — which is what
// makes it safe for a specification to promise.
//
// Never call it for an update body. A PATCH leaves an absent field alone, so
// every member of one is optional; that is the whole reason the update inputs
// are wrapped rather than plain pointers.
//
// It is deliberately not the rule that decides `omitempty` on the Go client's
// struct tags, which ignores Default. The two answer different questions — may
// I leave this out of the JSON I send, versus must the server have received it
// — and a field with a column default answers them differently.
func BodyRequired(fields []ir.Field) []string {
	var out []string
	for _, f := range fields {
		if f.IsNullable() || f.IsArray() || f.Default != "" {
			continue
		}
		out = append(out, f.Wire)
	}
	return out
}

// IdempotentWrite reports whether an endpoint's work is worth recording against
// an Idempotency-Key.
//
// The methods that can produce a second row if a client sends the same request
// twice, and only those. A GET has nothing to record. A DELETE is idempotent in
// what it leaves behind — its second answer is a 404 about a row that is
// genuinely gone — so buying a smoother answer for it would cost a transaction
// per delete to change an error nobody was wrong to receive.
//
// A search is a QUERY here even where it is also mounted as a POST alias, so it
// is excluded by the same switch that excludes reads. The alias is a second
// route to one handler, and the handler is generated from the operation. This
// is why the test is [ir.Endpoint.Method] rather than whatever method a caller
// happens to be reading off a route.
//
// A dedicated upload route never reaches this in the server generator: it takes
// a branch of its own, for reasons that branch explains.
//
// It is here rather than in the server generator because the specification and
// the server have to agree. A document that advertises the header on a route
// the server does not guard is a document that costs somebody a duplicated
// write.
func IdempotentWrite(ep *ir.Endpoint) bool {
	switch ep.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return true
	}
	return false
}

// RoutePath is the path half of a net/http pattern — "GET /v1/todos/{id}"
// becomes "/v1/todos/{id}".
//
// A pattern with no method is returned whole. That is unreachable from a
// compiled document, where [ir.Endpoint.Pattern] always carries one, and
// returning the input rather than the empty string is what makes it unreachable
// harmlessly.
func RoutePath(pattern string) string {
	if _, path, found := strings.Cut(pattern, " "); found {
		return path
	}
	return pattern
}

// QueryTypeName is the shape an endpoint's query parameters arrive in.
//
// One answer for both SDKs: the Go struct and the TypeScript interface are the
// same API described twice, and a caller reading one language's documentation to
// write the other has to find the same name there.
func QueryTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	return res.Name + ep.Name + "Query"
}

// FieldsTypeName is the shape a validation failure on this endpoint's body
// arrives in, and is empty when there is no body to be wrong about.
//
// Two kinds of body are left out. A body that is a named object is shared
// between endpoints, so its failure shape would have to be too, and nothing on
// the server emits one to fill in. And a generated endpoint whose body is its
// own — a search, a revert — is refused in some other shape than that body's: a
// search's filter is a question nothing validates, and a revert replays the
// version through the update path, so what comes back is the update's field
// errors and not one about a version identifier. A shape per call there would be
// the wrong shape, which decodes to an empty one rather than failing. Both fail
// as a plain client error, which is what everything did before any of this
// existed.
//
// A create and an update are generated too and are not left out: their bodies
// are the model's inputs, and the validator that refuses one is generated beside
// them.
func FieldsTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	if ep.Request.BodyObject != "" || len(ep.Request.BodyParams) == 0 {
		return ""
	}
	if ep.Impl.Kind == ir.EndpointGenerated && !UsesModelInput(ep) {
		return ""
	}
	return res.Name + ep.Name + "Fields"
}

// SearchFilterField is the member of a search body that carries the conditions.
//
// Found rather than assumed: the body is one object field, and taking it from
// the document means a rename in the compiler does not silently produce a client
// that sends the wrong key.
func SearchFilterField(ep *ir.Endpoint) (ir.Field, bool) {
	for _, f := range ep.Request.BodyParams {
		if f.TypeKind == ir.TypeKindObject && strings.HasSuffix(f.Type, "Filter") {
			return f, true
		}
	}
	return ir.Field{}, false
}

// Exposed is every resource with an API surface: endpoints to call, and not
// marked unexposed.
//
// Both SDK generators ask this and they have to agree. A resource one of them
// emits a client for and the other does not is not a difference between two
// languages; it is one of them being wrong.
//
// The OpenAPI generator asks a wider question — a shape route is its own read
// surface, so a resource can be unexposed and still belong in the specification
// — and answers it for itself.
func Exposed(doc *ir.Document) []*ir.Resource {
	var out []*ir.Resource
	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Unexposed || len(res.Endpoints) == 0 {
			continue
		}
		out = append(out, res)
	}
	return out
}

// FilterObjects are the search shapes belonging to a resource.
//
// A filter is reached from the search body rather than from the entity, so a
// resource with no Search endpoint has filter shapes in the document and no use
// for them here — which is why reachable is a parameter rather than something
// this could work out from the resource alone.
func FilterObjects(doc *ir.Document, reachable map[string]bool, res *ir.Resource) []*ir.Object {
	var out []*ir.Object
	for i := range doc.API.Objects {
		obj := &doc.API.Objects[i]
		if obj.Origin != ir.OriginFilter || !reachable[obj.Name] {
			continue
		}
		if strings.HasPrefix(obj.Name, res.Name+"Filter") {
			out = append(out, obj)
		}
	}
	return out
}

// UnclaimedObjects are the reachable objects no resource's own file emits —
// whatever a project declared for itself and a custom endpoint returns. They
// have to land somewhere, or a method that returns one does not compile.
//
// A resource's entity, its page shape and its filters are its files'. What is
// claimed beyond those differs by language and is the caller's to name: the Go
// client declares Error and Pagination in its base file, and the TypeScript
// client declares neither there. So claimed is a seed rather than a constant,
// and it is read rather than written — a caller may pass nil.
func UnclaimedObjects(
	doc *ir.Document, reachable map[string]bool, claimed map[string]bool,
) []*ir.Object {
	taken := make(map[string]bool, len(claimed))
	for name := range claimed {
		taken[name] = true
	}
	for _, res := range Exposed(doc) {
		taken[res.Name] = true
		taken[res.Name+"ListResponse"] = true
		for _, obj := range FilterObjects(doc, reachable, res) {
			taken[obj.Name] = true
		}
	}

	var out []*ir.Object
	for i := range doc.API.Objects {
		obj := &doc.API.Objects[i]
		if !taken[obj.Name] && reachable[obj.Name] {
			out = append(out, obj)
		}
	}
	return out
}
