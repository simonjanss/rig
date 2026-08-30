//go:build docker

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/examples/linearlite/internal/generated/api"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/serve"
)

// The shape routes, against a real sync service when one is around.
//
// Gated on $ELECTRIC_URL rather than assumed, because `make examples` hands
// the suite a database and nothing else; `rig db up` in this directory starts
// the sync service and a developer exports the URL it printed. The refusal
// below runs either way — it is the proxy's own answer and needs no upstream.
func TestTheShapeRoutes(t *testing.T) {
	api := newServer(t)
	api.seed(t)

	t.Run("a subscriber has to be somebody", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/todo/_stream?offset=-1",
			// Presence too, and it is the one where a 404 rather than a 401
			// would be the symptom of a real mistake: rig_presence has no REST
			// surface in this project, so the shape route is the whole read
			// path and a table left out of the document would leave nothing
			// here at all.
			"/api/v1/rig_presence/_stream?offset=-1&scope=board",
		} {
			// The identifier the caller wants this correlated by. A shape route
			// used to refuse through an error writer of its own that was handed
			// no request context and so could not echo one at all; it goes
			// through the same mapper as every other route now, which is the
			// whole of what mounting these beside the API bought.
			const correlate = "shape-refusal-1"

			res := api.do(t, request{
				method:  http.MethodGet,
				path:    path,
				headers: map[string]string{"X-Request-Id": correlate},
			})
			if res.status != http.StatusUnauthorized {
				t.Errorf("an anonymous stream of %s: %d %s, want 401", path, res.status, res.body)
			}

			var body struct {
				Code      string `json:"code"`
				RequestID string `json:"requestId"`
			}
			res.decode(t, &body)
			if body.Code == "" {
				t.Errorf("a refused stream of %s carries no error code: %s", path, res.body)
			}
			if body.RequestID != correlate {
				t.Errorf("a refused stream of %s came back with requestId %q, want the caller's %q: %s",
					path, body.RequestID, correlate, res.body)
			}
		}
	})

	if os.Getenv("ELECTRIC_URL") == "" {
		t.Skip("no $ELECTRIC_URL: run `rig db up` here and export the sync URL it prints to run the live half")
	}

	token := api.login(t, app.SeedEmail)
	for _, path := range []string{
		"/api/v1/todo/_stream?offset=-1",
		// The scope is what services/rig_presence narrows on, so this is also
		// the only thing that runs that stub against a real sync service.
		"/api/v1/rig_presence/_stream?offset=-1&scope=board",
	} {
		res := api.do(t, request{method: http.MethodGet, path: path, token: token})
		if res.status != http.StatusOK {
			t.Fatalf("the live shape %s: %d %s", path, res.status, res.body)
		}
		// The resume cursor is the protocol: a proxy that swallowed it would
		// stream once and never again.
		if res.headers.Get("electric-handle") == "" {
			t.Errorf("no electric-handle header on %s; the client cannot resume: %v", path, res.headers)
		}
	}
}

