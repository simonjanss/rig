//go:build docker

// The API, end to end, against the database the example actually runs on.
//
//	rig db up && go test -tags docker ./...
//
// Every layer this exercises was generated: the routing, the decoding, the
// filter translation, the SQL. What the test pins down is the behavior nobody
// wrote down anywhere — that a tenant sees only its own rows, that an absent
// field and an explicit null are different requests, and that a soft delete
// retires a row rather than removing it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/model"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/examples/todo/services/todo"
	todo_attachment "github.com/simonjanss/rig/examples/todo/services/todo_attachment"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

const dsnFallback = "postgres://rig:rig@localhost:55440/rig?sslmode=disable"

// The wire shapes, written out by hand rather than reused from the api package.
//
// Decoding into the generated types would prove only that they round-trip
// through themselves. These say what a client actually receives — the camelCase
// keys, and notes absent rather than null when it is unset.
type todoJSON struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenantId"`
	Title     string    `json:"title"`
	Notes     *string   `json:"notes"`
	IsDone    bool      `json:"isDone"`
	Priority  string    `json:"priority"`
	DueAt     *string   `json:"dueAt"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt *string   `json:"updatedAt"`

	// The snapshot triple. A live row carries Original and nothing else; a
	// copy carries which row it came from and which version of it.
	VersionType        string     `json:"versionType"`
	SnapshotFromTodoID *uuid.UUID `json:"snapshotFromTodoId"`
	SnapshotFromTodoAt *time.Time `json:"snapshotFromTodoAt"`
}

type listJSON struct {
	Data       []todoJSON `json:"data"`
	Pagination struct {
		Offset int   `json:"offset"`
		Limit  int   `json:"limit"`
		Total  int64 `json:"total"`
	} `json:"pagination"`
}

func TestAPI(t *testing.T) {
	srv, tenant := newServer(t)

	var created todoJSON
	t.Run("create", func(t *testing.T) {
		res := do(t, srv, tenant, "POST", "/api/v1/todos",
			`{"title":"Write the tutorial","priority":"high","notes":"start from an empty directory"}`)
		requireStatus(t, res, http.StatusCreated)
		decode(t, res, &created)

		if created.ID == uuid.Nil {
			t.Error("the row should have been given an identifier")
		}
		if created.TenantID != tenant {
			t.Errorf("tenant = %s, want %s", created.TenantID, tenant)
		}
		// Nothing in the request said so; the repository stamps it.
		if created.CreatedAt.IsZero() {
			t.Error("createdAt should have been stamped")
		}
	})

	t.Run("validation answers with the field that failed", func(t *testing.T) {
		// The database would have accepted this: the column is NOT NULL, and
		// three spaces are not null.
		res := do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"   "}`)
		requireStatus(t, res, http.StatusUnprocessableEntity)

		// The body is shaped like the request that failed, so a client puts the
		// message beside the input rather than parsing a sentence for a name.
		var failure failureJSON
		decode(t, res, &failure)

		if failure.Code != "UnprocessableEntity" {
			t.Errorf("code = %q", failure.Code)
		}
		if failure.Fields.Title == nil {
			t.Fatalf("the failure should name the title: %+v", failure)
		}
		// The code is what a client switches on, the message what it shows.
		if failure.Fields.Title.Code != "CannotBeEmpty" {
			t.Errorf("code = %q, want CannotBeEmpty", failure.Fields.Title.Code)
		}
		if failure.Fields.Title.Message == "" {
			t.Error("the field error should say what was wrong")
		}
		if failure.Fields.Notes != nil {
			t.Error("a field that was fine should be absent, not null-with-an-error")
		}
	})

	t.Run("a service rule names the field it is about", func(t *testing.T) {
		res := do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Untitled"}`)
		requireStatus(t, res, http.StatusUnprocessableEntity)

		var failure failureJSON
		decode(t, res, &failure)

		if failure.Fields.Title == nil {
			t.Fatalf("the validator's message should be under the title: %+v", failure)
		}
		// The hook chose the code; where it landed is decided by which hook
		// returned it, so no name travels with the error itself.
		if failure.Fields.Title.Code != "NotAllowed" {
			t.Errorf("code = %q, want NotAllowed", failure.Fields.Title.Code)
		}
	})

	// A rule that has to ask the database. It is also the one that shows why a
	// context knows which fields changed: on an update that leaves the title
	// alone, the row it would find is the one being changed.
	t.Run("a duplicate title is refused", func(t *testing.T) {
		const title = `{"title":"Write the tutorial"}`

		res := do(t, srv, tenant, "POST", "/api/v1/todos", title)
		requireStatus(t, res, http.StatusUnprocessableEntity)

		var failure failureJSON
		decode(t, res, &failure)

		if failure.Fields.Title == nil {
			t.Fatalf("the duplicate should be reported under title: %+v", failure)
		}
		if failure.Fields.Title.Code != "AlreadyExists" {
			t.Errorf("code = %q, want AlreadyExists", failure.Fields.Title.Code)
		}

		// Another tenant has its own titles.
		other := do(t, srv, uuid.New(), "POST", "/api/v1/todos", title)
		requireStatus(t, other, http.StatusCreated)
	})

	t.Run("changing something else does not conflict with itself", func(t *testing.T) {
		res := do(t, srv, tenant, "PATCH", "/api/v1/todos/"+created.ID.String(),
			`{"isDone":false}`)
		requireStatus(t, res, http.StatusOK)

		// And re-sending the same title it already has is not a conflict
		// either: the row that matches is this one.
		same := do(t, srv, tenant, "PATCH", "/api/v1/todos/"+created.ID.String(),
			`{"title":"Write the tutorial"}`)
		requireStatus(t, same, http.StatusOK)
	})

	t.Run("an unknown field is rejected", func(t *testing.T) {
		res := do(t, srv, tenant, "POST", "/api/v1/todos", `{"titel":"typo"}`)
		requireStatus(t, res, http.StatusBadRequest)
	})

	t.Run("get", func(t *testing.T) {
		res := do(t, srv, tenant, "GET", "/api/v1/todos/"+created.ID.String(), "")
		requireStatus(t, res, http.StatusOK)

		var got todoJSON
		decode(t, res, &got)
		if got.Title != "Write the tutorial" {
			t.Errorf("title = %q", got.Title)
		}
	})

	t.Run("an absent field is left alone", func(t *testing.T) {
		res := do(t, srv, tenant, "PATCH", "/api/v1/todos/"+created.ID.String(),
			`{"title":"Write the tutorial today"}`)
		requireStatus(t, res, http.StatusOK)

		var got todoJSON
		decode(t, res, &got)
		if got.Title != "Write the tutorial today" {
			t.Errorf("title = %q, want the updated one", got.Title)
		}
		if got.Notes == nil || *got.Notes != "start from an empty directory" {
			t.Errorf("notes = %v, want the original: an omitted field means leave it alone", got.Notes)
		}
	})

	t.Run("an explicit null clears the field", func(t *testing.T) {
		res := do(t, srv, tenant, "PATCH", "/api/v1/todos/"+created.ID.String(), `{"notes":null}`)
		requireStatus(t, res, http.StatusOK)

		var got todoJSON
		decode(t, res, &got)
		if got.Notes != nil {
			t.Errorf("notes = %v, want nil: an explicit null means clear it", *got.Notes)
		}
		if got.Title != "Write the tutorial today" {
			t.Errorf("title = %q, want it untouched", got.Title)
		}
	})

	t.Run("list", func(t *testing.T) {
		res := do(t, srv, tenant, "GET", "/api/v1/todos?limit=10", "")
		requireStatus(t, res, http.StatusOK)

		var page listJSON
		decode(t, res, &page)
		if page.Pagination.Total != 1 {
			t.Errorf("total = %d, want 1", page.Pagination.Total)
		}
		if page.Pagination.Limit != 10 {
			t.Errorf("limit = %d, want the one asked for", page.Pagination.Limit)
		}
	})

	// A read that carries a body: QUERY is safe and idempotent where POST
	// claims to mutate, and the alias exists only for intermediaries that
	// reject the method.
	t.Run("QUERY and its POST alias agree", func(t *testing.T) {
		const filter = `{"filter":{"like":{"title":"%tutorial%"}}}`

		viaQuery := do(t, srv, tenant, "QUERY", "/api/v1/todos", filter)
		requireStatus(t, viaQuery, http.StatusOK)
		viaPost := do(t, srv, tenant, "POST", "/api/v1/todos/_search", filter)
		requireStatus(t, viaPost, http.StatusOK)

		var a, b listJSON
		decode(t, viaQuery, &a)
		decode(t, viaPost, &b)

		if a.Pagination.Total != 1 {
			t.Errorf("the filter matched %d rows, want 1", a.Pagination.Total)
		}
		if len(a.Data) != len(b.Data) || (len(a.Data) > 0 && a.Data[0].ID != b.Data[0].ID) {
			t.Error("the alias returned something different from the primary route")
		}
	})

	t.Run("a filter that matches nothing returns nothing", func(t *testing.T) {
		res := do(t, srv, tenant, "QUERY", "/api/v1/todos", `{"filter":{"equals":{"priority":"low"}}}`)
		requireStatus(t, res, http.StatusOK)

		var page listJSON
		decode(t, res, &page)
		if page.Pagination.Total != 0 {
			t.Errorf("total = %d, want 0", page.Pagination.Total)
		}
	})

	t.Run("another tenant sees nothing", func(t *testing.T) {
		other := uuid.New()

		res := do(t, srv, other, "GET", "/api/v1/todos", "")
		requireStatus(t, res, http.StatusOK)
		var page listJSON
		decode(t, res, &page)
		if page.Pagination.Total != 0 {
			t.Errorf("a second tenant saw %d rows", page.Pagination.Total)
		}

		// 404 rather than 403, so an identifier cannot be probed for existence.
		byID := do(t, srv, other, "GET", "/api/v1/todos/"+created.ID.String(), "")
		requireStatus(t, byID, http.StatusNotFound)
	})

	t.Run("without a tenant nothing runs", func(t *testing.T) {
		res := do(t, srv, uuid.Nil, "GET", "/api/v1/todos", "")
		requireStatus(t, res, http.StatusUnauthorized)
	})

	// A Before hook on the deletion, not a validator rule: what makes the call
	// wrong is the state of the row it names, and the hook is handed that row.
	t.Run("an unfinished todo cannot be deleted", func(t *testing.T) {
		res := do(t, srv, tenant, "DELETE", "/api/v1/todos/"+created.ID.String(), "")
		requireStatus(t, res, http.StatusConflict)
	})

	// An endpoint the table configuration declares. rig routes it and decodes
	// it; what it does is the service layer's, and the build would have failed
	// without it.
	t.Run("a custom endpoint completes the task", func(t *testing.T) {
		res := do(t, srv, tenant, "POST", "/api/v1/todos/"+created.ID.String()+"/_complete",
			`{"note":"shipped it"}`)
		requireStatus(t, res, http.StatusOK)

		var got todoJSON
		decode(t, res, &got)
		if !got.IsDone {
			t.Error("the task should be done")
		}
		if got.Notes == nil || !strings.Contains(*got.Notes, "shipped it") {
			t.Errorf("the note should have been appended: %v", got.Notes)
		}
		// The endpoint writes through the repository, so the update stamp is
		// there exactly as a PATCH would have left it.
		if got.UpdatedAt == nil {
			t.Error("updatedAt should have been stamped")
		}
	})

	t.Run("completing it twice is a conflict", func(t *testing.T) {
		res := do(t, srv, tenant, "POST", "/api/v1/todos/"+created.ID.String()+"/_complete", `{}`)
		requireStatus(t, res, http.StatusConflict)
	})

	t.Run("delete retires the row", func(t *testing.T) {
		res := do(t, srv, tenant, "DELETE", "/api/v1/todos/"+created.ID.String(), "")
		requireStatus(t, res, http.StatusNoContent)

		list := do(t, srv, tenant, "GET", "/api/v1/todos", "")
		var page listJSON
		decode(t, list, &page)
		if page.Pagination.Total != 0 {
			t.Errorf("a deleted row is still listed (%d rows)", page.Pagination.Total)
		}
	})
}

