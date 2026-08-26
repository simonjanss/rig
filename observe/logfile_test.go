package observe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/observe"
	"go.opentelemetry.io/otel/trace"
)

// sink is a log file in a directory of its own, and a logger writing to it.
func sink(t *testing.T, cfg observe.LogConfig) (*slog.Logger, *observe.Logs, string) {
	t.Helper()

	if cfg.File == "" {
		cfg.File = filepath.Join(t.TempDir(), "log.jsonl")
	}
	logs, err := observe.OpenLogs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })

	if why := logs.Unarmed(); why != "" {
		t.Fatalf("the sink will write nothing: %s", why)
	}
	return slog.New(logs.Handler()), logs, cfg.File
}

// written is every line in the file, decoded.
func written(t *testing.T, logs *observe.Logs) []observe.LogRecord {
	t.Helper()

	recs, err := logs.Read(100)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// The four fields anybody scans for are at the front of the line, and
// everything else is under attrs — where a field called "level" cannot
// overwrite the level.
func TestALineBecomesARecord(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	logger.WarnContext(context.Background(), "disk is filling", "free_mb", 42, "level", "not this one")

	recs := written(t, logs)
	if len(recs) != 1 {
		t.Fatalf("wrote %d lines, want 1", len(recs))
	}

	rec := recs[0]
	if rec.Msg != "disk is filling" {
		t.Errorf("msg = %q", rec.Msg)
	}
	if rec.Level != "WARN" {
		t.Errorf("level = %q, want WARN", rec.Level)
	}
	if rec.Time.IsZero() {
		t.Error("the line carries no time")
	}
	if got := fmt.Sprint(rec.Attrs["free_mb"]); got != "42" {
		t.Errorf("attrs.free_mb = %q", got)
	}
	if rec.Attrs["level"] != "not this one" {
		t.Errorf(`attrs.level = %v, so an attribute called "level" did not survive`, rec.Attrs["level"])
	}
}

// The generated server writes one grouped attribute rather than five loose
// ones, and it is a slog.LogValuer. Both have to arrive, because the route and
// the request id in that group are what the page's columns are.
func TestAGroupIsNestedAndResolved(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	logger.InfoContext(context.Background(), "request served",
		slog.Any("request", requestish{Route: "GET /api/v1/teams", RequestID: "abc123"}),
		slog.Group("answer", slog.Int("status", 200)))

	rec := written(t, logs)[0]

	group, ok := rec.Attrs["request"].(map[string]any)
	if !ok {
		t.Fatalf("attrs.request = %#v, want a nested object", rec.Attrs["request"])
	}
	if group["route"] != "GET /api/v1/teams" || group["request_id"] != "abc123" {
		t.Errorf("attrs.request = %#v", group)
	}

	answer, ok := rec.Attrs["answer"].(map[string]any)
	if !ok || fmt.Sprint(answer["status"]) != "200" {
		t.Errorf("attrs.answer = %#v", rec.Attrs["answer"])
	}
}

// requestish is a stand-in for the generated RequestContext: a value that says
// what it is worth on a log line and nothing else.
type requestish struct {
	Route     string
	RequestID string
}

func (r requestish) LogValue() slog.Value {
	return slog.GroupValue(slog.String("route", r.Route), slog.String("request_id", r.RequestID))
}

// An error marshals to {} — most have no exported field — and the wrapped cause
// of a 500 is the single most valuable thing on any line rig writes.
func TestAnErrorBecomesItsMessage(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	cause := fmt.Errorf("listing teams: %w", errors.New("connection refused"))
	logger.ErrorContext(context.Background(), "request failed", slog.Any("error", cause))

	rec := written(t, logs)[0]
	if rec.Attrs["error"] != "listing teams: connection refused" {
		t.Errorf("attrs.error = %#v, so the cause of a 500 is not in the file", rec.Attrs["error"])
	}
}

// Newest first, which is what a person opening a log wants and the opposite of
// what [observe.ReadSpans] answers.
//
// Two other tests already depend on the order — they index `recs[1]` for the
// line written first and say so in a comment — but neither is named for it, and
// the doc comment on Read said "oldest first" for as long as it existed. A test
// whose name is the property is what makes the next reader who notices the
// disagreement fix the comment in the right direction.
func TestLogsReadIsNewestFirst(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	logger.InfoContext(context.Background(), "first")
	logger.InfoContext(context.Background(), "second")

	recs := written(t, logs)
	if len(recs) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(recs))
	}
	if recs[0].Msg != "second" || recs[1].Msg != "first" {
		t.Errorf("Read gave %q then %q, want the newest line first",
			recs[0].Msg, recs[1].Msg)
	}
}

