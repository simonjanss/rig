package rigclient_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/simonjanss/rig/rigclient"
)

// recorder is the seam a project fills with observe.Call, standing in for a
// tracer that this module deliberately knows nothing about.
type recorder struct {
	// spans are the names, in the order they were opened. Nesting is not
	// recorded because the calls are sequential here; what each test asserts is
	// which spans there were.
	spans []string
	// depth is how deep the innermost span was, so a test can tell "one call
	// with attempts under it" from "three spans in a row".
	depth, maxDepth int
}

func (rec *recorder) trace(ctx context.Context, name string, f func(context.Context) error) error {
	rec.spans = append(rec.spans, name)
	rec.depth++
	rec.maxDepth = max(rec.maxDepth, rec.depth)
	defer func() { rec.depth-- }()

	return f(ctx)
}

// One call is one span, named by the operation, with the attempt underneath.
func TestACallIsASpanWithItsAttemptInside(t *testing.T) {
	var rec recorder

	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Trace: rec.trace})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Name: "getTodo", Method: http.MethodGet, Path: "/todos/1",
	}); err != nil {
		t.Fatal(err)
	}

	want := []string{"getTodo", "send GET"}
	if len(rec.spans) != len(want) || rec.spans[0] != want[0] || rec.spans[1] != want[1] {
		t.Fatalf("spans = %v, want %v", rec.spans, want)
	}
	if rec.maxDepth != 2 {
		t.Errorf("the attempt is not inside the call: depth %d", rec.maxDepth)
	}
}

// The fallback is the reason the call is traced rather than only the attempt. A
// proxy refuses QUERY, the client tries the alias, and what a trace should show
// is one search that took two goes — not two unexplained requests.
func TestAFallbackIsTwoAttemptsOfOneCall(t *testing.T) {
	var rec recorder

	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == rigclient.MethodQuery {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte(`{"items":[]}`))
	}), rigclient.Config{Trace: rec.trace})

	type page struct {
		Items []todo `json:"items"`
	}
	if _, err := rigclient.Do[page](t.Context(), rt, rigclient.Op{
		Name: "searchTodos", Method: rigclient.MethodQuery, Path: "/todos",
		Fallback: "/todos/_search",
	}); err != nil {
		t.Fatal(err)
	}

	want := []string{"searchTodos", "send QUERY", "send POST"}
	if len(rec.spans) != len(want) {
		t.Fatalf("spans = %v, want %v", rec.spans, want)
	}
	for i := range want {
		if rec.spans[i] != want[i] {
			t.Fatalf("spans = %v, want %v", rec.spans, want)
		}
	}
}

// An Op nobody named is still traced, under a name that cannot be one per
// identifier: the path here has the row's id substituted into it already.
func TestAnUnnamedOpIsTracedByItsMethod(t *testing.T) {
	var rec recorder

	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Trace: rec.trace})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos/8d1c0f9e",
	}); err != nil {
		t.Fatal(err)
	}

	if rec.spans[0] != http.MethodGet {
		t.Errorf("the call's span is %q, want the method", rec.spans[0])
	}
}

// A client that was never told about tracing does not trace, and nothing about
// this file's seam is reachable from it.
func TestWithoutTheSeamNothingIsTraced(t *testing.T) {
	var rec recorder

	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Name: "getTodo", Method: http.MethodGet, Path: "/todos/1",
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.spans) != 0 {
		t.Errorf("spans = %v on a client with no Trace", rec.spans)
	}
}
