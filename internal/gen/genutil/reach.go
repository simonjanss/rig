package genutil

import "github.com/simonjanss/rig/pkg/ir"

// Walk collects every type reachable from the surface a generator decided to
// emit.
//
// It exists because a compiled document describes more than any one output
// exposes. The authentication foundation's own tables have models, repositories
// and filter shapes — and no endpoints, deliberately — so emitting every object
// in the document would put a filter for the session table in the SDK of an
// application that cannot search sessions. What an output should declare is what
// its own surface mentions, and what that mentions in turn.
//
// The recursion is here; the seeds are not. Three generators walk this graph and
// they genuinely start from different places: a client declares the shapes its
// methods mention, a specification the shapes its operations carry, and the
// TypeScript client reaches a streamed table's columns that no object mentions
// at all. So each caller writes its own seeding and shares the closure — which
// is the half that was copied three times, and the half where a divergence is a
// bug rather than a policy.
type Walk struct {
	doc  *ir.Document
	seen map[string]bool
}

// NewWalk starts an empty walk over a document.
func NewWalk(doc *ir.Document) *Walk {
	return &Walk{doc: doc, seen: make(map[string]bool)}
}

// Follow marks a named type reachable, along with everything its fields carry.
//
// The empty name and a name already seen are both no-ops, which is what makes a
// cyclic document terminate and lets a caller follow a field that may not be
// set.
func (w *Walk) Follow(name string) {
	if name == "" || w.seen[name] {
		return
	}
	w.seen[name] = true

	if obj := w.doc.Object(name); obj != nil {
		w.Fields(obj.Fields)
	}
}

// Fields follows whatever a list of fields names: an enum, an object, or a
// resource. A primitive names nothing.
func (w *Walk) Fields(fields []ir.Field) {
	for _, f := range fields {
		switch f.TypeKind {
		case ir.TypeKindEnum, ir.TypeKindObject, ir.TypeKindResource:
			w.Follow(f.Type)
		}
	}
}

// Endpoint follows everything one endpoint can carry, in either direction:
// its body, its parameters and its headers, and the same for every response.
//
// Headers are walked even though no compiled document fills
// [ir.EndpointRequest.Headers] today — see openapigen's sharedHeaders. The first
// one to carry an enum should reach that enum in every output at once, rather
// than in whichever ones remembered to ask.
func (w *Walk) Endpoint(ep *ir.Endpoint) {
	w.Follow(ep.Request.BodyObject)
	w.Fields(ep.Request.PathParams)
	w.Fields(ep.Request.QueryParams)
	w.Fields(ep.Request.BodyParams)
	w.Fields(ep.Request.Headers)

	for _, r := range ep.Responses {
		w.Follow(r.BodyObject)
		w.Fields(r.BodyFields)
		w.Fields(r.Headers)
	}
}

// Seen is every type the walk reached. The map is the walk's own, so a caller
// that keeps it should not go on walking.
func (w *Walk) Seen() map[string]bool { return w.seen }
