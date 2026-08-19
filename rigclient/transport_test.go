// The transport, against a server that is not a rig server.
//
// Everything here is hermetic: an httptest handler that answers whatever the
// case needs, which is the only way to exercise a proxy refusing QUERY or a
// gateway returning HTML. The end-to-end tests against a real generated server
// live in examples/todo.
package rigclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/rigerr"
)

type todo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// newClient builds a runtime pointed at a handler, with the base path the
// generated client would supply.
func newClient(t *testing.T, h http.Handler, cfg rigclient.Config) *rigclient.Runtime {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cfg.BaseURL = srv.URL
	rt, err := rigclient.New(cfg, rigclient.API{BasePath: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestTheBasePathIsPrefixed(t *testing.T) {
	var got string
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos/1",
	}); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/todos/1"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// A QUERY that never reaches the server is the case this exists for: an
// intermediary that has not heard of the method answers 405 itself.
func TestSearchFallsBackToPostAndRemembersIt(t *testing.T) {
	var methods []string
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Method == "QUERY" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{})

	search := rigclient.Op{
		Method: rigclient.MethodQuery, Path: "/todos",
		Fallback: "/todos/_search", Body: map[string]any{"filter": map[string]any{}},
	}
	for range 2 {
		if _, err := rigclient.Do[todo](t.Context(), rt, search); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		"QUERY /api/v1/todos",
		"POST /api/v1/todos/_search",
		// The second search does not try QUERY again.
		"POST /api/v1/todos/_search",
	}
	if len(methods) != len(want) {
		t.Fatalf("requests = %v, want %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, methods[i], want[i])
		}
	}
}

// A 501 means the same thing as a 405 here: nobody in the chain knows QUERY.
func TestSearchFallsBackOnNotImplemented(t *testing.T) {
	var last string
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = r.Method
		if r.Method == "QUERY" {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: rigclient.MethodQuery, Path: "/todos", Fallback: "/todos/_search",
	}); err != nil {
		t.Fatal(err)
	}
	if last != http.MethodPost {
		t.Errorf("last method = %q, want POST", last)
	}
}

// Without an alias there is nothing to fall back to, and the refusal is the
// answer rather than something to work around.
func TestAQueryWithNoAliasReportsTheRefusal(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}), rigclient.Config{})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: rigclient.MethodQuery, Path: "/todos",
	})

	var e *rigclient.Error
	if !errors.As(err, &e) || e.Status != http.StatusMethodNotAllowed {
		t.Fatalf("err = %v, want a 405", err)
	}
}

func TestAFailureDecodesIntoATypedError(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{
			"code":      "UnprocessableEntity",
			"message":   "the request is not valid",
			"requestId": "req-7",
			"fields": map[string]any{
				"title": map[string]string{"code": "CannotBeEmpty", "message": "cannot be empty"},
			},
		})
	}), rigclient.Config{})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})

	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *rigclient.Error", err)
	}
	if e.Code != rigerr.CodeUnprocessableEntity {
		t.Errorf("code = %q, want UnprocessableEntity", e.Code)
	}
	if e.RequestID != "req-7" {
		t.Errorf("request id = %q, want req-7", e.RequestID)
	}
	if !rigclient.IsInvalid(err) {
		t.Error("IsInvalid says no")
	}

	type fields struct {
		Title *rigerr.FieldError `json:"title"`
	}
	got, ok := rigclient.FieldsAs[fields](err)
	if !ok {
		t.Fatal("the field errors did not decode")
	}
	if got.Title == nil || got.Title.Code != rigerr.FieldCodeCannotBeEmpty {
		t.Errorf("title = %+v, want CannotBeEmpty", got.Title)
	}
}

// A gateway's error page is not JSON and is not the server's. It still has to
// arrive as an error with a status rather than as a decoding failure.
func TestSomethingThatIsNotARigServerStillFails(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>502 Bad Gateway</html>"))
	}), rigclient.Config{})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *rigclient.Error", err)
	}
	if e.Status != http.StatusBadGateway || e.Code != "" {
		t.Errorf("got %d/%q, want 502 and no code", e.Status, e.Code)
	}
	if e.Body == "" {
		t.Error("the body was not kept, so nobody can see what answered")
	}
}

func TestRetryAfterIsRead(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":"RateLimited","message":"slow down"}`))
	}), rigclient.Config{})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	var e *rigclient.Error
	if !errors.As(err, &e) || e.RetryAfter != 12*time.Second {
		t.Fatalf("err = %v, want a 429 asking for 12s", err)
	}
	if !rigclient.IsRateLimited(err) {
		t.Error("IsRateLimited says no")
	}
}

