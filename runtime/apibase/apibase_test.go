package apibase_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/apibase"
	"github.com/simonjanss/rig/runtime/apirev"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
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
		r.Header.Set(apibase.DefaultRequestIDHeader, "from-the-client")

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

// noClaims is a GetClaims that refuses, for the two prepares to differ over.
func noClaims(*http.Request) (tenancy.Claims, error) {
	return tenancy.Claims{}, rigerr.Unauthorized("no credential")
}

// someClaims is a GetClaims that succeeds.
func someClaims(*http.Request) (tenancy.Claims, error) {
	return tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}, nil
}

// TestResolveAnnouncesTheRevision covers the half of the revision story that is
// telemetry: a client that is behind should not have to make a successful
// request to find out.
func TestResolveAnnouncesTheRevision(t *testing.T) {
	s := apibase.Server{
		GetClaims: someClaims,
		Revision:  apirev.MustParse("2026-08-27"),
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if _, _, _, ok := apibase.Resolve(s, w, r, true); !ok {
		t.Fatal("resolve refused a request it should have served")
	}

	if got := w.Header().Get(apibase.DefaultRevisionHeader); got != "2026-08-27" {
		t.Fatalf("announced %q, want the revision this server was generated from", got)
	}
}

// TestMinRevisionRefusesOnlyACallerThatSaidAndSaidOlder is the other half. Both
// ways of not refusing are the one comparison: an unset MinRevision and a caller
// that sent nothing each leave a side unknown, and nothing is before an unknown
// revision.
func TestMinRevisionRefusesOnlyACallerThatSaidAndSaidOlder(t *testing.T) {
	s := apibase.Server{
		GetClaims:   someClaims,
		Revision:    apirev.MustParse("2026-08-27"),
		MinRevision: apirev.MustParse("2026-08-01"),
	}

	cases := []struct {
		name   string
		client string
		served bool
	}{
		{"older is refused", "2026-07-01", false},
		{"the same is served", "2026-08-01", true},
		{"newer is served", "2026-09-01", true},
		{"said nothing", "", true},
		{"said nonsense", "banana", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			if c.client != "" {
				r.Header.Set(apibase.DefaultRevisionHeader, c.client)
			}

			_, _, _, ok := apibase.Resolve(s, w, r, true)
			if ok != c.served {
				t.Fatalf("served = %v, want %v (body %q)", ok, c.served, w.Body.String())
			}
			if !c.served && w.Code != http.StatusUpgradeRequired {
				t.Fatalf("status = %d, want 426", w.Code)
			}
		})
	}
}

// TestTheRequestContextGoesOnTheContextBeforeTheHook is an ordering the whole
// service layer rests on: a validator and a hook are handed a context and
// nothing else, so what the caller was built against has to be on it — and it
// has to be there before the Context hook, so that hook can build on it.
func TestTheRequestContextGoesOnTheContextBeforeTheHook(t *testing.T) {
	var sawRequest bool
	s := apibase.Server{
		GetClaims: someClaims,
		Context: func(ctx context.Context, _ *http.Request) context.Context {
			_, sawRequest = apibase.RequestContextFrom(ctx)
			return ctx
		},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx, _, _, ok := apibase.Resolve(s, w, r, true)
	if !ok {
		t.Fatal("resolve refused")
	}
	if !sawRequest {
		t.Error("the Context hook ran before the request context was on the context")
	}
	if _, found := apibase.RequestContextFrom(ctx); !found {
		t.Error("the request context is not on the context a service will be handed")
	}
}

// TestPreparePublicServesACallerWithNoCredential is the one difference between
// the two entry points, and the reason there are two.
func TestPreparePublicServesACallerWithNoCredential(t *testing.T) {
	s := apibase.Server{GetClaims: noClaims}

	t.Run("required refuses", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		if _, _, _, ok := apibase.Prepare(s, w, r); ok {
			t.Fatal("a caller with no credential was served on a private route")
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})

	t.Run("public serves", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		_, claims, _, ok := apibase.PreparePublic(s, w, r)
		if !ok {
			t.Fatalf("a public route refused a caller with no credential: %s", w.Body)
		}
		if claims.Valid() {
			t.Error("the caller is nobody, not somebody")
		}
	})
}

// TestAPreHookCanStopTheRequest is the middleware story: there is no wrapper
// around the mux, because the matched pattern is only known once net/http has
// dispatched.
func TestAPreHookCanStopTheRequest(t *testing.T) {
	var reached bool
	s := apibase.Server{
		GetClaims: func(*http.Request) (tenancy.Claims, error) { reached = true; return tenancy.Claims{}, nil },
		PreHooks: []func(http.ResponseWriter, *http.Request) bool{
			func(w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(http.StatusTeapot)
				return false
			},
		},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if _, _, _, ok := apibase.Prepare(s, w, r); ok {
		t.Fatal("a hook that wrote a response did not stop the request")
	}
	if reached {
		t.Error("the claims were looked up after a hook had already answered")
	}
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the hook's own", w.Code)
	}
}

// TestTheCallersRequestIDIsBounded keeps a header out of a log line.
//
// The value reaches an error body and every log line the request writes, so it
// is client-controlled text in two places read by machines. What a caller gets
// for sending nonsense is the identifier it would have got for sending nothing.
func TestTheCallersRequestIDIsBounded(t *testing.T) {
	cases := []struct {
		name string
		send string
		want string
	}{
		{"an ordinary identifier", "0b9c1e2f-4a5b", "0b9c1e2f-4a5b"},
		{"nothing", "", ""},
		{"too long", strings.Repeat("x", 129), ""},
		{"a newline", "one\ntwo", ""},
		{"a control byte", "one\x00two", ""},
		{"beyond ascii", "café", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			if c.send != "" {
				r.Header.Set(apibase.DefaultRequestIDHeader, c.send)
			}
			if got := apibase.CallerRequestID(r, ""); got != c.want {
				t.Fatalf("CallerRequestID = %q, want %q", got, c.want)
			}
		})
	}
}
