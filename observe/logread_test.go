package observe_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/simonjanss/rig/observe"
)

// write puts lines in a file, as a process that had been running would have
// left them.
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()

	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	line1 = `{"time":"2026-01-01T10:00:01Z","level":"INFO","msg":"one"}`
	line2 = `{"time":"2026-01-01T10:00:02Z","level":"WARN","msg":"two"}`
	line3 = `{"time":"2026-01-01T10:00:03Z","level":"ERROR","msg":"three"}`
)

// Newest first, which is the order anybody reading a log wants and the opposite
// of the span reader, whose records are grouped before anybody sees them.
func TestReadLogsIsNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeLines(t, path, line1, line2, line3)

	recs, err := observe.ReadLogs(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("read %d lines, want 3", len(recs))
	}
	if recs[0].Msg != "three" || recs[2].Msg != "one" {
		t.Errorf("read %q, %q, %q — want newest first", recs[0].Msg, recs[1].Msg, recs[2].Msg)
	}
}

// A file that has just rotated still answers with the lines before the
// rotation, rather than with the two since.
func TestReadLogsCountsTheRotatedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeLines(t, path+".1", line1)
	writeLines(t, path, line2, line3)

	recs, err := observe.ReadLogs(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("read %d lines, want all three across both generations", len(recs))
	}
	if recs[2].Msg != "one" {
		t.Errorf("the oldest line is %q, want the one from the rotated file", recs[2].Msg)
	}
}

// The limit is the last n lines and not the first, because a monitoring page
// asking for fifty wants the fifty that just happened.
func TestReadLogsKeepsTheLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeLines(t, path, line1, line2, line3)

	recs, err := observe.ReadLogs(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Msg != "three" || recs[1].Msg != "two" {
		t.Errorf("read %d lines, first %q", len(recs), recs[0].Msg)
	}
}

// The last line of a file a process was killed while writing is half a JSON
// object, and a page that refused to render because of it would fail in exactly
// the case somebody opened it for.
func TestATornLastLineIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeLines(t, path, line1, line2, `{"time":"2026-01-01T10:00:04Z","level":"INFO","ms`)

	recs, err := observe.ReadLogs(path, 10)
	if err != nil {
		t.Fatalf("a torn line was an error: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("read %d lines, want the two that parse", len(recs))
	}
}

// A server that has written no line, or one running with no RIG_LOG_FILE, is
// the ordinary case and not a failure.
func TestAMissingLogFileIsNoRecordsAndNoError(t *testing.T) {
	recs, err := observe.ReadLogs(filepath.Join(t.TempDir(), "nothing.jsonl"), 10)
	if err != nil {
		t.Fatalf("a missing file was an error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("read %d lines from a file that is not there", len(recs))
	}

	if recs, err := observe.ReadLogs("", 10); err != nil || len(recs) != 0 {
		t.Errorf("no path = %v, %v", recs, err)
	}
	if recs, err := observe.ReadLogs("anything", 0); err != nil || len(recs) != 0 {
		t.Errorf("no limit = %v, %v", recs, err)
	}
}

// Nothing in the format is rig's beyond the field names, so a file written by a
// project's own JSON handler reads too — including the trace id, which such a
// handler will have put in a group rather than at the top level.
func TestAForeignLogFileReadsToo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.jsonl")
	writeLines(t, path,
		`{"time":"2026-01-01T10:00:01Z","level":"INFO","msg":"served","request":{"trace_id":"abc123","route":"GET /x"}}`,
		`{"time":"2026-01-01T10:00:02Z","level":"INFO","msg":"flat","trace_id":"def456"}`)

	recs, err := observe.ReadLogs(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("read %d lines, want 2", len(recs))
	}
	if recs[0].TraceID != "def456" {
		t.Errorf("a top-level trace_id read as %q", recs[0].TraceID)
	}
	if recs[1].TraceID != "abc123" {
		t.Errorf("a grouped trace_id read as %q", recs[1].TraceID)
	}
	if recs[1].Attrs["request"] == nil {
		t.Error("the rest of a foreign line did not land in attrs")
	}
}
