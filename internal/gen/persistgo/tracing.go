package persistgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// Where the tracing comes from. Both are named only by a project that set
// `tracing:` in its rig.yaml — which is what keeps OpenTelemetry out of the
// go.mod of every project that did not.
const (
	observeModule   = "github.com/simonjanss/rig/observe"
	otelTraceModule = "go.opentelemetry.io/otel/trace"
)

// tracing reports whether this document asked for spans.
func (e *emitter) tracing() bool {
	return e.doc.API.Tracing != nil && e.doc.API.Tracing.Enabled
}

// spanName is what one repository call is called in a trace.
//
// Built from the resource and the operation, both of which come from the
// document, so the set of names a service can produce is the size of its schema
// rather than the size of its traffic.
func spanName(res *ir.Resource, parts ...string) string {
	name := "repository." + res.Name
	for _, p := range parts {
		name += "." + p
	}
	return name
}

// storeTracerField adds the tracer to the store's configuration.
//
// A field rather than a global for the reason the logger is a field: something
// a repository uses is something it was handed, and a test that wants to see
// what a call produced should be able to hand it a different one. Nil is
// allowed and resolves in New, so a store built by a migration or a seed script
// is not a store that has to know about any of this.
func (e *emitter) storeTracerField(b *gobuf.Buf) {
	if !e.tracing() {
		return
	}

	b.NL()
	b.Comment("Tracer is where this store's spans go. Nil takes the one rig " +
		"installs, which is the global provider's — and that is a tracer whether " +
		"or not anything is exporting, so this is safe to leave alone.\n\n" +
		"\tstore.New(pool, store.Config{Tracer: observe.Tracer()})")
	b.L("Tracer %s.Tracer", b.Import(otelTraceModule))
}

// storeTracerHeld is the field on the Store itself.
func (e *emitter) storeTracerHeld(b *gobuf.Buf) {
	if !e.tracing() {
		return
	}
	b.L("tracer %s.Tracer", b.Import(otelTraceModule))
}

// storeTracerResolved settles the nil in New, so that every repository can use
// the field without asking whether it is there.
func (e *emitter) storeTracerResolved(b *gobuf.Buf) {
	if !e.tracing() {
		return
	}

	b.L("if cfg.Tracer == nil { cfg.Tracer = %s.Tracer() }", b.Import(observeModule))
	b.NL()
}

// traceHelper emits the one place a stage's span is ended.
//
// Every traced stage in this file goes through it, and it ends its span with a
// defer — so a hook that returns early, panics, or grows a branch next year
// cannot leave one open. It is also the single place an error becomes a failed
// span, rather than every call site remembering to say so.
func (e *emitter) traceHelper(b *gobuf.Buf, typeName string) {
	if !e.tracing() {
		return
	}

	ctxPkg := b.Import("context")

	b.Comment("trace runs one stage of a write inside a span of its own.\n\n" +
		"The stage is a callback rather than something bracketed by two calls, " +
		"because that is what makes the span a function's: it is opened and " +
		"ended in one place, and nothing at the call site is holding one.")
	b.L("func (%s *%s) trace(ctx %s.Context, name string, f func(%s.Context) error) error {",
		repo, typeName, ctxPkg, ctxPkg)
	b.L("return %s.Trace(ctx, %s.db.tracer, name, f)", b.Import(observeModule), repo)
	b.L("}")
	b.NL()
}

// methodSpan opens the span a repository method is, at the top of it.
//
// One span per function and ended by a defer, which is the rule everything here
// follows. The error is not recorded on it: what failed is the stage or the
// statement underneath, and both say so on spans of their own.
func (e *emitter) methodSpan(b *gobuf.Buf, res *ir.Resource, parts ...string) {
	if !e.tracing() {
		return
	}

	b.L("ctx, span := %s.db.tracer.Start(ctx, %s)", repo, gobuf.Quote(spanName(res, parts...)))
	b.L("defer span.End()")
	b.NL()
}

// hookCall emits a call into somebody else's code, guarded, and traced when the
// project asked for it.
//
// call is the expression, without the error handling: `in.Hooks.Before(ctx,
// claims, &in.Input)`. ret is what the enclosing function returns beside the
// error — "nil, " for a method with a value, empty for one without.
//
// Untraced, this is the one-liner it has always been. Traced, the call moves
// into a closure so the stage is a function with a span of its own, and the
// shape around it — check the error, return it — does not change.
func (e *emitter) hookCall(b *gobuf.Buf, res *ir.Resource, call, ret string, parts ...string) {
	if !e.tracing() {
		b.L("if err := %s; err != nil { return %serr }", call, ret)
		return
	}

	ctxPkg := b.Import("context")

	b.L("if err := %s.trace(ctx, %s, func(ctx %s.Context) error {",
		repo, gobuf.Quote(spanName(res, parts...)), ctxPkg)
	b.L("return %s", call)
	b.L("}); err != nil { return %serr }", ret)
}

// afterCommitSpan opens a span inside the callback that runs once the work has
// landed.
//
// Inside, and not around, because by the time this runs the method that
// registered it has returned and its span is closed. The parent is the
// transaction's context, which is still what the callback was given, so this
// lands under the request that caused it rather than beside it.
func (e *emitter) afterCommitSpan(b *gobuf.Buf, res *ir.Resource, parts ...string) {
	if !e.tracing() {
		return
	}

	b.L("ctx, span := %s.db.tracer.Start(ctx, %s)", repo, gobuf.Quote(spanName(res, parts...)))
	b.L("defer span.End()")
	b.NL()
}
