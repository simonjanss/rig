//go:build docker

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/examples/todo/services/todo"
	"github.com/simonjanss/rig/runtime/apirev"
)

// The revision, end to end, against the server this example actually runs.
//
// The question the whole feature exists to answer is "how old is the oldest
// client still calling", and the answer arrives at a service method rather than
// in a log line rig writes: rig has no logger, so it puts what it knows where
// the application's own logging can already reach it.
func TestTheRevisionTravelsBothWays(t *testing.T) {
	pool := openPool(t)
	tenant := newTenant(t, pool)

	srv := httptest.NewServer(newHandler(pool, nil))
	t.Cleanup(srv.Close)

	t.Run("the server announces its own", func(t *testing.T) {
		res := do(t, srv, tenant, "GET", "/api/v1/todos", "")
		defer res.Body.Close()

		if got := res.Header.Get(api.RevisionHeader); got != api.Revision {
			t.Errorf("%s = %q, want %q", api.RevisionHeader, got, api.Revision)
		}
	})

	// Including on the way out of a failure: a client that is behind should not
	// have to make a successful request to find out.
	t.Run("even when the request fails", func(t *testing.T) {
		req, err := http.NewRequest("GET", srv.URL+"/api/v1/todos/"+uuid.New().String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-Id", tenant.String())

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", res.StatusCode)
		}
		if got := res.Header.Get(api.RevisionHeader); got != api.Revision {
			t.Errorf("a 404 carried %s = %q, want %q", api.RevisionHeader, got, api.Revision)
		}
	})
}

// Stale is the reading of the header, and it is deliberately quiet about the
// two ways a caller can decline to say anything.
func TestStaleReportsTheGap(t *testing.T) {
	server, err := time.Parse("2006-01-02", api.Revision)
	if err != nil {
		t.Fatalf("this example has no revision recorded: %v", err)
	}
	old := server.AddDate(0, 0, -90).Format("2006-01-02")

	for _, tc := range []struct {
		name   string
		sent   string
		wantOK bool
		want   time.Duration
	}{
		{"ninety days behind", old, true, 90 * 24 * time.Hour},
		{"exactly current", api.Revision, false, 0},
		{"ahead of the server", server.AddDate(0, 0, 1).Format("2006-01-02"), false, 0},
		{"said nothing", "", false, 0},
		{"said nonsense", "yesterday-ish", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := api.RequestContext{ClientRevision: tc.sent}

			gap, ok := rc.Stale()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && gap != tc.want {
				t.Errorf("gap = %s, want %s", gap, tc.want)
			}
		})
	}
}

// BuiltBefore is what a compatibility shim is written in terms of, and the
// decision it encodes: a caller rig cannot place on the timeline is served the
// current behavior rather than a shim nobody can show it needs.
func TestBuiltBeforeReadsAnUnknownCallerAsCurrent(t *testing.T) {
	cutoff := apirev.MustParse("2026-04-30")

	for _, tc := range []struct {
		name string
		sent string
		want bool
	}{
		{"built before the cutoff", "2026-03-01", true},
		{"built on the cutoff", "2026-04-30", false},
		{"built after it", "2026-05-01", false},
		{"said nothing", "", false},
		{"said nonsense", "yesterday-ish", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := api.RequestContext{ClientRevision: tc.sent}
			if got := rc.BuiltBefore(cutoff); got != tc.want {
				t.Errorf("BuiltBefore = %v, want %v", got, tc.want)
			}
		})
	}

	// And an unset cutoff is before nothing, so a shim nobody has dated yet
	// fires for nobody rather than for everybody.
	rc := api.RequestContext{ClientRevision: "2020-01-01"}
	if rc.BuiltBefore(apirev.Revision{}) {
		t.Error("an unset cutoff refused a caller")
	}
}

// A validator and a hook are handed a context and nothing else. This is how
// what only the service method is given reaches them.
func TestTheRequestContextTravelsOnTheContext(t *testing.T) {
	pool := openPool(t)
	tenant := newTenant(t, pool)

	var seen api.RequestContext
	handler := api.Register(api.Handlers{
		Server: api.Server{
			GetClaims: headerClaims,
			Context: func(ctx context.Context, _ *http.Request) context.Context {
				// The server puts it on before this hook runs, which is the
				// only ordering that lets a hook build on it.
				seen, _ = api.RequestContextFrom(ctx)
				return ctx
			},
		},
		Todo: todo.New(store.New(pool, store.Config{}).Todos, api.NewFiles(pool), nil, nil, pool, nil),
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest("GET", srv.URL+"/api/v1/todos", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-Id", tenant.String())
	req.Header.Set(api.RevisionHeader, "2026-03-01")

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if seen.ClientRevision != "2026-03-01" {
		t.Errorf("ClientRevision on the context = %q, want what the caller sent", seen.ClientRevision)
	}
	if !seen.BuiltBefore(apirev.MustParse("2026-04-30")) {
		t.Error("a hook cannot tell that the caller is behind a cutoff")
	}
}

// Nothing put one there, and that is not a failure: a migration and a
// background job are not requests.
func TestRequestContextFromSaysSoWhenThereIsNone(t *testing.T) {
	if _, ok := api.RequestContextFrom(context.Background()); ok {
		t.Error("a bare context reported request metadata")
	}
}

// MinRevision is the endgame: you removed a field, you waited, the logs said
// nobody old was left, and now the door closes. Only on the callers that can be
// shown to be behind it, though.
func TestMinRevisionClosesTheDoor(t *testing.T) {
	pool := openPool(t)
	tenant := newTenant(t, pool)

	server, err := time.Parse("2006-01-02", api.Revision)
	if err != nil {
		t.Fatalf("this example has no revision recorded: %v", err)
	}

	srv := httptest.NewServer(handlerWithMinRevision(pool, apirev.MustParse(api.Revision)))
	t.Cleanup(srv.Close)

	get := func(t *testing.T, revision string) int {
		t.Helper()

		req, err := http.NewRequest("GET", srv.URL+"/api/v1/todos", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-Id", tenant.String())
		if revision != "" {
			req.Header.Set(api.RevisionHeader, revision)
		}

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	if got := get(t, server.AddDate(0, 0, -1).Format("2006-01-02")); got != http.StatusUpgradeRequired {
		t.Errorf("a client one day too old got %d, want 426", got)
	}
	if got := get(t, api.Revision); got != http.StatusOK {
		t.Errorf("a current client got %d, want 200", got)
	}

	// A curl says nothing, and saying nothing is not the same as being old.
	// Refusing it would make setting MinRevision an outage rather than a
	// deprecation.
	if got := get(t, ""); got != http.StatusOK {
		t.Errorf("a client that sent no revision got %d, want 200", got)
	}
	if got := get(t, "not-a-date"); got != http.StatusOK {
		t.Errorf("a client that sent nonsense got %d, want 200", got)
	}
}

// handlerWithMinRevision is [newHandler] with the door closed, which is the one
// thing about it that is not the default.
func handlerWithMinRevision(pool *pgxpool.Pool, min apirev.Revision) http.Handler {
	return api.Register(api.Handlers{
		Server: api.Server{GetClaims: headerClaims, MinRevision: min},
		Todo:   todo.New(store.New(pool, store.Config{}).Todos, api.NewFiles(pool), nil, nil, pool, nil),
	})
}
