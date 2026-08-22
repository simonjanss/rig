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
		res := api.do(t, request{method: http.MethodGet, path: "/api/v1/todo/_stream?offset=-1"})
		if res.status != http.StatusUnauthorized {
			t.Fatalf("an anonymous stream: %d %s, want 401", res.status, res.body)
		}
	})

	if os.Getenv("ELECTRIC_URL") == "" {
		t.Skip("no $ELECTRIC_URL: run `rig db up` here and export the sync URL it prints to run the live half")
	}

	token := api.login(t, SeedEmail)
	res := api.do(t, request{method: http.MethodGet, path: "/api/v1/todo/_stream?offset=-1", token: token})
	if res.status != http.StatusOK {
		t.Fatalf("the live shape: %d %s", res.status, res.body)
	}
	// The resume cursor is the protocol: a proxy that swallowed it would
	// stream once and never again.
	if res.headers.Get("electric-handle") == "" {
		t.Errorf("no electric-handle header; the client cannot resume: %v", res.headers)
	}
}
