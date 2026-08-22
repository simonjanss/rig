package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// spaHandler serves the built front end from dir, read from disk on every
// request rather than embedded.
//
// From disk, deliberately: `make examples` builds and tests this server with
// Go and Docker and no pnpm, and a go:embed of web/dist would make the Go
// build depend on a JavaScript one. The cost is that `go run .` serves
// whatever was last built, which for a development loop is the point —
// `pnpm build` and reload, or run `pnpm dev` and let Vite proxy to this
// server instead.
//
// Unknown extension-less paths fall back to index.html, because the router
// inside the application owns them: /todo/123 is a client-side route, and
// answering 404 to a page reload would make every deep link a bug. Go 1.22's
// mux precedence keeps every real route — /api/v1, /auth, /notifications, the
// _stream shapes, the probes — winning over this catch-all.
func spaHandler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The catch-all catches what fell past the real routes, and some of
		// that is not a page: a POST to an endpoint that does not exist, or a
		// misspelled API path, should say so rather than answer with HTML the
		// caller will try to parse as JSON.
		for _, prefix := range []string{"/api/", "/auth/", "/notifications"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				http.NotFound(w, r)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		index := filepath.Join(dir, "index.html")
		if _, err := os.Stat(index); err != nil {
			notBuilt(w)
			return
		}

		// A path with an extension is a file: serve it, and let a miss be the
		// 404 it is. A path without one is a route the front end owns.
		path := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if strings.Contains(filepath.Base(path), ".") {
			http.ServeFile(w, r, path)
			return
		}
		http.ServeFile(w, r, index)
	})
}

// notBuilt answers when web/dist does not exist yet: the server is fine, the
// front end has not been built, and the fix is one command.
func notBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html>
<meta charset="utf-8">
<title>linearlite</title>
<style>body{font:16px/1.6 system-ui;max-width:38rem;margin:6rem auto;padding:0 1rem;color:#333}code{background:#f4f4f4;padding:.1rem .35rem;border-radius:4px}</style>
<h1>The API is up. The front end is not built yet.</h1>
<p>This server looks for <code>web/dist</code> and serves it as-is. Build it once:</p>
<p><code>cd web &amp;&amp; pnpm install &amp;&amp; pnpm build</code></p>
<p>— then reload. For a development loop, run <code>pnpm dev</code> in <code>web/</code>
instead and open the Vite server, which proxies API calls back here.</p>
`))
}
