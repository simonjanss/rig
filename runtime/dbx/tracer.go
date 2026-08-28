package dbx

import "context"

// Tracer opens a span around one stage of a repository call.
//
// It is declared here rather than imported, and that is the whole trick:
// [github.com/simonjanss/rig/observe.DBTracer] satisfies it without either side
// knowing the other exists. A field typed as the observe package's own would
// put OpenTelemetry in the go.mod of every project that never set `tracing:`,
// which is the one thing the split between these modules exists to prevent.
//
// The stage is a callback rather than something bracketed by two calls, because
// that is what makes the span a function's: it is opened and ended in one
// place, and nothing at the call site is holding one. An error returned from f
// is what marks the span failed, once, rather than at every place an error is
// returned.
//
// Anything with this method works: a stub in a test, a wrapper that samples, an
// entirely different tracing library.
type Tracer interface {
	Trace(ctx context.Context, name string, f func(context.Context) error) error
}

// Trace runs f inside a span from tracer, or simply runs it when tracer is nil.
//
// The nil case is the ordinary one and it is why this function exists rather
// than a method call at each site. A repository compiled into one of rig's own
// modules cannot have spans woven into it by a generator that never runs there,
// so it calls this unconditionally — and a project that did not ask for tracing
// pays one nil check per stage and names no tracing library at all.
func Trace(ctx context.Context, tracer Tracer, name string, f func(context.Context) error) error {
	if tracer == nil {
		return f(ctx)
	}
	return tracer.Trace(ctx, name, f)
}
