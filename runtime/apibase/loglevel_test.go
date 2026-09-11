package apibase_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simonjanss/rig/runtime/apibase"
	"github.com/simonjanss/rig/runtime/reqlog"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// recordingTracer is a [apibase.Tracer] that remembers what it was told to
// redden, which the stub in apibase_test.go deliberately does not.
type recordingTracer struct{ failed *[]int }

func (t recordingTracer) Server(r *http.Request, _ string, _ func() int) (*http.Request, func()) {
	return r, func() {}
}
func (t recordingTracer) TraceID(*http.Request) string { return "trace-1" }
func (t recordingTracer) Fail(_ context.Context, status int, _ error) {
	*t.failed = append(*t.failed, status)
}

// line is the one JSON object a failure wrote.
type line struct {
	Level  string  `json:"level"`
	Msg    string  `json:"msg"`
	Status *int    `json:"status"`
	Code   *string `json:"code"`
	Error  string  `json:"error"`
}

// failWith runs one failure through the whole funnel and returns what was
// logged, what was written, and every status the tracer was asked to redden.
func failWith(t *testing.T, s apibase.Server, err error) (line, *httptest.ResponseRecorder, []int) {
	t.Helper()

	var buf bytes.Buffer
	var failed []int
	s.Logger = logging(&buf)
	s.Tracer = recordingTracer{failed: &failed}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil)
	rc := apibase.RequestContext{RequestID: "req-42", Method: http.MethodGet, Route: "GET /api/v1/todos"}

	apibase.Fail(s, w, r, rc, err)

	if buf.Len() == 0 {
		return line{}, w, failed
	}
	var got line
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", buf.String(), err)
	}
	return got, w, failed
}

// The level says whether anybody can act on the line, which is not the same
// question as the status class. The refusals in the first group are the ones rig
// produces structurally — one per expired session, one per page load before
// sign-in, one per cross-tenant row — and a warning stream made of those is one
// people learn to skim, which is what the rule is for.
func TestTheLevelSplitsByWhoCanActOnIt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err   error
		level string
		msg   string
	}{
		{rigerr.Unauthorized("sign in"), "DEBUG", "request refused"},
		{rigerr.Forbidden("not yours"), "DEBUG", "request refused"},
		{rigerr.NotFound("no such todo"), "DEBUG", "request refused"},
		{rigerr.Conflict("that slug is taken"), "DEBUG", "request refused"},
		{rigerr.Invalid("title is required"), "DEBUG", "request refused"},
		{rigerr.RateLimited("slow down"), "DEBUG", "request refused"},
		{rigerr.UpgradeRequired("regenerate your client"), "DEBUG", "request refused"},

		{rigerr.BadRequest("unknown field"), "WARN", "request refused"},
		{rigerr.TooLarge("too big"), "WARN", "request refused"},
		{rigerr.UnsupportedMediaType("send JSON"), "WARN", "request refused"},

		{fmt.Errorf("query: %w", context.DeadlineExceeded), "WARN", "request timed out"},
		{rigerr.Internal(errors.New("connection refused"), "listing todos"), "ERROR", "request failed"},
	}

	for _, tc := range cases {
		t.Run(string(rigerr.CodeOf(tc.err)), func(t *testing.T) {
			t.Parallel()

			got, _, _ := failWith(t, apibase.Server{}, tc.err)
			if got.Level != tc.level {
				t.Errorf("level = %s, want %s", got.Level, tc.level)
			}
			if got.Msg != tc.msg {
				t.Errorf("msg = %q, want %q", got.Msg, tc.msg)
			}
		})
	}
}

// A caller that went away is the quietest thing on the list, and used to be the
// loudest: it has no code, so it fell to CodeOf's default arm and arrived as a
// 500 with an ERROR line, a red span and a body written into a closed socket.
func TestAnAbandonedRequestIsNotAnError(t *testing.T) {
	t.Parallel()

	gone := rigerr.Internal(context.Canceled, "listing todos")
	got, w, failed := failWith(t, apibase.Server{}, gone)

	if got.Level != "DEBUG" {
		t.Errorf("level = %s, want DEBUG", got.Level)
	}
	if got.Msg != "request abandoned" {
		t.Errorf("msg = %q, want %q", got.Msg, "request abandoned")
	}
	// Absent rather than 500 and Internal. Printing the answer it would have
	// been is the same misfiling in a different field.
	if got.Status != nil {
		t.Errorf("status = %d, want none — there was no answer", *got.Status)
	}
	if got.Code != nil {
		t.Errorf("code = %q, want none — there was no answer", *got.Code)
	}

	if w.Body.Len() != 0 {
		t.Errorf("wrote %q into a socket nobody is reading", w.Body.String())
	}
	if len(failed) != 0 {
		t.Errorf("reddened the span with %v; nothing failed", failed)
	}
}

// The mapper is not reached either, so a project that replaced how a failure is
// answered does not get asked to answer one that needs no answer. It is still
// told what happened — the log line above is written first.
func TestAnAbandonedRequestNeverReachesTheErrorMapper(t *testing.T) {
	t.Parallel()

	mapped := false
	s := apibase.Server{
		OnError: func(http.ResponseWriter, *http.Request, apibase.RequestContext, error) {
			mapped = true
		},
	}

	got, _, _ := failWith(t, s, fmt.Errorf("listing todos: %w", context.Canceled))

	if mapped {
		t.Error("the mapper was asked to answer a request nobody is waiting for")
	}
	if got.Msg != "request abandoned" {
		t.Errorf("msg = %q; the line is still written", got.Msg)
	}
}

