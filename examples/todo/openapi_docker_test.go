//go:build docker

// The document, over HTTP, from the server it describes.
//
// It is the one thing the generator's own tests cannot prove: they check that
// the bytes are written, and this checks that a running build serves them — the
// go:embed line in main.go, the paths the compiler computed, and the routes
// api.Register mounted, all agreeing.
package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/simonjanss/rig/examples/todo/internal/api"
)

func TestTheDocumentIsServed(t *testing.T) {
	srv, _ := newServer(t)

	res, err := http.Get(srv.URL + api.OpenAPIJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q, want application/json", got)
	}

	// Parsed rather than grepped: what is served has to be the document, not a
	// file that happens to be at that path.
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Info    struct{ Title string }    `json:"info"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("the served document does not parse: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0", doc.OpenAPI)
	}
	// A route this example definitely has, and the document's own — so the thing
	// being served describes both the API and the fact that it is served.
	for _, want := range []string{"/api/v1/todos", api.OpenAPIJSONPath, api.OpenAPIYAMLPath} {
		if _, ok := doc.Paths[want]; !ok {
			t.Errorf("the served document does not describe %s", want)
		}
	}
}

func TestTheYAMLRenderingIsServed(t *testing.T) {
	srv, _ := newServer(t)

	res, err := http.Get(srv.URL + api.OpenAPIYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/yaml" {
		t.Errorf("content type = %q, want application/yaml", got)
	}
}

// No claims. Every other route in this example refuses a request with no
// X-Tenant-Id, and this one answers it: a specification nobody may fetch is one
// nobody can use.
func TestTheDocumentNeedsNoTenant(t *testing.T) {
	srv, _ := newServer(t)

	unauthorized, err := http.Get(srv.URL + "/api/v1/todos")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an ordinary route answered %d without a tenant, so this test "+
			"proves nothing", unauthorized.StatusCode)
	}

	res, err := http.Get(srv.URL + api.OpenAPIJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("the document answered %d without a tenant, want 200", res.StatusCode)
	}
}

// The ETag is what makes polling the document cheap, and a 304 with a body would
// be worse than no ETag at all.
func TestTheDocumentRevalidates(t *testing.T) {
	srv, _ := newServer(t)

	first, err := http.Get(srv.URL + api.OpenAPIJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()

	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+api.OpenAPIJSONPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", res.StatusCode)
	}
	if res.ContentLength > 0 {
		t.Errorf("a 304 carried %d bytes", res.ContentLength)
	}
}
