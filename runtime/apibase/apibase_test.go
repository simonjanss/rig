package apibase_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/apibase"
	"github.com/simonjanss/rig/runtime/apirev"
)

// tracer is an [apibase.Tracer] that records nothing and answers a fixed trace.
type tracer struct{ id string }

func (t tracer) Server(r *http.Request, _ string, _ func() int) (*http.Request, func()) {
	return r, func() {}
}
func (t tracer) TraceID(*http.Request) string     { return t.id }
func (t tracer) Fail(context.Context, int, error) {}

// TestStaleReadsBothSidesOffTheContext is the one behaviour that changed when
// this moved out of a generated package: the server's own revision used to be a
// package-level value and is now a field, so a context built by hand has it
// unset — and an unset one has to answer "not stale" rather than invent a
// distance from the zero time.
func TestStaleReadsBothSidesOffTheContext(t *testing.T) {
	server := apirev.MustParse("2026-08-27")

	t.Run("behind", func(t *testing.T) {
		rc := apibase.RequestContext{ClientRevision: "2026-08-20", ServerRevision: server}
		d, ok := rc.Stale()
		if !ok {
			t.Fatal("a caller a week behind is stale")
		}
		if want := 7 * 24 * time.Hour; d != want {
			t.Fatalf("distance = %v, want %v", d, want)
		}
	})

	t.Run("ahead is not stale", func(t *testing.T) {
		rc := apibase.RequestContext{ClientRevision: "2026-09-01", ServerRevision: server}
		if _, ok := rc.Stale(); ok {
			t.Fatal("a caller mid-deploy is ahead, not stale")
		}
	})

	t.Run("no server revision", func(t *testing.T) {
		rc := apibase.RequestContext{ClientRevision: "2026-08-20"}
		if _, ok := rc.Stale(); ok {
			t.Fatal("nothing to be behind, so nothing to report")
		}
	})
}

// TestRequestContextOf covers the two things the move made configurable: which
// header the revision travels in, and where a request identifier comes from
// when the caller sent none.
func TestRequestContextOf(t *testing.T) {
	t.Run("revision header defaults", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set(apibase.DefaultRevisionHeader, "2026-08-20")

		rc := apibase.RequestContextOf(apibase.Server{}, r)
		if rc.ClientRevision != "2026-08-20" {
			t.Fatalf("ClientRevision = %q, want the header rig defaults to", rc.ClientRevision)
		}
	})

	t.Run("revision header can be renamed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("X-Api-Rev", "2026-08-20")

		rc := apibase.RequestContextOf(apibase.Server{RevisionHeader: "X-Api-Rev"}, r)
		if rc.ClientRevision != "2026-08-20" {
			t.Fatalf("ClientRevision = %q, want the header this project named", rc.ClientRevision)
		}
	})

	t.Run("the caller's own identifier wins", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set(apibase.RequestIDHeader, "from-the-client")

		rc := apibase.RequestContextOf(apibase.Server{Tracer: tracer{id: "from-the-trace"}}, r)
		if rc.RequestID != "from-the-client" {
			t.Fatalf("RequestID = %q, want the client's own to be believed", rc.RequestID)
		}
	})

	t.Run("the trace is the fallback", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)

		rc := apibase.RequestContextOf(apibase.Server{Tracer: tracer{id: "from-the-trace"}}, r)
		if rc.RequestID != "from-the-trace" {
			t.Fatalf("RequestID = %q, want the trace", rc.RequestID)
		}
	})

	t.Run("untraced and unlabelled is empty", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)

		rc := apibase.RequestContextOf(apibase.Server{}, r)
		if rc.RequestID != "" {
			t.Fatalf("RequestID = %q, want nothing to correlate on", rc.RequestID)
		}
	})
}
