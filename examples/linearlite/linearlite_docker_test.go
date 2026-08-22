//go:build docker

// The example's docker suite shares one harness: a real database, the same
// newAPI main serves, and a handful of helpers for the requests every flow
// makes. Each concern then has a file of its own — the auth flow, the API's
// lifecycle, the notifications, the import job, the shapes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type server struct {
	pool *pgxpool.Pool
	http *httptest.Server
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

	// The same function main uses, so what the tests drive is what ships.
	handler, _, err := newAPI(ctx, pool, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &server{pool: pool, http: srv}
}

// seed runs the example's own seed task and answers the tenant it made.
func (s *server) seed(t *testing.T) uuid.UUID {
	t.Helper()
	if err := seed(context.Background(), s.pool); err != nil {
		t.Fatal(err)
	}
	return uuid.MustParse(SeedTenantID)
}

// login signs a seeded person in and answers their access token.
func (s *server) login(t *testing.T, email string) string {
	t.Helper()
	res := s.do(t, request{
		method: http.MethodPost, path: "/auth/login",
		body: map[string]any{"emailAddress": email, "password": SeedPassword},
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
