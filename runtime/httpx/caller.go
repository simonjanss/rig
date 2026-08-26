package httpx

import (
	"net/http"

	"github.com/simonjanss/rig/runtime/tenancy"
)

// Caller establishes who is asking, once, so that every route under it narrows
// to them structurally rather than as something each handler remembers to do.
type Caller struct {
	// Of identifies the caller. Required.
	//
	// Taken rather than assumed for the reason the generated server takes it: a
	// project authenticates its own way, and a route that established the caller
	// differently from every other route in the same application would be a
	// second answer to the one question a tenant boundary rests on.
	Of func(*http.Request) (tenancy.Claims, error)

	// Fail writes the refusal when Of cannot answer. Nil means [Fail].
	Fail func(http.ResponseWriter, *http.Request, error)
}

// Wrap resolves the caller, puts them on the request's context, and hands it on.
//
// The claims reach the handler through the context rather than as a parameter,
// because the context is where every layer beneath the handler already reads them
// from. A handler taking them twice is two answers to one question, and the
// interesting case is when they disagree.
func (c Caller) Wrap(next http.HandlerFunc) http.HandlerFunc {
	fail := c.Fail
	if fail == nil {
		fail = Fail
	}
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := c.Of(r)
		if err != nil {
			fail(w, r, err)
			return
		}
		next(w, r.WithContext(tenancy.NewContext(r.Context(), claims)))
	}
}
