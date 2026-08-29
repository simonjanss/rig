package authpg

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
)

// The tenant predicate is not optional and not overridable. Everything about
// this reader's safety rests on it, and it is one line of rendering away from
// being absent, so it gets a test of its own rather than being asserted in
// passing by the Docker suite.
func TestTheTenantIsAlwaysTheFirstPredicate(t *testing.T) {
	t.Parallel()

	tenant := uuid.New()
	where, args := logFilters(authlog.Query{TenantID: tenant})

	if len(where) != 1 || where[0] != "tenant_id = $1" {
		t.Fatalf("where = %v, want just the tenant", where)
	}
	if len(args) != 1 || args[0] != tenant {
		t.Fatalf("args = %v, want just the tenant", args)
	}
}

func TestEveryFilterIsAPlaceholder(t *testing.T) {
	t.Parallel()

	account := uuid.New()
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)

	where, args := logFilters(authlog.Query{
		TenantID:  uuid.New(),
		AccountID: &account,
		Event:     authlog.EventLoginFailed,
		Outcome:   authlog.Failed,
		Since:     since,
		Until:     until,
	})

	want := "tenant_id = $1 AND account_id = $2 AND event = $3 AND outcome = $4 " +
		"AND created_at >= $5 AND created_at < $6"
	if got := strings.Join(where, " AND "); got != want {
		t.Errorf("where =\n%s\nwant\n%s", got, want)
	}
	if len(args) != 6 {
		t.Fatalf("got %d arguments for six predicates", len(args))
	}
	// The event and the outcome reach the database as strings, because both
	// columns are Postgres enums and neither pgx nor the enum accepts a Go named
	// type it has never heard of.
	if _, ok := args[2].(string); !ok {
		t.Errorf("event argument is %T, want string", args[2])
	}
	if _, ok := args[3].(string); !ok {
		t.Errorf("outcome argument is %T, want string", args[3])
	}
}

// A window with one end open is the ordinary case — "since Monday" with no
// upper bound — and it must not render an empty comparison.
func TestOneEndedWindows(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	where, args := logFilters(authlog.Query{TenantID: uuid.New(), Since: since})
	if got := strings.Join(where, " AND "); got != "tenant_id = $1 AND created_at >= $2" {
		t.Errorf("where = %s", got)
	}
	if len(args) != 2 {
		t.Errorf("got %d arguments, want 2", len(args))
	}

	where, _ = logFilters(authlog.Query{TenantID: uuid.New(), Until: since})
	if got := strings.Join(where, " AND "); got != "tenant_id = $1 AND created_at < $2" {
		t.Errorf("where = %s", got)
	}
}
