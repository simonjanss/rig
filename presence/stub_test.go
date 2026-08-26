package presence_test

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// stubDB satisfies presence.DB and reaches no database.
//
// It exists so the pure tests can construct a Service: NewService requires a
// pool because a Service with none would be one whose every method failed at the
// first call, and the tests here never make one.
type stubDB struct{}

var errStub = errors.New("presence: the stub database was asked for something")

func (stubDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errStub }
func (stubDB) QueryRow(context.Context, string, ...any) pgx.Row        { return stubRow{} }
func (stubDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errStub
}

type stubRow struct{}

func (stubRow) Scan(...any) error { return errStub }