// newServer starts the real mux against the real database, in a tenant of its
// own so the test neither sees nor disturbs anything else in the table.
func newServer(t *testing.T) (*httptest.Server, uuid.UUID) {
	pool := openPool(t)

	srv := httptest.NewServer(newHandler(pool, nil))
	t.Cleanup(srv.Close)

	return srv, newTenant(t, pool)
}

// newHandler is the wiring from main, without the parts a test cannot use: no
// process to shut down, and a notifier the caller chooses.
//
// It is a second copy, and worth being aware of. main builds its handler inside
// the call to serve.Main, where a test cannot reach it, and the alternative —
// hoisting it into a named function so both could call it — puts the wiring
// somewhere other than where the server is started. A route registered in one
// and not the other is what this trades away; api.Register is the part that
// matters and it is the same call in both.
func newHandler(pool *pgxpool.Pool, notifier todo.Notifier) http.Handler {
	repos := store.New(pool, store.Config{})
	// The same file service main.go builds, from the same `files:` block. The
	// backend is memory, so uploads live as long as this process does — which is
	// exactly as long as a test needs them.
	files := api.NewFiles(pool)

	return api.Register(api.Handlers{
		Server:         api.Server{GetClaims: headerClaims},
		Todo:           todo.New(repos.Todos, files, notifier, nil),
		TodoAttachment: todo_attachment.New(repos.TodoAttachments, files),
	})
}

