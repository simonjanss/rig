package electric_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/electric"
)

// A shape route outlives the one write deadline the server set for every route.
//
// serve.Config.WriteTimeout is thirty seconds and its clock starts when the
// request's headers were read, so a poll the sync service holds for longer has
// its answer killed on the way out — a 200 the subscriber never receives. The
// proxy replaces that deadline per request, the way the file transfers do.
//
// Two things this test cannot be written with, so that nobody simplifies it into
// them: httptest.NewRecorder has no connection at all, so SetWriteDeadline
// returns ErrNotSupported and the call under test becomes a silent no-op; and
// httptest.NewServer sets no timeouts, so the failure cannot occur on one. The
// front server here is unstarted precisely so its WriteTimeout can be set first,
// which is the same *http.Server field serve builds.
//
// What is not covered, because reproducing it needs a hold longer than
// PollDeadline: that the read deadline is left alone. The reason is in
// extendWrite's own documentation.
func TestAPollOutlastsTheServersWriteTimeout(t *testing.T) {
	t.Parallel()

	// Longer than the front server's write deadline below, and shorter than
	// anything a test should wait for.
	const hold = 400 * time.Millisecond

	sync := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(hold):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("electric-handle", "the-handle")
		w.Header().Set("electric-offset", "0_0")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"1","value":{"id":"1"}}]`))
	}))
	defer sync.Close()

	proxy, err := electric.New(electric.Config{URL: sync.URL})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/shape", func(w http.ResponseWriter, r *http.Request) {
		proxy.Serve(w, r, electric.Shape{Table: "todo"})
	})

	front := httptest.NewUnstartedServer(mux)
	front.Config.WriteTimeout = 100 * time.Millisecond
	front.Start()
	defer front.Close()

	// One client across both, so that the second request also proves the
	// per-request deadline left the connection fit to be reused.
	client := front.Client()

	// The live poll is the loud case; the read from the beginning is why the
	// deadline is lifted for every request rather than only for a live one.
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"a live poll", "offset=0_0&handle=h&live=true"},
		{"a read from the beginning", "offset=-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := client.Get(front.URL + "/shape?" + tc.query)
			// The real assertion. Without the fix the write deadline expires
			// while the sync service is still holding, net/http tears the
			// connection down, and this is an unexpected EOF with no status to
			// inspect at all.
			if err != nil {
				t.Fatalf("the response was cut short on the way out: %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusOK {
				t.Errorf("status = %d", res.StatusCode)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("reading the body: %v", err)
			}
			if got := string(body); got != `[{"key":"1","value":{"id":"1"}}]` {
				t.Errorf("body = %q", got)
			}
			// The cursor the subscriber resumes from. A response that arrived
			// without it ends the subscription after one poll.
			if got := res.Header.Get("electric-offset"); got != "0_0" {
				t.Errorf("electric-offset = %q", got)
			}
		})
	}
}
