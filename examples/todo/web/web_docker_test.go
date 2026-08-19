//go:build docker

package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/examples/todo/services/todo"
	"github.com/simonjanss/rig/examples/todo/web"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The UI is what the lifecycle features look like to a person, so the test is
// the walk-through: add something, change it, finish it, delete it, bring it
// back, and put it back the way it was. Each step asserts what the page now
// says, because that is the part a reader of the example is being shown.
func TestTheLifecycleWalkthrough(t *testing.T) {
	ui := newUI(t)

	board := ui.post(t, "/ui/todos", url.Values{
		"title":    {"Water the plants"},
		"priority": {"normal"},
	})
	if !strings.Contains(board, "Water the plants") {
		t.Fatalf("the new row should be listed:\n%s", board)
	}
	id := firstID(t, board)

	t.Run("an update leaves the previous version behind", func(t *testing.T) {
		board := ui.post(t, "/ui/todos/"+id+"/title", url.Values{
			"title": {"Water the plants twice"},
			"open":  {id},
		})

		if !strings.Contains(board, "Water the plants twice") {
			t.Error("the row should show the new title")
		}
		// The history panel is open on the same response, and the old title is
		// in it — that is the snapshot the update wrote before changing the row.
		if !strings.Contains(board, `class="timeline"`) {
			t.Errorf("the history panel should be showing:\n%s", board)
		}
		if !strings.Contains(board, "Water the plants</span>") {
			t.Errorf("the previous title should be in the history:\n%s", board)
		}

		// The timeline says what each step changed, which is the same thing as
		// what reverting to it would put back.
		for _, want := range []string{
			`<span class="field">title</span>`,
			`<span class="was">Water the plants</span>`,
			`<span class="now-val">Water the plants twice</span>`,
			`<span class="badge">current</span>`,
			`<span class="badge">as created</span>`,
		} {
			if !strings.Contains(board, want) {
				t.Errorf("the timeline should hold %s:\n%s", want, board)
			}
		}
	})

	t.Run("a rule refuses the second completion", func(t *testing.T) {
		if got := ui.post(t, "/ui/todos/"+id+"/complete", nil); !strings.Contains(got, "Done.") {
			t.Error("the first completion should succeed")
		}

		// Completing a finished task is a conflict, and the service's message is
		// what the page shows rather than a generic failure.
		got := ui.post(t, "/ui/todos/"+id+"/complete", nil)
		if !strings.Contains(got, `class="flash error"`) {
			t.Errorf("the second completion should be refused:\n%s", got)
		}
		if !strings.Contains(got, "already done") {
			t.Errorf("the page should say why:\n%s", got)
		}
	})

	t.Run("the trash is folded away until there is a reason to look", func(t *testing.T) {
		page := ui.get(t, "/")

		// Closed, so nothing in it is rendered at all — the count is what says
		// whether it is worth opening.
		if strings.Contains(page, "Restore") {
			t.Errorf("the trash should be collapsed to begin with:\n%s", page)
		}
		if !strings.Contains(page, `aria-expanded="false"`) {
			t.Error("the toggle should report itself closed")
		}
		if !strings.Contains(collapse(page), `<span class="count">0</span>`) {
			t.Errorf("the count should be shown beside the heading:\n%s", page)
		}
	})

	t.Run("deleting moves it to the trash", func(t *testing.T) {
		board := ui.post(t, "/ui/todos/"+id+"/delete", nil)

		// A delete opens the trash whatever it was, because a soft delete that
		// leaves the page looking like a disappearance has explained nothing.
		if !strings.Contains(board, `aria-expanded="true"`) {
			t.Error("deleting should open the trash")
		}
		if !strings.Contains(collapse(board), `<span class="count">1</span>`) {
			t.Errorf("the count should have gone up:\n%s", board)
		}

		live, trash := split(t, board)
		if strings.Contains(live, id) {
			t.Error("the row should have left the live list")
		}
		if !strings.Contains(trash, id) {
			t.Errorf("the row should be in the trash:\n%s", trash)
		}
		// Deleted, not gone: the trash says when, which it can only do because
		// the row is still there with a stamp on it.
		if !strings.Contains(trash, "deleted ") {
			t.Error("the trash should say when the row was deleted")
		}
	})

	t.Run("restoring brings it back", func(t *testing.T) {
		board := ui.post(t, "/ui/todos/"+id+"/restore", nil)

		live, trash := split(t, board)
		if !strings.Contains(live, id) {
			t.Errorf("the row should be live again:\n%s", live)
		}
		if strings.Contains(trash, id) {
			t.Error("the row should have left the trash")
		}
	})

	t.Run("reverting puts an earlier version back", func(t *testing.T) {
		// The panel lists the versions with a button each, so the identifier the
		// page would post is the one this test posts.
		panel := ui.get(t, "/?open="+id)
		versionID := versionInPanel(t, panel)

		board := ui.post(t, "/ui/todos/"+id+"/revert", url.Values{
			"versionId": {versionID},
			"open":      {id},
		})

		if !strings.Contains(board, "Water the plants<") {
			t.Errorf("the original title should be back:\n%s", board)
		}
		// A revert is an update, so what it replaced is now in the history too:
		// the state before the revert was snapshotted on the way past.
		if !strings.Contains(board, "Water the plants twice") {
			t.Error("the reverted-away state should itself be a version now")
		}
	})
}