// The board still fills when the sync service is not there.
//
// This one needs no sync service, which is the point of it: it runs in `make
// examples`, where there has never been one, and it is the only test in the
// repository that drives the wiring in main.go rather than the proxy directly.
// $ELECTRIC_URL is pointed at a closed port so the answer cannot come from a
// service a developer happens to have running.
func TestTheBoardSurvivesTheSyncServiceBeingDown(t *testing.T) {
	t.Setenv("ELECTRIC_URL", "http://127.0.0.1:1")

	api := newServer(t)
	api.seed(t)
	token := api.login(t, app.SeedEmail)

	res := api.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/todo/_stream?offset=-1",
		token:  token,
	})
	if res.status != http.StatusOK {
		t.Fatalf("the board did not load without the sync service: %d %s", res.status, res.body)
	}
	if got := res.headers.Get("X-Rig-Sync-Fallback"); got != "snapshot" {
		t.Fatalf("X-Rig-Sync-Fallback = %q, so this was not the fallback", got)
	}

	// The seed puts a board's worth of rows in, and the snapshot is the read
	// that would have answered a list.
	var out []struct {
		Key     string         `json:"key"`
		Value   map[string]any `json:"value"`
		Headers map[string]any `json:"headers"`
	}
	if err := json.Unmarshal([]byte(res.body), &out); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if len(out) < 2 {
		t.Fatalf("got %d messages, want rows and a control message: %s", len(out), res.body)
	}
	if out[0].Value["title"] == nil {
		t.Errorf("the first row carries no title: %v", out[0].Value)
	}
	if out[0].Value["tenant_id"] == nil {
		t.Errorf("the first row carries no tenant, so the read was not scoped: %v", out[0].Value)
	}
	// Without this a subscriber stays in its loading state holding every row.
	if last := out[len(out)-1].Headers; last["control"] != "up-to-date" {
		t.Errorf("the snapshot does not end up-to-date: %v", last)
	}

	// A subscriber the snapshot already reached is asked to wait rather than
	// handed a second one.
	handle := res.headers.Get("electric-handle")
	again := api.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/todo/_stream?offset=0_inf&live=true&handle=" + handle,
		token:  token,
	})
	if again.status != http.StatusServiceUnavailable {
		t.Errorf("the poll after the snapshot: %d, want 503", again.status)
	}

	// And the tab that was already streaming when the service went: it is
	// resuming from a handle real sync gave it, which is neither of the two
	// above. It is told to start again, because the request after a must-refetch
	// is a read from the beginning and that is the one a snapshot answers.
	//
	// This is the case the board is actually in when somebody stops the sync
	// service while looking at it, and the answer used to be a 502 per poll for
	// as long as the outage lasted — a board that only a reload could rescue, on
	// a page nobody reloads because the rows are still on the screen.
	resumed := api.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/todo/_stream?offset=0_0&live=true&handle=21872282-1787670276304776",
		token:  token,
	})
	if resumed.status != http.StatusConflict {
		t.Errorf("a subscription resuming on a real handle: %d %s, want 409", resumed.status, resumed.body)
	}
	if !strings.Contains(resumed.body, "must-refetch") {
		t.Errorf("no must-refetch in %s", resumed.body)
	}
	// A client reads the handle to start again with off this response, and warns
	// about a proxy stripping headers when there is none.
	if resumed.headers.Get("electric-handle") == "" {
		t.Errorf("no handle to resume with: %v", resumed.headers)
	}

	// The other three screens' shapes answer too, and each is a different read:
	// the trash is ListDeleted, the history is ListSnapshots on one id, and the
	// bell is a List the repository narrows to this account.
	for _, path := range []string{
		"/api/v1/todo/_deleted/_stream?offset=-1",
		"/api/v1/rig_notification_recipient/_stream?offset=-1",
	} {
		got := api.do(t, request{method: http.MethodGet, path: path, token: token})
		if got.status != http.StatusOK {
			t.Errorf("%s without the sync service: %d %s", path, got.status, got.body)
		}
		if fb := got.headers.Get("X-Rig-Sync-Fallback"); fb != "snapshot" {
			t.Errorf("%s: X-Rig-Sync-Fallback = %q, so this was not the fallback", path, fb)
		}
	}

	// The history shape needs a real id, and the seeded board has one with
	// versions. Any todo will do — a row with no versions is an empty snapshot,
	// which is still a snapshot rather than a 502.
	var board []struct {
		Value map[string]any `json:"value"`
	}
	res.decode(t, &board)
	if len(board) > 0 && board[0].Value["id"] != nil {
		id, _ := board[0].Value["id"].(string)
		versions := api.do(t, request{
			method: http.MethodGet,
			path:   "/api/v1/todo/" + id + "/_versions/_stream?offset=-1",
			token:  token,
		})
		if versions.status != http.StatusOK {
			t.Errorf("the history shape without the sync service: %d %s", versions.status, versions.body)
		}
		if fb := versions.headers.Get("X-Rig-Sync-Fallback"); fb != "snapshot" {
			t.Errorf("the history shape: X-Rig-Sync-Fallback = %q", fb)
		}
	}

	// And presence, which rig gives no fallback on purpose, still refuses: a
	// snapshot of who was here a moment ago and then stopped updating is worth
	// less than nothing. Nothing in this project says so — applyPresenceTable
	// does — which makes this the guard on that decision rather than on a line
	// somebody wrote in main.go.
	presence := api.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/rig_presence/_stream?offset=-1&scope=board",
		token:  token,
	})
	if presence.status != http.StatusBadGateway {
		t.Errorf("presence answered %d, want the 502 it has no fallback for", presence.status)
	}
	// Including for a resumed subscription, which is the half worth asserting
	// separately: sending it to refetch would cost it the rows it is holding and
	// then refuse the read anyway.
	presenceResumed := api.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/rig_presence/_stream?offset=0_0&live=true&scope=board&handle=21872282-1787670276304776",
		token:  token,
	})
	if presenceResumed.status != http.StatusBadGateway {
		t.Errorf("a resumed presence subscription answered %d, want 502", presenceResumed.status)
	}
}

