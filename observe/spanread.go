package observe

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"time"
)

// spansPerTrace is how many records are read for each trace asked for.
//
// A request under rig opens a span for itself, one per repository call, one per
// stage of a write and one per statement, so a handful of writes is already a
// dozen. Thirty-two is generous for that and still bounded, and a trace that
// overflows it is shown with the spans that fit rather than dropped — the same
// answer as a trace whose start was rotated away.
const spansPerTrace = 32

// maxRecords is the ceiling on one read, whatever was asked for. Fifty thousand
// records is more than any page shows and keeps a misconfigured MaxTraces from
// turning into the whole file.
const maxRecords = 50_000

// ReadTraces reads the newest traces from a span file, newest first.
//
// This is [ReadSpans] and [GroupTraces] together, and it is what the monitoring
// page calls. A missing file is not an error: a server that has not written a
// span yet, or one running with no RIG_TRACE_FILE, is the ordinary case and
// answers as no traces rather than as a failure.
func ReadTraces(path string, maxTraces int) ([]TraceRecord, error) {
	if maxTraces <= 0 {
		return nil, nil
	}

	recs, err := ReadSpans(path, maxTraces*spansPerTrace)
	if err != nil {
		return nil, err
	}

	traces := GroupTraces(recs)
	if len(traces) > maxTraces {
		traces = traces[:maxTraces]
	}
	return traces, nil
}

// ReadSpans reads the last max records of a span file, oldest first, counting
// the rotated generation beside it — see [tailLines] for what bounds that.
//
// A line that does not parse is skipped. The last line of a file a process was
// killed while writing is a partial JSON object, and a page that refused to
// render because of it would fail in exactly the case somebody opened it for.
//
// A missing file returns no records and no error.
func ReadSpans(path string, max int) ([]SpanRecord, error) {
	lines, err := tailLines(path, max)
	if err != nil {
		return nil, err
	}

	out := make([]SpanRecord, 0, len(lines))
	for _, line := range lines {
		var rec SpanRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// tailLines is the last max lines of a JSONL file rig wrote, oldest first,
// counting the rotated generation beside it.
//
// The rotated file is read first, so a file that has just rotated still answers
// with the records before the rotation rather than with the three since. Both
// are read whole and the last max lines kept, which is what bounds this: a file
// rig writes is capped with one generation, so the read is bounded by twice
// that cap — eight mebibytes each, by default. There is no index, and building
// one would be a tracing backend, which is the thing rig decided not to be.
//
// A missing file contributes no lines and no error.
func tailLines(path string, max int) ([][]byte, error) {
	if path == "" || max <= 0 {
		return nil, nil
	}
	max = min(max, maxRecords)

	// A ring of raw lines rather than of decoded records: the whole file is
	// scanned, and decoding every line to throw all but the last few hundred
	// away is the one part of this that would actually cost something.
	ring := make([][]byte, 0, max)
	for _, p := range []string{path + rotatedSuffix, path} {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		ring = keepLast(ring, data, max)
	}
	return ring, nil
}

// keepLast appends data's non-empty lines to ring, dropping from the front so
// that at most max are kept.
//
// Trimmed once at the end rather than per line. Dropping the front of a slice
// costs the whole slice, and a full span file is tens of thousands of lines
// past the limit — which would make this a copy of the ring per line, and cost
// more than the decoding it exists to avoid.
func keepLast(ring [][]byte, data []byte, max int) [][]byte {
	for line := range bytes.Lines(data) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		ring = append(ring, line)
	}
	if len(ring) > max {
		ring = ring[len(ring)-max:]
	}
	return ring
}

// Trace is one request: every span that shared a trace id, and what the whole
// of it amounts to.
//
// It is what the monitoring page lists and what a script reading the span file
// most likely wants, which is why the grouping is here rather than in the page.
type TraceRecord struct {
	// ID is the trace id every span in it carries — the same string the
	// generated server puts in an error body as requestId, so an identifier
	// from somebody's screenshot is a search on this field.
	ID string `json:"id"`

	// Root is the span with no parent, usually the request. It is nil for a
	// trace whose beginning has been rotated away, or one still in flight when
	// the file was read; the rest of the trace is still worth showing, which is
	// why this is a pointer and not an assumption.
	Root *SpanRecord `json:"root,omitempty"`

	// Spans is every record in the trace, in start order. Nesting is not a
	// field: each record carries [SpanRecord.ParentID] and rebuilding the tree
	// from that is the reader's business, which for rig is the page.
	Spans []SpanRecord `json:"spans"`

	// Start is the earliest start in the trace and End the latest end, so the
	// duration covers the whole of it even when the root is missing.
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// DurationMS is End minus Start, in milliseconds. Not the root's own
	// duration: a trace missing its root would otherwise be zero.
	DurationMS float64 `json:"duration_ms"`

	// Status is "error" when any span in the trace failed, and absent
	// otherwise — the same convention [SpanRecord.Status] uses, so the page
	// filters both with one test.
	Status string `json:"status,omitempty"`
	// Name is the root's name, which for a request is its route. Empty when
	// there is no root.
	Name string `json:"name,omitempty"`
	// Service is who wrote the trace, from the first span that says.
	Service string `json:"service,omitempty"`
}

// GroupTraces groups records by trace id, newest first.
//
// Newest first because that is the order anybody looking at a monitoring page
// wants: what just happened is at the top. Within a trace the spans are in
// start order, which is the order they nest in.
func GroupTraces(recs []SpanRecord) []TraceRecord {
	byID := make(map[string][]SpanRecord)
	order := make([]string, 0, len(recs))
	for _, rec := range recs {
		if _, seen := byID[rec.TraceID]; !seen {
			order = append(order, rec.TraceID)
		}
		byID[rec.TraceID] = append(byID[rec.TraceID], rec)
	}

	traces := make([]TraceRecord, 0, len(order))
	for _, id := range order {
		traces = append(traces, newTrace(id, byID[id]))
	}

	// Sorted rather than reversed: the file is in end order, and a slow request
	// that started first can be written last. Stable so that two traces
	// finishing in the same millisecond keep the order the file had.
	slices.SortStableFunc(traces, func(a, b TraceRecord) int { return b.Start.Compare(a.Start) })
	return traces
}

// newTrace is one trace id's records, summarised.
func newTrace(id string, spans []SpanRecord) TraceRecord {
	slices.SortStableFunc(spans, func(a, b SpanRecord) int { return a.Start.Compare(b.Start) })

	t := TraceRecord{ID: id, Spans: spans, Start: spans[0].Start}
	for i := range spans {
		span := &spans[i]

		if span.ParentID == "" && t.Root == nil {
			t.Root = span
			t.Name = span.Name
		}
		if span.Time.After(t.End) {
			t.End = span.Time
		}
		if span.Status == "error" {
			t.Status = "error"
		}
		if t.Service == "" {
			t.Service = span.Service
		}
	}

	t.DurationMS = float64(t.End.Sub(t.Start)) / float64(time.Millisecond)
	return t
}
