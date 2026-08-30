// Package introspect reads a live Postgres schema into rig's intermediate
// representation.
//
// This is the only impure stage of the pipeline. Everything downstream is a
// pure function of what comes out of here, which is why [Read] returns a value
// rather than holding a connection: the rest of the compiler runs from a JSON
// dump with no database in sight.
//
// Queries go to pg_catalog rather than information_schema. The catalog is less
// portable and less pleasant to read, but it exposes things the standard views
// hide — index methods, partial predicates, generated-column kinds, enum sort
// order — and rig needs all of them.
package introspect

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/simonjanss/rig/pkg/ir"
)

// Querier is the subset of pgx a read needs. Taking an interface lets a caller
// pass a pool, a connection, or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Options tune a read.
type Options struct {
	// Schema is the Postgres schema to read. Defaults to public.
	Schema string
	// IncludeViews reads views and materialized views alongside base tables.
	IncludeViews bool
}

// Read introspects a schema.
//
// The result is unsorted and unlinked: it says what the database contains and
// nothing more. Canonicalizing it — ordering, resolving enum types onto columns,
// classifying join tables — is [compile.Normalize]'s job, so that the same
// normalization applies whether the schema came from a database or from a file.
func Read(ctx context.Context, q Querier, opt Options) (ir.Schema, error) {
	schemaName := opt.Schema
	if schemaName == "" {
		schemaName = "public"
	}

	out := ir.Schema{Name: schemaName}

	enums, err := readEnums(ctx, q, schemaName)
	if err != nil {
		return ir.Schema{}, fmt.Errorf("read enums: %w", err)
	}
	out.Enums = enums

	tables, err := readTables(ctx, q, schemaName, opt.IncludeViews)
	if err != nil {
		return ir.Schema{}, fmt.Errorf("read tables: %w", err)
	}

	byName := make(map[string]*ir.Table, len(tables))
	for i := range tables {
		byName[tables[i].Name] = &tables[i]
	}

	// Replication comes before the empty-schema shortcut on purpose: it is the
	// one thing here that describes the server rather than the tables, and a
	// caller distinguishes "no database was read" from "read, and nothing is
	// published" by whether it is nil.
	replication, err := readReplication(ctx, q, schemaName, byName)
	if err != nil {
		return ir.Schema{}, fmt.Errorf("read replication: %w", err)
	}
	out.Replication = replication

	if len(tables) == 0 {
		return out, nil
	}

	if err := readColumns(ctx, q, schemaName, byName); err != nil {
		return ir.Schema{}, fmt.Errorf("read columns: %w", err)
	}
	if err := readConstraints(ctx, q, schemaName, byName); err != nil {
		return ir.Schema{}, fmt.Errorf("read constraints: %w", err)
	}
	if err := readIndexes(ctx, q, schemaName, byName); err != nil {
		return ir.Schema{}, fmt.Errorf("read indexes: %w", err)
	}

	out.Tables = tables
	return out, nil
}
