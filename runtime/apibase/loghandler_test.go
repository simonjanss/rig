package apibase_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/simonjanss/rig/runtime/apibase"
)

// logging returns a logger writing JSON into buf, wrapped the way the generated
// Mount wraps the one an application is handed.
func logging(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(apibase.LogHandler(h))
}

// inRequest is a context as a handler hands it to a service.
func inRequest() context.Context {
	return apibase.NewContext(context.Background(), apibase.RequestContext{
		RequestID: "req-42",
		Method:    http.MethodGet,
		Route:     "GET /api/v1/todos",
	})
}

// TestALineWrittenInsideARequestSaysWhichOne is the whole point of the handler:
// a service writes what it has to say, and the request arrives on the line
// without the service saying so.
func TestALineWrittenInsideARequestSaysWhichOne(t *testing.T) {
	var buf bytes.Buffer
	logging(&buf).InfoContext(inRequest(), "importing", slog.Int("rows", 12))

	for _, want := range []string{`"rows":12`, `"request_id":"req-42"`, `"route":"GET /api/v1/todos"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %s in:\n%s", want, buf.String())
		}
	}
}

// TestWorkThatCameFromNoRequestSaysNothingAboutOne is the other half. A
// migration and a background job log through the same logger, and a line that
// claimed to belong to a request would be worse than one that says nothing.
func TestWorkThatCameFromNoRequestSaysNothingAboutOne(t *testing.T) {
	var buf bytes.Buffer
	logging(&buf).InfoContext(context.Background(), "sweeping")

	if strings.Contains(buf.String(), "request") {
		t.Errorf("a line outside a request was labelled with one:\n%s", buf.String())
	}
}

// TestTheRequestIsNotWrittenTwice covers a service that puts the group on the
// line itself, which is what rig's own two lines do and what its documentation
// used to ask for. One group, not two.
func TestTheRequestIsNotWrittenTwice(t *testing.T) {
	var buf bytes.Buffer
	ctx := inRequest()
	rc, _ := apibase.RequestContextFrom(ctx)
	logging(&buf).InfoContext(ctx, "importing", slog.Any("request", rc))

	if n := strings.Count(buf.String(), `"request":`); n != 1 {
		t.Errorf("the request appears %d times, want 1:\n%s", n, buf.String())
	}
}

// TestWrappingTwiceIsWrappingOnce is what lets the generated Mount wrap a
// logger without knowing whether the application wrapped it already.
func TestWrappingTwiceIsWrappingOnce(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)

	once := apibase.LogHandler(inner)
	if twice := apibase.LogHandler(once); twice != once {
		t.Error("wrapping a wrapped handler produced a second layer")
	}
}

// TestADerivedLoggerKeepsTheDecoration is the test that catches the one
// implementation mistake worth catching: embedding slog.Handler promotes
// WithAttrs and WithGroup, each of which returns the inner handler — so the
// first With anywhere in an application would silently undo this.
func TestADerivedLoggerKeepsTheDecoration(t *testing.T) {
	t.Run("With", func(t *testing.T) {
		var buf bytes.Buffer
		logging(&buf).With(slog.String("component", "importer")).InfoContext(inRequest(), "importing")

		if !strings.Contains(buf.String(), `"request_id":"req-42"`) {
			t.Errorf("With dropped the request:\n%s", buf.String())
		}
	})

	// Nested, because an attribute a handler adds at Handle time lands inside
	// whatever group is open. That is ordinary slog semantics rather than
	// something to undo, and the point of the test is that it is still there.
	t.Run("WithGroup nests it", func(t *testing.T) {
		var buf bytes.Buffer
		logging(&buf).WithGroup("db").InfoContext(inRequest(), "querying")

		var line struct {
			DB struct {
				Request struct {
					RequestID string `json:"request_id"`
				} `json:"request"`
			} `json:"db"`
		}
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("the line is not JSON: %v\n%s", err, buf.String())
		}
		if line.DB.Request.RequestID != "req-42" {
			t.Errorf("db.request.request_id = %q, want req-42:\n%s", line.DB.Request.RequestID, buf.String())
		}
	})
}

// TestTheLevelIsStillTheInnerHandlersToDecide covers the cheap half of the
// contract: a sink that answers no is not turned on by being wrapped.
func TestTheLevelIsStillTheInnerHandlersToDecide(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.New(apibase.LogHandler(h)).DebugContext(inRequest(), "counting")

	if buf.Len() != 0 {
		t.Errorf("a debug line got through a warn handler:\n%s", buf.String())
	}
}

// TestRequestLoggerTakesNilAsTheDefault is [apibase.Server.Logger]'s reading of
// nil, in the one other place a logger arrives possibly unset.
func TestRequestLoggerTakesNilAsTheDefault(t *testing.T) {
	if apibase.RequestLogger(nil) == nil {
		t.Fatal("RequestLogger(nil) is nil, want the default logger wrapped")
	}
}

// ExampleLogHandler is what a service's own line looks like once the logger it
// was handed has been through here. Nothing in the call site names the request.
func ExampleLogHandler() {
	// The time is dropped so this example has one output; a real handler keeps it.
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	logger := slog.New(apibase.LogHandler(h))

	// The server puts this on the context before any service is called.
	ctx := apibase.NewContext(context.Background(), apibase.RequestContext{
		RequestID: "01930f3c-6f7a-7b8c-9d0e-1f2a3b4c5d6e",
		Method:    http.MethodPost,
		Route:     "POST /api/v1/todos",
	})

	logger.InfoContext(ctx, "importing", slog.Int("rows", 12))

	// Output:
	// {"level":"INFO","msg":"importing","rows":12,"request":{"request_id":"01930f3c-6f7a-7b8c-9d0e-1f2a3b4c5d6e","method":"POST","route":"POST /api/v1/todos"}}
}