// recorder stands in for whatever a real deployment would tell about a new
// todo. It is here so a test can assert that the hook actually fired, which is
// the part of "after the transaction commits" nothing else proves.
type recorder struct {
	mu       sync.Mutex
	messages []string
}

func (r *recorder) Record(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.messages = append(r.messages, message)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.messages)
}

// openPool connects to the database the example runs on.
func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// newTenant is a tenant of this test's own, so it neither sees nor disturbs
// anything else in the table.
func newTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()

	tenant := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM todo WHERE tenant_id = $1", tenant)
	})

	return tenant
}

func do(t *testing.T, srv *httptest.Server, tenant uuid.UUID, method, path, body string) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}

	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tenant != uuid.Nil {
		req.Header.Set("X-Tenant-Id", tenant.String())
		req.Header.Set("X-Account-Id", uuid.New().String())
	}

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func requireStatus(t *testing.T, res *http.Response, want int) {
	t.Helper()
	if res.StatusCode == want {
		return
	}
	body, _ := io.ReadAll(res.Body)
	t.Fatalf("%s: status %d, want %d\n%s", res.Request.URL.Path, res.StatusCode, want, body)
}

func decode(t *testing.T, res *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", res.Request.URL.Path, err)
	}
}

// AfterCommit is the only place a write may be announced from: the row is
// certain to exist, and nothing that happens there can take it back. What this
// pins down is that it fired at all, and that a refused write said nothing.
//
// It wires the service by hand rather than going through mount, because the
// notifier is the thing under test and mount builds the real one.
func TestTheCreateHookAnnouncesOnlyWhatCommitted(t *testing.T) {
	pool := openPool(t)
	tenant := newTenant(t, pool)
	notifier := &recorder{}

	srv := httptest.NewServer(newHandler(pool, notifier))
	t.Cleanup(srv.Close)

	res := do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Tell somebody"}`)
	requireStatus(t, res, http.StatusCreated)

	var created todoJSON
	decode(t, res, &created)

	messages := notifier.all()
	if len(messages) != 1 {
		t.Fatalf("recorded %v, want the one created todo", messages)
	}
	if !strings.Contains(messages[0], created.ID.String()) {
		t.Errorf("the message should name the row: %q", messages[0])
	}

	// A write that never happened is never announced.
	refused := do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Untitled"}`)
	requireStatus(t, refused, http.StatusUnprocessableEntity)

	if got := notifier.all(); len(got) != 1 {
		t.Errorf("recorded %v after a refused create, want no change", got)
	}
}

