package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/todo/client"
	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// The import job: a spreadsheet somebody exported, turned into rows in the API.
//
// This is the shape of program a client library actually gets used for, and it
// is worth its own demonstration because none of what makes it hard is about
// HTTP. A job that runs over somebody else's data has to decide four things, and
// a `for` loop around a create call decides none of them:
//
//   - What a bad row costs. One unparseable date should not stop the other four
//     thousand, and at the end somebody has to be told which line to fix.
//   - Which failures are worth trying again. A 429 or a 503 is the server asking
//     for a moment; a 422 will fail identically forever, and retrying it is just
//     a slower way to get the same answer.
//   - What happens when it is run twice. Somebody will run it twice.
//   - How hard to push. Unbounded goroutines against an API is a denial of
//     service you wrote yourself.
//
// The generated client is what makes each of those a few lines rather than a
// project: a validation failure arrives already shaped like the row that caused
// it, and a rate limit arrives with the interval the server asked for.

// importDemo reads a CSV and creates a todo per row.
func importDemo(ctx context.Context, args []string) error {
	baseURL := client.DefaultBaseURL
	set := flags("import", args, &baseURL)
	path := set.String("file", "testdata/todos.csv", "the CSV to import")
	tenant := set.String("tenant", uuid.NewString(), "the tenant to import into")
	workers := set.Int("workers", 4, "how many rows to import at once")
	attempts := set.Int("attempts", 3, "how many times to try a row the server asked us to retry")
	skipExisting := set.Bool("skip-existing", true, "leave a todo alone when its title is already there")
	dryRun := set.Bool("dry-run", false, "read and check the file, and write nothing")
	if err := set.Parse(args); err != nil {
		return err
	}

	file, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer file.Close()

	c, err := client.New(rigclient.Config{
		BaseURL:   baseURL,
		Header:    map[string][]string{"X-Tenant-Id": {*tenant}},
		UserAgent: "rig-sdk-import/1",
	})
	if err != nil {
		return err
	}
	fmt.Printf("importing %s into %s as tenant %s\n", *path, baseURL, *tenant)

	job := &importJob{
		client:       c,
		workers:      *workers,
		attempts:     *attempts,
		skipExisting: *skipExisting,
		dryRun:       *dryRun,
	}
	report := job.run(ctx, file)
	report.print(os.Stdout)

	// A job that half worked has to exit non-zero, or whatever runs it on a
	// schedule will report a clean night.
	if report.failed > 0 {
		return &importFailed{failed: report.failed, total: report.total()}
	}
	return nil
}

// importFailed is rows the job could not import, as opposed to a job that could
// not run. The difference matters at the top level, where an ordinary error is
// worth suggesting the server might not be running and this one is not: the
// report above already said exactly which lines were wrong.
type importFailed struct{ failed, total int }

func (e *importFailed) Error() string {
	return fmt.Sprintf("%d of %d rows did not import", e.failed, e.total)
}

// importJob is one run.
type importJob struct {
	client       *client.Client
	workers      int
	attempts     int
	skipExisting bool
	dryRun       bool
}

// row is one line of the file, already turned into something the API would take.
type row struct {
	// line is the line in the file, which is the only thing a person fixing the
	// data can act on. It is carried all the way to the report for that reason.
	line  int
	input client.TodoCreateInput
}

// outcome is what happened to one row.
type outcome struct {
	line   int
	title  string
	status status
	detail string
}

type status string

const (
	created status = "created"
	skipped status = "skipped"
	// parsed is a dry run: the file's own problems are found, and nothing is
	// sent. It does not mean the server would accept the row — the rules are the
	// server's, and a client that kept a second copy of them would be a second
	// copy to keep in step.
	parsed status = "parsed"
	failed status = "failed"
)

// run reads the file and imports what it can.
func (j *importJob) run(ctx context.Context, r io.Reader) *report {
	rows, bad := readCSV(r)

	rep := &report{outcomes: bad}
	if len(rows) == 0 {
		rep.finish()
		return rep
	}

	// A bounded pool rather than a goroutine per row. The bound is the whole
	// point: it is the one number that says how hard this job is willing to push
	// an API that other people are also using.
	workers := max(j.workers, 1)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		queue = make(chan row)
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for in := range queue {
				out := j.importRow(ctx, in)

				mu.Lock()
				rep.outcomes = append(rep.outcomes, out)
				mu.Unlock()
			}
		}()
	}

	for _, in := range rows {
		select {
		case queue <- in:
		case <-ctx.Done():
			// Stopped part-way. What was already imported stays imported, which
			// is why running it again has to be safe.
			close(queue)
			wg.Wait()
			rep.finish()
			return rep
		}
	}
	close(queue)
	wg.Wait()

	rep.finish()
	return rep
}

