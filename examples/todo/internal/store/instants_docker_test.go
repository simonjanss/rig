//go:build docker

package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/model"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// A timestamp comes back in UTC whatever the session and whatever the host.
//
// pgx decodes a timestamptz into time.Local, so without the repository settling
// it the location on a scanned time is the machine's. The instant is right either
// way — which is exactly why this is easy to miss: everything compares equal, and
// then the API's JSON carries +01:00 on a laptop and Z in a container, and a
// golden file or a log comparison starts depending on where it ran.
func TestAScannedInstantIsUTC(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55440/rig?sslmode=disable"
	}
	// A session in a zone that is not UTC, and one whose offset is not zero at
	// any time of year, so a wrong location cannot pass by coincidence.
	dsn += "&TimeZone=Asia/Kolkata"

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

	var zone string
	if err := pool.QueryRow(ctx, "SHOW TimeZone").Scan(&zone); err != nil {
		t.Fatal(err)
	}
	if zone == "UTC" {
		t.Fatal("this test is only meaningful with a session that is not in UTC")
	}

	repo := store.New(pool, store.Config{}).Todos
	claims := tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}
	ctx = tenancy.NewContext(ctx, claims)

	due := time.Date(2026, 3, 1, 23, 30, 0, 0, time.UTC)
	made, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
		Input: model.TodoCreateInput{Title: "When did this happen", DueAt: &due},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		got  time.Time
	}{
		{"created_at", made.CreatedAt},
		{"due_at", *made.DueAt},
	} {
		if tc.got.Location() != time.UTC {
			t.Errorf("%s came back in %s, want UTC", tc.name, tc.got.Location())
		}
	}

	// And the instant survived the round trip, which is the part that would
	// matter if the normalization were doing something more than relabelling.
	if !made.DueAt.Equal(due) {
		t.Errorf("due_at = %v, want the instant that went in (%v)", made.DueAt, due)
	}

	// A read is the same, because it goes through the same scan.
	read, err := repo.Get(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.CreatedAt.Location() != time.UTC {
		t.Errorf("a read came back in %s, want UTC", read.CreatedAt.Location())
	}
	if !read.CreatedAt.Equal(made.CreatedAt) {
		t.Error("the write and the read should describe the same instant")
	}
}
