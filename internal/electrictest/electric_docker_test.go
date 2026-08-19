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

	remove(pgName)
	remove(syncName)

	cfg := dockerdb.Config{
		Image: "postgres:17-alpine", Name: pgName, Port: pgPort,
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
	if _, err := dockerdb.Start(ctx, cfg); err != nil {
		return nil, "", err
	}

	p, err := pgxpool.New(ctx, cfg.URL())
	if err != nil {
		return nil, "", err
	}
	if _, err := p.Exec(ctx, schema); err != nil {
		return nil, "", fmt.Errorf("create the table: %w", err)
	}

	// The sync service reaches Postgres over the container network, so it needs
	// the host's address rather than the loopback the test uses.
	out, err := exec.Command("docker", "run", "--detach",
		"--name", syncName,
		"--publish", fmt.Sprintf("127.0.0.1:%d:3000", syncPort),
		"--add-host", "host.docker.internal:host-gateway",
		"--env", fmt.Sprintf("DATABASE_URL=postgresql://rig:rig@host.docker.internal:%d/rig?sslmode=disable", pgPort),
		"--env", "ELECTRIC_INSECURE=true",
		"electricsql/electric:1.6.9",
	).CombinedOutput()
	if err != nil {
		return nil, "", fmt.Errorf("start the sync service: %w\n%s", err, out)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", syncPort)
	if err := waitReady(ctx, url); err != nil {
		logs, _ := exec.Command("docker", "logs", "--tail", "40", syncName).CombinedOutput()
		return nil, "", fmt.Errorf("%w\n%s", err, logs)
	}
	return p, url, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS lesson (
    id           uuid PRIMARY KEY,
    tenant_id    uuid NOT NULL,
    title        text NOT NULL,
    secret_note  text,
    deleted_at   timestamptz,
    version_type text NOT NULL DEFAULT 'Original'
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

// front serves the shape the way the generated handler does: the tenant and
// lifecycle conditions first, then whatever the application adds.
func front(t *testing.T, proxy *electric.Proxy, tenant uuid.UUID, narrow func(*electric.Where)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var where electric.Where
		where.Eq("tenant_id", tenant.String()).
			IsNull("deleted_at").
			Eq("version_type", "Original")

		if narrow != nil {
			narrow(&where)
		}

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
		var titles []string
		for _, r := range rows {
			titles = append(titles, fmt.Sprint(r.Value["title"]))
		}
		t.Fatalf("got %d rows %v, want only the live one", len(rows), titles)
	}
	if got := fmt.Sprint(rows[0].Value["id"]); got != wanted.String() {
		t.Errorf("id = %s, want %s", got, wanted)
	}

	// A shape carries every column it names to every subscriber, forever.
	if _, leaked := rows[0].Value["secret_note"]; leaked {
		t.Error("a column outside the projection reached the stream")
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
	insert(t, p, mine, "mine", false, "")
	insert(t, p, theirs, "theirs", false, "")

	srv := front(t, proxy, mine, nil)

	for _, attempt := range []string{
		"where=true",
		"table=lesson&where=1%3D1",
		"columns=id,tenant_id,title,secret_note",
		"params[1]=" + theirs.String(),
	} {
		t.Run(attempt, func(t *testing.T) {
			rows := fetch(t, srv, attempt)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want only this tenant's one", len(rows))
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