// importRow imports one, and never panics on somebody else's data.
func (j *importJob) importRow(ctx context.Context, in row) outcome {
	// A dry run reaches the server for nothing at all, not even to look. Which
	// means it works with no server: `-dry-run` is how somebody checks a file
	// that arrived by email before deciding whether to run the import.
	if j.dryRun {
		return outcome{line: in.line, title: in.input.Title, status: parsed}
	}

	if j.skipExisting {
		existing, err := j.exists(ctx, in.input.Title)
		if err != nil {
			return outcome{line: in.line, title: in.input.Title, status: failed,
				detail: "checking whether it is already there: " + err.Error()}
		}
		if existing {
			return outcome{line: in.line, title: in.input.Title, status: skipped,
				detail: "a todo with this title is already there"}
		}
	}

	err := j.retrying(ctx, func() error {
		_, err := j.client.Todos.Create(ctx, in.input,
			// A create the client had to send twice is still one todo, on a
			// server that honours the header. rig does not yet, so this is a
			// statement of intent — and the check above is what stands in for it
			// in the meantime.
			rigclient.WithIdempotencyKey(idempotencyKey(in)))
		return err
	})
	if err != nil {
		return outcome{line: in.line, title: in.input.Title, status: failed, detail: explainRow(err)}
	}
	return outcome{line: in.line, title: in.input.Title, status: created}
}

// exists reports whether a todo with this title is already there.
//
// It is a check-then-act, and a check-then-act races: two copies of this job
// started at once will both look, both see nothing, and both write. The only
// thing that really closes that is a unique index, which is a schema decision
// and belongs in the migration rather than here. What this buys is the ordinary
// case — somebody running the import a second time after fixing three rows.
func (j *importJob) exists(ctx context.Context, title string) (bool, error) {
	var found bool
	err := j.retrying(ctx, func() error {
		res, err := j.client.Todos.Search(ctx, client.TodoFilter{
			Equals: &client.TodoFilterEquals{Title: &title},
		}, client.TodoSearchQuery{Limit: rigclient.P(1)})
		if err != nil {
			return err
		}
		found = res != nil && len(res.Data) > 0
		return nil
	})
	return found, err
}

