package observe

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// LogFileEnv is where the log file's path comes from unless the application
// names one itself. It is rig's, and so is named for rig, the same as
// [FileEnv].
const LogFileEnv = "RIG_LOG_FILE"

// LogRecord is one line of the log file: one [log/slog] record, as JSON.
//
// The format is rig's own for [SpanRecord]'s reason — a person is reading this,
// with grep or with the monitoring page — so the four fields anybody scans for
// are at the front of the line and every identifier is a hex string that can be
// pasted into a search.
//
// It is exported because reading the file is a thing to do: decode a line into
// this and every field is named.
type LogRecord struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`

	// TraceID and SpanID are the trace this line was written inside, taken from
	// the context the log call was given. Empty for a line written outside a
	// request, or by a process that is not tracing.
	//
	// This is what makes a log line and a request the same view on the
	// monitoring page, and it is why every log call in rig passes the context:
	// slog hands the context to the handler, and a bare Info would arrive here
	// with nothing to find.
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`

	// Attrs is everything else the line carried, nested rather than flattened
	// into the object above: a line is free to have an attribute called "level"
	// or "msg", and a flat record would let it overwrite one. A slog group is a
	// map here, so the generated server's slog.Any("request", rc) arrives as
	// attrs.request.route and attrs.request.request_id.
	Attrs map[string]any `json:"attrs,omitempty"`
}

// UnmarshalJSON reads one line, whether rig wrote it or some other handler did.
//
// The five named fields are taken by name and *every other top-level key falls
// into [LogRecord.Attrs]*, which is the whole of what makes this reader work
// over a file a project's own [log/slog.JSONHandler] produced: that handler
// writes its attributes at the top level, where the plain struct decoding would
// drop them on the floor.
//
// Marshalling is the ordinary struct encoding, so a line rig writes round-trips
// and one it did not is normalised on the way in. A level that is not a string
// is left empty rather than failing the line: one field rig does not recognise
// should not lose the message beside it.
func (rec *LogRecord) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*rec = LogRecord{}
	for key, val := range raw {
		switch key {
		case "time":
			if err := json.Unmarshal(val, &rec.Time); err != nil {
				return err
			}
		case "level":
			_ = json.Unmarshal(val, &rec.Level)
		case "msg":
			_ = json.Unmarshal(val, &rec.Msg)
		case "trace_id":
			_ = json.Unmarshal(val, &rec.TraceID)
		case "span_id":
			_ = json.Unmarshal(val, &rec.SpanID)
		case "attrs":
			if err := json.Unmarshal(val, &rec.Attrs); err != nil {
				return err
			}
		default:
			var into any
			if err := json.Unmarshal(val, &into); err != nil {
				return err
			}
			if rec.Attrs == nil {
				rec.Attrs = make(map[string]any, len(raw))
			}
			rec.Attrs[key] = into
		}
	}
	return nil
}

// LogConfig says where the log file goes and what it keeps.
//
// Where it goes is deliberately not in rig.yaml, for [Config]'s reason: the
// same binary runs on a laptop, in CI and in production, and a project that had
// to regenerate to stop writing a file would be a project that writes one from
// its test suite.
type LogConfig struct {
	// File is the path to append log lines to, one JSON object per line. Empty
	// falls back to $RIG_LOG_FILE and then to writing none, which is the
	// ordinary case rather than the broken one.
	File string

	// FileMaxBytes is where File rotates, keeping one previous generation. Zero
	// means [DefaultFileMaxBytes]. An append-only file with no ceiling is the
	// thing that fills a disk at three in the morning.
	FileMaxBytes int64

	// Level is what the *file* keeps, and nil means [log/slog.LevelDebug].
	//
	// Its own level, and not the application logger's, is the point. rig's
	// request line and its refusal line are at debug — logging a 404 at error
	// is how a log becomes a thing nobody reads — so a deployment running at
	// info writes them nowhere and nobody ever sees them. The file keeps them
	// and stderr does not, which is the whole reason the monitoring page has a
	// request to list at all.
	//
	// A [log/slog.Leveler] rather than a Level because Level's zero value is
	// LevelInfo, and a field whose zero meant debug would be a lie.
	Level slog.Leveler
}

// Logs is the log file this process writes, and the reader over it.
//
// It is one object rather than a path passed to two places, because the page
// and the writer agreeing on a file is not something a main should have to
// arrange twice — the same argument [Provider.Page] makes about the span file.
// Hand it to [PageConfig.Logs].
type Logs struct {
	f   *rotatingFile
	cfg LogConfig
	// unarmed is why this sink will write nothing, or empty when it will.
	unarmed string
}

