// What the generated client puts on the wire, checked against a server that is
// only a mux.
//
// No database and no build tag: these are the properties a caller depends on and
// nobody would notice breaking, and they should fail in `go test ./...` rather
// than only where Docker is available. The end-to-end tests against a real
// generated server live in examples/todo and examples/auth.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/todo/client"
	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// recorder is a server that remembers what it was asked and answers whatever the
// test needs.
type recorder struct {
	t        *testing.T
	requests []request
	handler  func(w http.ResponseWriter, r *http.Request, body []byte)
}

type request struct {
	method string
	path   string
	query  string
	body   string
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec.requests = append(rec.requests, request{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body),
	})
	rec.handler(w, r, body)
}

func newRecorded(t *testing.T, handler func(http.ResponseWriter, *http.Request, []byte)) (*client.Client, *recorder) {
	t.Helper()

	rec := &recorder{t: t, handler: handler}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	c, err := client.New(rigclient.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

// The property the whole patch design rests on: a PATCH names only what it
// changes. Anything else and an update of the title quietly clears the notes.
func TestAPatchSendsOnlyWhatItNames(t *testing.T) {
	c, rec := newRecorded(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Write([]byte(`{"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","title":"x"}`))
	})

	if _, err := c.Todos.Update(t.Context(), mustID(t), client.TodoUpdateInput{
		Title: patch.NewOptional("only the title"),
	}); err != nil {
		t.Fatal(err)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.requests[0].body), &sent); err != nil {
		t.Fatalf("the body did not parse: %s", rec.requests[0].body)
	}
	if len(sent) != 1 {
		t.Fatalf("the update sent %d keys, want only title: %s", len(sent), rec.requests[0].body)
	}
	if sent["title"] != "only the title" {
		t.Errorf("title = %v", sent["title"])
	}
}

// And the other half: a field explicitly cleared does travel, as null.
func TestAClearedFieldTravelsAsNull(t *testing.T) {
	c, rec := newRecorded(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Write([]byte(`{"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}`))
	})

	if _, err := c.Todos.Update(t.Context(), mustID(t), client.TodoUpdateInput{
		Notes: patch.Null[string](),
	}); err != nil {
		t.Fatal(err)
	}

	if got := rec.requests[0].body; got != "{\"notes\":null}\n" && got != `{"notes":null}` {
		t.Errorf("body = %q, want notes cleared and nothing else", got)
	}
}

// A parameter the caller did not set is not sent, because the server applies its
// default only to a parameter that is absent.
func TestAnUnsetQueryParameterIsNotSent(t *testing.T) {
	c, rec := newRecorded(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Write([]byte(`{"data":[],"pagination":{"offset":0,"limit":50,"total":0}}`))
	})

	if _, err := c.Todos.List(t.Context(), client.TodoListQuery{}); err != nil {
		t.Fatal(err)
	}
	if rec.requests[0].query != "" {
		t.Errorf("query = %q, want nothing at all", rec.requests[0].query)
	}

	if _, err := c.Todos.List(t.Context(), client.TodoListQuery{Limit: rigclient.P(10)}); err != nil {
		t.Fatal(err)
	}
	if rec.requests[1].query != "limit=10" {
		t.Errorf("query = %q, want limit=10 and no offset", rec.requests[1].query)
	}
}

// The fallback exists for intermediaries that have never heard of QUERY. It is
// tried once and then remembered, so a client behind such a proxy pays for the
// discovery a single time.
func TestSearchFallsBackOnceAndRemembers(t *testing.T) {
	c, rec := newRecorded(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == "QUERY" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte(`{"data":[],"pagination":{"offset":0,"limit":50,"total":0}}`))
	})

	for range 2 {
		if _, err := c.Todos.Search(t.Context(), client.TodoFilter{}, client.TodoSearchQuery{}); err != nil {
			t.Fatal(err)
		}
	}

	want := []request{
		{method: "QUERY", path: "/api/v1/todos"},
		{method: "POST", path: "/api/v1/todos/_search"},
		{method: "POST", path: "/api/v1/todos/_search"},
	}
	if len(rec.requests) != len(want) {
		t.Fatalf("made %d requests, want %d: %+v", len(rec.requests), len(want), rec.requests)
	}
	for i, w := range want {
		if rec.requests[i].method != w.method || rec.requests[i].path != w.path {
			t.Errorf("request %d = %s %s, want %s %s",
				i, rec.requests[i].method, rec.requests[i].path, w.method, w.path)
		}
	}
}

// A 422 arrives as a struct shaped like the input, which is what lets a caller
// put each message beside the control it belongs to.
func TestAValidationFailureDecodesIntoItsInputsShape(t *testing.T) {
	c, _ := newRecorded(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{
			"code":      "UnprocessableEntity",
			"message":   "the request is not valid",
			"requestId": "req-42",
			"fields": map[string]any{
				"title": map[string]string{"code": "CannotBeEmpty", "message": "cannot be empty"},
			},
		})
	})

	_, err := c.Todos.Create(t.Context(), client.TodoCreateInput{})
	if !rigclient.IsInvalid(err) {
		t.Fatalf("err = %v, want a validation failure", err)
	}

	fields, ok := rigclient.FieldsAs[client.TodoCreateFields](err)
	if !ok {
		t.Fatal("the failure carried no field errors")
	}
	if fields.Title == nil || fields.Title.Code != rigerr.FieldCodeCannotBeEmpty {
		t.Fatalf("title = %+v, want CannotBeEmpty", fields.Title)
	}
	if fields.Notes != nil {
		t.Error("a field nobody complained about should be nil")
	}
}

// The iterator asks for one page after another until the total is reached, and
// carries the caller's own page size while doing it.
func TestAllWalksEveryPage(t *testing.T) {
	c, rec := newRecorded(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		offset := r.URL.Query().Get("offset")
		if offset == "" {
			offset = "0"
		}
		switch offset {
		case "0":
			w.Write([]byte(`{"data":[{"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","title":"one"}],
				"pagination":{"offset":0,"limit":1,"total":2}}`))
		default:
			w.Write([]byte(`{"data":[{"id":"6ba7b811-9dad-11d1-80b4-00c04fd430c8","title":"two"}],
				"pagination":{"offset":1,"limit":1,"total":2}}`))
		}
	})

	var titles []string
	for todo, err := range c.Todos.All(t.Context(), client.TodoListQuery{Limit: rigclient.P(1)}) {
		if err != nil {
			t.Fatal(err)
		}
		titles = append(titles, todo.Title)
	}

	if len(titles) != 2 || titles[0] != "one" || titles[1] != "two" {
		t.Fatalf("titles = %v", titles)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("made %d requests, want one per page", len(rec.requests))
	}
	if rec.requests[1].query != "limit=1&offset=1" {
		t.Errorf("the second page asked for %q", rec.requests[1].query)
	}
}

func mustID(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
}
