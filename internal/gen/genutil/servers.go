package genutil

import "github.com/simonjanss/rig/pkg/ir"

// Servers are the deployments the project named, in the order it named them, or
// nil for a project that named none.
//
// One accessor rather than three reads of the same field, for the reason
// [RevisionHeader] is one: the OpenAPI document, the Go client and the
// TypeScript client all have to say where the API is, and a document whose
// server list disagreed with the SDK beside it is the failure the `servers:`
// block exists to prevent.
func Servers(doc *ir.Document) []ir.Server { return doc.API.Servers }

// DefaultServer is the deployment a caller who names none receives.
//
// It takes the list rather than the document, because a client generator's list
// is not always the document's: a project still configuring the deprecated
// per-generator option has one nameless deployment that never reached the IR,
// and the question asked of both lists is the same one.
//
// ok is false for a project that named no deployment, which is what tells a
// client generator to emit no constant and go on requiring a URL from its
// caller.
//
// The project resolved which entry this is — an explicit marker, or the first
// when nobody claimed it — before the document was written, so this looks for a
// mark rather than reimplementing the rule.
func DefaultServer(servers []ir.Server) (ir.Server, bool) {
	for _, s := range servers {
		if s.Default {
			return s, true
		}
	}
	return ir.Server{}, false
}
