package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The import job, against a server that is only a mux.
//
// What is worth testing here is not the HTTP — demo_test.go covers what the
// client puts on the wire — but the four decisions a batch job makes: what a bad
// row costs, which failures are worth retrying, what a second run does, and how
// hard it pushes. Each of those is a property of this file and of nothing else.

const importable = `title,notes,priority,dueAt
Write the tutorial,start from an empty directory,high,2026-09-01
Fix the flaky test,,normal,
Review the SDK pull request,two pairs of eyes,high,2026-08-20
,this row forgot its title,high,
Buy milk,,urgent,
`

// createdTodo is the answer a create gets, which the job does not read but the
// client decodes.
const createdTodo = `{"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","title":"x"}`

const emptyPage = `{"data":[],"pagination":{"offset":0,"limit":1,"total":0}}`

// A row that cannot be parsed never becomes a request, and never stops the rows
// after it. Which line was wrong is the only thing a person fixing a spreadsheet
// can act on, so it is what the report says.
func TestABadRowIsCaughtBeforeItIsSent(t *testing.T) {
	c, rec := newRecorded(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/_search") || r.Method == "QUERY" {
			w.Write([]byte(emptyPage))
			return
		}
		w.Write([]byte(createdTodo))
	})

	job := &importJob{client: c, workers: 2, attempts: 1}
	rep := job.run(t.Context(), strings.NewReader(importable))

	// Line 6 is `Buy milk,,urgent,` — urgent is not one of this schema's
	// priorities, and the generated enum is what says so.
	var caught *outcome
	for i := range rep.outcomes {
		if rep.outcomes[i].line == 6 {
			caught = &rep.outcomes[i]
		}
	}
	if caught == nil || caught.status != failed {
		t.Fatalf("line 6 = %+v, want a failure", caught)
	}
	if !strings.Contains(caught.detail, "not a priority") {
		t.Errorf("detail = %q, want it to name the problem", caught.detail)
	}

	// Nothing was sent for it: four rows parsed, and one of those is refused by
	// the server rather than here.
	var creates int
	for _, req := range rec.requests {
		if req.method == http.MethodPost && !strings.HasSuffix(req.path, "/_search") {
			creates++
		}
	}
	if creates != 4 {
		t.Errorf("sent %d creates, want one per parseable row", creates)
	}
}

// A row the server refuses is reported with the column that was wrong, which is
// the generated 422 shape doing the work.
func TestARefusedRowNamesItsColumn(t *testing.T) {
	c, _ := newRecorded(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.HasSuffix(r.URL.Path, "/_search") || r.Method == "QUERY" {
			w.Write([]byte(emptyPage))
			return
		}

		var sent map[string]any
		json.Unmarshal(body, &sent)
		if title, _ := sent["title"].(string); strings.TrimSpace(title) == "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{
				"code":    "UnprocessableEntity",
				"message": "the request is not valid",
				"fields": map[string]any{
					"title": map[string]string{"code": "CannotBeEmpty", "message": "cannot be empty"},
				},
			})
			return
		}
		w.Write([]byte(createdTodo))
	})

	job := &importJob{client: c, workers: 2, attempts: 3}
	rep := job.run(t.Context(), strings.NewReader(importable))

	if rep.created != 3 {
		t.Errorf("created %d, want the three good rows", rep.created)
	}
	if rep.failed != 2 {
		t.Errorf("failed %d, want the empty title and the bad priority", rep.failed)
	}

	var refused *outcome
	for i := range rep.outcomes {
		if rep.outcomes[i].line == 5 {
			refused = &rep.outcomes[i]
		}
	}
	if refused == nil || !strings.Contains(refused.detail, "title: cannot be empty") {
		t.Fatalf("line 5 = %+v, want the column named", refused)
	}
}

