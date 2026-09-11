package apibase

import (
	"net/http"

	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/reqlog"
)

// Tracing returns a router that opens a span for every route registered
// through it, named by the pattern it was registered under.
//
// A generated handler used to open its own span, which meant the trace stopped
// where the generator stopped: the authentication routes, the inbox, presence,
// the live-sync shapes and the OpenAPI document are mounted by packages a
// generator never sees, and none of them had one. Registering all of them
// through this is what makes the answer the same for every route, and for
// whatever is mounted next.
//
// A nil Tracer returns r itself. That is the ordinary case — a project that
// never set `tracing:` — and it costs nothing, not a wrapper and not a branch
// per request.
//
// This rather than a handler around the mux, which is the shape that first
// suggests itself: a span is named by the route and not by the path, and the
// only way to learn the route from outside is to match the request a second
// time. Here the pattern is already in hand, because registering it is what
// this is. It also leaves the mux alone — a caller that mounts its own
// catch-all on what Register returns is not competing with a wrapper for it.
func Tracing(r httpx.Router, t Tracer) httpx.Router {
	if t == nil {
		return r
	}
	return tracingRouter{to: r, tracer: t}
}

// tracingRouter is [Tracing]'s answer.
type tracingRouter struct {
	to     httpx.Router
	tracer Tracer
}

func (m tracingRouter) Handle(pattern string, handler http.Handler) {
	m.to.Handle(pattern, m.traced(pattern, handler))
}

func (m tracingRouter) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.to.Handle(pattern, m.traced(pattern, http.HandlerFunc(handler)))
}

// traced is one route, wrapped in the span that reports it.
//
// The recorder is this wrapper's own rather than the one a generated handler
// makes for its request line. Wrapping twice costs a pointer; sharing one would
// mean every route rig mounts had to make one to be traced, which is the
// arrangement this replaced.
func (m tracingRouter) traced(pattern string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := reqlog.Wrap(w)

		r, end := m.tracer.Server(r, pattern, rec.Status)
		defer end()

		handler.ServeHTTP(rec, r)
	})
}
