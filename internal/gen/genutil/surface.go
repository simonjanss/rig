package genutil

import (
	"net/http"

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
