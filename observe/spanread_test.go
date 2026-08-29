package observe_test

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/simonjanss/rig/observe"
)

// write is a span file with exactly these records in it, which is what the
// grouping tests need: going through the exporter would give them a real
// tracer's ids and no way to say what should end up beside what.
func write(t *testing.T, path string, recs ...observe.SpanRecord) {
	t.Helper()

	var out []byte
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, line...), '\n')
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// at is a record, positioned in time and in its trace.
func at(trace, span, parent, name string, start time.Time, d time.Duration) observe.SpanRecord {
	return observe.SpanRecord{
		Time: start.Add(d), TraceID: trace, SpanID: span, ParentID: parent,
		Name: name, Kind: "internal", Start: start,
		DurationMS: float64(d) / float64(time.Millisecond),
	}
}

var noon = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

// A file that has just rotated still answers with the requests before the
// rotation. Reading only the current file would mean that the moment the
// history is longest is the moment the page is emptiest.
func TestReadSpansReadsTheRotatedGenerationFirst(t *testing.T) {
	path := spanFile(t)
	write(t, path+".1", at("a", "1", "", "old", noon, time.Millisecond))
	write(t, path, at("b", "2", "", "new", noon.Add(time.Second), time.Millisecond))

	recs, err := observe.ReadSpans(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("want both generations, got %d records", len(recs))
	}
	if recs[0].Name != "old" || recs[1].Name != "new" {
		t.Errorf("want the rotated generation first, got %q then %q", recs[0].Name, recs[1].Name)
	}
}

// max is a ceiling on the newest end, because the newest end is the one
// somebody opened the page for.
func TestReadSpansKeepsTheNewestRecords(t *testing.T) {
	path := spanFile(t)

	recs := make([]observe.SpanRecord, 0, 50)
	for i := range 50 {
		recs = append(recs, at("t", strconv.Itoa(i), "", "span "+strconv.Itoa(i), noon, time.Millisecond))
	}
	write(t, path, recs...)

	got, err := observe.ReadSpans(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5 records, got %d", len(got))
	}
	if got[0].Name != "span 45" || got[4].Name != "span 49" {
		t.Errorf("want the last five, got %q…%q", got[0].Name, got[4].Name)
	}
}

// A process killed mid-write leaves half a JSON object on the end of the file.
// A page that refused to render because of it would fail in exactly the case
// somebody opened it for.
func TestReadSpansSkipsAPartialLine(t *testing.T) {
	path := spanFile(t)
	write(t, path, at("a", "1", "", "complete", noon, time.Millisecond))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"trace_id":"b","name":"cut o`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	recs, err := observe.ReadSpans(path, 10)
	if err != nil {
		t.Fatalf("a truncated last line should be skipped, not fatal: %v", err)
	}
	if len(recs) != 1 || recs[0].Name != "complete" {
		t.Errorf("want only the complete record, got %v", recs)
	}
}

// A server that has never written a span is the ordinary case, not a failure.
func TestReadSpansOnAMissingFile(t *testing.T) {
	recs, err := observe.ReadSpans(spanFile(t), 10)
	if err != nil {
		t.Fatalf("a missing span file should not be an error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("want no records, got %d", len(recs))
	}
}

// The page lists requests, so the grouping has to survive a file that
// interleaves them — which every concurrent server's does.
func TestGroupTracesSeparatesInterleavedRequests(t *testing.T) {
	recs := []observe.SpanRecord{
		at("aaa", "1", "", "GET /api/v1/todos", noon, 30*time.Millisecond),
		at("bbb", "3", "", "POST /api/v1/todos", noon.Add(10*time.Millisecond), 50*time.Millisecond),
		at("aaa", "2", "1", "repository.Todo.List", noon.Add(5*time.Millisecond), 20*time.Millisecond),
		at("bbb", "4", "3", "repository.Todo.Create", noon.Add(20*time.Millisecond), 30*time.Millisecond),
	}

	traces := observe.GroupTraces(recs)
	if len(traces) != 2 {
		t.Fatalf("want two traces, got %d", len(traces))
	}

	// Newest first: what just happened is what somebody is looking for.
	if traces[0].ID != "bbb" {
		t.Errorf("want the newer trace first, got %q", traces[0].ID)
	}
	for _, tr := range traces {
		if len(tr.Spans) != 2 {
			t.Errorf("trace %s has %d spans, want 2", tr.ID, len(tr.Spans))
		}
		if tr.Root == nil {
			t.Fatalf("trace %s has no root", tr.ID)
		}
		if tr.Root.Name != tr.Name {
			t.Errorf("trace %s is named %q and its root is %q", tr.ID, tr.Name, tr.Root.Name)
		}
		// Start order, which is the order they nest in.
		if tr.Spans[0].ParentID != "" {
			t.Errorf("trace %s does not start with its root", tr.ID)
		}
	}
}

// A failure anywhere in a request makes the request a failure, which is what
// the page's "errors only" filter tests.
func TestGroupTracesCarriesAFailureUpward(t *testing.T) {
	root := at("aaa", "1", "", "POST /api/v1/todos", noon, 30*time.Millisecond)
	stage := at("aaa", "2", "1", "repository.Todo.Create.Validator", noon, 2*time.Millisecond)
	stage.Status, stage.Error = "error", "Invalid: a todo needs a title"

	traces := observe.GroupTraces([]observe.SpanRecord{root, stage})
	if len(traces) != 1 {
		t.Fatalf("want one trace, got %d", len(traces))
	}
	if traces[0].Status != "error" {
		t.Errorf("a trace with a failed stage is %q, want error", traces[0].Status)
	}
}

// The whole of a trace, not the root's own duration: a trace whose beginning
// was rotated away would otherwise be zero milliseconds long.
func TestGroupTracesSpansTheWholeRequestWithoutARoot(t *testing.T) {
	traces := observe.GroupTraces([]observe.SpanRecord{
		at("aaa", "2", "1", "repository.Todo.List", noon, 20*time.Millisecond),
		at("aaa", "3", "2", "SELECT todo", noon.Add(5*time.Millisecond), 30*time.Millisecond),
	})

	if len(traces) != 1 {
		t.Fatalf("want one trace, got %d", len(traces))
	}
	if traces[0].Root != nil {
		t.Errorf("want no root, got %q", traces[0].Root.Name)
	}
	if got := traces[0].DurationMS; got != 35 {
		t.Errorf("duration = %vms, want 35 — the earliest start to the latest end", got)
	}
}

// ReadTraces counts traces, not records, which is what a page listing requests
// means by a limit.
func TestReadTracesLimitsByTrace(t *testing.T) {
	path := spanFile(t)

	var recs []observe.SpanRecord
	for i := range 10 {
		id := strconv.Itoa(i)
		start := noon.Add(time.Duration(i) * time.Second)
		recs = append(recs,
			at(id, id+"a", "", "GET /api/v1/todos", start, 10*time.Millisecond),
			at(id, id+"b", id+"a", "repository.Todo.List", start, 5*time.Millisecond))
	}
	write(t, path, recs...)

	traces, err := observe.ReadTraces(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 3 {
		t.Fatalf("want 3 traces, got %d", len(traces))
	}
	if traces[0].ID != "9" {
		t.Errorf("want the newest three, got %q first", traces[0].ID)
	}
	if len(traces[0].Spans) != 2 {
		t.Errorf("a limited trace lost spans: %d, want 2", len(traces[0].Spans))
	}
}