// failureJSON is the 422 body: a code and a message at the top for a person,
// and a structure underneath shaped like the request that failed.
type failureJSON struct {
	Code   string `json:"code"`
	Fields struct {
		Title *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"title"`
		Notes any `json:"notes"`
	} `json:"fields"`
}

// The table carries the snapshot columns, so rig made it versioned: an update
// copies the row as it was before writing the change. Nothing in the service
// layer maintains that — no trigger, no second table, no diff — which is
// exactly why it is worth pinning that it happens at all.
func TestEveryUpdateLeavesTheVersionBehindIt(t *testing.T) {
	srv, tenant := newServer(t)

	res := do(t, srv, tenant, "POST", "/api/v1/todos",
		`{"title":"Draft the talk","notes":"outline only","priority":"low"}`)
	requireStatus(t, res, http.StatusCreated)

	var task todoJSON
	decode(t, res, &task)

	if task.VersionType != "Original" {
		t.Errorf("versionType = %q, want Original", task.VersionType)
	}
	if task.SnapshotFromTodoID != nil {
		t.Errorf("a live row points at nothing: %v", task.SnapshotFromTodoID)
	}

	// Nothing has changed yet, so there is nothing to have kept.
	if got := versions(t, srv, tenant, task.ID); len(got) != 0 {
		t.Fatalf("a task nobody has changed has %d versions, want none", len(got))
	}

	patchTask(t, srv, tenant, task.ID, `{"title":"Draft the talk properly"}`)
	patchTask(t, srv, tenant, task.ID, `{"priority":"high","notes":null}`)

	history := versions(t, srv, tenant, task.ID)
	if len(history) != 2 {
		t.Fatalf("got %d versions after two updates, want 2", len(history))
	}

	// Newest first, and each one is the row as it was *before* the update that
	// produced it — the first still says "Draft the talk".
	if history[0].Title != "Draft the talk properly" {
		t.Errorf("the newest version is %q", history[0].Title)
	}
	if history[1].Title != "Draft the talk" {
		t.Errorf("the oldest version is %q, want the original title", history[1].Title)
	}
	if history[1].Notes == nil || *history[1].Notes != "outline only" {
		t.Errorf("the cleared notes should survive in the version: %v", history[1].Notes)
	}

	for i, v := range history {
		if v.VersionType != "Snapshot" {
			t.Errorf("version %d is %q, want Snapshot", i, v.VersionType)
		}
		if v.SnapshotFromTodoID == nil || *v.SnapshotFromTodoID != task.ID {
			t.Errorf("version %d points at %v, want %s", i, v.SnapshotFromTodoID, task.ID)
		}
		// The source row's updated_at at the moment the copy was taken. It
		// identifies the version captured, not when the copy was made.
		if v.SnapshotFromTodoAt == nil {
			t.Errorf("version %d records no source time", i)
		}
		if v.ID == task.ID {
			t.Error("a version is a row of its own, not the task")
		}
	}
}

// A history that turned up in the task list would make every list twice as long
// after a day's editing. Reads filter to the live row; a version is reachable
// only by asking for it.
func TestVersionsStayOutOfOrdinaryReads(t *testing.T) {
	srv, tenant := newServer(t)

	res := do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Book the room"}`)
	requireStatus(t, res, http.StatusCreated)
	var task todoJSON
	decode(t, res, &task)

	patchTask(t, srv, tenant, task.ID, `{"title":"Book the big room"}`)

	var page listJSON
	decode(t, do(t, srv, tenant, "GET", "/api/v1/todos", ""), &page)
	if page.Pagination.Total != 1 {
		t.Errorf("the list has %d rows, want only the live one", page.Pagination.Total)
	}

	var found listJSON
	decode(t, do(t, srv, tenant, "QUERY", "/api/v1/todos",
		`{"filter":{"like":{"title":"%room%"}}}`), &found)
	if found.Pagination.Total != 1 {
		t.Errorf("search matched %d rows, want only the live one", found.Pagination.Total)
	}
}

// Reverting goes through the ordinary update, which is what makes it undoable:
// the state being replaced is snapshotted on the way past, the same as any
// other change.
func TestRevertingIsItselfAnUpdate(t *testing.T) {
	srv, tenant := newServer(t)

	res := do(t, srv, tenant, "POST", "/api/v1/todos",
		`{"title":"Write the changelog","priority":"low"}`)
	requireStatus(t, res, http.StatusCreated)
	var task todoJSON
	decode(t, res, &task)

	patchTask(t, srv, tenant, task.ID, `{"title":"Write the release notes","priority":"high"}`)

	history := versions(t, srv, tenant, task.ID)
	if len(history) != 1 {
		t.Fatalf("got %d versions, want 1", len(history))
	}

	reverted := do(t, srv, tenant, "POST", "/api/v1/todos/"+task.ID.String()+"/_revert",
		`{"versionId":"`+history[0].ID.String()+`"}`)
	requireStatus(t, reverted, http.StatusOK)

	var back todoJSON
	decode(t, reverted, &back)
	if back.ID != task.ID {
		t.Errorf("the revert answered with %s, want the task itself", back.ID)
	}
	if back.Title != "Write the changelog" || back.Priority != "low" {
		t.Errorf("got %q/%s, want the version's values", back.Title, back.Priority)
	}

	// And the state it replaced is now history too, so the revert can be
	// reverted.
	after := versions(t, srv, tenant, task.ID)
	if len(after) != 2 {
		t.Fatalf("got %d versions after the revert, want 2", len(after))
	}
	if after[0].Title != "Write the release notes" {
		t.Errorf("the newest version is %q, want what the revert replaced", after[0].Title)
	}
}

