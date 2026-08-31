package electric_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/simonjanss/rig/runtime/electric"
)

// The secret is what a deployed sync service authorises on, and it goes on the query
// string because that is the only place Electric reads it from.
func TestTheSecretReachesTheSyncService(t *testing.T) {
	t.Parallel()

	const secret = "sync3cret"

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("secret")
		w.Header().Set("electric-offset", "0_0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	p, err := newProxy(electric.Config{URL: srv.URL, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/todos/_stream?offset=-1", nil)
	p.Serve(httptest.NewRecorder(), req, electric.Shape{Table: "todo"})

	if got != secret {
		t.Errorf("the sync service was sent secret=%q, want %q", got, secret)
	}
}

// The bug this field exists to make impossible on the probe as well as on the shapes: a
// sync service authorising on the query string used to turn away the boot check and the
// monitoring page while every subscription worked, because Health applied the headers and
// not the query.
func TestTheSecretReachesTheHealthEndpoint(t *testing.T) {
	t.Parallel()

	const secret = "sync3cret"

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("secret")
		if r.URL.Query().Get("secret") != secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"status":"active"}`))
	}))
	defer srv.Close()

	p, err := newProxy(electric.Config{URL: srv.URL, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health = %v, want nil", err)
	}
	if got != secret {
		t.Errorf("the health endpoint was sent secret=%q, want %q", got, secret)
	}
}

// The whole reason Secret is a field rather than an entry in Extra. A transport failure is
// an *url.Error, whose message is the URL it was given — and net/url redacts a password in
// the userinfo and nothing else, so without this the credential goes straight into the log
// the first time the sync service is unreachable.
func TestAnUnreachableSyncServiceDoesNotLogTheSecret(t *testing.T) {
	t.Parallel()

	const secret = "sync3cret"

	// A server that is closed before anything asks, so the upstream call is a refused
	// connection and the error is the one net/http builds.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	var (
		mu   sync.Mutex
		said []string
	)
	p, err := newProxy(electric.Config{
		URL:    url,
		Secret: secret,
		OnError: func(_ context.Context, err error) {
			mu.Lock()
			defer mu.Unlock()
			said = append(said, err.Error())
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/todos/_stream?offset=-1", nil)
	p.Serve(httptest.NewRecorder(), req, electric.Shape{Table: "todo"})

	mu.Lock()
	defer mu.Unlock()
	if len(said) == 0 {
		t.Fatal("an unreachable sync service reported nothing")
	}
	for _, msg := range said {
		if strings.Contains(msg, secret) {
			t.Errorf("the reported error carries the secret: %s", msg)
		}
		if !strings.Contains(msg, "[redacted]") {
			t.Errorf("the secret was neither carried nor marked as removed: %s", msg)
		}
	}

	// And the error Health returns to its caller, which reaches a log through whoever
	// asked rather than through OnError.
	herr := p.Health(context.Background())
	if herr == nil {
		t.Fatal("Health = nil against a closed server")
	}
	if strings.Contains(herr.Error(), secret) {
		t.Errorf("the error Health returns carries the secret: %v", herr)
	}
}

// The URL inside an *url.Error is the encoded one, so a secret containing anything outside
// the unreserved set appears percent-encoded there and verbatim nowhere. Redacting only the
// literal spelling would leave it in the log in the one case somebody chose a strong value.
func TestTheEncodedSpellingIsRedactedToo(t *testing.T) {
	t.Parallel()

	const secret = "a secret/with+punctuation"

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	var said string
	p, err := newProxy(electric.Config{
		URL:     closed,
		Secret:  secret,
		OnError: func(_ context.Context, err error) { said = err.Error() },
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/todos/_stream?offset=-1", nil)
	p.Serve(httptest.NewRecorder(), req, electric.Shape{Table: "todo"})

	if said == "" {
		t.Fatal("nothing was reported")
	}
	for _, spelling := range []string{secret, url.QueryEscape(secret)} {
		if strings.Contains(said, spelling) {
			t.Errorf("the reported error carries the secret as %q: %s", spelling, said)
		}
	}
}

// Redaction rewrites the message and nothing else. A caller asking what actually failed —
// a deadline, a refused connection — must still get an answer through errors.Is.
func TestRedactionDoesNotBreakErrorMatching(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	var got error
	p, err := newProxy(electric.Config{
		URL:     closed,
		Secret:  "sync3cret",
		OnError: func(_ context.Context, err error) { got = err },
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/todos/_stream?offset=-1", nil)
	p.Serve(httptest.NewRecorder(), req, electric.Shape{Table: "todo"})

	if got == nil {
		t.Fatal("nothing was reported")
	}
	var uerr *url.Error
	if !errors.As(got, &uerr) {
		t.Errorf("errors.As no longer reaches the *url.Error underneath: %v", got)
	}
}

// Two answers to one question, and whichever this package applied second would win
// silently. Extra is where a secret had to go before Config.Secret existed, so the one
// that loses may well be the one somebody meant.
func TestTheSecretStatedTwiceIsRefused(t *testing.T) {
	t.Parallel()

	_, err := newProxy(electric.Config{
		URL:    "http://sync:3000",
		Secret: "one",
		Extra:  url.Values{"secret": {"another"}},
	})
	if err == nil {
		t.Fatal("accepted a secret stated in two places")
	}
	if !strings.Contains(err.Error(), "Config.Secret") {
		t.Errorf("the refusal does not say where to state it: %v", err)
	}
}

// The spelling that was the only one there used to be. A project that put its secret in
// Extra before Config.Secret existed still puts it on every upstream URL, so it is the
// one most likely to be logging its credential right now — and redaction that read only
// the new field would leave exactly that project leaking with nothing to say so.
func TestASecretStatedInExtraIsRedactedToo(t *testing.T) {
	t.Parallel()

	const secret = "sync3cret"

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	var said string
	p, err := newProxy(electric.Config{
		URL:     closed,
		Extra:   url.Values{"secret": {secret}},
		OnError: func(_ context.Context, err error) { said = err.Error() },
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/todos/_stream?offset=-1", nil)
	p.Serve(httptest.NewRecorder(), req, electric.Shape{Table: "todo"})

	if said == "" {
		t.Fatal("nothing was reported")
	}
	if strings.Contains(said, secret) {
		t.Errorf("a secret stated in Extra reaches the log: %s", said)
	}
	if !strings.Contains(said, "[redacted]") {
		t.Errorf("the secret was neither carried nor marked as removed: %s", said)
	}

	// And the error Health hands back to whoever asked, which is the other way out.
	herr := p.Health(context.Background())
	if herr == nil {
		t.Fatal("Health = nil against a closed server")
	}
	if strings.Contains(herr.Error(), secret) {
		t.Errorf("the error Health returns carries the secret: %v", herr)
	}
}

// No secret, nothing added — not an empty one, which a secured sync service reads as a
// wrong one. This is `rig db up`'s sync service, which runs without a secret at all.
func TestWithoutASecretNothingIsAdded(t *testing.T) {
	t.Parallel()

	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.URL.Query()["secret"]
		_, _ = w.Write([]byte(`{"status":"active"}`))
	}))
	defer srv.Close()

	p, err := newProxy(electric.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("a proxy with no secret sent one anyway")
	}
}
