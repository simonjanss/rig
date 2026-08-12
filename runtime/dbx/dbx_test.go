package dbx_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/simonjanss/rig/runtime/dbx"
)

func TestAfterCommitRunsWhenThereIsNoTransaction(t *testing.T) {
	t.Parallel()

	ran := false
	dbx.AfterCommit(t.Context(), func() { ran = true })

	if !ran {
		t.Error("with nothing to wait for, the work should just happen")
	}
}

func TestAfterCommitWaitsForTheCommit(t *testing.T) {
	t.Parallel()

	var order []string
	db := &fakeDB{onCommit: func() { order = append(order, "commit") }}

	err := dbx.InTx(t.Context(), db, func(ctx context.Context, _ dbx.Conn) error {
		dbx.AfterCommit(ctx, func() { order = append(order, "hook") })
		order = append(order, "write")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The hook exists to tell something outside the database. Running it before
	// the commit would announce a change that a failed commit then undoes.
	want := []string{"write", "commit", "hook"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// The nested call does not commit — the outer one does — so its hook has to
// wait for that, not for its own return.
func TestAfterCommitWaitsForTheOutermostCommit(t *testing.T) {
	t.Parallel()

	var order []string
	db := &fakeDB{onCommit: func() { order = append(order, "commit") }}

	err := dbx.InTx(t.Context(), db, func(outer context.Context, _ dbx.Conn) error {
		return dbx.InTx(outer, db, func(inner context.Context, _ dbx.Conn) error {
			dbx.AfterCommit(inner, func() { order = append(order, "hook") })
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(order) != 2 || order[0] != "commit" || order[1] != "hook" {
		t.Errorf("order = %v, want [commit hook]", order)
	}
	if db.begins != 1 {
		t.Errorf("the nested call should join the transaction, not open one: %d begins", db.begins)
	}
}

func TestAfterCommitDoesNotRunWhenTheWriteFails(t *testing.T) {
	t.Parallel()

	ran := false
	db := &fakeDB{}
	boom := errors.New("no")

	err := dbx.InTx(t.Context(), db, func(ctx context.Context, _ dbx.Conn) error {
		dbx.AfterCommit(ctx, func() { ran = true })
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if ran {
		t.Error("nothing landed, so nothing should have been announced")
	}
	if db.commits != 0 {
		t.Error("a failed unit of work should not commit")
	}
}

// The write has already landed by the time these run. Letting a panic out
// would report a failure that did not happen.
func TestAPanickingHookIsContained(t *testing.T) {
	t.Parallel()

	ran := false
	db := &fakeDB{}

	err := dbx.InTx(t.Context(), db, func(ctx context.Context, _ dbx.Conn) error {
		dbx.AfterCommit(ctx, func() { panic("hook") })
		dbx.AfterCommit(ctx, func() { ran = true })
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ran {
		t.Error("one hook panicking should not stop the next")
	}
}

// fakeDB is enough of a pool to drive InTx without a database.
type fakeDB struct {
	begins   int
	commits  int
	onCommit func()
}

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	d.begins++
	return &fakeTx{db: d}, nil
}

type fakeTx struct {
	db *fakeDB
	pgx.Tx
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.db.commits++
	if tx.db.onCommit != nil {
		tx.db.onCommit()
	}
	return nil
}

func (tx *fakeTx) Rollback(context.Context) error { return nil }

func (tx *fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// The constraint predicates are what turn a driver error into a status a
// client can act on. Getting one wrong means a duplicate key answered as 500.
func TestTheConstraintPredicatesReadTheCode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		code  string
		is    func(error) bool
		named string
	}{
		{"23505", dbx.IsUniqueViolation, "IsUniqueViolation"},
		{"23503", dbx.IsForeignKeyViolation, "IsForeignKeyViolation"},
		{"23514", dbx.IsCheckViolation, "IsCheckViolation"},
		{"23502", dbx.IsNotNullViolation, "IsNotNullViolation"},
	} {
		err := &pgconn.PgError{Code: tc.code, ConstraintName: "lesson_title_key"}

		if !tc.is(err) {
			t.Errorf("%s should recognise %s", tc.named, tc.code)
		}
		// Still recognised once a repository has wrapped it.
		if !tc.is(fmt.Errorf("insert lesson: %w", err)) {
			t.Errorf("%s should see through a wrap", tc.named)
		}
		// And not confused with its neighbours.
		if tc.is(&pgconn.PgError{Code: "42P01"}) {
			t.Errorf("%s matched an unrelated code", tc.named)
		}
		if tc.is(errors.New("not from the driver")) {
			t.Errorf("%s matched an ordinary error", tc.named)
		}
		if tc.is(nil) {
			t.Errorf("%s matched nothing at all", tc.named)
		}
	}
}

// The constraint name is what turns "duplicate key" into a message about the
// field somebody actually typed.
func TestConstraintName(t *testing.T) {
	t.Parallel()

	err := &pgconn.PgError{Code: "23505", ConstraintName: "lesson_title_key"}

	if got := dbx.ConstraintName(fmt.Errorf("insert: %w", err)); got != "lesson_title_key" {
		t.Errorf("ConstraintName = %q", got)
	}
	if got := dbx.ConstraintName(errors.New("not from the driver")); got != "" {
		t.Errorf("ConstraintName = %q, want empty", got)
	}
}

// A read that matched nothing is an ordinary answer, and every generated Get
// branches on it to produce a 404 rather than a 500.
func TestIsNoRows(t *testing.T) {
	t.Parallel()

	if !dbx.IsNoRows(fmt.Errorf("scan lesson: %w", pgx.ErrNoRows)) {
		t.Error("a wrapped ErrNoRows is still no rows")
	}
	if dbx.IsNoRows(errors.New("connection refused")) {
		t.Error("a connection failure is not an empty result")
	}
}

// A single statement is already atomic. Opening a transaction around one costs
// two round trips to protect nothing, so the caller says when it is needed.
func TestInTxIfOnlyOpensOneWhenAsked(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	conn := &fakeTx{db: db}

	var gotWithout dbx.Conn
	if err := dbx.InTxIf(t.Context(), db, conn, false, func(_ context.Context, c dbx.Conn) error {
		gotWithout = c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if db.begins != 0 {
		t.Errorf("began %d transactions for work that needed none", db.begins)
	}
	if gotWithout != dbx.Conn(conn) {
		t.Error("without a transaction the work should run against the connection it was given")
	}

	if err := dbx.InTxIf(t.Context(), db, conn, true, func(context.Context, dbx.Conn) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if db.begins != 1 || db.commits != 1 {
		t.Errorf("begins = %d, commits = %d, want one of each", db.begins, db.commits)
	}
}

// A repository method has to know whether it is already inside somebody's
// transaction, because that is what decides between joining and beginning.
func TestTxReportsWhatIsOnTheContext(t *testing.T) {
	t.Parallel()

	if _, ok := dbx.Tx(t.Context()); ok {
		t.Error("a bare context carries no transaction")
	}

	db := &fakeDB{}
	err := dbx.InTx(t.Context(), db, func(ctx context.Context, _ dbx.Conn) error {
		if _, ok := dbx.Tx(ctx); !ok {
			t.Error("inside a transaction the context should carry it")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