// A version identifier is a row identifier, so it can name somebody else's row.
// The endpoint answers about the task it was asked about and nothing else.
func TestAVersionOfAnotherTaskIsNotFound(t *testing.T) {
	srv, tenant := newServer(t)

	var mine, theirs todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Mine"}`), &mine)
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Theirs"}`), &theirs)

	patchTask(t, srv, tenant, theirs.ID, `{"title":"Theirs, edited"}`)
	other := versions(t, srv, tenant, theirs.ID)
	if len(other) != 1 {
		t.Fatalf("got %d versions, want 1", len(other))
	}

	res := do(t, srv, tenant, "POST", "/api/v1/todos/"+mine.ID.String()+"/_revert",
		`{"versionId":"`+other[0].ID.String()+`"}`)
	requireStatus(t, res, http.StatusNotFound)

	// The live row is not one of its own versions, whatever its identifier
	// looks like.
	itself := do(t, srv, tenant, "POST", "/api/v1/todos/"+mine.ID.String()+"/_revert",
		`{"versionId":"`+mine.ID.String()+`"}`)
	requireStatus(t, itself, http.StatusNotFound)

	// And a task belonging to another tenant has no readable history at all,
	// rather than an empty one — an empty list would confirm the identifier.
	elsewhere := do(t, srv, uuid.New(), "GET", "/api/v1/todos/"+mine.ID.String()+"/_versions", "")
	requireStatus(t, elsewhere, http.StatusNotFound)
}

// versions reads a task's history through the endpoint.
func versions(t *testing.T, srv *httptest.Server, tenant, id uuid.UUID) []todoJSON {
	t.Helper()

	res := do(t, srv, tenant, "GET", "/api/v1/todos/"+id.String()+"/_versions", "")
	requireStatus(t, res, http.StatusOK)

	var page listJSON
	decode(t, res, &page)

	// A history is not paged: the whole of it comes back, and the block says so.
	if page.Pagination.Total != int64(len(page.Data)) {
		t.Errorf("total = %d for %d rows", page.Pagination.Total, len(page.Data))
	}
	return page.Data
}

// patchTask applies an update and fails the test if it did not take.
func patchTask(t *testing.T, srv *httptest.Server, tenant, id uuid.UUID, body string) {
	t.Helper()

	res := do(t, srv, tenant, "PATCH", "/api/v1/todos/"+id.String(), body)
	requireStatus(t, res, http.StatusOK)
}

// A soft delete retires the row rather than removing it, so there has to be
// somewhere it went. The trash is a listing of exactly that, and it is
// generated: the table has a deleted_at column, so the resource has retired
// rows and a route to read them.
func TestTheTrashListsWhatWasDeleted(t *testing.T) {
	srv, tenant := newServer(t)

	var kept, binned todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Still to do"}`), &kept)
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Changed my mind"}`), &binned)

	if empty := deleted(t, srv, tenant); len(empty) != 0 {
		t.Fatalf("nothing has been deleted, got %d rows", len(empty))
	}

	// This example refuses to delete an unfinished task, so finish it first.
	requireStatus(t, do(t, srv, tenant, "POST",
		"/api/v1/todos/"+binned.ID.String()+"/_complete", `{}`), http.StatusOK)
	requireStatus(t, do(t, srv, tenant, "DELETE",
		"/api/v1/todos/"+binned.ID.String(), ""), http.StatusNoContent)

	trash := deleted(t, srv, tenant)
	if len(trash) != 1 || trash[0].ID != binned.ID {
		t.Fatalf("the trash holds %+v, want the deleted task", trash)
	}

	// The live list is the mirror image: what is in one is not in the other.
	var live listJSON
	decode(t, do(t, srv, tenant, "GET", "/api/v1/todos", ""), &live)
	if live.Pagination.Total != 1 || live.Data[0].ID != kept.ID {
		t.Errorf("the live list holds %+v, want only the kept task", live.Data)
	}

	// And another tenant's trash is its own.
	if other := deleted(t, srv, uuid.New()); len(other) != 0 {
		t.Errorf("a second tenant saw %d retired rows", len(other))
	}
}

// The trash sits beside the collection, so it has to win against the route that
// reads one row by identifier — `_deleted` is a literal segment and `{id}` is
// not, which is the rule the mux applies and the reason the name has a prefix.
func TestTheTrashRouteIsNotMistakenForAnIdentifier(t *testing.T) {
	srv, tenant := newServer(t)

	res := do(t, srv, tenant, "GET", "/api/v1/todos/_deleted", "")
	requireStatus(t, res, http.StatusOK)

	// The other shape of the same mistake: an identifier that is not one still
	// reaches Get, and is refused there rather than routed somewhere odd.
	requireStatus(t, do(t, srv, tenant, "GET", "/api/v1/todos/not-a-uuid", ""), http.StatusBadRequest)
}