// A 429 is the server asking for a moment, and it says how long. A 422 is the
// row being wrong, and will be just as wrong in a second.
func TestOnlyTheRetryableFailuresAreRetried(t *testing.T) {
	var (
		attempts atomic.Int32
		refusals atomic.Int32
	)
	c, _ := newRecorded(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.HasSuffix(r.URL.Path, "/_search") || r.Method == "QUERY" {
			w.Write([]byte(emptyPage))
			return
		}

		var sent map[string]any
		json.Unmarshal(body, &sent)
		title, _ := sent["title"].(string)

		switch {
		case title == "Fix the flaky test":
			// Refused twice, then accepted: the shape of a real rate limit.
			if attempts.Add(1) <= 2 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"code":"RateLimited","message":"slow down"}`))
				return
			}
			w.Write([]byte(createdTodo))

		case strings.TrimSpace(title) == "":
			refusals.Add(1)
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"code":"UnprocessableEntity","message":"the request is not valid"}`))

		default:
			w.Write([]byte(createdTodo))
		}
	})

	job := &importJob{client: c, workers: 1, attempts: 4}
	rep := job.run(t.Context(), strings.NewReader(importable))

	if got := attempts.Load(); got != 3 {
		t.Errorf("the rate-limited row was tried %d times, want 3", got)
	}
	if got := refusals.Load(); got != 1 {
		t.Errorf("the invalid row was sent %d times, want once — a 422 will not change", got)
	}
	if rep.created != 3 {
		t.Errorf("created %d, want the rate-limited row to have made it in", rep.created)
	}
}

// Somebody will run it twice. The second run should be quiet rather than
// twice as full.
func TestASecondRunSkipsWhatIsAlreadyThere(t *testing.T) {
	c, _ := newRecorded(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/_search") || r.Method == "QUERY" {
			// Everything the file holds is already there.
			w.Write([]byte(`{"data":[{"id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8",` +
				`"title":"Write the tutorial"}],"pagination":{"offset":0,"limit":1,"total":1}}`))
			return
		}
		t.Errorf("a second run wrote %s %s", r.Method, r.URL.Path)
		w.Write([]byte(createdTodo))
	})

	job := &importJob{client: c, workers: 2, attempts: 1, skipExisting: true}
	rep := job.run(t.Context(), strings.NewReader(importable))

	if rep.created != 0 {
		t.Errorf("created %d on a second run, want none", rep.created)
	}
	if rep.skipped != 4 {
		t.Errorf("skipped %d, want every parseable row", rep.skipped)
	}
}

// A dry run reads the file, says what is wrong with it, and touches nothing —
// not even to look, which is what makes it usable before there is a server to
// look at.
func TestADryRunSendsNothingAtAll(t *testing.T) {
	c, rec := newRecorded(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("a dry run sent %s %s", r.Method, r.URL.Path)
		w.Write([]byte(createdTodo))
	})

	job := &importJob{client: c, workers: 2, attempts: 1, dryRun: true, skipExisting: true}
	rep := job.run(t.Context(), strings.NewReader(importable))

	if len(rec.requests) != 0 {
		t.Errorf("a dry run made %d requests", len(rec.requests))
	}
	if rep.parsed != 4 || rep.failed != 1 {
		t.Errorf("checked %d and failed %d, want 4 and 1", rep.parsed, rep.failed)
	}
}

// Unbounded goroutines against somebody else's API is a denial of service you
// wrote yourself. The bound is the one number that says how hard this pushes.
func TestTheWorkerPoolIsBounded(t *testing.T) {
	var (
		mu      sync.Mutex
		now     int
		highest int
	)
	c, _ := newRecorded(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		mu.Lock()
		now++
		highest = max(highest, now)
		mu.Unlock()

		// Long enough that a job with no bound would pile up here.
		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		now--
		mu.Unlock()

		if strings.HasSuffix(r.URL.Path, "/_search") || r.Method == "QUERY" {
			w.Write([]byte(emptyPage))
			return
		}
		w.Write([]byte(createdTodo))
	})

	job := &importJob{client: c, workers: 2, attempts: 1}
	job.run(t.Context(), strings.NewReader(importable))

	mu.Lock()
	defer mu.Unlock()
	if highest > 2 {
		t.Errorf("%d requests were in flight at once, want at most the 2 workers", highest)
	}
}

// The workers finish in whatever order they finish. A report that shuffles
// itself between runs is one nobody can diff against last night's.
func TestTheReportIsInTheFilesOrder(t *testing.T) {
	c, _ := newRecorded(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/_search") || r.Method == "QUERY" {
			w.Write([]byte(emptyPage))
			return
		}
		w.Write([]byte(createdTodo))
	})

	job := &importJob{client: c, workers: 4, attempts: 1}
	rep := job.run(t.Context(), strings.NewReader(importable))

	if len(rep.outcomes) != 5 {
		t.Fatalf("%d outcomes, want one per row", len(rep.outcomes))
	}
	for i, out := range rep.outcomes {
		if want := i + 2; out.line != want {
			t.Errorf("outcome %d is line %d, want %d", i, out.line, want)
		}
	}
}
