package observe

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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
type fileExporter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

var _ sdktrace.SpanExporter = (*fileExporter)(nil)

// newFileExporter opens the file, creating the directory it lives in.
//
// Opened at setup rather than at the first span, so that a path nothing can
// write is a startup error naming the path, rather than a file that quietly
// never appears and a monitoring page that is quietly empty.
func newFileExporter(path string, max int64) (*fileExporter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	// Appending to what a previous run left, so the ceiling is on the file and
	// not on this process's share of it.
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	return &fileExporter{path: path, max: max, f: f, size: info.Size()}, nil
}

// ExportSpans implements [go.opentelemetry.io/otel/sdk/trace.SpanExporter].
func (e *fileExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.f == nil {
		return nil
	}

	for _, span := range spans {
		line, err := json.Marshal(newSpanRecord(span))
		if err != nil {
			return err
		}
		line = append(line, '\n')

		if err := e.rotateIfFull(int64(len(line))); err != nil {
			return err
		}

		n, err := e.f.Write(line)
		e.size += int64(n)
		if err != nil {
			return err
		}
	}
	return nil
}

// rotateIfFull moves the file aside when the next line would not fit.
//
// One generation is kept, so the disk cost is twice the cap and not more. The
// alternative — numbered generations, a count to configure — is a log rotation
// policy, and a deployment that wants one has one already and should point this
// somewhere it can see.
//
// Rotating before the line rather than after keeps the promise exact: no file
// ever exceeds the cap, so nobody has to reason about how much one span can
// overshoot by.
func (e *fileExporter) rotateIfFull(next int64) error {
	if e.size+next <= e.max {
		return nil
	}

	if err := e.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(e.path, e.path+rotatedSuffix); err != nil {
		return err
	}

	f, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// The file is gone and nothing is open: give up on writing rather than
		// retry per span, which would be one failed syscall per span forever.
		e.f = nil
		return err
	}

	e.f, e.size = f, 0
	return nil
}

// Shutdown implements [go.opentelemetry.io/otel/sdk/trace.SpanExporter].
func (e *fileExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.f == nil {
		return nil
	}

	err := e.f.Close()
	e.f = nil
	return err
}

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
