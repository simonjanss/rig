package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// observeModule is where the OpenTelemetry wiring lives. It is named only from
// the files this project asked for by setting `tracing:` — which is what keeps
// otel out of the go.mod of every project that did not.
const observeModule = "github.com/simonjanss/rig/observe"

// tracing reports whether this document asked for spans.
func (e *emitter) tracing() bool {
	return e.doc.API.Tracing != nil && e.doc.API.Tracing.Enabled
}

// span opens one request's span, at the top of the handler.
//
// Before prepare, so that everything answering early — a pre-hook that writes a
// response, a caller too old to be served, a refused permission — is inside the
// span rather than invisible to it. That is the same argument the deferred
// request line makes one line further down.
//
// The span is named by the route and not by the path: the pattern is what the
// duplicate-route diagnostic already proves unique, and a name per identifier
// that ever appeared in a URL is the thing that makes a trace unusable. It is
// also why this is written here rather than as a wrapper around the mux —
// net/http knows which pattern matched only once it has dispatched, so a
// middleware in front has a request that has matched nothing.
//
// The status reaches the span through the recorder wrapped a line above, as a
// method value: observe takes a func() int rather than the writer so that it
// does not depend on rig/runtime.
func (e *emitter) span(b *gobuf.Buf, ep *ir.Endpoint) {
	if !e.tracing() {
		return
	}

	b.L("r, span := %s.Server(r, %s, rec.Status)", b.Import(observeModule), gobuf.Quote(ep.Pattern))
	b.L("defer span.End()")
	b.NL()
}

// tracingFile emits the one thing a main function needs to start tracing.
//
// A file of its own for the reason auth.gen.go is one: a project without the
// block gets no file, and so its API package — and its module — names no
// tracing library at all.
func (e *emitter) tracingFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)
	obsPkg := b.Import(observeModule)

	b.Comment("Tracing is this API's tracing configuration, as far as generated " +
		"code can know it.\n\n" +
		"\tprovider, err := observe.Setup(ctx, api.Tracing())\n" +
		"\tif err != nil { return nil, err }\n" +
		"\tapp.CloseWithin(\"traces\", 5*time.Second, provider.Shutdown)\n\n" +
		"The name comes from rig.yaml, so nothing is typed twice. What is not " +
		"here is where the spans go: a collector, or a file, is a property of " +
		"the deployment rather than of this build, and the same binary runs " +
		"where there is one and where there is not. Set " +
		"[github.com/simonjanss/rig/observe.Config.Endpoint] on what this " +
		"returns, or leave it to $OTEL_EXPORTER_OTLP_ENDPOINT.\n\n" +
		"ServiceVersion is left empty on purpose. The build is the " +
		"application's own fact — a tag, a commit, whatever the pipeline " +
		"stamps in — and a generator that filled it with the API version would " +
		"be answering a question nobody asked it.")
	b.L("func Tracing() %s.Config {", obsPkg)
	b.L("return %s.Config{ServiceName: %s}", obsPkg, gobuf.Quote(e.doc.API.Tracing.ServiceName))
	b.L("}")
	b.NL()

	return artifact("tracing.gen.go", b)
}
