//go:build docker

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
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
			res := api.do(t, request{method: http.MethodGet, path: path})
			if res.status != http.StatusUnauthorized {
				t.Errorf("an anonymous stream of %s: %d %s, want 401", path, res.status, res.body)
			}
		}
	})

	if os.Getenv("ELECTRIC_URL") == "" {
		t.Skip("no $ELECTRIC_URL: run `rig db up` here and export the sync URL it prints to run the live half")
	}

	token := api.login(t, SeedEmail)
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
	token := api.login(t, SeedEmail)

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

	// And presence, which wires no fallback on purpose, still refuses: a
	// snapshot of who was here a moment ago and then stopped updating is worth
	// less than nothing.
	presence := api.do(t, request{
		method: http.MethodGet,
		path:   "/api/v1/rig_presence/_stream?offset=-1&scope=board",
		token:  token,
	})
	if presence.status != http.StatusBadGateway {
		t.Errorf("presence answered %d, want the 502 it has no fallback for", presence.status)
	}
}
