// Package audit records what changed.
//
// This is separate from the audit columns on a row. Those say who last touched
// it; this says what they did, field by field, and survives the next update.
package audit

import (
	"context"

	"github.com/google/uuid"
)

// Operation is what happened to a row.
type Operation string

const (
	OperationCreate  Operation = "Create"
	OperationUpdate  Operation = "Update"
	OperationDelete  Operation = "Delete"
	OperationRestore Operation = "Restore"
)

// Value is one column's change.
type Value struct {
	Column string
	// Type is the column's IR type name, so a reader can interpret the values
	// without consulting the schema.
	Type string
	// Old and New are rendered forms. They are strings rather than `any`
	// because an audit row outlives the Go type that produced it, and a value
	// nobody can read back is not a record of anything.
	Old *string
	New *string
}

// Entry is one recorded change.
type Entry struct {
	TenantID  uuid.UUID
	AccountID *uuid.UUID
	Operation Operation
	// Entity is the table name and the row's identifier.
	Entity   string
	EntityID uuid.UUID
	Values   []Value
}

// Log records changes. An application supplies its own implementation; the
// generated foundation provides one that writes to the audit tables.
type Log interface {
	Record(ctx context.Context, e Entry) error
}

// Noop discards everything. It is the default, so a project that has not set up
// audit storage still runs.
type Noop struct{}

// Record implements [Log].
func (Noop) Record(context.Context, Entry) error { return nil }

type ignoreKey struct{}

// Ignore returns a context in which nothing is recorded.
//
// This is for rig's own bookkeeping — the snapshot a change writes, the field
// clearing a restore performs — where an entry would describe machinery rather
// than intent. A restore should read as one Restore, not as an Update diff
// nobody performed.
func Ignore(ctx context.Context) context.Context {
	return context.WithValue(ctx, ignoreKey{}, true)
}

// Ignored reports whether recording is suppressed.
func Ignored(ctx context.Context) bool {
	ignored, _ := ctx.Value(ignoreKey{}).(bool)
	return ignored
}

// Record writes an entry unless the context suppresses it, and never fails the
// operation it describes.
//
// A failure to write history is worth knowing about, but not worth rolling back
// the change that succeeded. The error is returned so a caller can log it.
func Record(ctx context.Context, log Log, e Entry) error {
	if log == nil || Ignored(ctx) {
		return nil
	}
	return log.Record(ctx, e)
}