// The trace is the whole reason every log call in rig passes the context.
func TestTheTraceComesFromTheContext(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	logger.InfoContext(ctx, "inside a request")
	logger.InfoContext(context.Background(), "outside one")

	recs := written(t, logs)
	// Newest first.
	if recs[1].TraceID != traceID.String() || recs[1].SpanID != spanID.String() {
		t.Errorf("the line inside a trace carries %q/%q", recs[1].TraceID, recs[1].SpanID)
	}
	if recs[0].TraceID != "" {
		t.Errorf("a line outside a trace carries trace_id %q", recs[0].TraceID)
	}
}

// The point of the sink having its own level. rig's request line is at debug, so
// a deployment running at info writes it nowhere — and the monitoring page has
// no requests to list. The file keeps it; stderr does not.
func TestTheFileKeepsDebugWhileTheOtherHandlerDoesNot(t *testing.T) {
	var stderr bytes.Buffer
	logs, err := observe.OpenLogs(observe.LogConfig{File: filepath.Join(t.TempDir(), "log.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()

	logger := slog.New(observe.Tee(
		slog.NewJSONHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
		logs.Handler(),
	))

	logger.DebugContext(context.Background(), "request served")
	logger.ErrorContext(context.Background(), "request failed")

	if got := strings.Count(stderr.String(), "\n"); got != 1 {
		t.Errorf("stderr got %d lines, want only the error one:\n%s", got, stderr.String())
	}
	if recs := written(t, logs); len(recs) != 2 {
		t.Errorf("the file got %d lines, want both", len(recs))
	}
}

// A logger derived with attributes or a group is one handler shared by several
// loggers, which is the place a hand-written handler leaks one line's fields
// onto the next.
func TestWithAttrsAndWithGroupAccumulate(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	base := logger.With("service", "todo")
	base.With("worker", 3).WithGroup("job").InfoContext(context.Background(), "started", "name", "prune")
	base.InfoContext(context.Background(), "plain")

	recs := written(t, logs)
	if len(recs) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(recs))
	}

	plain, nested := recs[0], recs[1]
	if plain.Attrs["service"] != "todo" {
		t.Errorf("the plain line lost its accumulated attribute: %#v", plain.Attrs)
	}
	if _, leaked := plain.Attrs["worker"]; leaked {
		t.Errorf("the plain line picked up the other logger's attributes: %#v", plain.Attrs)
	}
	if _, leaked := plain.Attrs["job"]; leaked {
		t.Errorf("the plain line picked up the other logger's group: %#v", plain.Attrs)
	}

	if nested.Attrs["service"] != "todo" || fmt.Sprint(nested.Attrs["worker"]) != "3" {
		t.Errorf("the nested line's accumulated attributes = %#v", nested.Attrs)
	}
	job, ok := nested.Attrs["job"].(map[string]any)
	if !ok || job["name"] != "prune" {
		t.Errorf("attrs.job = %#v, want the record's own attributes inside the group", nested.Attrs["job"])
	}
}

// A group nothing was ever added to is absent rather than present and empty,
// which is the rule slog itself follows.
func TestAnEmptyGroupIsAbsent(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	logger.WithGroup("job").InfoContext(context.Background(), "nothing in it")
	logger.InfoContext(context.Background(), "empty group attr", slog.Group("also"))

	for _, rec := range written(t, logs) {
		if _, present := rec.Attrs["job"]; present {
			t.Errorf("an empty group survived: %#v", rec.Attrs)
		}
		if _, present := rec.Attrs["also"]; present {
			t.Errorf("an empty group attribute survived: %#v", rec.Attrs)
		}
	}
}

// The promise is exact: no file ever exceeds the cap, and the disk cost is
// twice it because one generation is kept.
func TestTheLogFileRotatesAtItsCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")

	logger, logs, _ := sink(t, observe.LogConfig{File: path, FileMaxBytes: 900})
	for i := range 40 {
		logger.InfoContext(context.Background(), "a line with something on it", "i", i)
	}

	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if current.Size() > 900 {
		t.Errorf("the current file is %d bytes, over its 900-byte cap", current.Size())
	}

	rotated, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("nothing rotated: %v", err)
	}
	if rotated.Size() > 900 {
		t.Errorf("the rotated file is %d bytes, over the cap", rotated.Size())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the directory holds %d files, want the current one and one generation", len(entries))
	}

	// And the lines that are left are still readable.
	if recs := written(t, logs); len(recs) == 0 {
		t.Error("rotation left nothing to read")
	}
}

