package rigclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// The collection endpoints share one helper, and each of them is still its own
// span. Six methods behind one line is exactly the shape that ends up filed
// under "GET" if the name is left to the fallback.
func TestTheAuthCollectionsAreNamedSeparately(t *testing.T) {
	var rec recorder

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{
		BaseURL: srv.URL,
		Trace:   rec.trace,
	}, rigclient.API{BasePath: "/api/v1", Auth: &profile})
	if err != nil {
		t.Fatal(err)
	}

	auth := rt.Auth()
	for _, call := range []struct {
		want string
		run  func() error
	}{
		{"authTenants", func() error { _, err := auth.Tenants(t.Context()); return err }},
		{"authMyTenants", func() error { _, err := auth.MyTenants(t.Context(), "identity"); return err }},
		{"authMyInvitations", func() error { _, err := auth.MyInvitations(t.Context(), "identity"); return err }},
		{"authInvitations", func() error { _, err := auth.Invitations(t.Context()); return err }},
		{"authSessions", func() error { _, err := auth.Sessions(t.Context()); return err }},
		{"authAPIKeys", func() error { _, err := auth.APIKeys(t.Context()); return err }},
	} {
		rec.spans = nil
		if err := call.run(); err != nil {
			t.Fatalf("%s: %v", call.want, err)
		}
		if len(rec.spans) == 0 || rec.spans[0] != call.want {
			t.Errorf("spans = %v, want the call named %q", rec.spans, call.want)
		}
	}
}
