package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/observe"
)

// The monitoring page's front door, which needs no database: whether it exists
// at all is decided by the environment, before anything is served.
//
// The generated configuration says the page is wanted and where it would
// listen; the password says whether this deployment serves it. With nothing in
// $RIG_MONITOR_PASSWORD there is no handler and no address — so the server this
// example runs opens no second port, rather than one that refuses, which would
// tell anybody scanning that there is a page here.
func TestTheMonitoringPageIsOffWithoutAPassword(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "")

	page := buildPage(t)
	if page.Unarmed() == "" {
		t.Fatal("with no password the page should say why it will serve nothing")
	}
	if page.Handler() != nil || page.Addr() != "" {
		t.Errorf("an unarmed page handed back %v at %q; both halves should be zero",
			page.Handler(), page.Addr())
	}
}

// With one, the page exists and asks for it. The password is compared against
// what the request carries, so the unauthenticated answer is 401 rather than
// the page.
func TestTheMonitoringPageAsksForThePassword(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "a password worth having")

	page := buildPage(t)
	if why := page.Unarmed(); why != "" {
		t.Fatalf("with a password the page should serve: %s", why)
	}

	// httptest picks the port here rather than binding the one rig.yaml names:
	// what is under test is the handler, and a suite that took 9084 would fail
	// on a machine already running `make demo`.
	srv := httptest.NewServer(page.Handler())
	t.Cleanup(srv.Close)

	res, err := srv.Client().Get(srv.URL + "/_rig/monitor/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("the page answered %d to an anonymous request, want 401", res.StatusCode)
	}
}

// The address rig.yaml names is what the page listens on, and it is loopback:
// this example runs wherever it is cloned, and a page listing every path,
// request id and error cause on that machine should not be on an interface
// somebody else can reach.
func TestTheMonitoringPageListensOnLoopback(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "a password worth having")
	t.Setenv(observe.AddrEnv, "")

	if got := buildPage(t).Addr(); got != "127.0.0.1:9084" {
		t.Errorf("the page listens on %q, want the 127.0.0.1:9084 in rig.yaml", got)
	}
}

// The tour's link has to be absolute now: the page is on a port of its own, so
// it is a different origin from the one the front end was served from, and the
// href it used to carry — /_rig/monitor — reaches the API instead.
func TestTheTourLinksAcrossTheOrigin(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "a password worth having")
	t.Setenv(observe.AddrEnv, "")

	got := app.MonitorURL(buildPage(t))
	if !strings.HasPrefix(got, "http://127.0.0.1:9084/") {
		t.Errorf("the tour link is %q, want an absolute URL at the page's own port", got)
	}
	if !strings.HasSuffix(got, "/_rig/monitor/") {
		t.Errorf("the tour link is %q, want the base path rig.yaml named", got)
	}
}

// A wildcard bind is a valid thing to listen on and not a valid thing to put in
// an href. The browser asking is on this machine by construction — it is
// reading a page this process served.
func TestTheTourLinkRewritesAWildcardBind(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "a password worth having")
	t.Setenv(observe.AddrEnv, ":9084")

	if got := app.MonitorURL(buildPage(t)); !strings.HasPrefix(got, "http://localhost:9084/") {
		t.Errorf("the tour link is %q, want localhost in place of the wildcard", got)
	}
}

// No page, no link. A nav item that leads nowhere is worse than no nav item,
// and this is the ordinary case on a laptop.
func TestTheTourHasNoLinkWithoutAPage(t *testing.T) {
	t.Setenv("RIG_MONITOR_PASSWORD", "")

	if got := app.MonitorURL(buildPage(t)); got != "" {
		t.Errorf("an unarmed page produced the link %q", got)
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
