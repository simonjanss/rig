package apibase

import (
	"context"
	"log/slog"
)

// requestAttr is the key the request group is written under, in the one place
// it is decided.
//
// Three call sites write it — [LogFailure], [LogRequest] and the handler below
// — and the third has to recognise what the first two wrote so that a line does
// not carry the group twice. A constant is what keeps them from drifting apart
// silently, which is a thing that shows up as a duplicated key in a log index
// rather than as a failing test.
const requestAttr = "request"

// LogHandler returns h with the request on every record written inside one.
//
// It is the whole of how a service's own lines come to carry the identifier in
// the error body, the route that matched and the method, without a call site
// saying so:
//
//	logger := apibase.RequestLogger(app.Logger)
//	...
//	s.logger.InfoContext(ctx, "importing", slog.Int("rows", len(items)))
//	// {"msg":"importing","rows":12,"request":{"request_id":"…","route":"…"}}
//
// The request comes off the context, which is the only reason the context is
// worth having here — and the reason [RequestContextFrom] and sloglint's
// `context: all` are two halves of one mechanism. A call that drops the context
// drops the request with it.
//
// A record written outside a request gains nothing. A migration, a subcommand
// and a background job all find no request context, and a line that says it
// belongs to one would be worse than a line that says nothing.
//
// A record that already carries a "request" attribute is left alone, so a
// service that puts the group on the line itself — which is what rig's own two
// lines do, and what its documentation used to ask for — gets one group rather
// than two.
//
// Wrapping twice is wrapping once: the generated Mount wraps the logger the
// application was handed, and an application that wrapped it first is not
// punished for it.
//
// One thing worth knowing: an attribute a handler adds at Handle time lands
// inside whatever group is open, so a logger derived with
// slog.Logger.WithGroup("db") writes the request at db.request. That is
// ordinary slog semantics for a decorating handler rather than something this
// can undo.
func LogHandler(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	if _, ok := h.(*requestLogHandler); ok {
		return h
	}
	return &requestLogHandler{inner: h}
}

// RequestLogger is l with [LogHandler] over its handler, and the default logger
// when l is nil.
//
// Nil is a caller that configured no logging rather than one that asked for
// silence — the same reading [Server.Logger] gives it, and for the same reason.
//
// This is what the generated Mount applies to serve.App.Logger, so that
// everything the application builds out of it — its services, its repositories,
// the authentication configuration — is request-aware without being told.
func RequestLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	return slog.New(LogHandler(l.Handler()))
}

// requestLogHandler is [LogHandler].
//
// A field rather than an embedded slog.Handler, and that is not a style choice:
// embedding would promote WithAttrs and WithGroup, each of which returns the
// inner handler — so the first logger.With(…) anywhere in an application would
// quietly return an undecorated logger and the request would stop appearing on
// lines that used to have it.
type requestLogHandler struct{ inner slog.Handler }

var _ slog.Handler = (*requestLogHandler)(nil)

// Enabled implements [log/slog.Handler].
func (h *requestLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements [log/slog.Handler].
func (h *requestLogHandler) Handle(ctx context.Context, r slog.Record) error {
	rc, ok := RequestContextFrom(ctx)
	if !ok || hasRequestAttr(r) {
		return h.inner.Handle(ctx, r)
	}

	// Cloned rather than added to. A Handler is handed a record it does not own —
	// a tee hands the same one to every handler under it — and the one guarantee
	// that makes that safe is that nobody writes to it.
	r = r.Clone()
	r.AddAttrs(slog.Any(requestAttr, rc))
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements [log/slog.Handler].
func (h *requestLogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	return &requestLogHandler{inner: h.inner.WithAttrs(as)}
}

// WithGroup implements [log/slog.Handler].
func (h *requestLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &requestLogHandler{inner: h.inner.WithGroup(name)}
}

// hasRequestAttr reports whether this record already says what request it
// belongs to.
//
// Only the record's own attributes, not the ones a derived logger accumulated:
// those went through WithAttrs, which cannot see a context and so cannot be the
// group this handler writes. A rig log line carries four or five attributes, so
// this is a scan of four or five, and only on a line written inside a request.
func hasRequestAttr(r slog.Record) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == requestAttr {
			found = true
			return false
		}
		return true
	})
	return found
}
