package observe_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simonjanss/rig/observe"
)

// answered is what a generated handler passes: the Status method of the
// recorder it wraps every response in. A function rather than the writer
// itself, so that observe does not depend on rig/runtime — and so a test does
// not either.
func answered(status *int) func() int {
	return func() int { return *status }
}

// The route is the span's name, so a trace groups by endpoint rather than by
// every identifier that has ever appeared in a path.
func TestServerNamesTheSpanAfterTheRoute(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	status := 0
	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos/7f3a", nil)

	r, span := observe.Server(r, "GET /api/v1/todos/{id}", answered(&status))
	status = http.StatusOK
	span.End()

	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}

	got := spans[0]
	if got.Name != "GET /api/v1/todos/{id}" {
		t.Errorf("span name is %q; a name per identifier is what routes exist to avoid", got.Name)
	}
	if got.Attributes["http.route"] != "GET /api/v1/todos/{id}" {
		t.Errorf("http.route is %v", got.Attributes["http.route"])
	}
	if got.Attributes["url.path"] != "/api/v1/todos/7f3a" {
		t.Errorf("url.path is %v; the path still belongs on the span", got.Attributes["url.path"])
	}
	if got.Attributes["http.response.status_code"] != float64(http.StatusOK) {
		t.Errorf("status is %v, want 200", got.Attributes["http.response.status_code"])
	}
	if observe.TraceID(r) != got.TraceID {
		t.Errorf("TraceID(r) is %q but the span is %q", observe.TraceID(r), got.TraceID)
	}
}

// A handler that wrote nothing answered 200, which is net/http's rule. A span
// reporting the recorder's zero would be reporting a status no client ever saw.
func TestServerReadsAnUnwrittenStatusAsOK(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	status := 0
	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil)

	_, span := observe.Server(r, "GET /api/v1/todos", answered(&status))
	span.End()

	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	if spans[0].Attributes["http.response.status_code"] != float64(http.StatusOK) {
		t.Errorf("status is %v, want 200", spans[0].Attributes["http.response.status_code"])
	}
}

// A caller that arrives inside somebody else's trace stays in it. This is the
// whole reason the span is started from the header rather than from nothing.
func TestServerContinuesAnIncomingTrace(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil)
	r.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")

	r, span := observe.Server(r, "GET /api/v1/todos", nil)
	span.End()

	if observe.TraceID(r) != traceID {
		t.Errorf("TraceID(r) is %q, want the caller's %q", observe.TraceID(r), traceID)
	}

	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	if spans[0].TraceID != traceID {
		t.Errorf("the request started a trace of its own: %q", spans[0].TraceID)
	}
	if spans[0].ParentID != spanID {
		t.Errorf("parent is %q, want the caller's span %q", spans[0].ParentID, spanID)
	}
}

// A 500 is a failed span; a 404 is not. A trace where every not-found is red is
// a trace nobody reads, which is the same argument that keeps refusals at debug
// in the log.
func TestFailBlamesTheServerOnlyForItsOwnFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"internal", http.StatusInternalServerError, "error"},
		{"not found", http.StatusNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := spanFile(t)
			p := setup(t, observe.Config{ServiceName: "todo", File: path})

			status := 0
			r := httptest.NewRequest(http.MethodGet, "/api/v1/todos/7", nil)
			r, span := observe.Server(r, "GET /api/v1/todos/{id}", answered(&status))
			observe.Fail(r.Context(), tc.status, errors.New("listing todos: connection refused"))
			status = tc.status
			span.End()

			flush(t, p)

			spans := readSpans(t, path)
			if len(spans) != 1 {
				t.Fatalf("want one span, got %d", len(spans))
			}
			if spans[0].Status != tc.want {
				t.Errorf("status is %q, want %q", spans[0].Status, tc.want)
			}
		})
	}
}

// The cause survives the status. Fail knows why the request failed and End
// knows only that it was a 500, and End runs last — so an End that set the
// status again would replace the one thing on the span worth reading with the
// word for the status code.
func TestEndKeepsTheReasonFailRecorded(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	const cause = "listing todos: connection refused"

	status := 0
	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil)
	r, span := observe.Server(r, "GET /api/v1/todos", answered(&status))
	observe.Fail(r.Context(), http.StatusInternalServerError, errors.New(cause))
	status = http.StatusInternalServerError
	span.End()

	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	if spans[0].Error != cause {
		t.Errorf("the span says %q, want the cause %q", spans[0].Error, cause)
	}
}

// A 500 that reached the client without going through Fail is still a failed
// span. Nothing recorded a reason, so the status code is the only thing there
// is to say, and saying nothing would leave a red request with a green span.
func TestEndBlamesAFailureNobodyExplained(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})

	status := 0
	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil)
	_, span := observe.Server(r, "GET /api/v1/todos", answered(&status))
	status = http.StatusBadGateway
	span.End()

	flush(t, p)

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	if spans[0].Status != "error" {
		t.Errorf("a %d left the span at %q", http.StatusBadGateway, spans[0].Status)
	}
	if spans[0].Error != http.StatusText(http.StatusBadGateway) {
		t.Errorf("the span says %q, want the status text", spans[0].Error)
	}
}

// Without a span there is no trace id, and the generated server falls back to
// whatever it had. Nothing here may panic on a request nobody traced.
func TestTraceIDIsEmptyWithoutASpan(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil)
	if got := observe.TraceID(r); got != "" {
		t.Errorf("TraceID is %q on an untraced request", got)
	}
}
