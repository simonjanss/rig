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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

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

	if err := fn(context.WithValue(ctx, txKey{}, tx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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