// OpenLogs opens the log file, creating the directory it lives in.
//
// Having no file is not an error: it is the ordinary case on a laptop and in CI,
// and it returns a sink whose handler is never enabled, so teeing it into a
// logger costs a branch per line and nothing else. [Logs.Unarmed] says which,
// in a line a main can log.
//
//	logs, err := observe.OpenLogs(observe.LogConfig{})
//	if err != nil {
//	    return err
//	}
//	logger := slog.New(observe.Tee(base, logs.Handler()))
//
// The file is opened here rather than at the first line, so a path nothing can
// write is a startup error naming the path rather than a page that is quietly
// empty.
func OpenLogs(cfg LogConfig) (*Logs, error) {
	cfg.File = cmp.Or(cfg.File, os.Getenv(LogFileEnv))
	cfg.FileMaxBytes = cmp.Or(cfg.FileMaxBytes, DefaultFileMaxBytes)
	if cfg.Level == nil {
		cfg.Level = slog.LevelDebug
	}

	if cfg.File == "" {
		return &Logs{cfg: cfg, unarmed: "no log file: set $" + LogFileEnv}, nil
	}

	f, err := openRotating(cfg.File, cfg.FileMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("observe: opening the log file: %w", err)
	}
	return &Logs{f: f, cfg: cfg}, nil
}

// Unarmed is why this sink will write nothing, or empty when it will.
//
// A reason rather than a boolean, because the answer is worth logging: a log
// file that was configured and is absent at run time should say which end is
// missing.
func (l *Logs) Unarmed() string {
	if l == nil {
		return "no log sink"
	}
	return l.unarmed
}

// File is the path being written, or empty for a sink that is unarmed. The
// monitoring page shows it, so that "no lines here" and "no file here" are
// distinguishable from the page itself.
func (l *Logs) File() string {
	if l == nil {
		return ""
	}
	return l.cfg.File
}

// Handler is this sink as a [log/slog.Handler]. Tee it beside the one the
// application already writes to — see [Tee].
//
// An unarmed sink returns a handler that is never enabled, so the logger it is
// teed into behaves exactly as it did without one.
func (l *Logs) Handler() slog.Handler {
	if l == nil || l.f == nil {
		return offHandler{}
	}
	return &logHandler{f: l.f, level: l.cfg.Level}
}

// Read is the last max lines of this sink's file, newest first. It is what the
// monitoring page calls, and it is [ReadLogs] over the path this sink is
// writing.
func (l *Logs) Read(max int) ([]LogRecord, error) {
	if l == nil {
		return nil, nil
	}
	return ReadLogs(l.cfg.File, max)
}

// Close closes the file. It is nil-safe and safe to call twice.
//
// Writes are unbuffered, so there is nothing to flush and nothing is lost by
// never calling this: it exists for a test that wants the file settled, and so
// that a long-running process can let one go. A main does not need a shutdown
// step for it, and should not have one — the last lines a server writes are
// written during its shutdown.
func (l *Logs) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.close()
}

// logHandler writes [LogRecord] lines. It is [Logs.Handler].
type logHandler struct {
	f     *rotatingFile
	level slog.Leveler
	// attrs is what WithAttrs accumulated, already nested under whatever groups
	// were open when each was added.
	attrs map[string]any
	// groups is the group path WithGroup opened, which is where this handler's
	// records put their own attributes.
	groups []string
}

var _ slog.Handler = (*logHandler)(nil)

// Enabled implements [log/slog.Handler].
func (h *logHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle implements [log/slog.Handler].
//
// The trace comes off the context, which is the only reason the context is
// worth having here — and the reason sloglint is configured to insist every
// call in rig passes one.
func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := LogRecord{Time: r.Time, Level: r.Level.String(), Msg: r.Message}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.TraceID = sc.TraceID().String()
		rec.SpanID = sc.SpanID().String()
	}

	// Copied rather than added to: a handler is shared by every logger derived
	// from it, and one that wrote a record's attributes into its own would leak
	// one line's fields onto the next.
	attrs := cloneAttrs(h.attrs)
	if r.NumAttrs() > 0 {
		if attrs == nil {
			attrs = make(map[string]any, r.NumAttrs())
		}
		dst := descend(attrs, h.groups)
		r.Attrs(func(a slog.Attr) bool {
			putAttr(dst, a)
			return true
		})
	}
	rec.Attrs = attrs

	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return h.f.write(append(line, '\n'))
}

// WithAttrs implements [log/slog.Handler].
func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}

	next := *h
	next.attrs = cloneAttrs(h.attrs)
	if next.attrs == nil {
		next.attrs = make(map[string]any, len(as))
	}
	dst := descend(next.attrs, h.groups)
	for _, a := range as {
		putAttr(dst, a)
	}
	return &next
}

