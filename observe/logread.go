package observe

import (
	"slices"
	"strings"
)

// ReadLogs reads the last max lines of a log file, newest first.
//
// Newest first because that is the order anybody looking at a log wants — the
// opposite of [ReadSpans], which is oldest first because its records are
// grouped into traces before anybody sees them.
//
// It is [Logs.Read] without a sink, for a script over a file this process is not
// the one writing. The rotated generation beside it counts, a line that does not
// parse is skipped, and a missing file is no records and no error: the same
// three properties [ReadSpans] has, and for the same reasons.
//
// Nothing here is specific to the file rig writes beyond the field names, so a
// log a project's own [log/slog.JSONHandler] produced reads as well — time,
// level and msg are slog's own keys, and anything else lands in
// [LogRecord.Attrs].
func ReadLogs(path string, max int) ([]LogRecord, error) {
	out, err := decodeLines[LogRecord](path, max)
	if err != nil {
		return nil, err
	}

	// A foreign file's handler may have used slog's own group nesting for the
	// trace, or none at all. Either way the page wants one field.
	for i := range out {
		if out[i].TraceID == "" {
			out[i].TraceID = attrString(out[i].Attrs, "trace_id")
		}
	}

	slices.Reverse(out)
	return out, nil
}

// attrString is one string-valued attribute, looked up at the top level and
// then one group down.
//
// One level and not a walk: this exists so a foreign log file's trace id is
// found whether it was written flat or inside the group a request line puts its
// fields in, and searching arbitrarily deep for a key would start matching
// things that are not it.
func attrString(attrs map[string]any, key string) string {
	if s, ok := attrs[key].(string); ok {
		return s
	}
	for _, v := range attrs {
		group, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := group[key].(string); ok {
			return s
		}
	}
	return ""
}

// logLevels is the order [log/slog]'s own level names sort in, so that a filter
// on the monitoring page can mean "this and above" rather than "exactly this".
//
// By name and not by [log/slog.Level], because what is in the file is a string —
// and one written by a handler that is not rig's may hold a name rig never
// emits, which belongs at the end rather than in the middle.
var logLevels = []string{"DEBUG", "INFO", "WARN", "ERROR"}

// atLeast reports whether a level name is min or louder.
//
// A level neither of them names is kept: a filter is for narrowing a list
// somebody is reading, and silently dropping the one line whose level rig does
// not recognise is the failure that would be hardest to notice.
func atLeast(level, min string) bool {
	li := slices.Index(logLevels, strings.ToUpper(level))
	mi := slices.Index(logLevels, strings.ToUpper(min))
	if li < 0 || mi < 0 {
		return true
	}
	return li >= mi
}
