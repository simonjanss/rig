package observe

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// SpanRecord is one line of the span file: a finished span, as JSON.
//
// The format is rig's own rather than OTLP's JSON encoding, and the reason is
// what it is for. A collector is not reading this — a person is, with grep, or
// rig's own monitoring page. So it is flat, every identifier is a hex string
// that can be pasted into a search, and the fields somebody actually scans for
// are at the front of the line.
//
// It is exported because reading the file is a thing to do: decode a line into
// this and every field is named.
type SpanRecord struct {
	// Time is when the span ended, which is when the line was written and so
	// the order the file is in.
	Time time.Time `json:"time"`

	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	// ParentID is empty for the root of a trace — the request span, usually.
	ParentID string `json:"parent_id,omitempty"`

	Name string `json:"name"`
	Kind string `json:"kind"`

	Start time.Time `json:"start"`
	// DurationMS is milliseconds, as a float, because the interesting spans
	// here are shorter than one and an integer would round most of them to
	// zero.
	DurationMS float64 `json:"duration_ms"`

	// Status is "error" or absent. A span that succeeded says nothing, which is
	// what makes `grep '"status":"error"'` the first thing to try.
	Status string `json:"status,omitempty"`
	// Error is the status description — for rig's spans, the error that caused
	// the failure.
	Error string `json:"error,omitempty"`

	Service    string         `json:"service,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// fileExporter writes finished spans to a file, one JSON object per line.
//
// Behind the SDK's batch processor, so the write is off the request path: a
// handler that opened five spans does not wait for five lines to reach a disk.
// The file itself, and the ceiling on it, are [rotatingFile]'s — the same store
// the log file uses.
type fileExporter struct {
	f *rotatingFile
}

var _ sdktrace.SpanExporter = (*fileExporter)(nil)

// newFileExporter opens the file, creating the directory it lives in.
func newFileExporter(path string, max int64) (*fileExporter, error) {
	f, err := openRotating(path, max)
	if err != nil {
		return nil, err
	}
	return &fileExporter{f: f}, nil
}

// ExportSpans implements [go.opentelemetry.io/otel/sdk/trace.SpanExporter].
//
// The whole batch is encoded before any of it is written, so a record that
// cannot be marshalled — which a [SpanRecord] cannot, but the compiler does not
// know that — fails without having left half a batch on the disk.
func (e *fileExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	lines := make([][]byte, 0, len(spans))
	for _, span := range spans {
		line, err := json.Marshal(newSpanRecord(span))
		if err != nil {
			return err
		}
		lines = append(lines, append(line, '\n'))
	}
	return e.f.write(lines...)
}

// Shutdown implements [go.opentelemetry.io/otel/sdk/trace.SpanExporter].
func (e *fileExporter) Shutdown(context.Context) error { return e.f.close() }

// newSpanRecord flattens a finished span into what gets written.
func newSpanRecord(span sdktrace.ReadOnlySpan) SpanRecord {
	sc := span.SpanContext()

	rec := SpanRecord{
		Time:       span.EndTime(),
		TraceID:    sc.TraceID().String(),
		SpanID:     sc.SpanID().String(),
		Name:       span.Name(),
		Kind:       span.SpanKind().String(),
		Start:      span.StartTime(),
		DurationMS: float64(span.EndTime().Sub(span.StartTime())) / float64(time.Millisecond),
		Service:    resourceValue(span, semconv.ServiceNameKey),
	}

	if parent := span.Parent(); parent.IsValid() {
		rec.ParentID = parent.SpanID().String()
	}
	if span.Status().Code == codes.Error {
		rec.Status = "error"
		rec.Error = span.Status().Description
	}

	if attrs := span.Attributes(); len(attrs) > 0 {
		rec.Attributes = make(map[string]any, len(attrs))
		for _, kv := range attrs {
			rec.Attributes[string(kv.Key)] = kv.Value.AsInterface()
		}
	}

	return rec
}

// resourceValue reads one attribute off the span's resource.
func resourceValue(span sdktrace.ReadOnlySpan, key attribute.Key) string {
	res := span.Resource()
	if res == nil {
		return ""
	}
	for _, kv := range res.Attributes() {
		if kv.Key == key {
			return kv.Value.String()
		}
	}
	return ""
}