// deleted reads the trash through the endpoint.
func deleted(t *testing.T, srv *httptest.Server, tenant uuid.UUID) []todoJSON {
	t.Helper()

	res := do(t, srv, tenant, "GET", "/api/v1/todos/_deleted", "")
	requireStatus(t, res, http.StatusOK)

	var page listJSON
	decode(t, res, &page)
	if page.Pagination.Total != int64(len(page.Data)) {
		t.Errorf("total = %d for %d rows", page.Pagination.Total, len(page.Data))
	}
	return page.Data
}

// The other half of a soft delete: the row went somewhere, and this is the way
// back. Both ends are generated from the deleted_at column alone.
func TestARetiredTaskCanBeBroughtBack(t *testing.T) {
	srv, tenant := newServer(t)

	var task todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Cancel the thing"}`), &task)

	requireStatus(t, do(t, srv, tenant, "POST",
		"/api/v1/todos/"+task.ID.String()+"/_complete", `{}`), http.StatusOK)
	requireStatus(t, do(t, srv, tenant, "DELETE",
		"/api/v1/todos/"+task.ID.String(), ""), http.StatusNoContent)

	if len(deleted(t, srv, tenant)) != 1 {
		t.Fatal("the task should be in the trash")
	}

	res := do(t, srv, tenant, "POST", "/api/v1/todos/"+task.ID.String()+"/_restore", "")
	requireStatus(t, res, http.StatusOK)

	var back todoJSON
	decode(t, res, &back)
	if back.ID != task.ID || back.Title != "Cancel the thing" {
		t.Errorf("got %+v, want the task as it was", back)
	}

	// It is out of the trash and back in the list — the two are mirror images
	// on the way back as well as on the way out.
	if trash := deleted(t, srv, tenant); len(trash) != 0 {
		t.Errorf("the trash still holds %+v", trash)
	}
	var live listJSON
	decode(t, do(t, srv, tenant, "GET", "/api/v1/todos", ""), &live)
	if live.Pagination.Total != 1 || live.Data[0].ID != task.ID {
		t.Errorf("the restored task is not in the list: %+v", live.Data)
	}
}

// Restoring a row that was never deleted is not an error. It is already in the
// state the caller asked for, and answering otherwise would make a retry of a
// request whose response went missing look like a failure.
func TestRestoringALiveTaskIsNotAFailure(t *testing.T) {
	srv, tenant := newServer(t)

	var task todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Never deleted"}`), &task)

	for i := range 2 {
		res := do(t, srv, tenant, "POST", "/api/v1/todos/"+task.ID.String()+"/_restore", "")
		requireStatus(t, res, http.StatusOK)

		var back todoJSON
		decode(t, res, &back)
		if back.ID != task.ID {
			t.Errorf("attempt %d answered with %s", i+1, back.ID)
		}
	}

	// And somebody else's task is a 404, not a row brought back into a tenant
	// that cannot see it.
	elsewhere := do(t, srv, uuid.New(), "POST", "/api/v1/todos/"+task.ID.String()+"/_restore", "")
	requireStatus(t, elsewhere, http.StatusNotFound)
}

// The gap a restore leaves open, and the one this example exists to show.
//
// Deleting a task frees its title, deliberately: the create rule does not look
// in the trash, because refusing to reuse the name of something you deleted for
// the thirty days it stays restorable would be a strange thing to explain. But
// the retired row still carries the old title, so bringing it back would put
// two live tasks under one name.
//
// A restore carries no fields, so there is nothing for a rule to judge and
// nothing for a caller to fix. The decision is the restore hook's, and this
// example renames rather than refuses.
func TestARestoredTaskIsRenamedWhenItsTitleWasTaken(t *testing.T) {
	srv, tenant := newServer(t)

	var first todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos",
		`{"title":"Same title","notes":"keep me"}`), &first)

	requireStatus(t, do(t, srv, tenant, "POST",
		"/api/v1/todos/"+first.ID.String()+"/_complete", `{}`), http.StatusOK)
	requireStatus(t, do(t, srv, tenant, "DELETE",
		"/api/v1/todos/"+first.ID.String(), ""), http.StatusNoContent)

	// Allowed, and the point: the title is going spare while its holder is in
	// the trash.
	var second todoJSON
	res := do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Same title"}`)
	requireStatus(t, res, http.StatusCreated)
	decode(t, res, &second)

	restored := do(t, srv, tenant, "POST", "/api/v1/todos/"+first.ID.String()+"/_restore", "")
	requireStatus(t, restored, http.StatusOK)

	var back todoJSON
	decode(t, restored, &back)
	if back.ID != first.ID {
		t.Fatalf("restored %s, want the retired task", back.ID)
	}
	if !strings.HasPrefix(back.Title, "Same title (restored @ ") {
		t.Errorf("title = %q, want the original with a suffix", back.Title)
	}
	// Only the title. The hook set one field, so one field changed.
	if back.Notes == nil || *back.Notes != "keep me" {
		t.Errorf("notes = %v, want the value it was deleted with", back.Notes)
	}
	if !back.IsDone {
		t.Error("the row should come back as it was, not reset")
	}

	// Both are live, under names that do not collide.
	var live listJSON
	decode(t, do(t, srv, tenant, "GET", "/api/v1/todos", ""), &live)
	if live.Pagination.Total != 2 {
		t.Fatalf("the live list holds %d rows, want both", live.Pagination.Total)
	}
	titles := map[string]bool{}
	for _, row := range live.Data {
		if titles[row.Title] {
			t.Errorf("two live tasks under %q", row.Title)
		}
		titles[row.Title] = true
	}
	if trash := deleted(t, srv, tenant); len(trash) != 0 {
		t.Errorf("the trash should be empty: %+v", trash)
	}
}