// A 426 is what a server with a MinRevision answers a client built before it.
// Nothing the caller does at runtime fixes it, so it is worth telling apart
// from every other refusal.
func TestAnUpgradeRequiredIsRecognized(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
		w.Write([]byte(`{"code":"UpgradeRequired","message":"this client was built against API revision 2026-03-01"}`))
	}), rigclient.Config{})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if !rigclient.IsUpgradeRequired(err) {
		t.Errorf("IsUpgradeRequired says no to %v", err)
	}
	// And not as one of the refusals it is not: the whole reason it is its own
	// code is that "regenerate your client" is not advice anybody can take from
	// a 400 that also means a malformed body.
	if rigclient.IsInvalid(err) || rigclient.IsForbidden(err) {
		t.Error("a 426 answers to another predicate as well")
	}

	var e *rigclient.Error
	if !errors.As(err, &e) || e.Status != http.StatusUpgradeRequired {
		t.Fatalf("err = %v, want a 426", err)
	}
}

func TestANoContentAnswerDecodesToNil(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	got, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestTheCredentialAndHeadersTravel(t *testing.T) {
	var got http.Header
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{
		Credential: rigclient.StaticToken("rig_at_abc"),
		Header:     http.Header{"X-Tenant-Id": {"tenant-1"}},
		UserAgent:  "demo/1",
		Revision:   "2026-08-15",
		RequestID:  func() string { return "req-1" },
	})

	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}, rigclient.WithHeader("X-Extra", "yes")); err != nil {
		t.Fatal(err)
	}

	for header, want := range map[string]string{
		"Authorization": "Bearer rig_at_abc",
		"X-Tenant-Id":   "tenant-1",
		"User-Agent":    "demo/1",
		"Api-Revision":  "2026-08-15",
		"X-Request-Id":  "req-1",
		"X-Extra":       "yes",
		"Accept":        "application/json",
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, want %q", header, got.Get(header), want)
		}
	}
}

// The revision is what the generated client was built against, so it comes from
// the API a generated New supplies rather than from something a caller has to
// remember to set.
func TestTheRevisionComesFromTheGeneratedClient(t *testing.T) {
	var got http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{BaseURL: srv.URL}, rigclient.API{
		BasePath:       "/api/v1",
		Revision:       "2026-08-01",
		RevisionHeader: "X-Demo-Revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}); err != nil {
		t.Fatal(err)
	}

	if got.Get("X-Demo-Revision") != "2026-08-01" {
		t.Errorf("X-Demo-Revision = %q, want the generated revision", got.Get("X-Demo-Revision"))
	}
	if got.Get(rigclient.DefaultRevisionHeader) != "" {
		t.Error("the revision went to the default header as well as the configured one")
	}
}

// A client somebody assembled by hand says nothing, rather than saying
// something it cannot vouch for.
func TestAClientWithNoRevisionSendsNone(t *testing.T) {
	var got http.Header
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}); err != nil {
		t.Fatal(err)
	}
	if v := got.Get(rigclient.DefaultRevisionHeader); v != "" {
		t.Errorf("%s = %q, want nothing", rigclient.DefaultRevisionHeader, v)
	}
}

func TestAnonymousSendsNoCredential(t *testing.T) {
	var got string
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{Credential: rigclient.StaticToken("rig_at_abc")})

	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}, rigclient.Anonymous()); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

func TestPaginateWalksEveryPage(t *testing.T) {
	rows := []todo{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}, {ID: "5"}}

	var calls int
	seq := rigclient.Paginate(t.Context(), 0,
		func(_ context.Context, offset int) (rigclient.Page[todo], error) {
			calls++
			end := min(offset+2, len(rows))
			return rigclient.Page[todo]{
				Items: rows[offset:end], Total: int64(len(rows)), Offset: offset,
			}, nil
		})

	var got []string
	for item, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, item.ID)
	}

	if len(got) != len(rows) {
		t.Fatalf("got %v, want every row", got)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// The failure is the last pair, and nothing after it is yielded.
func TestPaginateStopsAtTheFirstFailure(t *testing.T) {
	boom := errors.New("boom")
	seq := rigclient.Paginate(t.Context(), 0,
		func(_ context.Context, offset int) (rigclient.Page[todo], error) {
			if offset > 0 {
				return rigclient.Page[todo]{}, boom
			}
			return rigclient.Page[todo]{Items: []todo{{ID: "1"}}, Total: 9}, nil
		})

	var seen, failed int
	for _, err := range seq {
		if err != nil {
			failed++
			if !errors.Is(err, boom) {
				t.Errorf("err = %v, want boom", err)
			}
			continue
		}
		seen++
	}
	if seen != 1 || failed != 1 {
		t.Errorf("seen = %d, failed = %d, want 1 and 1", seen, failed)
	}
}
