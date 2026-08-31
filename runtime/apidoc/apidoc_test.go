package apidoc_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/simonjanss/rig/runtime/apidoc"
)

// opts are the routes the generated router passes.
var opts = apidoc.Options{
	JSONPath: "/api/v1/openapi.json",
	YAMLPath: "/api/v1/openapi.yaml",
}

func bothFS() fstest.MapFS {
	return fstest.MapFS{
		"docs/openapi.gen.json": {Data: []byte(`{"openapi":"3.1.0"}` + "\n")},
		"docs/openapi.gen.yaml": {Data: []byte("openapi: 3.1.0\n")},
	}
}

func TestMountsBothRenderings(t *testing.T) {
	h, err := apidoc.New(bothFS(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()
	h.Mount(mux)

	for _, tc := range []struct{ path, contentType, body string }{
		{opts.JSONPath, "application/json", `{"openapi":"3.1.0"}`},
		{opts.YAMLPath, "application/yaml", "openapi: 3.1.0"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tc.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != tc.contentType {
			t.Errorf("%s: content type = %q, want %q", tc.path, got, tc.contentType)
		}
		if got := rec.Body.String(); !strings.Contains(got, tc.body) {
			t.Errorf("%s: body = %q, want it to contain %q", tc.path, got, tc.body)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("%s: no ETag", tc.path)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: cache control = %q, want no-cache", tc.path, got)
		}
	}
}

// The prefix is the project's, not this package's: a project whose openapi
// generator writes somewhere else changes one go:embed line and nothing here.
func TestFindsTheDocumentWhereverItWasEmbeddedFrom(t *testing.T) {
	for _, dir := range []string{"", "docs/", "api/docs/", "internal/spec/"} {
		fsys := fstest.MapFS{dir + "openapi.gen.json": {Data: []byte("{}")}}

		h, err := apidoc.New(fsys, opts)
		if err != nil {
			t.Fatalf("%q: New: %v", dir, err)
		}
		if got := h.Paths(); len(got) != 1 || got[0] != opts.JSONPath {
			t.Errorf("%q: paths = %v, want [%s]", dir, got, opts.JSONPath)
		}
	}
}

// One route per document found, so a project that writes `formats: [json]` gets
// one route and the document describing it lists one — without either side
// knowing what the other was configured with.
func TestMountsOnlyWhatItFound(t *testing.T) {
	fsys := fstest.MapFS{"docs/openapi.gen.json": {Data: []byte("{}")}}

	h, err := apidoc.New(fsys, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := h.Paths(); len(got) != 1 || got[0] != opts.JSONPath {
		t.Fatalf("paths = %v, want [%s]", got, opts.JSONPath)
	}

	mux := http.NewServeMux()
	h.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, opts.YAMLPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("yaml status = %d, want 404", rec.Code)
	}
}

func TestRefusesAFilesystemWithNoDocument(t *testing.T) {
	_, err := apidoc.New(fstest.MapFS{"docs/README.md": {Data: []byte("hello")}}, opts)
	if err == nil {
		t.Fatal("New: no error")
	}
	// The message has to name what it looked for, because the one cause is a
	// go:embed line pointing at the wrong directory.
	for _, want := range []string{apidoc.JSONName, apidoc.YAMLName, "go:embed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRefusesANilFilesystem(t *testing.T) {
	if _, err := apidoc.New(nil, opts); err == nil {
		t.Fatal("New: no error")
	}
}

func TestRefusesTwoCopiesOfOneRendering(t *testing.T) {
	fsys := fstest.MapFS{
		"docs/openapi.gen.json":   {Data: []byte("{}")},
		"vendor/openapi.gen.json": {Data: []byte(`{"openapi":"3.1.0"}`)},
	}
	_, err := apidoc.New(fsys, opts)
	if err == nil {
		t.Fatal("New: no error")
	}
	if !strings.Contains(err.Error(), "more than one") {
		t.Errorf("error = %q, want it to say more than one", err)
	}
}

func TestRefusesADocumentWithNowhereToAnswer(t *testing.T) {
	fsys := fstest.MapFS{"docs/openapi.gen.json": {Data: []byte("{}")}}
	if _, err := apidoc.New(fsys, apidoc.Options{}); err == nil {
		t.Fatal("New: no error")
	}
}

// The ETag is the point of serving through ServeContent: a client holding the
// current document revalidates for no body at all.
func TestConditionalRequest(t *testing.T) {
	h, err := apidoc.New(bothFS(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)

	first := httptest.NewRecorder()
	mux.ServeHTTP(first, httptest.NewRequest(http.MethodGet, opts.JSONPath, nil))
	etag := first.Header().Get("ETag")

	second := httptest.NewRecorder()
	mux.ServeHTTP(second, httptest.NewRequest(http.MethodGet, opts.JSONPath, nil))
	if got := second.Header().Get("ETag"); got != etag {
		t.Errorf("ETag = %q on the second request, %q on the first", got, etag)
	}

	req := httptest.NewRequest(http.MethodGet, opts.JSONPath, nil)
	req.Header.Set("If-None-Match", etag)
	cached := httptest.NewRecorder()
	mux.ServeHTTP(cached, req)

	if cached.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", cached.Code)
	}
	if cached.Body.Len() != 0 {
		t.Errorf("body = %q, want none", cached.Body.String())
	}
}

// Two documents that differ have to have different ETags, or a client holding
// one is told it already has the other.
func TestETagFollowsTheContent(t *testing.T) {
	one, err := apidoc.New(fstest.MapFS{
		"openapi.gen.json": {Data: []byte(`{"openapi":"3.1.0"}`)},
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	two, err := apidoc.New(fstest.MapFS{
		"openapi.gen.json": {Data: []byte(`{"openapi":"3.1.0","info":{}}`)},
	}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if etagOf(t, one) == etagOf(t, two) {
		t.Error("two different documents share an ETag")
	}
}

// A HEAD costs nothing extra, because ServeContent already answers it.
func TestHead(t *testing.T) {
	h, err := apidoc.New(bothFS(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, opts.JSONPath, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want none", rec.Body.String())
	}
}

// A nil handler mounts nothing rather than panicking, so a caller that ignored
// New's error does not take the server down at startup.
func TestNilHandler(t *testing.T) {
	var h *apidoc.Handler
	h.Mount(http.NewServeMux())
	if got := h.Paths(); got != nil {
		t.Errorf("paths = %v, want none", got)
	}
}

func etagOf(t *testing.T, h *apidoc.Handler) string {
	t.Helper()

	mux := http.NewServeMux()
	h.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, opts.JSONPath, nil))
	return rec.Header().Get("ETag")
}