// retrying runs a call, trying again only where trying again could help.
//
// The distinction is the whole of it. A 429 and a 5xx are the server asking for
// a moment; a 422 is the row being wrong, and will be just as wrong in a second.
// Retrying the second kind turns a report somebody could act on into a job that
// takes three times as long to produce the same one.
func (j *importJob) retrying(ctx context.Context, call func() error) error {
	attempts := max(j.attempts, 1)

	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = call(); err == nil {
			return nil
		}
		if !worthRetrying(err) || attempt == attempts {
			return err
		}

		select {
		case <-time.After(backoff(err, attempt)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// worthRetrying says which failures are the server's rather than the row's.
func worthRetrying(err error) bool {
	var e *rigclient.Error
	if !errors.As(err, &e) {
		// Not a refusal at all: a connection reset, a timeout, a server that was
		// restarting. Those are the ones a second attempt is actually for.
		return true
	}
	return e.Status >= 500 || rigclient.IsRateLimited(err)
}

// backoff is how long to wait before trying again.
//
// The server's own answer wins when it gave one: Retry-After is not a hint, it
// is the interval after which the request will stop being refused, and guessing
// a shorter one just spends the next attempt on another 429.
func backoff(err error, attempt int) time.Duration {
	var e *rigclient.Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return time.Duration(attempt) * 200 * time.Millisecond
}

// idempotencyKey names this row's write, the same way on every attempt.
func idempotencyKey(in row) string {
	return fmt.Sprintf("import-%d-%s", in.line, in.input.Title)
}

// explainRow turns a failure into something a person fixing a spreadsheet can
// act on.
//
// This is where the generated client earns its place in a job like this: a 422
// arrives shaped like the input that caused it, so the report can say which
// column was wrong rather than quoting a sentence about the request.
func explainRow(err error) string {
	fields, ok := rigclient.FieldsAs[client.TodoCreateFields](err)
	if !ok {
		return err.Error()
	}

	// One entry per member of the input, because that is what the shape is: a
	// column nobody complained about is nil, and the rest name themselves.
	said := problems(map[string]*rigerr.FieldError{
		"title":    fields.Title,
		"notes":    fields.Notes,
		"priority": fields.Priority,
		"dueAt":    fields.DueAt,
		"isDone":   fields.IsDone,
		"":         fields.Entity, // wrong about the row rather than about a field
	})
	if len(said) == 0 {
		return err.Error()
	}
	return strings.Join(said, "; ")
}

// problems renders the members that have something wrong with them, sorted —
// a map is not ordered, and a report that reorders itself between runs is one
// nobody can diff against last night's.
func problems(fields map[string]*rigerr.FieldError) []string {
	var said []string
	for column, problem := range fields {
		switch {
		case problem == nil:
		case column == "":
			said = append(said, problem.Message)
		default:
			said = append(said, column+": "+problem.Message)
		}
	}
	slices.Sort(said)
	return said
}

// readCSV turns the file into rows, and turns anything it cannot into a failure
// naming the line.
//
// Nothing here talks to the server. A row that cannot be parsed is wrong however
// the API is feeling, and finding that out locally means a `-dry-run` can check
// a file before anybody's data is touched.
func readCSV(r io.Reader) ([]row, []outcome) {
	in := csv.NewReader(r)
	in.FieldsPerRecord = -1
	in.TrimLeadingSpace = true

	var (
		rows []row
		bad  []outcome
		cols map[string]int
	)
	for line := 1; ; line++ {
		record, err := in.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			bad = append(bad, outcome{line: line, status: failed, detail: err.Error()})
			continue
		}

		if cols == nil {
			cols = header(record)
			continue
		}

		parsed, err := parseRow(cols, record)
		if err != nil {
			bad = append(bad, outcome{line: line, title: field(cols, record, "title"),
				status: failed, detail: err.Error()})
			continue
		}
		rows = append(rows, row{line: line, input: parsed})
	}
	return rows, bad
}

// header maps a column name to its position, so the file may order them however
// whoever exported it felt like.
func header(record []string) map[string]int {
	cols := make(map[string]int, len(record))
	for i, name := range record {
		cols[strings.ToLower(strings.TrimSpace(name))] = i
	}
	return cols
}

func field(cols map[string]int, record []string, name string) string {
	i, ok := cols[name]
	if !ok || i >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[i])
}

// parseRow turns one record into what a create takes.
//
// The types are the generated ones, so a priority that is not a priority and a
// date that is not a date are caught here rather than by the server. That is not
// about saving a request: it is that the error can say "line 6" while the line
// is still in hand.
func parseRow(cols map[string]int, record []string) (client.TodoCreateInput, error) {
	in := client.TodoCreateInput{
		Title:    field(cols, record, "title"),
		Priority: client.TodoPriorityNormal,
	}

	if notes := field(cols, record, "notes"); notes != "" {
		in.Notes = &notes
	}

	if raw := field(cols, record, "priority"); raw != "" {
		priority := client.TodoPriority(strings.ToLower(raw))
		if !slices.Contains(client.AllTodoPriority, priority) {
			return in, fmt.Errorf("%q is not a priority; use one of %s",
				raw, join(client.AllTodoPriority))
		}
		in.Priority = priority
	}

	if raw := field(cols, record, "dueat"); raw != "" {
		due, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			return in, fmt.Errorf("%q is not a date; use YYYY-MM-DD", raw)
		}
		in.DueAt = &due
	}

	return in, nil
}

func join(values []client.TodoPriority) string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return strings.Join(out, ", ")
}

// report is what the job has to show for itself.
type report struct {
	outcomes []outcome

	created, skipped, parsed, failed int
}

func (r *report) total() int { return len(r.outcomes) }

// finish counts the outcomes and puts them back in the file's order.
//
// The workers finished in whatever order they finished, and a report that
// shuffles itself between runs is one nobody can diff against the last one.
func (r *report) finish() {
	slices.SortFunc(r.outcomes, func(a, b outcome) int { return a.line - b.line })

	for _, out := range r.outcomes {
		switch out.status {
		case created:
			r.created++
		case skipped:
			r.skipped++
		case parsed:
			r.parsed++
		case failed:
			r.failed++
		}
	}
}

func (r *report) print(w io.Writer) {
	for _, out := range r.outcomes {
		title := out.title
		if title == "" {
			title = "(no title)"
		}

		switch out.status {
		case failed:
			fmt.Fprintf(w, "  line %-3d %-8s %-32s %s\n", out.line, out.status, title, out.detail)
		case skipped:
			fmt.Fprintf(w, "  line %-3d %-8s %-32s %s\n", out.line, out.status, title, out.detail)
		default:
			fmt.Fprintf(w, "  line %-3d %-8s %s\n", out.line, out.status, title)
		}
	}

	fmt.Fprintf(w, "\n%d created, %d skipped, %d parsed, %d failed\n",
		r.created, r.skipped, r.parsed, r.failed)

	if r.parsed > 0 {
		fmt.Fprintln(w, "nothing was sent: a parsed row is one this file agrees with, "+
			"not one the server has accepted")
	}
}
