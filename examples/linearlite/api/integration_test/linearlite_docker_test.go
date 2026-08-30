//go:build docker

// Package integration is this example's tests, all of them.
//
//	go test -tags docker ./integration/
//
// The docker suite shares one harness: a real database, the same app.New main
// serves, and a handful of helpers for the requests every flow makes. Each
// concern then has a file of its own — the auth flow, the API's lifecycle, the
// notifications, the import job, the shapes. monitor_test.go is the one file
// here with no build tag, so `go test ./...` runs it and nothing else.
//
// A folder of its own rather than the example root, which is where the rest of
// the repository keeps a docker suite: these files build the server through
// internal/app, and no test can import a `main`. The example root is the
// application.
package integration

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/examples/linearlite/internal/generated/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/services/outbox"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/serve"
)

// electricURL is where the servers this suite builds forward a subscription.
//
// The env var is this suite's knob, and the tests that care set it: one points
// at a closed port to prove the fallback, another at a container it started.
// `make examples` sets nothing, which is what makes the live half skip rather
// than fail, and rig.yaml's default is then what a developer who ran `rig db
// up` in this directory already has.
func electricURL() string {
	return cmp.Or(os.Getenv("ELECTRIC_URL"), api.DefaultElectricURL)
}

type server struct {
	pool *pgxpool.Pool
	http *httptest.Server
	// engine is this server's own, kept so a test can run a dispatch pass
	// against the sender this server registered. dispatchNotifications builds
	// a second API and therefore a second outbox — which is right for cron and
	// wrong for asserting on what this server sent.
	engine *notify.Engine
}

func newServer(t *testing.T) *server {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55444/rig?sslmode=disable"
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

	// The same function main uses, so what the tests drive is what ships. No
	// monitoring page: mounting one would need a password in the environment
	// and a span file to read, and what it serves is covered on its own in
	// monitor_test.go without a database.
	//
	// An App of this test's own rather than nil: the shape routes register
	// their drain on it, which is the thing this server cannot end without.
	// Everything else rig would put on one belongs to a process that is
	// listening, and this one is not.
	lifecycle := &serve.App{Pool: pool, Logger: slog.Default()}
	parts, err := app.New(ctx, app.Config{
		Pool:        pool,
		Logger:      slog.Default(),
		App:         lifecycle,
		ElectricURL: electricURL(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(parts.Handler)
	t.Cleanup(srv.Close)

	// The shape proxy holds connections to the sync service and a poll may be
	// open when a test ends. Draining it is what a shutdown does, and running
	// the app's own drain is how this test does exactly that rather than
	// something that resembles it.
	//
	// Registered after srv.Close so that it runs before it: cleanups run in
	// reverse, and httptest.Server.Close blocks until every outstanding request
	// has finished — including the poll that only this can release. The other
	// order is the two of them waiting for each other.
	t.Cleanup(func() { _ = lifecycle.RunDrain(context.Background()) })

	return &server{pool: pool, http: srv, engine: parts.Engine}
}

// dispatch runs one whole notification pass on this server's engine: resolve,
// which turns notifications into inbox lines and the delivery rows a channel
// owes, and then dispatch, which is what actually calls a sender.
//
// Both halves, because they are two different guarantees and a test that ran
// only the first would assert that a copy was owed rather than that it was
// sent. This server's engine rather than dispatchNotifications, which builds a
// second API and therefore a second outbox — right for cron, wrong here.
func (s *server) dispatch(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.engine.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.engine.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
}

// outbox reads /_demo/outbox as the front end does.
func (s *server) outbox(t *testing.T, token string) []outbox.Message {
	t.Helper()
	res := s.do(t, request{method: http.MethodGet, path: "/_demo/outbox", token: token})
	if res.status != http.StatusOK {
		t.Fatalf("outbox: %d %s", res.status, res.body)
	}
	var out []outbox.Message
	res.decode(t, &out)
	return out
}

// seed runs the example's own seed task and answers the tenant it made.
func (s *server) seed(t *testing.T) uuid.UUID {
	t.Helper()
	if err := app.Seed(context.Background(), s.pool); err != nil {
		t.Fatal(err)
	}
	return uuid.MustParse(app.SeedTenantID)
}

// login signs a seeded person in and answers their access token.
func (s *server) login(t *testing.T, email string) string {
	t.Helper()
	res := s.do(t, request{
		method: http.MethodPost, path: "/auth/login",
		body: map[string]any{"emailAddress": email, "password": app.SeedPassword},
	})
	if res.status != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, res.status, res.body)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	res.decode(t, &out)
	if out.AccessToken == "" {
		t.Fatalf("login %s answered no session: %s", email, res.body)
	}
	return out.AccessToken
}

// accountID answers who an address is in the seeded tenant.
func (s *server) accountID(t *testing.T, tenant uuid.UUID, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM rig_account WHERE tenant_id = $1 AND lower(email_address) = lower($2) AND deleted_at IS NULL`,
		tenant, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type request struct {
	method string
	path   string
	token  string
	body   any
	// raw overrides body with bytes already encoded, for multipart.
	raw         []byte
	contentType string
	// headers are set on the way out, for the handful of tests that assert
	// something about a header the server reads rather than one it writes.
	headers map[string]string
}

type response struct {
	status  int
	body    string
	headers http.Header
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), into); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (s *server) do(t *testing.T, in request) response {
	t.Helper()

	var payload []byte
	contentType := "application/json"
	switch {
	case in.raw != nil:
		payload, contentType = in.raw, in.contentType
	case in.body != nil:
		raw, err := json.Marshal(in.body)
		if err != nil {
			t.Fatal(err)
		}
		payload = raw
	}

	req, err := http.NewRequest(in.method, s.http.URL+in.path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if in.token != "" {
		req.Header.Set("Authorization", "Bearer "+in.token)
	}
	for k, v := range in.headers {
		req.Header.Set(k, v)
	}

	res, err := s.http.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", in.method, in.path, err)
	}
	defer res.Body.Close()

	var got bytes.Buffer
	if _, err := got.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	return response{status: res.StatusCode, body: got.String(), headers: res.Header}
}
