//go:build docker

// Live sync, against a real sync service.
//
//	go test -tags docker ./internal/electrictest/
//
// The proxy's unit tests use a stub upstream, which proves what rig sends and
// nothing about what comes back. This one runs Postgres with logical
// replication, an ElectricSQL container in front of it, and the real proxy in
// front of that — so the claim that a subscriber sees only their own tenant's
// live rows is checked against the thing that decides it.
package electrictest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/runtime/electric"
)

const (
	pgName    = "rigElectric-db"
	pgPort    = dockerdb.PortElectricDB
	syncName  = "rigElectric-sync"
	syncPort  = dockerdb.PortElectricSync
	startWait = 60 * time.Second
	pollEvery = 250 * time.Millisecond
)

var (
	once     sync.Once
	pool     *pgxpool.Pool
	syncURL  string
	setupErr error
)

func environment(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()

	once.Do(func() { pool, syncURL, setupErr = start() })
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	return pool, syncURL
}

// hostBind is the address the database publishes on: every interface where a
// sibling container has to reach it, and loopback where it does not.
func hostBind() string {
	if runtime.GOOS == "linux" {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

func start() (*pgxpool.Pool, string, error) {
	ctx := context.Background()

	pg, sync := dockerdb.Qualify(pgName), dockerdb.Qualify(syncName)
	remove(pg)
	remove(sync)

	cfg := dockerdb.Config{
		Image: "postgres:17-alpine", Name: pg, Port: dockerdb.HostPort(pgPort),
		Database: "rig", User: "rig", Password: "rig",
		// Logical replication is how the sync service follows changes, and it
		// cannot be turned on after the server has started.
		Settings:  []string{"wal_level=logical"},
		StartWait: startWait,
		// The sync service is a second container, and on Linux it reaches this
		// one over the bridge rather than through the host's loopback. Docker
		// Desktop routes host.docker.internal to the VM's host either way, which
		// is why publishing on loopback is enough on a Mac and not on CI.
		Bind: hostBind(),
	}
	db, err := dockerdb.Start(ctx, cfg)
	if err != nil {
		return nil, "", err
	}

	p, err := pgxpool.New(ctx, db.URL())
	if err != nil {
		return nil, "", err
	}
	if _, err := p.Exec(ctx, schema); err != nil {
		return nil, "", fmt.Errorf("create the table: %w", err)
	}

	// The sync service reaches Postgres over the container network, so it needs
	// the host's address rather than the loopback the test uses.
	out, err := exec.Command("docker", "run", "--detach",
		"--name", sync,
		"--publish", dockerdb.Publish("127.0.0.1", syncPort, 3000),
		"--add-host", "host.docker.internal:host-gateway",
		// The port the database really publishes, which under isolation is not
		// the one in the constant: the sibling has to reach the same container
		// this test does.
		"--env", fmt.Sprintf("DATABASE_URL=postgresql://rig:rig@host.docker.internal:%d/rig?sslmode=disable", db.Port()),
		"--env", "ELECTRIC_INSECURE=true",
		"electricsql/electric:1.6.9",
	).CombinedOutput()
	if err != nil {
		return nil, "", fmt.Errorf("start the sync service: %w\n%s", err, out)
	}

	published, err := dockerdb.PortOf(ctx, "docker", sync)
	if err != nil {
		return nil, "", fmt.Errorf("read the sync service's port: %w", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", published)
	if err := waitReady(ctx, url); err != nil {
		logs, _ := exec.Command("docker", "logs", "--tail", "40", sync).CombinedOutput()
		return nil, "", fmt.Errorf("%w\n%s", err, logs)
	}
	return p, url, nil
}

const schema = `
DO $$ BEGIN
    CREATE TYPE lesson_version_type AS ENUM ('Original', 'Snapshot');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS lesson (
    id           uuid PRIMARY KEY,
    tenant_id    uuid NOT NULL,
    title        text NOT NULL,
    secret_note  text,
    deleted_at   timestamptz,
    -- A real enum, the way rig's snapshot migrations write it. It used to be
    -- text, and that is how a where clause the sync service cannot type an
    -- enum value for stayed green here while failing against a real schema:
    -- the generated filters compare version_type::text for exactly this.
    version_type lesson_version_type NOT NULL DEFAULT 'Original',
    snapshot_from_lesson_id uuid
);
`

func waitReady(ctx context.Context, base string) error {
	deadline := time.Now().Add(startWait)
	client := &http.Client{Timeout: 5 * time.Second}

	var last string
	for time.Now().Before(deadline) {
		res, err := client.Get(base + "/v1/health")
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			// The service answers before it has finished connecting, and
			// reports its own state in the body.
			if res.StatusCode == http.StatusOK && strings.Contains(string(body), "active") {
				return nil
			}
			last = fmt.Sprintf("status %d: %s", res.StatusCode, body)
		} else {
			last = err.Error()
		}
		time.Sleep(pollEvery)
	}
	return fmt.Errorf("the sync service never became ready: %s", last)
}

func remove(name string) {
	_ = exec.Command("docker", "rm", "-f", "-v", name).Run()
}

// row is one entry of a shape response.
type row struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

// serve stands a shape up behind the real proxy, with the filter built the way
// a generated handler builds it: the conditions first, then the request.
func serve(t *testing.T, proxy *electric.Proxy, build func(*electric.Where)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var where electric.Where
		build(&where)

		proxy.Serve(w, r, electric.Shape{
			Table:   "lesson",
			Where:   where.SQL(),
			Params:  where.Params(),
			Columns: []string{"id", "tenant_id", "title"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// front serves the live shape the way the generated handler does: the tenant and
// lifecycle conditions first, then whatever the application adds.
func front(t *testing.T, proxy *electric.Proxy, tenant uuid.UUID, narrow func(*electric.Where)) *httptest.Server {
	t.Helper()

	return serve(t, proxy, func(where *electric.Where) {
		where.Eq("tenant_id", tenant.String()).
			IsNull("deleted_at").
			EqText("version_type", "Original")

		if narrow != nil {
			narrow(where)
		}
	})
}

// frontDeleted serves the trash shape: the live one inverted.
func frontDeleted(t *testing.T, proxy *electric.Proxy, tenant uuid.UUID) *httptest.Server {
	t.Helper()

	return serve(t, proxy, func(where *electric.Where) {
		where.Eq("tenant_id", tenant.String()).
			NotNull("deleted_at").
			EqText("version_type", "Original")
	})
}

// frontVersions serves one row's history shape.
func frontVersions(t *testing.T, proxy *electric.Proxy, tenant, of uuid.UUID) *httptest.Server {
	t.Helper()

	return serve(t, proxy, func(where *electric.Where) {
		where.Eq("tenant_id", tenant.String()).
			EqText("version_type", "Snapshot").
			Eq("snapshot_from_lesson_id", of.String()).
			IsNull("deleted_at")
	})
}

// fetch reads one shape response.
func fetch(t *testing.T, srv *httptest.Server, query string) []row {
	t.Helper()

	res, err := srv.Client().Get(srv.URL + "?offset=-1&" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d\n%s", res.StatusCode, body)
	}

	var out []row
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}

	// The last entry is a control message saying the snapshot is complete.
	var data []row
	for _, r := range out {
		if r.Key != "" {
			data = append(data, r)
		}
	}
	return data
}

func insert(t *testing.T, p *pgxpool.Pool, tenant uuid.UUID, title string, deleted bool, version string) uuid.UUID {
	t.Helper()

	id := uuid.New()
	var deletedAt *time.Time
	if deleted {
		now := time.Now()
		deletedAt = &now
	}
	if version == "" {
		version = "Original"
	}

	if _, err := p.Exec(context.Background(), `
		INSERT INTO lesson (id, tenant_id, title, secret_note, deleted_at, version_type)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, tenant, title, "not for the wire", deletedAt, version); err != nil {
		t.Fatal(err)
	}
	return id
}

// titles names what came back, for a failure that has to say which rows.
func titles(rows []row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprint(r.Value["title"]))
	}
	return out
}

// snapshot writes a prior version of one row.
func snapshot(t *testing.T, p *pgxpool.Pool, tenant uuid.UUID, title string, of uuid.UUID) uuid.UUID {
	t.Helper()

	id := uuid.New()
	if _, err := p.Exec(context.Background(), `
		INSERT INTO lesson (id, tenant_id, title, secret_note, version_type, snapshot_from_lesson_id)
		VALUES ($1, $2, $3, $4, 'Snapshot', $5)`,
		id, tenant, title, "not for the wire", of); err != nil {
		t.Fatal(err)
	}
	return id
}

// The claim the whole package rests on.
func TestAShapeStreamsOnlyTheCallersLiveRows(t *testing.T) {
	p, url := environment(t)

	proxy, err := electric.New(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	mine, theirs := uuid.New(), uuid.New()

	wanted := insert(t, p, mine, "mine, live", false, "")
	insert(t, p, mine, "mine, deleted", true, "")
	insert(t, p, mine, "mine, a snapshot", false, "Snapshot")
	insert(t, p, theirs, "theirs, live", false, "")

	rows := fetch(t, front(t, proxy, mine, nil), "")

	if len(rows) != 1 {
		t.Fatalf("got %d rows %v, want only the live one", len(rows), titles(rows))
	}
	if got := fmt.Sprint(rows[0].Value["id"]); got != wanted.String() {
		t.Errorf("id = %s, want %s", got, wanted)
	}

	// A shape carries every column it names to every subscriber, forever.
	if _, leaked := rows[0].Value["secret_note"]; leaked {
		t.Error("a column outside the projection reached the stream")
	}
}

// The trash shape carries exactly what the live one refuses, against the thing
// that decides it rather than against a string of SQL.
func TestATrashShapeStreamsOnlyRetiredRows(t *testing.T) {
	p, url := environment(t)

	proxy, err := electric.New(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	mine, theirs := uuid.New(), uuid.New()

	insert(t, p, mine, "mine, live", false, "")
	wanted := insert(t, p, mine, "mine, deleted", true, "")
	insert(t, p, theirs, "theirs, deleted", true, "")

	rows := fetch(t, frontDeleted(t, proxy, mine), "")

	if len(rows) != 1 {
		t.Fatalf("got %d rows %v, want only the retired one", len(rows), titles(rows))
	}
	if got := fmt.Sprint(rows[0].Value["id"]); got != wanted.String() {
		t.Errorf("id = %s, want %s", got, wanted)
	}
}

// History is per row. A shape that carried every version of every row would be
// a different and much larger thing than the GET /{id}/_versions it mirrors.
func TestAHistoryShapeStreamsOneRowsVersions(t *testing.T) {
	p, url := environment(t)

	proxy, err := electric.New(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	mine := uuid.New()

	live := insert(t, p, mine, "the row", false, "")
	other := insert(t, p, mine, "another row", false, "")

	wanted := snapshot(t, p, mine, "the row, as it was", live)
	snapshot(t, p, mine, "another row, as it was", other)

	rows := fetch(t, frontVersions(t, proxy, mine, live), "")

	if len(rows) != 1 {
		t.Fatalf("got %d rows %v, want only this row's history", len(rows), titles(rows))
	}
	if got := fmt.Sprint(rows[0].Value["id"]); got != wanted.String() {
		t.Errorf("id = %s, want %s", got, wanted)
	}

	// And never the live row itself, which is what the live shape is for.
	for _, r := range rows {
		if fmt.Sprint(r.Value["id"]) == live.String() {
			t.Error("the live row reached its own history")
		}
	}
}

// The handler cannot look the row up — the electric server has no database
// handle — so an id from another tenant is answered by the tenant condition
// rather than by a 404. What matters is that it is answered.
func TestAHistoryShapeRefusesAnotherTenantsRow(t *testing.T) {
	p, url := environment(t)

	proxy, err := electric.New(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	mine, theirs := uuid.New(), uuid.New()

	hers := insert(t, p, theirs, "hers", false, "")
	snapshot(t, p, theirs, "hers, as it was", hers)

	if rows := fetch(t, frontVersions(t, proxy, mine, hers), ""); len(rows) != 0 {
		t.Fatalf("got %d rows %v, want none", len(rows), titles(rows))
	}
}

// A client that could set the filter could subscribe to anybody.
func TestAClientCannotWidenTheShape(t *testing.T) {
	p, url := environment(t)

	proxy, err := electric.New(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	mine, theirs := uuid.New(), uuid.New()
	kept := insert(t, p, mine, "mine", false, "")
	insert(t, p, theirs, "theirs", false, "")

	// The caller's own retired row and its own history, so that a widening
	// attempt aimed at the lifecycle conditions has something to find if it
	// works. These belong to the trash and history shapes, which are routes.
	insert(t, p, mine, "mine, deleted", true, "")
	snapshot(t, p, mine, "mine, as it was", kept)

	srv := front(t, proxy, mine, nil)

	for _, attempt := range []string{
		"where=true",
		"table=lesson&where=1%3D1",
		"columns=id,tenant_id,title,secret_note",
		"params[1]=" + theirs.String(),
		"deleted=true",
		"version_type=Snapshot",
	} {
		t.Run(attempt, func(t *testing.T) {
			rows := fetch(t, srv, attempt)
			if len(rows) != 1 {
				t.Fatalf("got %d rows %v, want only this tenant's live one", len(rows), titles(rows))
			}
			if got := fmt.Sprint(rows[0].Value["title"]); got != "mine" {
				t.Errorf("title = %q", got)
			}
			if _, leaked := rows[0].Value["secret_note"]; leaked {
				t.Error("a client chose the projection")
			}
		})
	}
}

// A scoping function can only ever show less.
func TestAScopeNarrows(t *testing.T) {
	p, url := environment(t)

	proxy, err := electric.New(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	tenant := uuid.New()
	insert(t, p, tenant, "kept", false, "")
	insert(t, p, tenant, "filtered out", false, "")

	srv := front(t, proxy, tenant, func(w *electric.Where) {
		w.Eq("title", "kept")
	})

	rows := fetch(t, srv, "")
	if len(rows) != 1 || fmt.Sprint(rows[0].Value["title"]) != "kept" {
		t.Fatalf("got %d rows, want the one the scope kept", len(rows))
	}
}

// The handle and offset are how a subscription continues. Without them a client
// re-reads the whole shape on every poll, which is the difference between live
// sync and a slow list endpoint.
func TestTheCursorComesBack(t *testing.T) {
	p, url := environment(t)

	proxy, err := electric.New(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	tenant := uuid.New()
	insert(t, p, tenant, "one", false, "")

	srv := front(t, proxy, tenant, nil)
	res, err := srv.Client().Get(srv.URL + "?offset=-1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.ReadAll(res.Body)

	for _, header := range []string{"electric-handle", "electric-offset"} {
		if res.Header.Get(header) == "" {
			t.Errorf("%s was not passed back", header)
		}
	}
}
