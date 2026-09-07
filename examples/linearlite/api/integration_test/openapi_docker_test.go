//go:build docker

// The document, over HTTP, from the server it describes — and from the layout
// that made it hard.
//
// examples/todo already proves the routes work. What only this example can prove
// is that the import behind them is right: rig.yaml sits above both halves and
// the Go module begins at api/, so `out_dir: api/docs` and the import path
// .../examples/linearlite/docs are not the same string. rig reads the offset
// between them out of api/go.mod. Get that wrong and the router imports a
// package that does not exist, which is rig#128 — and this example is the only
// one whose build would notice.
package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/simonjanss/rig/examples/linearlite/internal/generated/api"
)

func TestTheDocumentIsServed(t *testing.T) {
	s := newServer(t)

	res, err := http.Get(s.http.URL + api.OpenAPIJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	// Parsed rather than grepped: what is served has to be the document this
	// build embedded, not a file that happens to sit at that path.
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("the served document does not parse: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0", doc.OpenAPI)
	}
	// A route this example definitely has, and the document's own.
	for _, want := range []string{"/api/v1/todos", api.OpenAPIJSONPath, api.OpenAPIYAMLPath} {
		if _, ok := doc.Paths[want]; !ok {
			t.Errorf("the served document does not describe %s", want)
		}
	}
}