// A timeout is the mirror image and gets the opposite treatment: somebody is
// waiting, so it is answered, the span is red, and the level is loud enough to
// look at. Latency and capacity are worth a warning; a closed tab is not.
func TestATimeoutIsWarnedAboutAndStillAnswered(t *testing.T) {
	t.Parallel()

	slow := fmt.Errorf("acquire a connection: %w", context.DeadlineExceeded)
	got, w, failed := failWith(t, apibase.Server{}, slow)

	if got.Level != "WARN" {
		t.Errorf("level = %s, want WARN", got.Level)
	}
	if got.Msg != "request timed out" {
		t.Errorf("msg = %q, want %q", got.Msg, "request timed out")
	}
	if got.Status == nil || *got.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %v, want 503", got.Status)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("answered %d, want 503", w.Code)
	}
	if len(failed) != 1 || failed[0] != http.StatusServiceUnavailable {
		t.Errorf("reddened %v, want one span at 503", failed)
	}
}

// LevelFor is exported so a project's own OnError can match rig rather than
// guess, which is only true while it agrees with what LogFailure actually
// writes. This is the test that keeps the two from drifting.
func TestLevelForIsTheLevelRigWritesAt(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		rigerr.NotFound("no such todo"),
		rigerr.BadRequest("unknown field"),
		rigerr.Internal(errors.New("nope"), "listing todos"),
		fmt.Errorf("query: %w", context.DeadlineExceeded),
	} {
		code := rigerr.CodeOf(err)
		got, _, _ := failWith(t, apibase.Server{}, err)

		if want := apibase.LevelFor(code).String(); got.Level != want {
			t.Errorf("%s: wrote %s, LevelFor says %s", code, got.Level, want)
		}
	}
}

// A project can disagree, for the one thing rig cannot know: that a particular
// route should never 404, or that its public API takes enough hand-written
// requests that a 400 is noise rather than somebody's deploy.
func TestAProjectCanChooseItsOwnLevel(t *testing.T) {
	t.Parallel()

	s := apibase.Server{
		LogLevel: func(code rigerr.Code) slog.Level {
			if code == rigerr.CodeNotFound {
				return slog.LevelWarn
			}
			return apibase.LevelFor(code)
		},
	}

	got, _, _ := failWith(t, s, rigerr.NotFound("no such todo"))
	if got.Level != "WARN" {
		t.Errorf("level = %s, want the project's WARN", got.Level)
	}

	// And the codes it did not name are still rig's.
	got, _, _ = failWith(t, s, rigerr.Forbidden("not yours"))
	if got.Level != "DEBUG" {
		t.Errorf("level = %s, want DEBUG", got.Level)
	}
}

// A hook cannot silence a failure, however it answers. The lowest level it can
// return is still a level, and the line that says why a 500 happened is the one
// thing this package writes whether or not anybody asked it to.
func TestAProjectsLevelStillWritesTheLine(t *testing.T) {
	t.Parallel()

	s := apibase.Server{
		LogLevel: func(rigerr.Code) slog.Level { return slog.LevelDebug },
	}

	got, _, _ := failWith(t, s, rigerr.Internal(errors.New("nope"), "listing todos"))
	if got.Msg != "request failed" {
		t.Errorf("msg = %q, want the line to still be written", got.Msg)
	}
	if got.Error == "" {
		t.Error("the cause is the whole reason the line exists")
	}
}

// selfCoded carries a code this package has never heard of, which is what a
// project's own Coder is.
type selfCoded struct{ code rigerr.Code }

func (selfCoded) Error() string            { return "something a project invented" }
func (s selfCoded) ErrorCode() rigerr.Code { return s.code }

// An unmapped code is levelled by the status it will be answered with, rather
// than by a list this package would have to be told about. That is the same
// `>= 500` rule the span uses, so the line and the span cannot disagree.
func TestAnUnknownCodeIsLevelledByItsStatus(t *testing.T) {
	t.Parallel()

	got, _, failed := failWith(t, apibase.Server{}, selfCoded{code: rigerr.Code("Wat")})

	if got.Level != "ERROR" {
		t.Errorf("level = %s, want ERROR — an unmapped code is answered 500", got.Level)
	}
	if got.Msg != "request failed" {
		t.Errorf("msg = %q, want %q", got.Msg, "request failed")
	}
	if len(failed) != 1 || failed[0] != http.StatusInternalServerError {
		t.Errorf("reddened %v, want one span at 500", failed)
	}
}

// And the request line reports no status at all for it, which is what the
// abandoned line is for.
//
// Zero rather than 200: reqlog.Writer records what a handler wrote, and nothing
// was written, so there is no status to report. net/http still puts its implicit
// 200 on a socket nobody is reading, and a request line that repeated it would
// be filing an abandoned request next to every request that succeeded — the same
// misfiling one field over.
func TestTheRequestLineReportsNoStatusForAnAbandonedRequest(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := apibase.Server{Logger: logging(&buf)}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/todos", nil)
	rc := apibase.RequestContext{RequestID: "req-42", Method: http.MethodGet, Route: "GET /api/v1/todos"}
	rec := reqlog.Wrap(httptest.NewRecorder())

	apibase.Fail(s, rec, r, rc, rigerr.Internal(context.Canceled, "listing todos"))
	apibase.LogRequest(s, r, rec, rc)

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want the failure and the request line: %s", len(lines), buf.String())
	}

	var served line
	if err := json.Unmarshal(lines[1], &served); err != nil {
		t.Fatalf("decoding %q: %v", lines[1], err)
	}
	if served.Msg != "request served" {
		t.Fatalf("second line is %q, want the request line", served.Msg)
	}
	if served.Status == nil || *served.Status != 0 {
		t.Errorf("status = %v, want 0 — nothing was written", served.Status)
	}
}