// The switch the demonstration stops the sync service with.
//
// The outage itself is the test above; this one is about the route, and mostly
// about the two answers it gives when it should not work: no container named in
// the environment and there is no route at all, and no session and there is no
// answer. That first one is the security property — the handler shells out to
// `docker stop`, so a build that was never told which container to touch must
// not offer the endpoint — and a property nothing else would notice breaking.
//
// What is not here is a container actually being stopped. `make examples` brings
// up a database and has never had a sync service, so there is nothing to stop;
// the container's lifecycle is internal/cli's suite, and the button is the
// person clicking it.
func TestTheSyncSwitch(t *testing.T) {
	t.Run("is not there when no container is named", func(t *testing.T) {
		t.Setenv(app.SyncSwitchEnv, "")

		api := newServer(t)
		api.seed(t)
		token := api.login(t, app.SeedEmail)

		// Not 401, not 403: the route does not exist, which is what keeps a
		// scan from learning this process can reach a container engine.
		res := api.do(t, request{method: http.MethodGet, path: "/_demo/sync", token: token})
		if res.status != http.StatusNotFound {
			t.Errorf("GET /_demo/sync: %d %s, want 404", res.status, res.body)
		}
		if sync := api.tourSync(t, token); sync {
			t.Error("the tour offers a switch this build did not mount")
		}
	})

	t.Run("answers a session and nobody else", func(t *testing.T) {
		if !hasContainerEngine() {
			t.Skip("no docker or podman on PATH: the switch mounts nothing without one")
		}
		// A name no container has. The route then reports honestly rather than
		// touching anything — the same answer a checkout that has never run
		// `rig db up` would get, and the reason this test needs no sync service.
		t.Setenv(app.SyncSwitchEnv, "linearlite-electric-no-such-container")

		api := newServer(t)
		api.seed(t)

		anon := api.do(t, request{method: http.MethodGet, path: "/_demo/sync"})
		if anon.status != http.StatusUnauthorized {
			t.Errorf("an anonymous GET /_demo/sync: %d %s, want 401", anon.status, anon.body)
		}

		token := api.login(t, app.SeedEmail)
		if sync := api.tourSync(t, token); !sync {
			t.Error("the tour hides a switch this build did mount")
		}

		res := api.do(t, request{method: http.MethodGet, path: "/_demo/sync", token: token})
		if res.status != http.StatusOK {
			t.Fatalf("GET /_demo/sync: %d %s", res.status, res.body)
		}
		var state app.SyncState
		res.decode(t, &state)
		if state.Container != "missing" {
			t.Errorf("container = %q, want \"missing\" for one that was never created", state.Container)
		}
		// The port this process forwards shapes to, read back off the URL the
		// proxy was built with — rig.yaml's, since no $ELECTRIC_URL is set here.
		// It is on the wire so the front end can say which two numbers
		// disagree when a restarted container comes back somewhere else.
		if state.Upstream == "" {
			t.Error("no upstream port reported; a moved container could not be named")
		}
		if state.Moved {
			t.Errorf("moved, with nothing published: %+v", state)
		}
	})
}

// tourSync is what /_demo/tour says about the switch, which is what decides
// whether the front end renders it.
func (s *server) tourSync(t *testing.T, token string) bool {
	t.Helper()
	res := s.do(t, request{method: http.MethodGet, path: "/_demo/tour", token: token})
	if res.status != http.StatusOK {
		t.Fatalf("tour: %d %s", res.status, res.body)
	}
	var tour struct {
		Sync bool `json:"sync"`
	}
	res.decode(t, &tour)
	return tour.Sync
}

func hasContainerEngine() bool {
	for _, bin := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	return false
}

// The boot check says which it found, and — with electric_required off, which
// is what this project generates — starts either way.
//
// It drives api.Mount rather than app.New, because Mount is where the check
// lives: app.New builds the proxy and Mount is what asks it anything. That is
// also the only difference between this and the test above, which goes through
// app.New and therefore never reaches the check at all.
func TestTheBootSaysWhetherTheSyncServiceIsThere(t *testing.T) {
	pool := testPool(t)

	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		// A closed port rather than an absent variable, so the answer cannot
		// come from a sync service a developer happens to have running.
		{"not there", "http://127.0.0.1:1", "the sync service is not answering"},
		{"there", electricURL(), "the sync service is answering"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "there" && os.Getenv("ELECTRIC_URL") == "" {
				t.Skip("no $ELECTRIC_URL: run `rig db up` here and export the sync URL it prints")
			}
			t.Setenv("ELECTRIC_URL", tc.url)

			var said strings.Builder
			log := slog.New(slog.NewTextHandler(&said, &slog.HandlerOptions{Level: slog.LevelInfo}))
			lifecycle := &serve.App{Pool: pool, Logger: log}

			handler, err := api.Mount(func(ctx context.Context, a *serve.App, _ *observe.Page) (api.Parts, error) {
				return app.New(ctx, app.Config{
					Pool: pool, Logger: log, App: a, ElectricURL: tc.url,
				})
			})(context.Background(), lifecycle)
			// Not a refusal: this project did not say live sync is required, and
			// every shape here has a fallback.
			if err != nil {
				t.Fatalf("the server did not start without the sync service: %v", err)
			}
			if handler == nil {
				t.Fatal("no handler")
			}
			if !strings.Contains(said.String(), tc.want) {
				t.Errorf("the boot never said %q:\n%s", tc.want, said.String())
			}
		})
	}
}
