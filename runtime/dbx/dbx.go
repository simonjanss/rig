// Package dbx is the thin layer between generated repositories and pgx.
//
// It exists for one reason: a repository method has to work the same whether it
// was called directly or inside a transaction someone else opened. Taking a
// narrow interface rather than a concrete pool is what makes an Update able to
// write a snapshot and the row itself atomically without the caller arranging
// anything.
package dbx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// UTC is how a scanned instant reaches the rest of the program.
//
// A timestamptz column holds no zone — it is microseconds since the epoch, an
// instant — but pgx decodes one into [time.Local], which is the zone of the
// process rather than of the database or of the row. The instant is right either
// way, so a comparison is safe; what changes is everything that reads the clock
// off it. Format, Day, and JSON all follow the location, so the same row is
// rendered "2026-03-02T00:30:00+01:00" on a laptop in Stockholm and
// "2026-03-01T23:30:00Z" in a container with no TZ set.
//
// Both are the same moment and neither is wrong, which is exactly why this is
// worth pinning: a response that changes shape with the host's environment
// breaks golden files, makes two replicas disagree in their logs, and invites
// somebody to compare the strings.
//
// Generated repositories call this on every timestamp they scan, so the answer
// does not depend on how the pool was built. Pinning `TimeZone=UTC` in the
// connection string is still worth doing, and does a different job: it settles
// what `::date` and `date_trunc` mean inside SQL, which no amount of care on this
// side of the wire can affect.
func UTC(t time.Time) time.Time { return t.UTC() }

// UTCPtr is [UTC] for a nullable column. Nil stays nil: a row with no deleted_at
// has not been deleted, and inventing a zero time would say it was.
func UTCPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	in := t.UTC()
	return &in
}

// UTCSlice is [UTC] for a timestamptz[] column.
//
// It exists so the rule has no exceptions. pgx decodes the elements of an array
// the same way it decodes a scalar, so without this a column of instants would
// come back in the host's zone while every instant beside it came back in UTC —
// and an invariant with one silent exception is worse than none, because nobody
// checks the case they were told was handled.
//
// The slice is modified in place and returned: it was just built by the scan and
// is not shared with anything. Nil stays nil, which is how a null array arrives.
func UTCSlice(ts []time.Time) []time.Time {
	for i := range ts {
		ts[i] = ts[i].UTC()
	}
	return ts
}

// Conn is what a repository needs from a database. A pool, a connection, and a
// transaction all satisfy it.
type Conn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Beginner can start a transaction. A pool satisfies it; a transaction does
// too, through savepoints.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type txKey struct{}

type commitKey struct{}

// pending collects what should run once the outermost transaction commits.
type pending struct{ fns []func() }

// AfterCommit runs fn once the transaction this context belongs to has
// committed, or immediately when there is no transaction.
//
// The distinction matters for anything outside the database: an email sent from
// inside a transaction that later rolls back has told somebody about a change
// that did not happen. Registering the work here moves it past the commit
// without the caller having to know whether it is the one that opened the
// transaction — and when a repository call is nested inside a larger unit of
// work, "committed" correctly means the outer one.
//
// A panic in fn is recovered. The write has already landed, and unwinding a
// request that succeeded would report a failure that did not occur.
func AfterCommit(ctx context.Context, fn func()) {
	if fn == nil {
		return
	}
	if p, ok := ctx.Value(commitKey{}).(*pending); ok {
		p.fns = append(p.fns, fn)
		return
	}
	recovered(fn)
}

func recovered(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// InTx runs fn inside a transaction, reusing one already on the context.
//
// Reuse is what makes nesting safe. A repository method that opens its own
// transaction unconditionally cannot be composed: two of them in one unit of
// work would commit independently, and the second failing would leave the first
// applied.
func InTx(ctx context.Context, db Beginner, fn func(ctx context.Context, tx Conn) error) error {
	if existing, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx, existing)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Rollback after a successful commit is a no-op, so the deferred call
	// covers a panic without interfering with the happy path.
	defer func() { _ = tx.Rollback(ctx) }()

	p := &pending{}
	inner := context.WithValue(context.WithValue(ctx, txKey{}, tx), commitKey{}, p)

	if err := fn(inner, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Only now, and only for the transaction that actually committed.
	for _, done := range p.fns {
		recovered(done)
	}
	return nil
}

// InTxIf runs fn inside a transaction only when one is called for.
//
// A single statement is already atomic, and wrapping it in BEGIN and COMMIT
// costs two round trips to protect nothing. What does call for a transaction is
// a hook that has to be able to undo the write — so the caller decides, and
// gets the same body either way.
//
// conn is what fn runs against when there is no transaction: the pool, or the
// transaction already on the context.
func InTxIf(ctx context.Context, db Beginner, conn Conn, need bool, fn func(ctx context.Context, tx Conn) error) error {
	if !need {
		return fn(ctx, conn)
	}
	return InTx(ctx, db, fn)
}

// Tx returns the transaction on the context, if any.
func Tx(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// IsNoRows reports whether an error means the query matched nothing.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// Postgres error codes worth distinguishing. A constraint violation is the
// caller's mistake and deserves a 409; anything else is the server's.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeNotNullViolation    = "23502"
)

// IsUniqueViolation reports whether an error is a duplicate-key failure.
func IsUniqueViolation(err error) bool { return hasCode(err, codeUniqueViolation) }

// IsForeignKeyViolation reports whether an error is a missing or still-
// referenced row.
func IsForeignKeyViolation(err error) bool { return hasCode(err, codeForeignKeyViolation) }

// IsCheckViolation reports whether an error is a failed CHECK constraint.
func IsCheckViolation(err error) bool { return hasCode(err, codeCheckViolation) }

// IsNotNullViolation reports whether an error is a missing required value.
func IsNotNullViolation(err error) bool { return hasCode(err, codeNotNullViolation) }

// ConstraintName returns the constraint an error names, or empty. It is what
// lets a handler turn "duplicate key" into a message about the field the user
// actually typed.
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
