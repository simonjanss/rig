package observe

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/trace"
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

// DBTracer is [Trace] as the one method a repository's tracer field takes.
//
// Same trick as [APITracer] and for the same reason: the interface lives in
// [github.com/simonjanss/rig/runtime/dbx], speaks only in standard-library
// types, and this fits it without either module importing the other. It is what
// lets a repository compiled into one of rig's own modules open a span, where a
// generated one has spans written into it by the generator.
//
// The zero value traces on rig's own tracer, which is the global provider
// [Setup] installed — so a project that never called Setup gets a no-op and
// every query runs untraced rather than differently. Set Tracer to put these
// spans somewhere else.
type DBTracer struct {
	// Tracer is where the spans go. Nil is rig's own, from [Tracer].
	Tracer trace.Tracer
}

// Trace runs one stage of a repository call inside a span of its own. An error
// returned from f is what marks that span failed, once.
func (t DBTracer) Trace(ctx context.Context, name string, f func(context.Context) error) error {
	return Trace(ctx, t.Tracer, name, f)
}
