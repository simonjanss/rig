// Package observe is where OpenTelemetry lives, so that nothing else in rig has
// to depend on it.
//
// Every generated application imports rig/runtime, which has two direct
// dependencies and is meant to keep them. otel is a large decision — a
// dependency tree, an exporter, a collector somebody runs — and plenty of
// projects will trace some other way or not at all. So it is here, in a module
// of its own, and a project that does not set `tracing:` in its rig.yaml gets
// no span in its generated source, no import of this package, and no otel in
// its go.mod. Optional, in rig, means absent rather than switched off.
//
// # The three prices
//
// Logging is always on and has no configuration: the generated server writes
// the cause of every 500 whether or not anybody thought about it. Spans are the
// second price and are what `tracing: enabled` buys. Exporting them is the
// third, and it is a runtime decision rather than a generated one — see
// [Config].
//
// # Nothing configured is not nothing
//
// [Setup] with no endpoint and no file still installs a real provider, sampling
// nothing. Spans are not recorded and nothing is exported, and the trace and
// span identifiers are still real — which is the point, because the generated
// server's RequestID is one of them. A laptop with no collector still writes
// log lines carrying a trace id that will mean something when the same binary
// runs where there is one.
//
// # One span per function
//
// Everything here is arranged so that a span is opened at the top of a function
// and ended by a defer, and never ended anywhere else. [Trace] and [Call] take
// the work as a callback for exactly that reason: the stage becomes a function,
// so its span is that function's span, and no early return can skip the end.
// The generated code follows the same rule.
package observe

import (
	"cmp"
	"context"
	"errors"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope every span rig opens is attributed to.
// A collector groups by it, which is how "this span came from the framework"
// stays distinguishable from a span the application opened itself.
const ScopeName = "github.com/simonjanss/rig/observe"

// The environment variables [Config] falls back to. Both are read once, in
// [Setup], so a deployment configures where its spans go without a code change
// and without a flag rig would have to define.
const (
	// EndpointEnv is the OTLP collector, in the variable OpenTelemetry's own
	// tooling already reads.
	EndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// FileEnv is the span file, which is rig's and so is named for rig.
	FileEnv = "RIG_TRACE_FILE"
)

// DefaultFileMaxBytes is where a span file rotates. Eight mebibytes is a few
// hundred thousand spans, which is more than anybody reads and less than
// anybody notices — and the ceiling is twice this, because one rotated file is
// kept.
const DefaultFileMaxBytes int64 = 8 << 20

// Config says who this service is and where its spans go.
//
// Where they go is deliberately not in rig.yaml. The same binary runs on a
// laptop, in CI and in production, and only the last of those has a collector;
// a project that had to regenerate to stop exporting would be a project that
// exports from its test suite.
type Config struct {
	// ServiceName is what this application is called in a collector. The
	// generated Tracing() passes the API's name from rig.yaml, so nothing has
	// to be typed twice.
	ServiceName string

	// ServiceVersion is the build, if it knows. Empty leaves the attribute off
	// rather than sending "unknown", which sorts and groups as a version.
	ServiceVersion string

	// Endpoint is an OTLP/HTTP collector, for example
	// "http://localhost:4318". Empty falls back to $OTEL_EXPORTER_OTLP_ENDPOINT
	// and, failing that, to exporting nothing at all.
	//
	// Nothing is the ordinary case rather than the broken one. An exporter
	// pointed at a host that was never there retries, and the retry comes due
	// during shutdown — so the failure mode of getting this wrong is a slow
	// exit in exactly the environments nobody is watching.
	Endpoint string

	// File is a path to write finished spans to, one JSON object per line.
	// Empty falls back to $RIG_TRACE_FILE and then to writing none.
	//
	// It is the small deployment's answer to a collector it is not going to
	// run: the file survives a restart, which is when you most want to know
	// what the last few requests were doing, and it is the store rig's own
	// monitoring page reads.
	File string

	// FileMaxBytes is where File rotates, keeping one previous generation.
	// Zero means [DefaultFileMaxBytes]. An append-only file with no ceiling is
	// the thing that fills a disk at three in the morning.
	FileMaxBytes int64

	// SampleRatio is the fraction of traces recorded, between 0 and 1. Zero
	// means one — every trace — because zero is what a field nobody set holds,
	// and a project that wants no traces leaves Endpoint and File empty
	// instead.
	//
	// A trace started elsewhere is honoured rather than re-decided: the sampler
	// is parent-based, so a request that arrives already sampled is recorded
	// whatever this says.
	SampleRatio float64
}

// Provider is what [Setup] installed, kept only so that it can be shut down.
//
// Hand [Provider.Shutdown] to serve.App.CloseWithin with a limit of its own.
// The flush at exit is the one part of tracing that can hold a process open,
// and a limit there is the difference between a slow deploy and a stuck one.
type Provider struct {
	tp *sdktrace.TracerProvider
}

// Setup installs a tracer provider and the W3C propagator, and returns what to
// shut down.
//
// It is safe to call before anything else is constructed: a tracer taken from
// the global provider before this runs — which is what [Tracer] returns —
// starts delegating to the real one as soon as this installs it. So the order
// of the lines in a main function is not something anybody has to get right.
func Setup(ctx context.Context, cfg Config) (*Provider, error) {
	cfg.Endpoint = cmp.Or(cfg.Endpoint, os.Getenv(EndpointEnv))
	cfg.File = cmp.Or(cfg.File, os.Getenv(FileEnv))
	cfg.FileMaxBytes = cmp.Or(cfg.FileMaxBytes, DefaultFileMaxBytes)
	cfg.SampleRatio = cmp.Or(cfg.SampleRatio, 1)

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(serviceResource(cfg)),
		sdktrace.WithSampler(sampler(cfg)),
	}

	if cfg.Endpoint != "" {
		exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint))
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	if cfg.File != "" {
		// Opened here rather than at the first span, so a path that cannot be
		// written is a startup failure with a reason attached rather than a
		// file that silently never appears.
		exp, err := newFileExporter(cfg.File, cfg.FileMaxBytes)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return &Provider{tp: tp}, nil
}