// WithGroup implements [log/slog.Handler].
//
// The group is remembered and not created: a group nothing was ever added to is
// absent from the line rather than present and empty, which is the rule slog
// itself follows.
func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	next := *h
	next.groups = append(slices.Clip(h.groups), name)
	return &next
}

// descend walks — creating as it goes — to the map a group path names.
func descend(m map[string]any, groups []string) map[string]any {
	for _, g := range groups {
		child, ok := m[g].(map[string]any)
		if !ok {
			child = map[string]any{}
			m[g] = child
		}
		m = child
	}
	return m
}

// putAttr writes one attribute into dst, resolving what it holds.
//
// The three rules are slog's own: an attribute whose key and value are both
// zero is ignored, a group with an empty key is inlined, and a group with
// nothing in it is dropped.
func putAttr(dst map[string]any, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}

	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			return
		}
		if a.Key == "" {
			for _, g := range group {
				putAttr(dst, g)
			}
			return
		}

		child := descend(dst, []string{a.Key})
		for _, g := range group {
			putAttr(child, g)
		}
		return
	}

	if a.Key == "" {
		return
	}
	dst[a.Key] = attrValue(a.Value)
}

// attrValue is one value, as something JSON will render as a person would want
// to read it.
//
// The two special cases are the two that matter most on the page. An error
// marshals to {} — most errors have no exported field — and the wrapped cause
// of a 500 is the single most valuable thing on any line rig writes, so it
// becomes its message. A duration marshals to a count of nanoseconds, and
// "1.5s" is what somebody reading a log wants.
func attrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindAny:
		switch a := v.Any().(type) {
		case error:
			return a.Error()
		case json.Marshaler:
			return a
		case fmt.Stringer:
			return a.String()
		}
		return v.Any()
	default:
		return v.Any()
	}
}

// cloneAttrs is a deep copy of an accumulated attribute map, or nil for an
// empty one.
//
// Deep because a group is a map and a shallow copy would share it. Called once
// per line, over what WithAttrs accumulated, which for rig's own loggers is
// nothing at all.
func cloneAttrs(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}

	out := make(map[string]any, len(m))
	for k, v := range m {
		if child, ok := v.(map[string]any); ok {
			out[k] = cloneAttrs(child)
			continue
		}
		out[k] = v
	}
	return out
}

// Tee is a [log/slog.Handler] that writes every record to each of hs.
//
// It is how the log file is added to a logger that already writes somewhere:
//
//	slog.New(observe.Tee(slog.NewJSONHandler(os.Stderr, nil), logs.Handler()))
//
// Enabled is true when any of them is, and each is asked again before it is
// handed the record — so a file keeping debug lines does not turn stderr into
// debug output. That asymmetry is the reason this exists rather than a
// [log/slog.Logger] per destination.
func Tee(hs ...slog.Handler) slog.Handler {
	switch len(hs) {
	case 0:
		return offHandler{}
	case 1:
		return hs[0]
	default:
		return teeHandler{hs: slices.Clone(hs)}
	}
}

// teeHandler is [Tee].
type teeHandler struct {
	hs []slog.Handler
}

var _ slog.Handler = teeHandler{}

// Enabled implements [log/slog.Handler].
func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range t.hs {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle implements [log/slog.Handler].
//
// Every handler is given its own copy of the record, because a handler is
// allowed to add to the one it is handed and two sharing a record would be two
// handlers writing each other's fields.
func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range t.hs {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WithAttrs implements [log/slog.Handler].
func (t teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		out[i] = h.WithAttrs(as)
	}
	return teeHandler{hs: out}
}

// WithGroup implements [log/slog.Handler].
func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		out[i] = h.WithGroup(name)
	}
	return teeHandler{hs: out}
}

// offHandler is a handler that is never enabled, which is what an unarmed sink
// contributes to a [Tee]. Nothing rather than a nil to branch on at every call
// site.
type offHandler struct{}

var _ slog.Handler = offHandler{}

// Enabled implements [log/slog.Handler].
func (offHandler) Enabled(context.Context, slog.Level) bool { return false }

// Handle implements [log/slog.Handler].
func (offHandler) Handle(context.Context, slog.Record) error { return nil }

// WithAttrs implements [log/slog.Handler].
func (offHandler) WithAttrs([]slog.Attr) slog.Handler { return offHandler{} }

// WithGroup implements [log/slog.Handler].
func (offHandler) WithGroup(string) slog.Handler { return offHandler{} }
