package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/observe"
)

// The monitoring page's front door, which needs no database: whether it exists
// at all is decided by the environment, before anything is served.
//
// The generated configuration says the page is wanted; the password says
// whether this deployment serves it. With nothing in $RIG_MONITOR_PASSWORD
// nothing is mounted — not a route that refuses, which would tell anybody
// scanning that there is a page here, but no route at all.
func TestTheMonitoringPageIsOffWithoutAPassword(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "")

	page := buildPage(t)
	if page.Unarmed() == "" {
		t.Fatal("with no password the page should say why it will serve nothing")
	}

	mux := http.NewServeMux()
	page.Mount(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := srv.Client().Get(srv.URL + "/_rig/monitor/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unmounted page answered %d, want 404", res.StatusCode)
	}
}

// With one, the page exists and asks for it. The password is compared against
// what the request carries, so the unauthenticated answer is 401 rather than
// the page.
func TestTheMonitoringPageAsksForThePassword(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "a password worth having")

	page := buildPage(t)
	if why := page.Unarmed(); why != "" {
		t.Fatalf("with a password the page should mount: %s", why)
	}

	mux := http.NewServeMux()
	page.Mount(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := srv.Client().Get(srv.URL + "/_rig/monitor/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("mounted page answered %d to an anonymous request, want 401", res.StatusCode)
	}
}

// buildPage is what main does, minus the server: the generated configuration,
// over a provider exporting nowhere.
func buildPage(t *testing.T) *observe.Page {
	t.Helper()

	provider, err := observe.Setup(context.Background(), api.Tracing())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	page, err := provider.Page(api.Monitoring())
	if err != nil {
		t.Fatal(err)
	}
	return page
}