// One tenant cannot see another's rows, and the UI is not a way around that:
// it passes the claims it was built with, and the repository scopes by them.
func TestTheUIIsScopedToItsTenant(t *testing.T) {
	first, second := newUI(t), newUI(t)

	first.post(t, "/ui/todos", url.Values{"title": {"Only mine"}})

	if got := second.get(t, "/"); strings.Contains(got, "Only mine") {
		t.Errorf("a second tenant should not see the first's rows:\n%s", got)
	}
	if got := first.get(t, "/"); !strings.Contains(got, "Only mine") {
		t.Error("the first tenant should still see its own")
	}
}

// The count and the rows come from the same read, so a collapsed trash is only
// hidden rather than unknown — and expanding it is a request like any other.
func TestTheTrashExpandsAndFoldsAgain(t *testing.T) {
	ui := newUI(t)

	board := ui.post(t, "/ui/todos", url.Values{"title": {"Throw me away"}})
	id := firstID(t, board)

	// Finished first: the example's delete hook refuses to retire a task that is
	// not done, which is the sort of rule a service layer is for.
	ui.post(t, "/ui/todos/"+id+"/complete", nil)
	ui.post(t, "/ui/todos/"+id+"/delete", nil)

	open := ui.get(t, "/ui/board?trash=1")
	if !strings.Contains(open, "Throw me away") || !strings.Contains(open, "Restore") {
		t.Errorf("the expanded trash should list what is in it:\n%s", open)
	}

	// What the toggle posts when it is already open.
	closed := ui.get(t, "/ui/board?trash=")
	if strings.Contains(closed, "Restore") {
		t.Errorf("it should fold away again:\n%s", closed)
	}
	if !strings.Contains(collapse(closed), `<span class="count">1</span>`) {
		t.Error("the count should still be there when it is closed")
	}
}

// collapse squeezes runs of whitespace, so an assertion about the markup does
// not depend on how the template happened to indent it.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// client drives one tenant's UI.
type client struct {
	mux *http.ServeMux
}

func newUI(t *testing.T) *client {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55440/rig?sslmode=disable"
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

	repos := store.New(pool, store.Config{})
	svc := todo.New(repos.Todos, api.NewFiles(pool), quiet{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A tenant of its own, so one run's rows are invisible to the next and two
	// clients in one test are two tenants.
	ui, err := web.New(svc, tenancy.Claims{
		TenantID:  uuid.New(),
		AccountID: uuid.New(),
		Subject:   tenancy.SubjectAccount,
	})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	ui.Mount(mux)
	return &client{mux: mux}
}

func (c *client) get(t *testing.T, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, rec.Code)
	}
	return rec.Body.String()
}

func (c *client) post(t *testing.T, path string, form url.Values) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	c.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: %d", path, rec.Code)
	}
	return rec.Body.String()
}

// firstID reads an identifier out of the rendered actions, so the test acts on
// what the page offers rather than on something it looked up another way.
var actionPattern = regexp.MustCompile(`/ui/todos/([0-9a-f-]{36})/delete`)

func firstID(t *testing.T, html string) string {
	t.Helper()
	m := actionPattern.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no row action found in:\n%s", html)
	}
	return m[1]
}

var versionPattern = regexp.MustCompile(`name="versionId" value="([0-9a-f-]{36})"`)

func versionInPanel(t *testing.T, html string) string {
	t.Helper()
	m := versionPattern.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no version offered in the panel:\n%s", html)
	}
	return m[1]
}

// split separates the two lists, so "is it live or is it in the trash" is a
// question the test can ask.
func split(t *testing.T, html string) (live, trash string) {
	t.Helper()
	i := strings.Index(html, ">Trash")
	if i < 0 {
		t.Fatalf("no trash section in:\n%s", html)
	}
	return html[:i], html[i:]
}

// quiet stands in for the notifier: the walk-through is about the page, not
// about what got announced.
type quiet struct{}

func (quiet) Record(string) {}