// Nothing is renamed when nothing is in the way. The hook asks first, and a
// restore into a world that has not moved on is the row exactly as it was.
func TestARestoreLeavesTheTitleAloneWhenItIsFree(t *testing.T) {
	srv, tenant := newServer(t)

	var task todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Uncontested"}`), &task)

	requireStatus(t, do(t, srv, tenant, "POST",
		"/api/v1/todos/"+task.ID.String()+"/_complete", `{}`), http.StatusOK)
	requireStatus(t, do(t, srv, tenant, "DELETE",
		"/api/v1/todos/"+task.ID.String(), ""), http.StatusNoContent)

	res := do(t, srv, tenant, "POST", "/api/v1/todos/"+task.ID.String()+"/_restore", "")
	requireStatus(t, res, http.StatusOK)

	var back todoJSON
	decode(t, res, &back)
	if back.Title != "Uncontested" {
		t.Errorf("title = %q, want it untouched", back.Title)
	}
}

// The rule is a check and then a write, so two requests racing can both pass
// it. The partial unique index is what actually prevents the duplicate — and
// the predicate has to exclude the trash and the snapshots both, or an ordinary
// update would collide with the copy it had just taken.
func TestTheIndexIsWhatActuallyPreventsIt(t *testing.T) {
	srv, tenant := newServer(t)

	var task todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Once"}`), &task)

	// An update writes a snapshot carrying the old title and then the new one.
	// Both live under the same tenant, so an index that did not exclude
	// snapshots would refuse this.
	requireStatus(t, do(t, srv, tenant, "PATCH", "/api/v1/todos/"+task.ID.String(),
		`{"title":"Twice"}`), http.StatusOK)
	requireStatus(t, do(t, srv, tenant, "PATCH", "/api/v1/todos/"+task.ID.String(),
		`{"title":"Once"}`), http.StatusOK)

	// And a delete-then-recreate leaves a retired row holding the title, which
	// an index that did not exclude the trash would refuse.
	requireStatus(t, do(t, srv, tenant, "POST",
		"/api/v1/todos/"+task.ID.String()+"/_complete", `{}`), http.StatusOK)
	requireStatus(t, do(t, srv, tenant, "DELETE",
		"/api/v1/todos/"+task.ID.String(), ""), http.StatusNoContent)
	requireStatus(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Once"}`),
		http.StatusCreated)
}

// A read hook narrows what every filtered read may see, and the narrowing has
// to survive a caller who asks with an OR.
//
// This is why Narrow returns a filter instead of editing the caller's: the two
// are combined with AND. A hook that appended a condition to a filter whose
// OrCondition was set would widen it instead — the condition meant to restrict
// the read would become another way to match.
func TestAReadHookNarrowsAndCannotBeWidenedByAnOr(t *testing.T) {
	pool := openPool(t)
	tenant := newTenant(t, pool)

	// Only high-priority todos are visible through this service.
	high := model.TodoPriorityHigh
	narrowed := func(context.Context, *tenancy.Claims) (*model.TodoFilter, error) {
		f := model.NewTodoFilter()
		f.Equals = model.NewTodoFilterEquals()
		f.Equals.Priority = &high
		return &f, nil
	}

	var seen int
	counted := func(_ context.Context, _ *tenancy.Claims, rows []*model.Todo) error {
		// Once per read, not once per row: a hook that has to look something up
		// gets to do it once.
		seen++
		return nil
	}

	srv := httptest.NewServer(newScopedHandler(pool, narrowed, counted))
	t.Cleanup(srv.Close)

	for _, body := range []string{
		`{"title":"Urgent","priority":"high"}`,
		`{"title":"Whenever","priority":"low"}`,
	} {
		requireStatus(t, do(t, srv, tenant, "POST", "/api/v1/todos", body), http.StatusCreated)
	}

	var live listJSON
	decode(t, do(t, srv, tenant, "GET", "/api/v1/todos", ""), &live)
	if live.Pagination.Total != 1 || live.Data[0].Title != "Urgent" {
		t.Errorf("the list holds %+v, want only the high-priority one", live.Data)
	}

	// The interesting one. An OR filter that would match the hidden row on its
	// own is still bounded by the hook, because the two are ANDed rather than
	// merged into one set of conditions.
	var found listJSON
	decode(t, do(t, srv, tenant, "QUERY", "/api/v1/todos",
		`{"filter":{"orCondition":true,"equals":{"priority":"low"},"like":{"title":"%Urgent%"}}}`), &found)
	for _, row := range found.Data {
		if row.Priority != "high" {
			t.Errorf("an OR widened its way past the hook: %+v", row)
		}
	}

	// The total is the narrowed count, not the whole table with rows dropped
	// afterwards — the condition is in the WHERE clause, so paging is right.
	var page listJSON
	decode(t, do(t, srv, tenant, "GET", "/api/v1/todos?limit=1", ""), &page)
	if page.Pagination.Total != 1 {
		t.Errorf("total = %d, want the narrowed count", page.Pagination.Total)
	}

	if seen == 0 {
		t.Error("the row hook never ran")
	}
}

