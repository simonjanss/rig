package observe

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Span is one request's span, and the status it has not answered with yet.
//
// It exists so that a generated handler is two lines — start it, defer the end
// — and so that the status still reaches the span without a second thing to
// remember at the bottom of a function that has a dozen ways out.
type Span struct {
	span   trace.Span
	status func() int
}

// Server starts the span for one request, under whatever trace the caller
// arrived carrying.
//
// The route is the matched pattern, not the path: "GET /api/v1/todos/{id}"
// rather than one span name per identifier anybody ever fetched. That is why
// this is called from inside the generated handler rather than from a
// middleware in front of the mux — net/http sets the pattern on the request the
// mux dispatches, and a wrapper outside has a request that has matched nothing.
//
// status is how the span learns what was answered, and it is a function rather
// than the response writer so that this package does not depend on rig/runtime.
// The generated server passes the Status method of the recorder it already
// wraps every response in. Nil is allowed and means the attribute is left off.
//
// The returned request carries the span. Everything after this — the claims,
// the hooks, the service, the SQL — is inside it because it takes that context.
func Server(r *http.Request, route string, status func() int) (*http.Request, *Span) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	ctx, span := Tracer().Start(ctx, route,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(r.Method),
			semconv.HTTPRoute(route),
			semconv.URLPath(r.URL.Path),
			semconv.UserAgentOriginal(r.UserAgent()),
			semconv.ClientAddress(r.RemoteAddr),
		),
	)

	return r.WithContext(ctx), &Span{span: span, status: status}
}

// End records what was answered and closes the span.
//
// Deferred, always, and the only place a request's span ends. A handler that
// ended one on the way out of each branch would eventually grow a branch that
// forgot.
//
// A handler that wrote no header answered 200: that is net/http's rule, and a
// span claiming a status of zero would be reporting the recorder's zero value
// as the client's answer.
func (s *Span) End() {
	if s.status != nil {
		code := s.status()
		if code == 0 {
			code = http.StatusOK
		}
		s.span.SetAttributes(semconv.HTTPResponseStatusCode(code))

		// Only the server's own failures. The error itself was recorded by
		// Fail, which is where the reason is; this is the case where a status
		// was written by something that never called it.
		if code >= 500 && s.span.IsRecording() {
			s.span.SetStatus(codes.Error, http.StatusText(code))
		}
	}
	s.span.End()
}

// TraceID is the trace this request belongs to, or "" when it belongs to none.
//
// It is what the generated server's RequestID returns when a project has
// tracing on and set nothing else, which makes the identifier in an error body,
// the request_id on every log line, and the trace in a collector the same
// string. That is the whole correlation story, and it needs no field anywhere
// and no otel in rig/runtime.
//
// Empty when there is no span — which is what an untraced request looks like,
// and what makes the fallback safe to apply unconditionally.
func TraceID(r *http.Request) string {
	sc := trace.SpanContextFromContext(r.Context())
	if !sc.TraceID().IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