// serviceResource is who this process says it is, on every span it exports.
//
// The version is appended rather than always set. An attribute holding the
// empty string is still an attribute, and the generated Tracing() leaves the
// version for the application to fill in — so setting it unconditionally would
// put an empty service.version on every span most projects ever export, and a
// collector grouping by it would file them under a version nobody stamped in.
func serviceResource(cfg Config) *resource.Resource {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	return resource.NewWithAttributes(semconv.SchemaURL, attrs...)
}

// sampler records nothing when there is nowhere to put it.
//
// Not a no-op provider, which is the other way to spend nothing: a no-op
// invents no identifiers, and the identifiers are useful on their own. They are
// what the request id in an error body and the request_id on every log line
// are, so a run with no collector still hands out something that will match a
// trace once there is one.
func sampler(cfg Config) sdktrace.Sampler {
	if cfg.Endpoint == "" && cfg.File == "" {
		return sdktrace.NeverSample()
	}
	if cfg.SampleRatio >= 1 {
		return sdktrace.AlwaysSample()
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))
}

// Shutdown flushes what has not been exported and stops the exporters.
//
// Nil-safe, so a main that failed to set tracing up can defer this without a
// branch first.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// Tracer is rig's tracer, from whatever provider is installed. It is what a
// generated store is handed and what everything here opens spans through.
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Trace runs f inside a span of its own.
//
// The callback is the whole point. A span opened this way is a function's span:
// it is ended by a defer inside this call, so no early return, no added branch
// and no panic can leave it open, and the call site never holds a span it could
// end twice. It is also where an error becomes a failed span, once, rather than
// at every place an error is returned.
//
// A nil tracer is [Tracer], so a repository that was constructed without one
// still runs.
func Trace(ctx context.Context, tracer trace.Tracer, name string, f func(context.Context) error) error {
	if tracer == nil {
		tracer = Tracer()
	}

	ctx, span := tracer.Start(ctx, name)
	defer span.End()

	err := f(ctx)
	if err != nil {
		record(span, err)
	}
	return err
}

// Call is [Trace] on rig's own tracer, shaped to fit rigclient.Config.Trace.
//
// That field is a function rather than an interface so that rigclient — which
// every generated client imports — depends on no tracing library at all. This
// is the value to put in it.
func Call(ctx context.Context, name string, f func(context.Context) error) error {
	return Trace(ctx, Tracer(), name, f)
}

// Fail records why a request is being refused, on whatever span the context is
// in.
//
// It does not end anything: the span belongs to the handler and is ended by the
// handler's defer. This is what the generated server calls from the one funnel
// every error response passes through.
//
// Only a 500 makes the span itself a failure. A 404, a 422 or a refused
// permission is a server that worked, and a trace where every not-found is red
// is a trace nobody reads — the same argument that puts those lines at debug
// rather than at error.
func Fail(ctx context.Context, status int, err error) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	span.RecordError(err)
	if status >= 500 {
		span.SetStatus(codes.Error, err.Error())
		// So [Span.End] leaves the reason alone. It runs after this and knows
		// only the status code, and a second SetStatus would replace "listing
		// todos: connection refused" with "Internal Server Error".
		if s := requestSpan(ctx); s != nil {
			s.failed = true
		}
	}
}

// record marks a span failed, with the reason.
func record(span trace.Span, err error) {
	if !span.IsRecording() {
		return
	}

	span.RecordError(err)
	if errors.Is(err, context.Canceled) {
		// The caller went away. Recorded, because a span that stops halfway
		// with no explanation is the thing you would go looking for, but not an
		// error: nothing here failed.
		return
	}
	span.SetStatus(codes.Error, err.Error())
}
