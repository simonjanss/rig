package apibase_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simonjanss/rig/runtime/apibase"
	"github.com/simonjanss/rig/runtime/httpx"
)

// spanRecorder is an [apibase.Tracer] that remembers the route each request was
// named by, and what the span was told it answered.
type spanRecorder struct {
	routes []string
	ended  int
	status []int
}

func (s *spanRecorder) Server(r *http.Request, route string, status func() int) (*http.Request, func()) {
	s.routes = append(s.routes, route)
	return r, func() {
		s.ended++
		if status != nil {
			s.status = append(s.status, status())
		}
	}
}

func (s *spanRecorder) TraceID(*http.Request) string     { return "" }
func (s *spanRecorder) Fail(context.Context, int, error) {}

// A span is named by the pattern its route was registered under, whoever
// registered it. That is the whole point: the authentication routes, the inbox,
// the shapes and the OpenAPI document are mounted by packages a generator never
// sees, and a route registered through this is named exactly as a generated one
// is.
func TestTracingNamesEveryRouteByItsPattern(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	rec := &spanRecorder{}
	routes := apibase.Tracing(mux, rec)

	routes.HandleFunc("GET /api/v1/todos/{id}", func(http.ResponseWriter, *http.Request) {})
	routes.Handle("POST /auth/login", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/todos/7", nil),
		httptest.NewRequest(http.MethodPost, "/auth/login", nil),
	} {
		mux.ServeHTTP(httptest.NewRecorder(), r)
	}

	want := []string{"GET /api/v1/todos/{id}", "POST /auth/login"}
	if len(rec.routes) != len(want) {
		t.Fatalf("opened %d spans, want %d: %q", len(rec.routes), len(want), rec.routes)
	}
	for i, w := range want {
		if rec.routes[i] != w {
			t.Errorf("span %d is named %q, want %q", i, rec.routes[i], w)
		}
	}
	if rec.ended != len(want) {
		t.Errorf("%d of %d spans were ended", rec.ended, len(want))
	}
}

// The wildcards still reach the handler. A wrapper that dispatched the request
// itself — which is what asking the mux for the handler and calling it would
// be — would leave every r.PathValue empty, on every route with an identifier
// in it.
func TestTracingStillFillsInThePathValues(t *testing.T) {
	t.Parallel()

	var got string
	mux := http.NewServeMux()
	apibase.Tracing(mux, &spanRecorder{}).
		HandleFunc("GET /api/v1/todos/{id}", func(_ http.ResponseWriter, r *http.Request) {
			got = r.PathValue("id")
		})

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/todos/7", nil))

	if got != "7" {
		t.Errorf(`the handler saw id=%q, want "7"`, got)
	}
}

// The span learns what was answered, which is why each route gets a recorder in
// front of it rather than the writer passed straight through.
func TestTracingTellsTheSpanWhatWasAnswered(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	rec := &spanRecorder{}
	apibase.Tracing(mux, rec).HandleFunc("GET /teapot", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/teapot", nil))

	if len(rec.status) != 1 || rec.status[0] != http.StatusTeapot {
		t.Errorf("the span was told %v, want [%d]", rec.status, http.StatusTeapot)
	}
}

// Nil is a project that never set `tracing:`, and it pays for nothing — not
// even a wrapper per route. What it registers through is the mux itself.
func TestTracingWithoutATracerIsTheRouterItself(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	if got := apibase.Tracing(mux, nil); got != httpx.Router(mux) {
		t.Error("a nil tracer wrapped the mux anyway")
	}
}

// The mux is left alone, which is what makes Register able to go on returning
// one: a caller that mounts its own catch-all on what comes back is not
// competing with a wrapper that already took "/".
func TestTracingLeavesTheCatchAllForTheCaller(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	rec := &spanRecorder{}
	apibase.Tracing(mux, rec).HandleFunc("GET /api/v1/todos", func(http.ResponseWriter, *http.Request) {})

	var served bool
	// The line that used to panic: a wrapper around the mux had registered "/"
	// on it already, and http.ServeMux refuses a duplicate pattern.
	mux.Handle("/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/index.html", nil))

	if !served {
		t.Error("the caller's own catch-all did not answer")
	}
	if len(rec.routes) != 0 {
		t.Errorf("spans %q, want none for a route rig did not register", rec.routes)
	}
}
