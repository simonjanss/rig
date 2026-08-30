// What the generated server says when a request fails.
//
// No database and no build tag: a handler takes the service as an interface, so
// a service that only fails is enough to drive the whole path a real 500 takes.
// That is the point of these being here rather than in the generator suites —
// those compile the emitted code, and this runs it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/runtime/apibase"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The milestone this file exists for. DefaultErrorMapper rewrites an internal
// message to "something went wrong" before it reaches the client, deliberately,
// because the original is the kind of thing that leaks a table name. Without a
// line here the cause would exist nowhere at all: the client is told nothing and
// so is everybody else.
func TestAnInternalFailureIsLoggedWithItsCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("dial tcp 10.0.0.7:5432: connection refused")
	log, res := call(t, api.Server{}, rigerr.Internal(cause, "listing todos"))

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.Code)
	}

	entry := only(t, log, slog.LevelError)
	if !strings.Contains(entry, cause.Error()) {
		t.Errorf("the error line does not carry the cause:\n%s", entry)
	}

	// The other half, and it is the reason the cause is worth keeping: what the
	// client got says nothing, so the log is the only copy.
	var body struct {
		Message   string `json:"message"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}
	if body.Message != "something went wrong" {
		t.Errorf("message = %q, want the internal detail withheld", body.Message)
	}
	if strings.Contains(res.Body.String(), cause.Error()) {
		t.Error("the cause reached the client")
	}
}

// A project that set OnError replaced how a failure is answered. It did not ask
// to stop being told what the failure was, which is why the line is written in
// fail rather than inside the mapper OnError displaces.
func TestACustomErrorMapperStillGetsTheLine(t *testing.T) {
	t.Parallel()

	var called bool
	srv := api.Server{
		OnError: func(w http.ResponseWriter, _ *http.Request, _ api.RequestContext, _ error) {
			called = true
			w.WriteHeader(http.StatusTeapot)
		},
	}

	cause := errors.New("the pool is closed")
	log, res := call(t, srv, rigerr.Internal(cause, "listing todos"))

	if !called || res.Code != http.StatusTeapot {
		t.Fatalf("OnError did not answer: called=%v status=%d", called, res.Code)
	}
	if entry := only(t, log, slog.LevelError); !strings.Contains(entry, cause.Error()) {
		t.Errorf("the error line does not carry the cause:\n%s", entry)
	}
}

// A 404 is the server working. Logging it at error is how a log becomes a thing
// nobody reads, so it is debug and the level is the assertion.
func TestARefusalIsNotAnError(t *testing.T) {
	t.Parallel()

	log, res := call(t, api.Server{}, rigerr.NotFound("no such todo"), slog.LevelDebug)

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
	if strings.Contains(log.String(), `"level":"ERROR"`) {
		t.Errorf("a 404 was logged as an error:\n%s", log)
	}
	if entry := only(t, log, slog.LevelDebug, "request refused"); !strings.Contains(entry, `"status":404`) {
		t.Errorf("the refusal does not say what was answered:\n%s", entry)
	}
}

// The same 404, at the level a server actually runs at. A refusal is debug, so
// nothing at all comes out — which is the point of it being debug.
func TestARefusalIsSilentAtTheDefaultLevel(t *testing.T) {
	t.Parallel()

	log, _ := call(t, api.Server{}, rigerr.NotFound("no such todo"))

	if log.Len() != 0 {
		t.Errorf("a 404 wrote to the log at the default level:\n%s", log)
	}
}

// The request identifier is the whole mechanism for answering "what happened to
// my request": it goes out in the body and it comes out in the log, and a pair
// that did not match would be two facts about nothing.
func TestTheLineCarriesWhatTheClientWasHanded(t *testing.T) {
	t.Parallel()

	srv := api.Server{RequestID: func(*http.Request) string { return "req-42" }}
	log, res := call(t, srv, rigerr.Internal(errors.New("boom"), "listing todos"))

	var body struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}
	if body.RequestID != "req-42" {
		t.Fatalf("request_id in the body = %q, want req-42", body.RequestID)
	}

	entry := only(t, log, slog.LevelError)
	for _, want := range []string{`"request_id":"req-42"`, `"route":"GET /api/v1/todos"`} {
		if !strings.Contains(entry, want) {
			t.Errorf("the error line is missing %s:\n%s", want, entry)
		}
	}
}

// This project does not trace and this caller sent no header, which used to be
// a request nothing could be asked about: an error body with no requestId and a
// line with no request_id. It is named anyway now, and the pair is the point —
// somebody quoting the string from their screen is quoting the string in the
// log.
func TestEveryRequestIsNamedEvenWithoutTracing(t *testing.T) {
	t.Parallel()

	log, rec := call(t, api.Server{}, rigerr.Internal(errors.New("boom"), "listing todos"))

	var body struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error body is not JSON: %v", err)
	}
	if body.RequestID == "" {
		t.Fatal("the error body names no request")
	}
	if _, err := uuid.Parse(body.RequestID); err != nil {
		t.Errorf("requestId = %q, want a uuid: %v", body.RequestID, err)
	}

	if want := `"request_id":"` + body.RequestID + `"`; !strings.Contains(log.String(), want) {
		t.Errorf("the log does not carry what the client was told (%s):\n%s", want, log)
	}
}

// The request line is debug, so a server at the default level writes one line
// per failed request and nothing at all per successful one.
func TestTheRequestLineIsDebugAndCarriesTheAnswer(t *testing.T) {
	t.Parallel()

	log, _ := call(t, api.Server{}, rigerr.NotFound("no such todo"), slog.LevelDebug)

	entry := only(t, log, slog.LevelDebug, "request served")
	for _, want := range []string{`"status":404`, `"bytes":`, `"route":"GET /api/v1/todos"`} {
		if !strings.Contains(entry, want) {
			t.Errorf("the request line is missing %s:\n%s", want, entry)
		}
	}
}

// A service that wants the request on its own lines can still reach it the way
// anything else does: RequestContextFrom, and one attribute. This is the shape
// for a logger the application built itself and did not get from app.Logger —
// see the test below for the one it did.
func TestAServiceCanPutTheRequestOnItsOwnLines(t *testing.T) {
	t.Parallel()

	var log bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&log, nil))
	srv := api.Server{RequestID: func(*http.Request) string { return "req-7" }}

	todos := &stubTodos{list: func(ctx context.Context) error {
		rc, ok := api.RequestContextFrom(ctx)
		if !ok {
			t.Error("a service method cannot see the request it is serving")
		}
		logger.InfoContext(ctx, "counting", slog.Any("request", rc))
		return rigerr.NotFound("no such todo")
	}}
	drive(t, srv, todos)

	entry := only(t, &log, slog.LevelInfo, "counting")
	for _, want := range []string{`"request_id":"req-7"`, `"route":"GET /api/v1/todos"`} {
		if !strings.Contains(entry, want) {
			t.Errorf("the service's line is missing %s:\n%s", want, entry)
		}
	}
}

// And the ordinary case: the logger the application was handed has already been
// through apibase.LogHandler — api.Mount does it before anything is built out
// of it — so a service writes what it has to say and the request arrives on the
// line without the service naming it.
//
// This is the same test as the one above with the attribute deleted, which is
// the diff worth reading: nothing at the call site says which request this is.
func TestAServiceGetsTheRequestWithoutAskingForIt(t *testing.T) {
	t.Parallel()

	var log bytes.Buffer
	logger := apibase.RequestLogger(slog.New(slog.NewJSONHandler(&log, nil)))
	srv := api.Server{RequestID: func(*http.Request) string { return "req-8" }}

	todos := &stubTodos{list: func(ctx context.Context) error {
		logger.InfoContext(ctx, "counting")
		return rigerr.NotFound("no such todo")
	}}
	drive(t, srv, todos)

	entry := only(t, &log, slog.LevelInfo, "counting")
	for _, want := range []string{`"request_id":"req-8"`, `"route":"GET /api/v1/todos"`} {
		if !strings.Contains(entry, want) {
			t.Errorf("the service's line is missing %s:\n%s", want, entry)
		}
	}
}

// The request is one grouped attribute, not five loose ones, so a line about a
// request has a shape rather than a spelling.
func TestTheRequestIsOneGroupedAttribute(t *testing.T) {
	t.Parallel()

	srv := api.Server{RequestID: func(*http.Request) string { return "req-9" }}
	log, _ := call(t, srv, rigerr.Internal(errors.New("boom"), "listing todos"))

	entry := only(t, log, slog.LevelError)
	if !strings.Contains(entry, `"request":{`) {
		t.Errorf("the request is not a group:\n%s", entry)
	}
	// Outside the group, because they are about the answer rather than the ask.
	for _, want := range []string{`"status":500`, `"code":"Internal"`} {
		if !strings.Contains(entry, want) {
			t.Errorf("the line is missing %s:\n%s", want, entry)
		}
	}
}

// call drives GET /api/v1/todos against a service that fails with err.
func call(t *testing.T, srv api.Server, err error, level ...slog.Leveler) (*bytes.Buffer, *httptest.ResponseRecorder) {
	t.Helper()

	var log bytes.Buffer
	opts := &slog.HandlerOptions{}
	if len(level) > 0 {
		opts.Level = level[0]
	}
	if srv.Logger == nil {
		srv.Logger = slog.New(slog.NewJSONHandler(&log, opts))
	}

	return &log, drive(t, srv, &stubTodos{list: func(context.Context) error { return err }})
}

func drive(t *testing.T, srv api.Server, todos api.TodoService) *httptest.ResponseRecorder {
	t.Helper()

	srv.GetClaims = func(*http.Request) (tenancy.Claims, error) {
		return tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}, nil
	}
	// Register requires one, and nothing here reaches it: every request below is
	// a read, and a write only opens a transaction when it carries an
	// Idempotency-Key. A Beginner that refuses says so rather than pretending.
	srv.DB = noDB{}

	mux := api.Register(api.Handlers{Server: srv, Todo: todos})
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil))
	return res
}

// only returns the one log record at level with the given message, and fails
// when there is not exactly one. Counting matters: the bug worth catching is a
// line emitted twice, once per mapper.
//
// The message is matched whole. "request" and "request refused" are two records
// on the same failed request, and a substring match would find both and call it
// a duplicate.
func only(t *testing.T, log *bytes.Buffer, level slog.Leveler, msg ...string) string {
	t.Helper()

	var found []string
	for line := range strings.SplitSeq(strings.TrimSpace(log.String()), "\n") {
		if line == "" || !strings.Contains(line, `"level":"`+level.Level().String()+`"`) {
			continue
		}
		if len(msg) > 0 && !strings.Contains(line, `"msg":"`+msg[0]+`"`) {
			continue
		}
		found = append(found, line)
	}

	if len(found) != 1 {
		t.Fatalf("want exactly one %s record, got %d:\n%s", level.Level(), len(found), log)
	}
	return found[0]
}

// stubTodos is a TodoService that only does the one thing a test asked for.
//
// The interface is embedded and nil, so any other method panics rather than
// quietly answering: a test that reached one meant to drive a different route.
type stubTodos struct {
	api.TodoService
	list func(context.Context) error
}

func (s *stubTodos) List(ctx context.Context, _ api.Request[struct{}, api.TodoListQuery, struct{}]) (*api.TodoListResponse, error) {
	return nil, s.list(ctx)
}

// AdoptChildren is called by Register, on every service, before any route is
// mounted — so it is the one method a stub has to answer whatever it is testing.
func (*stubTodos) AdoptChildren(api.TodoChildDeletes) {}

// noDB satisfies Server.DB for a suite with no database.
//
// Register refuses a nil one, deliberately: a nil pool would make every
// Idempotency-Key a header nobody read. Nothing in this file sends one, so this
// is never called, and it fails loudly rather than quietly if that changes.
type noDB struct{}

func (noDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("log_test: no database in this suite")
}