// No file is the ordinary case on a laptop and in CI. It is not an error, and
// teeing it into a logger changes nothing about what that logger does.
func TestAnUnarmedSinkWritesNothingAndSaysWhy(t *testing.T) {
	t.Setenv(observe.LogFileEnv, "")

	logs, err := observe.OpenLogs(observe.LogConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if why := logs.Unarmed(); !strings.Contains(why, observe.LogFileEnv) {
		t.Errorf("Unarmed() = %q, and it should name the variable to set", why)
	}
	if logs.File() != "" {
		t.Errorf("File() = %q for a sink with no file", logs.File())
	}

	var stderr bytes.Buffer
	logger := slog.New(observe.Tee(slog.NewJSONHandler(&stderr, nil), logs.Handler()))
	logger.InfoContext(context.Background(), "still writes to the other one")

	if !strings.Contains(stderr.String(), "still writes") {
		t.Errorf("teeing an unarmed sink swallowed the line: %q", stderr.String())
	}
	if recs, err := logs.Read(10); err != nil || len(recs) != 0 {
		t.Errorf("Read() = %d records, %v", len(recs), err)
	}
}

// A nil sink is what a page gets from a project that wired none, and it has to
// answer rather than panic — the page asks it three questions.
func TestANilSinkAnswers(t *testing.T) {
	var logs *observe.Logs

	if why := logs.Unarmed(); why == "" {
		t.Error("a nil sink says it is armed")
	}
	if logs.File() != "" {
		t.Error("a nil sink names a file")
	}
	if err := logs.Close(); err != nil {
		t.Errorf("closing a nil sink = %v", err)
	}
	if recs, err := logs.Read(10); err != nil || recs != nil {
		t.Errorf("reading a nil sink = %v, %v", recs, err)
	}
	if h := logs.Handler(); h.Enabled(context.Background(), slog.LevelError) {
		t.Error("a nil sink's handler is enabled")
	}
}

// Tee with one handler is that handler, so the ordinary case costs nothing.
func TestTeeOfOneIsThatOne(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	if got := observe.Tee(h); got != slog.Handler(h) {
		t.Errorf("Tee(h) wrapped a single handler")
	}
	if observe.Tee().Enabled(context.Background(), slog.LevelError) {
		t.Error("Tee() with nothing in it is enabled")
	}
}

// A duration on a line is read by a person, and a count of nanoseconds is not.
func TestADurationIsReadable(t *testing.T) {
	logger, logs, _ := sink(t, observe.LogConfig{})

	logger.InfoContext(context.Background(), "slow", slog.Duration("took", 1500*1000*1000))

	rec := written(t, logs)[0]
	if rec.Attrs["took"] != "1.5s" {
		t.Errorf("attrs.took = %#v, want 1.5s", rec.Attrs["took"])
	}
}

// The file is JSON per line, so grep and jq work on it without rig in the loop.
func TestTheFileIsOneJSONObjectPerLine(t *testing.T) {
	logger, _, path := sink(t, observe.LogConfig{})

	logger.InfoContext(context.Background(), "one")
	logger.InfoContext(context.Background(), "two")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("the file holds %d lines, want 2:\n%s", len(lines), data)
	}
	for _, line := range lines {
		var into map[string]any
		if err := json.Unmarshal([]byte(line), &into); err != nil {
			t.Errorf("line %q does not parse: %v", line, err)
		}
	}
}
