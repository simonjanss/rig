package auth_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// Assembling the foundation must not need a reachable database.
//
// A pool is lazy, and a server that refused to start because Postgres was slow
// to accept its first connection would be a worse thing than one that starts and
// reports itself unready. Everything New does is construction.
func TestNewDoesNotNeedTheDatabaseToBeUp(t *testing.T) {
	t.Parallel()

	front, err := auth.New(auth.Config{Pool: unconnected(t)})
	if err != nil {
		t.Fatalf("assembling: %v", err)
	}

	// And what it assembled is there, because the point of the façade is to save
	// the wiring rather than to hide the parts.
	parts := front.Parts()
	for _, missing := range []struct {
		what string
		nil  bool
	}{
		{"accounts", parts.Accounts == nil},
		{"sessions", parts.Sessions == nil},
		{"api keys", parts.APIKeys == nil},
		{"limiter", parts.Limiter == nil},
		{"stores", parts.Stores == nil},
	} {
		if missing.nil {
			t.Errorf("Parts has no %s", missing.what)
		}
	}
}

// The pool is the one thing this cannot default, and the error says why rather
// than leaving somebody to find a nil dereference.
func TestNewRefusesWithoutAPool(t *testing.T) {
	t.Parallel()

	_, err := auth.New(auth.Config{})
	if err == nil {
		t.Fatal("assembling without a pool should fail")
	}
	if !strings.Contains(err.Error(), "Pool") {
		t.Errorf("the error should name what is missing: %v", err)
	}
}

// The endpoints land where the documentation says they do, and on the caller's
// own mux — the same one the generated API is registered on.
func TestMountRegistersTheEndpoints(t *testing.T) {
	t.Parallel()

	front, err := auth.New(auth.Config{Pool: unconnected(t)})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	front.Mount(mux)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/auth/login"},
		{http.MethodPost, "/auth/logout"},
		{http.MethodPost, "/auth/refresh"},
		{http.MethodGet, "/auth/sessions"},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("%s %s is not routed", route.method, route.path)
		}
	}
}

// A base path is a single field, because a project that already publishes /api
// may want /api/auth and should not have to rebuild the handler to get it.
func TestTheBasePathMoves(t *testing.T) {
	t.Parallel()

	front, err := auth.New(auth.Config{Pool: unconnected(t), BasePath: "/api/v1/auth"})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	front.Mount(mux)

	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)); pattern == "" {
		t.Error("the moved login route is not routed")
	}
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/auth/login", nil)); pattern != "" {
		t.Error("the default path should not also be routed")
	}
}

// The default tenant resolver is a header, and it refuses rather than guesses.
func TestTenantFromHeader(t *testing.T) {
	t.Parallel()

	want := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.Header.Set(auth.TenantHeader, want.String())

	got, err := auth.TenantFromHeader(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("tenant = %s, want %s", got, want)
	}

	// Absent means unspecified, not wrong. Only login and a password reset ask,
	// and login signs somebody in to one of their own tenants when nothing
	// named one — which is what a single sign-in page needs, because a visitor
	// cannot say which tenants an address belongs to before the password has
	// been checked.
	req = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	id, err := auth.TenantFromHeader(req)
	if err != nil {
		t.Errorf("a missing header is not an error: %v", err)
	}
	if id != uuid.Nil {
		t.Errorf("a missing header should resolve to no tenant, got %s", id)
	}

	// Present and malformed is still refused: that is a caller getting it wrong
	// rather than a caller leaving it out, and a bad request says which header.
	req = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.Header.Set(auth.TenantHeader, "the-first-one")
	if _, err := auth.TenantFromHeader(req); rigerr.CodeOf(err) != rigerr.CodeBadRequest {
		t.Errorf("%v, want a bad request", err)
	}
}

// unconnected is a pool pointed at nothing.
//
// pgxpool connects lazily, so this is a valid pool that has never spoken to a
// database — which is exactly what a server has during construction.
func unconnected(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody:nothing@127.0.0.1:1/nowhere?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A custom base path moves every route, and the provider routes stay under it.
//
// They used to fall out from under the /oauth segment the moment anybody set a
// base: each package defaulted its own, so the provider routes landed beside login
// as GET <base>/{provider}/start — a wildcard next to a literal, matching far more
// than it should. The base is defaulted once now, in one place, and this is what
// says so.
func TestTheBasePathMovesEveryRouteTogether(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ base, login, provider string }{
		{"", "/auth/login", "/auth/oauth/google/start"},
		{"/api/auth", "/api/auth/login", "/api/auth/oauth/google/start"},
		// A trailing slash is somebody's copy-paste, not a different intent.
		{"/api/auth/", "/api/auth/login", "/api/auth/oauth/google/start"},
	} {
		front, err := auth.New(auth.Config{
			Pool:     &pgxpool.Pool{},
			BasePath: tc.base,
			OAuth: auth.OAuth{
				Providers:  []oauth.Provider{oauth.Google("id", "secret")},
				BaseURL:    "https://app.example.com",
				SigningKey: bytes.Repeat([]byte("k"), 32),
			},
		})
		if err != nil {
			t.Fatalf("base %q: %v", tc.base, err)
		}

		mux := http.NewServeMux()
		front.Mount(mux)

		for _, want := range []struct{ method, path string }{
			{http.MethodPost, tc.login},
			{http.MethodGet, tc.provider},
		} {
			req := httptest.NewRequest(want.method, "http://example.com"+want.path, nil)
			if _, pattern := mux.Handler(req); pattern == "" {
				t.Errorf("base %q: nothing serves %s %s", tc.base, want.method, want.path)
			}
		}

		// And nothing answers the shape the bug produced.
		req := httptest.NewRequest(http.MethodGet,
			"http://example.com"+strings.TrimRight(cmpOr(tc.base, "/auth"), "/")+"/google/start", nil)
		if _, pattern := mux.Handler(req); pattern != "" {
			t.Errorf("base %q: a provider route sits beside login, at %s", tc.base, pattern)
		}
	}
}

// cmpOr is cmp.Or, spelled out so the test reads without an import for one call.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