// Get is not narrowed — it fetches by primary key, and there is no filter to
// add a condition to. It still runs the row hook, which is where a rule about
// whether this caller may have this row goes.
func TestGetRunsTheRowHookAndCanBeRefusedByIt(t *testing.T) {
	pool := openPool(t)
	tenant := newTenant(t, pool)

	refuse := false
	rows := func(_ context.Context, caller *tenancy.Claims, rows []*model.Todo) error {
		// Typed and handed over, rather than fished out of the context. Nil
		// would mean an anonymous caller, which cannot happen here: nothing on
		// this resource is public.
		if caller == nil {
			return rigerr.Internal(nil, "no caller on a protected read")
		}
		if refuse {
			return rigerr.Forbidden("not yours")
		}
		return nil
	}

	srv := httptest.NewServer(newScopedHandler(pool, nil, rows))
	t.Cleanup(srv.Close)

	var task todoJSON
	decode(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Readable"}`), &task)

	requireStatus(t, do(t, srv, tenant, "GET", "/api/v1/todos/"+task.ID.String(), ""), http.StatusOK)

	refuse = true
	requireStatus(t, do(t, srv, tenant, "GET", "/api/v1/todos/"+task.ID.String(), ""),
		http.StatusForbidden)

	// And a write still finds its row: the hooks shape what a read answers
	// with, not what the repository can reach. A narrowed read on the way to an
	// update would have the update judge a row that is not the stored one.
	requireStatus(t, do(t, srv, tenant, "PATCH", "/api/v1/todos/"+task.ID.String(),
		`{"notes":"still writable"}`), http.StatusOK)
}

// newScopedHandler wires a service with read hooks of the caller's choosing.
//
// It builds its own rather than reusing the example's, whose read hooks are
// nil: a todo is visible to its whole tenant, and putting a narrowing rule in
// the example to make this test possible would be a rule nobody wanted.
func newScopedHandler(
	pool *pgxpool.Pool,
	narrow func(context.Context, *tenancy.Claims) (*model.TodoFilter, error),
	rows func(context.Context, *tenancy.Claims, []*model.Todo) error,
) http.Handler {
	svc := &scopedService{}
	svc.DefaultTodoService = api.NewDefaultTodoService(
		store.New(pool, store.Config{}).Todos,
		api.TodoContract{
			Hooks: api.TodoHooks{
				Read: dbhook.ReadHooks[model.TodoFilter, model.Todo]{Narrow: narrow, Rows: rows},
			},
			Endpoints: svc,
		},
		api.NewFiles(pool),
	)

	return api.Register(api.Handlers{
		Server: api.Server{GetClaims: headerClaims},
		Todo:   svc,
	})
}

// scopedService is the smallest thing that satisfies the interface: the
// generated default, plus the one custom endpoint it has no implementation for.
type scopedService struct {
	api.DefaultTodoService
}

func (s *scopedService) Complete(context.Context, api.Request[api.TodoCompletePath, struct{}, api.TodoCompleteBody]) (*model.Todo, error) {
	return nil, rigerr.Internal(nil, "not part of this test")
}

// The tenant is not a condition anybody gets to write.
//
// Every read is scoped to the caller's, ANDed above whatever they sent, so a
// filter cannot reach past it — not with an OR, not by nesting one. The field
// is not on the wire at all, which is the other half: a filter that could only
// ever be a no-op or a contradiction is worse documented than absent.
func TestTheTenantCannotBeAskedFor(t *testing.T) {
	srv, tenant := newServer(t)
	other := uuid.New()

	requireStatus(t, do(t, srv, tenant, "POST", "/api/v1/todos", `{"title":"Mine"}`), http.StatusCreated)

	// Unknown fields are rejected, so naming it is a 400 rather than a
	// condition that quietly does nothing.
	res := do(t, srv, tenant, "QUERY", "/api/v1/todos",
		`{"filter":{"equals":{"tenantId":"`+other.String()+`"}}}`)
	requireStatus(t, res, http.StatusBadRequest)

	// And the scoping itself holds whatever shape the filter takes, which is
	// what makes the field's absence a tidiness fix rather than the defence.
	for name, body := range map[string]string{
		"a plain filter": `{"filter":{"like":{"title":"%Mine%"}}}`,
		"an or":          `{"filter":{"orCondition":true,"like":{"title":"%Mine%"},"equals":{"priority":"low"}}}`,
		"a nested or":    `{"filter":{"nestedFilters":[{"orCondition":true,"like":{"title":"%Mine%"}}]}}`,
	} {
		var mine, theirs listJSON
		decode(t, do(t, srv, tenant, "QUERY", "/api/v1/todos", body), &mine)
		decode(t, do(t, srv, other, "QUERY", "/api/v1/todos", body), &theirs)

		if mine.Pagination.Total != 1 {
			t.Errorf("%s: the owner saw %d rows, want 1", name, mine.Pagination.Total)
		}
		if theirs.Pagination.Total != 0 {
			t.Errorf("%s: another tenant saw %d rows", name, theirs.Pagination.Total)
		}
	}
}
