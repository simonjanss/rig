//go:build docker

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/examples/todo/services/todo"
	rignotify "github.com/simonjanss/rig/notify"
)

// The inbox in an application with no authentication at all, which is the half
// of the story examples/auth cannot tell.
//
// Nothing here signs in. Claims come from two headers, exactly as they did
// before notifications existed, and the inbox works because what a notification
// needs is an account to be addressed to — not a sign-in endpoint. The tenancy
// migration brought rig_account and nothing else did.
func TestTheInboxWithoutAuthentication(t *testing.T) {
	w := newInboxWorld(t)

	// Two people in one tenant: one writes the task, the other is told about it.
	author := w.account(t, "author")
	reader := w.account(t, "reader")

	created := w.create(t, author, "Water the plants")

	// The engine's own pass, run here rather than waited for: the in-process
	// one is latency and this is the same code the cron task calls.
	report, err := w.engine.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if report.Resolved == 0 {
		t.Fatalf("nothing was resolved: %s", report)
	}
	if report.Empty > 0 {
		t.Errorf("a resolution found nobody to tell: %s", report)
	}

	t.Run("the badge counts what the reader has not seen", func(t *testing.T) {
		var body struct{ Unread int }
		w.get(t, reader, "/notifications/_unread-count").decode(t, &body)
		if body.Unread == 0 {
			t.Error("the reader was told about a task and the badge says nothing")
		}
	})

	t.Run("the author who wrote it is told nothing", func(t *testing.T) {
		var body struct{ Unread int }
		w.get(t, author, "/notifications/_unread-count").decode(t, &body)
		if body.Unread != 0 {
			t.Errorf("unread = %d for the account that caused the change", body.Unread)
		}
	})

	t.Run("the inbox names the task without carrying it", func(t *testing.T) {
		var body struct {
			Data []struct {
				ID             uuid.UUID `json:"id"`
				NotificationID uuid.UUID `json:"notificationId"`
				Kind           string    `json:"kind"`
				EventCount     int       `json:"eventCount"`
			} `json:"data"`
		}
		w.get(t, reader, "/notifications").decode(t, &body)

		if len(body.Data) == 0 {
			t.Fatal("the reader's inbox is empty")
		}
		if got := body.Data[0].Kind; got != todo.KindTodoCreated {
			t.Errorf("kind = %q, want %q", got, todo.KindTodoCreated)
		}
		// Identifiers rather than the rows themselves. A client doing live sync
		// already has the task; one that is not turns notifications.expose on
		// and gets a resource with embeddable relations.
		if body.Data[0].NotificationID == uuid.Nil {
			t.Error("a line should name the notification it stands for")
		}
	})

	t.Run("one person clearing their inbox changes nobody else's", func(t *testing.T) {
		third := w.account(t, "third")
		w.create(t, author, "Take the bins out")
		if _, err := w.engine.Resolve(context.Background()); err != nil {
			t.Fatal(err)
		}

		before := w.unread(t, third)
		if before == 0 {
			t.Fatal("the third account should have been told")
		}

		w.post(t, reader, "/notifications/_read-all")
		if after := w.unread(t, third); after != before {
			t.Errorf("unread = %d after somebody else read theirs, want %d", after, before)
		}
		if after := w.unread(t, reader); after != 0 {
			t.Errorf("the reader's own unread = %d after marking all read", after)
		}
	})

	_ = created
}

// inboxWorld is the example's own wiring, over a real database, with a tenant of
// its own so one run's rows are invisible to the next.
type inboxWorld struct {
	pool   *pgxpool.Pool
	http   *httptest.Server
	tenant uuid.UUID
	engine *rignotify.Engine
	svc    api.DefaultTodoService
}

func newInboxWorld(t *testing.T) *inboxWorld {
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

	w := &inboxWorld{pool: pool, tenant: uuid.New()}
	if _, err := pool.Exec(ctx,
		`INSERT INTO rig_tenant (id, name, slug) VALUES ($1, $2, $3)`,
		w.tenant, "Inbox test", w.tenant.String()); err != nil {
		t.Fatal(err)
	}

	// The same shape main.go builds, and in the same order: the registry first
	// and empty, because a service needs the notify service and the dispatcher
	// needs the service.
	repos := store.New(pool, store.Config{})
	reg := rignotify.NewRegistry()
	inbox := api.NewNotifications(pool, reg)
	w.svc = todo.New(repos.Todos, api.NewFiles(pool), nil, inbox, pool, nil)
	reg.Register(api.NewTodoSubject(w.svc))
	w.engine = api.NewNotificationEngine(pool, reg)

	mux := api.Register(api.Handlers{
		Server:        api.Server{GetClaims: headerClaims},
		Todo:          w.svc,
		Notifications: inbox,
	})
	w.http = httptest.NewServer(mux)
	t.Cleanup(w.http.Close)

	return w
}

// account makes somebody in this world's tenant.
//
// An identity as well as an account, because an account for a person has one:
// the CHECK on rig_account makes that structural rather than a convention.
func (w *inboxWorld) account(t *testing.T, name string) uuid.UUID {
	t.Helper()
	email := name + "+" + uuid.NewString()[:8] + "@example.test"

	var identityID uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `
		INSERT INTO rig_identity (id, email_address, display_name)
		VALUES ($1, $2, $2) RETURNING id`, uuid.New(), email).Scan(&identityID); err != nil {
		t.Fatal(err)
	}

	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name)
		VALUES ($1, $2, $3, $4, $4) RETURNING id`,
		uuid.New(), w.tenant, identityID, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// create writes a task through the API, which is what runs the hook that
// announces it.
func (w *inboxWorld) create(t *testing.T, as uuid.UUID, title string) uuid.UUID {
	t.Helper()

	res := w.request(t, http.MethodPost, "/api/v1/todos", as, `{"title":`+quote(title)+`}`)
	if res.status != http.StatusCreated {
		t.Fatalf("create: %d %s", res.status, res.body)
	}
	var body struct {
		ID uuid.UUID `json:"id"`
	}
	res.decode(t, &body)
	return body.ID
}

func (w *inboxWorld) unread(t *testing.T, as uuid.UUID) int {
	t.Helper()
	var body struct{ Unread int }
	w.get(t, as, "/notifications/_unread-count").decode(t, &body)
	return body.Unread
}

func (w *inboxWorld) get(t *testing.T, as uuid.UUID, path string) inboxResponse {
	t.Helper()
	return w.request(t, http.MethodGet, path, as, "")
}

func (w *inboxWorld) post(t *testing.T, as uuid.UUID, path string) inboxResponse {
	t.Helper()
	return w.request(t, http.MethodPost, path, as, "")
}

type inboxResponse struct {
	status int
	body   string
}

func (r inboxResponse) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), into); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// request is the two headers this example authenticates with, and nothing else.
func (w *inboxWorld) request(t *testing.T, method, path string, as uuid.UUID, body string) inboxResponse {
	t.Helper()

	req, err := http.NewRequest(method, w.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", w.tenant.String())
	req.Header.Set("X-Account-Id", as.String())

	res, err := w.http.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	var got bytes.Buffer
	if _, err := got.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	return inboxResponse{status: res.StatusCode, body: got.String()}
}

func quote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
