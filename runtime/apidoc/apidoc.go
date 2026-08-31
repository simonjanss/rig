// Package apidoc serves the OpenAPI document a rig project generates.
//
// It exists because a document on disk is not a document anybody can reach. The
// generated specification describes exactly the routes the generated router
// mounts — that is the whole claim the openapi generator makes — and until this
// package there was no way for a running server to say so. A viewer, an SDK
// generator or a contract test had to be pointed at the repository rather than
// at the API, and a deployed build carried no statement of what it answers.
//
// What it does not do is hold the bytes. go:embed cannot reach out of the
// package it is written in, and the document is written to the openapi
// generator's out_dir — `docs/` by convention — rather than beside the router.
// So the application embeds the file and hands the [io/fs.FS] over, exactly as
// it already does with its migrations, and this package finds the document
// inside it. That is also what makes the embed path the project's business: a
// project with `out_dir: api/docs` writes a different go:embed line and nothing
// here changes.
//
// The routes are public. No claims are read and no permission is checked, for
// the reason a health probe reads none: what the document says is what every
// client was generated against, and a specification nobody may fetch is a
// specification nobody can use. A project that has to gate it leaves the
// generated router's field unset and mounts this itself, behind whatever it
// gates the rest with.
//
// No CORS header, deliberately. A viewer served from another origin needs one,
// and which origins may read this API is a decision about the whole API rather
// than about these two routes — so it belongs in a wrapper around the mux,
// where it applies to everything.
package apidoc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"time"
)

// Names of the two renderings, as the openapi generator writes them.
//
// A project's own directory prefix is not here: the FS handed to [New] is
// whatever the application embedded, and it is searched by these names rather
// than by a path this package guessed.
const (
	// JSONName is the JSON rendering's filename.
	JSONName = "openapi.gen.json"
	// YAMLName is the YAML rendering's filename.
	YAMLName = "openapi.gen.yaml"
)

// Media types the two renderings are served as. YAML's is the one RFC 9512
// registered; before it there were four spellings in the wild and no right
// answer.
const (
	jsonType = "application/json"
	yamlType = "application/yaml"
)

// Options say where the document answers.
//
// Both paths are absolute and already carry the API's base path, because the
// generated router passes what the compiler computed — the same base every
// other route was expanded against, checked against every other route for a
// collision. Nothing here joins a prefix.
type Options struct {
	// JSONPath is where the JSON rendering answers, for example
	// "/api/v1/openapi.json". Empty leaves it unmounted.
	JSONPath string
	// YAMLPath is where the YAML rendering answers. Empty leaves it unmounted.
	YAMLPath string
}

// Handler serves the renderings it found.
type Handler struct {
	docs []document
}

// document is one rendering, with everything a conditional request needs
// computed once.
type document struct {
	path        string
	contentType string
	body        []byte
	etag        string
}

// New reads the document out of an embedded filesystem.
//
// The two renderings are found by name anywhere in fsys, so the directory the
// project embedded from is not this package's business. Either may be absent: a
// project whose openapi generator writes `formats: [json]` gets one route and
// not two, which is what keeps the routes and the document that describes them
// from disagreeing without either side knowing what the other was configured
// with.
//
// Both absent is an error, and it is returned rather than tolerated because it
// has exactly one cause worth reporting: the go:embed line names a directory
// with no document in it. Two routes quietly answering 404 would be the same
// mistake, found by whoever tried to use them.
func New(fsys fs.FS, opts Options) (*Handler, error) {
	if fsys == nil {
		return nil, fmt.Errorf("apidoc: no filesystem; embed the openapi generator's " +
			"out_dir and pass it, for example //go:embed docs/openapi.gen.json")
	}

	found, err := find(fsys)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("apidoc: found neither %s nor %s in the filesystem given; "+
			"the go:embed line should name the openapi generator's out_dir, for example "+
			"//go:embed docs/openapi.gen.json docs/openapi.gen.yaml", JSONName, YAMLName)
	}

	h := &Handler{}
	for _, d := range []struct {
		name, route, contentType string
	}{
		{JSONName, opts.JSONPath, jsonType},
		{YAMLName, opts.YAMLPath, yamlType},
	} {
		body, ok := found[d.name]
		if !ok || d.route == "" {
			continue
		}
		sum := sha256.Sum256(body)
		h.docs = append(h.docs, document{
			path:        d.route,
			contentType: d.contentType,
			body:        body,
			etag:        `"` + hex.EncodeToString(sum[:]) + `"`,
		})
	}
	if len(h.docs) == 0 {
		return nil, fmt.Errorf("apidoc: a document was found and neither Options.JSONPath " +
			"nor Options.YAMLPath names a route for it")
	}
	return h, nil
}

// find reads every rendering in fsys, by name rather than by path.
func find(fsys fs.FS) (map[string][]byte, error) {
	out := map[string][]byte{}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := path.Base(p)
		if name != JSONName && name != YAMLName {
			return nil
		}
		if _, dup := out[name]; dup {
			// Two copies of one rendering, and no way to tell which the
			// generator wrote last. Serving either would be a guess.
			return fmt.Errorf("apidoc: the filesystem given holds more than one %s; "+
				"embed one document, not a tree that contains several", name)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("apidoc: read %s: %w", p, err)
		}
		out[name] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Paths are the routes this handler answers, in the order they are mounted.
//
// Useful in a test that wants to assert what got mounted without reaching into
// a mux, and in a log line at startup.
func (h *Handler) Paths() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.docs))
	for _, d := range h.docs {
		out = append(out, d.path)
	}
	return out
}

// Mount registers a GET route per rendering found.
//
// One route per document rather than one route that content-negotiates. A
// specification is fetched by URL far more often than by Accept header — by a
// viewer, by a code generator, by curl — and two URLs are two things a person
// can paste.
//
// Nothing here is traced, logged or throttled, and that is not arranged: rig
// opens its spans and writes its request lines inside each generated handler,
// so anything that is not one is already outside both.
func (h *Handler) Mount(mux *http.ServeMux) {
	if h == nil {
		return
	}
	for _, d := range h.docs {
		mux.Handle("GET "+d.path, d.serve())
	}
}

// serve answers one rendering.
//
// Through [net/http.ServeContent] rather than a Write, which is what buys the
// conditional request: with the ETag set, a client holding the current document
// gets a 304 and no body, and HEAD works without a second code path. The modtime
// is zero on purpose — a binary's embedded files all carry the build's own
// timestamp, so Last-Modified would say something about the build rather than
// about the document, and If-Modified-Since would compare against it.
//
// no-cache rather than a max-age: the document changes when the API does, which
// is when a new build is deployed, and a cached copy that outlived the deploy is
// a client generated against an API that has moved. The ETag makes revalidating
// cheap.
func (d document) serve() http.HandlerFunc {
	body := d.body
	contentType := d.contentType
	etag := d.etag

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}
}
