package dbx_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
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

// The visited set is what makes a cycle in the schema terminate. A child that
// deletes its own rows by calling its own Delete triggers their children the
// same way, so depth is the call stack — and a table that reaches itself would
// exhaust it.
func TestEnterDeleteRefusesASecondVisitToTheSameRow(t *testing.T) {
	t.Parallel()

	err := dbx.InTx(t.Context(), &fakeDB{}, func(ctx context.Context, _ dbx.Conn) error {
		inner, more, err := dbx.EnterDelete(ctx, "team", "a")
		if err != nil || !more {
			t.Fatalf("the first visit should proceed: more=%v err=%v", more, err)
		}

		// A different row of the same table is a different delete.
		if _, more, _ := dbx.EnterDelete(inner, "team", "b"); !more {
			t.Error("a different row should proceed")
		}
		// The same row further down the same transaction is a no-op rather than
		// an error: the row is going, which is what the caller asked for.
		if _, more, err := dbx.EnterDelete(inner, "team", "a"); more || err != nil {
			t.Errorf("a repeat visit should stop quietly: more=%v err=%v", more, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The depth cap is the other half, and it is an error rather than a no-op:
// stopping halfway through a propagation would leave the transaction in a state
// nobody asked for.
func TestEnterDeleteStopsAtTheDepthCap(t *testing.T) {
	t.Parallel()

	err := dbx.InTx(t.Context(), &fakeDB{}, func(ctx context.Context, _ dbx.Conn) error {
		for i := range dbx.MaxCascadeDepth {
			next, more, err := dbx.EnterDelete(ctx, "team", strconv.Itoa(i))
			if err != nil || !more {
				t.Fatalf("level %d should proceed: more=%v err=%v", i, more, err)
			}
			ctx = next
		}

		_, more, err := dbx.EnterDelete(ctx, "team", "one too many")
		if err == nil {
			t.Fatal("the cap should be an error, not a silent stop")
		}
		if more {
			t.Error("a refused delete should not proceed")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Errorf("the error should name the usual cause: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Outside a transaction there is no parent to have been visited, so there is
// nothing to guard and the guard says so rather than allocating a set nobody
// reads.
func TestEnterDeleteOutsideATransactionAlwaysProceeds(t *testing.T) {
	t.Parallel()

	for range 2 {
		if _, more, err := dbx.EnterDelete(t.Context(), "team", "a"); !more || err != nil {
			t.Errorf("more=%v err=%v, want true and nil", more, err)
		}
	}
}

// The exception to reuse. A notification published on the caller's transaction
// is discarded when that transaction rolls back, so the write that must outlive
// a rollback needs a transaction that is not the caller's — and asking for one
// has to actually get one.
func TestWithoutTxOpensATransactionOfItsOwn(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	var inner, outer pgx.Tx

	err := dbx.InTx(t.Context(), db, func(ctx context.Context, tx dbx.Conn) error {
		outer, _ = dbx.Tx(ctx)

		stripped := dbx.WithoutTx(ctx)
		if _, ok := dbx.Tx(stripped); ok {
			t.Error("the transaction should not be visible on a stripped context")
		}
		return dbx.InTx(stripped, db, func(ctx context.Context, _ dbx.Conn) error {
			inner, _ = dbx.Tx(ctx)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.begins != 2 {
		t.Errorf("began %d transactions, want 2: the inner one should not have joined the outer", db.begins)
	}
	if inner == nil || inner == outer {
		t.Error("the inner work ran on the caller's transaction, which a rollback would discard")
	}

	// And a context that carries none is handed back untouched, so the helper is
	// free to sit on a path that is usually already outside one.
	ctx := t.Context()
	if dbx.WithoutTx(ctx) != ctx {
		t.Error("a context with no transaction should be returned as it is")
	}
}

// Null and Deref are the two halves of a nullable column whose Go type is not a
// pointer. The interesting case is the zero value, because that is the one a
// hand-written branch gets wrong: writing `”` where the column means "nothing"
// produces a row that compares unequal to NULL in every query looking for one.
func TestNullIsNilAtTheZeroValue(t *testing.T) {
	t.Parallel()

	if got := dbx.Null(""); got != nil {
		t.Errorf("Null(\"\") = %v, want nil", *got)
	}
	if got := dbx.Null("a"); got == nil || *got != "a" {
		t.Errorf("Null(%q) = %v", "a", got)
	}

	if got := dbx.Null(uuid.Nil); got != nil {
		t.Errorf("Null(uuid.Nil) = %v, want nil", *got)
	}
	id := uuid.New()
	if got := dbx.Null(id); got == nil || *got != id {
		t.Errorf("Null(%v) = %v", id, got)
	}

	// A non-string, non-uuid zero, because the constraint is comparable rather
	// than a fixed list of types.
	if got := dbx.Null(0); got != nil {
		t.Errorf("Null(0) = %v, want nil", *got)
	}
}

func TestDerefIsTheZeroValueForNil(t *testing.T) {
	t.Parallel()

	if got := dbx.Deref[string](nil); got != "" {
		t.Errorf("Deref[string](nil) = %q", got)
	}
	s := "a"
	if got := dbx.Deref(&s); got != "a" {
		t.Errorf("Deref(&%q) = %q", s, got)
	}
	if got := dbx.Deref[uuid.UUID](nil); got != uuid.Nil {
		t.Errorf("Deref[uuid.UUID](nil) = %v", got)
	}
}

// Round-tripping is the property the call sites rely on: what Null wrote, Deref
// reads back, including for the value that became NULL on the way out.
func TestNullAndDerefRoundTrip(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"", "a", "a longer one"} {
		if got := dbx.Deref(dbx.Null(v)); got != v {
			t.Errorf("Deref(Null(%q)) = %q", v, got)
		}
	}
	for _, v := range []uuid.UUID{uuid.Nil, uuid.New()} {
		if got := dbx.Deref(dbx.Null(v)); got != v {
			t.Errorf("Deref(Null(%v)) = %v", v, got)
		}
	}
}

// ConnFor is the transaction on the context, or the fallback. With no
// transaction it is the fallback, which is the ordinary case.
func TestConnForFallsBackToThePool(t *testing.T) {
	t.Parallel()

	var pool dbx.Conn = fallbackConn{}
	if got := dbx.ConnFor(context.Background(), pool); got != pool {
		t.Errorf("ConnFor with no transaction on the context = %v, want the fallback", got)
	}
}

// fallbackConn stands in for a pool. Nothing runs a statement on it.
type fallbackConn struct{}

func (fallbackConn) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errNotUsed
}
func (fallbackConn) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (fallbackConn) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errNotUsed
}

var errNotUsed = errors.New("dbx_test: the fallback was asked for something")
