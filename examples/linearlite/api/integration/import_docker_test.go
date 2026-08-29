//go:build docker

package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/simonjanss/rig/examples/linearlite/client"
	"github.com/simonjanss/rig/examples/linearlite/importer"
	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/rigclient"
)

// The import job's whole story: a personal key minted over the API, the
// generated client authenticated with it, a todo per row, and a rerun that
// creates nothing — the idempotency keys replay the recorded answers.
func TestTheImportJob(t *testing.T) {
	api := newServer(t)
	api.seed(t)
	token := api.login(t, app.SeedEmail2)

	minted := api.do(t, request{
		method: http.MethodPost, path: "/auth/api-keys", token: token,
		body: map[string]any{"name": "import test", "kind": "Personal", "scopes": []string{"todo.read", "todo.write"}},
	})
	if minted.status != http.StatusCreated {
		t.Fatalf("mint a personal key: %d %s", minted.status, minted.body)
	}
	var key struct {
		Secret string `json:"secret"`
	}
	minted.decode(t, &key)

	c, err := client.New(rigclient.Config{
		BaseURL:    api.http.URL,
		Credential: rigclient.APIKey(key.Secret),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A file of this run's own, so its idempotency keys — which carry the path
	// — are fresh however many times the suite has run against a warm
	// database.
	csv := filepath.Join(t.TempDir(), "todos.csv")
	if err := os.WriteFile(csv, []byte(
		"title,description,status,priority\n"+
			"Imported one,,todo,normal\n"+
			"Imported two,with words,in_progress,high\n"+
			"Imported three,,backlog,low\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := total(t, api, token)

	created, failed, err := importer.Run(context.Background(), c, csv, 0, os.Stderr)
	if err != nil || failed != 0 || created != 3 {
		t.Fatalf("run: created=%d failed=%d err=%v", created, failed, err)
	}
	if got := total(t, api, token); got != before+3 {
		t.Fatalf("the board grew by %d, want 3", got-before)
	}

	// The rerun reports the replayed answers and writes nothing.
	if _, _, err := importer.Run(context.Background(), c, csv, 0, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if got := total(t, api, token); got != before+3 {
		t.Fatalf("a rerun must not grow the board: %d, want %d", got, before+3)
	}
}

func total(t *testing.T, api *server, token string) int {
	t.Helper()
	res := api.do(t, request{method: http.MethodGet, path: "/api/v1/todos?limit=1", token: token})
	if res.status != http.StatusOK {
		t.Fatalf("list: %d %s", res.status, res.body)
	}
	var page struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	res.decode(t, &page)
	return page.Pagination.Total
}
