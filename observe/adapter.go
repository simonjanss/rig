package observe

import (
	"context"
	"net/http"
)

// APITracer is [Server], [TraceID] and [Fail] gathered into the three methods a
// generated server's tracer field takes.
//
// It satisfies that field without either package importing the other, which is
// the point of it: the interface is declared in
// [github.com/simonjanss/rig/runtime/apibase] and every one of its methods
// speaks in standard-library types, so a value of this type simply fits. A
// field typed as this package's own would put OpenTelemetry in the go.mod of
// every project that never set `tracing:`.
//
// The zero value is ready, and there is nothing to configure on it: where the
// spans go is [Setup]'s business, and this is only how a request reaches them.
//
//	api.Server{Tracer: observe.APITracer{}}
//
// A generated server assigns it for you when `tracing:` is on. This is the
// spelling for a server built by hand.
type APITracer struct{}

// Server opens the span for one request and returns the function that ends it.
//
// The span itself is [Server]'s. What changes here is only the shape: a
// func() rather than a *Span, so that the interface this satisfies can be
// written without naming a type from this module.
func (APITracer) Server(r *http.Request, route string, status func() int) (*http.Request, func()) {
	r, span := Server(r, route, status)
	return r, span.End
}

// TraceID names the trace this request belongs to, or empty when it is in none.
func (APITracer) TraceID(r *http.Request) string { return TraceID(r) }

// Fail records why a request is being refused, on whatever span the context is
// in. It ends nothing.
func (APITracer) Fail(ctx context.Context, status int, err error) { Fail(ctx, status, err) }
