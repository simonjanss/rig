package observe_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/simonjanss/rig/observe"
)

// setup installs a provider for one test and takes it down again.
//
// Sequential by construction: a tracer provider is global, so two tests that
// installed one at once would be reading each other's spans.
func setup(t *testing.T, cfg observe.Config) *observe.Provider {
	t.Helper()

	p, err := observe.Setup(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return p
}

// flush is how a test reads the file: export is batched, and shutting the
// provider down is what empties the batch. Shutting down twice is harmless,
// which is what leaves the cleanup above alone.
func flush(t *testing.T, p *observe.Provider) {
	t.Helper()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// spanFile is a temporary file to export to, and the lines that ended up in it.
func spanFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "spans.jsonl")
}

func readSpans(t *testing.T, path string) []observe.SpanRecord {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []observe.SpanRecord
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var rec observe.SpanRecord
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
			t.Fatalf("line is not a span record: %v\n%s", err, scan.Text())
		}
		out = append(out, rec)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A provider with nowhere to export to still hands out identifiers. That is the
// whole reason it is not a no-op: the trace id is the request id, and a laptop
// with no collector still wants one.
func TestSetupWithoutAnExporterStillMakesIdentifiers(t *testing.T) {
	setup(t, observe.Config{ServiceName: "todo"})

	_, span := observe.Tracer().Start(t.Context(), "unwatched")
	defer span.End()

	if !span.SpanContext().TraceID().IsValid() {
		t.Error("no trace id, so nothing to correlate a log line with")
	}
	if span.IsRecording() {
		t.Error("recording with nowhere to export to; that is the cost this avoids")
	}
}

func TestSpansReachTheFile(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	ctx, parent := observe.Tracer().Start(t.Context(), "GET /api/v1/todos")
	err := observe.Trace(ctx, nil, "repository.Todo.List", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	parent.End()

	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 2 {
		t.Fatalf("want the stage and the request, got %d spans", len(spans))
	}

	stage, request := spans[0], spans[1]
	if stage.Name != "repository.Todo.List" || request.Name != "GET /api/v1/todos" {
		t.Fatalf("wrong names: %q then %q", stage.Name, request.Name)
	}
	if stage.ParentID != request.SpanID {
		t.Errorf("the stage is not under the request: parent %q, request %q", stage.ParentID, request.SpanID)
	}
	if stage.TraceID != request.TraceID {
		t.Errorf("two traces where there should be one: %q and %q", stage.TraceID, request.TraceID)
	}
	if request.Service != "todo" {
		t.Errorf("service is %q, want the name from the configuration", request.Service)
	}
	if stage.Status != "" {
		t.Errorf("a stage that returned nil is not an error, got status %q", stage.Status)
	}
}

// The error is on the span that failed, and the call still returns it. Trace is
// a wrapper around work, not a replacement for handling what the work said.
func TestTraceRecordsTheError(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	want := errors.New("the validator refused")
	got := observe.Trace(t.Context(), nil, "repository.Todo.Create.Validator",
		func(context.Context) error { return want })

	if !errors.Is(got, want) {
		t.Fatalf("the error did not come back: %v", got)
	}
	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	if spans[0].Status != "error" || spans[0].Error != want.Error() {
		t.Errorf("span says %q / %q; want an error carrying the reason", spans[0].Status, spans[0].Error)
	}
}

// A cancelled request is not a failure. It is recorded, because a span that
// stops halfway with no explanation is what you would go looking for, but the
// server did not do anything wrong.
func TestTraceDoesNotBlameACancelledCaller(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	_ = observe.Trace(t.Context(), nil, "repository.Todo.List",
		func(context.Context) error { return context.Canceled })

	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	if spans[0].Status == "error" {
		t.Error("a cancelled caller was reported as a server failure")
	}
}

// Trace with no tracer of its own still works, which is what makes a repository
// constructed without one — a test, a migration, a seed — run unchanged.
func TestTraceTakesANilTracer(t *testing.T) {
	setup(t, observe.Config{ServiceName: "todo"})

	var inner trace.SpanContext
	err := observe.Trace(t.Context(), nil, "stage", func(ctx context.Context) error {
		inner = trace.SpanContextFromContext(ctx)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inner.SpanID().IsValid() {
		t.Error("the callback was not given the span's context")
	}
}

func TestSetupRefusesAFileItCannotWrite(t *testing.T) {
	// A directory where the file should be: opening it for writing fails, and
	// the point is that it fails now rather than at the first span.
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := observe.Setup(t.Context(), observe.Config{File: path}); err == nil {
		t.Fatal("setup accepted a span file it cannot write")
	}
}
