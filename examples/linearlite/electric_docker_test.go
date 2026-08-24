//go:build docker

package main

import (
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
