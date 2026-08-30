package introspect

import (
	"context"

	"github.com/simonjanss/rig/pkg/ir"
)

// enumQuery reads every enum type and its labels.
//
// Ordering by enumsortorder rather than by label matters: it is the order the
// values were declared in, which is the order they will appear in generated
// code and documentation. Alphabetizing them would turn a deliberate sequence
// like planned/in_progress/completed into nonsense.
const enumQuery = `
SELECT
    t.typname                                  AS name,
    coalesce(obj_description(t.oid, 'pg_type'), '') AS comment,
    e.enumlabel                                AS value
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
JOIN pg_enum e      ON e.enumtypid = t.oid
WHERE t.typtype = 'e'
  AND n.nspname = $1
ORDER BY t.typname, e.enumsortorder
`

func readEnums(ctx context.Context, q Querier, schema string) ([]ir.PgEnum, error) {
	rows, err := q.Query(ctx, enumQuery, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out   []ir.PgEnum
		index = map[string]int{}
	)
	for rows.Next() {
		var name, comment, value string
		if err := rows.Scan(&name, &comment, &value); err != nil {
			return nil, err
		}

		i, seen := index[name]
		if !seen {
			out = append(out, ir.PgEnum{Name: name, Comment: comment})
			i = len(out) - 1
			index[name] = i
		}
		out[i].Values = append(out[i].Values, ir.PgEnumValue{Value: value})
	}
	return out, rows.Err()
}

// tableQuery reads relations. relkind 'r' is an ordinary table, 'p' a
// partitioned one, 'v' a view, and 'm' a materialized view.
const tableQuery = `
SELECT
    c.relname                                       AS name,
    c.relkind                                       AS kind,
    c.relpersistence = 'u'                          AS unlogged,
    coalesce(obj_description(c.oid, 'pg_class'), '') AS comment
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1
  AND c.relkind = ANY($2)
ORDER BY c.relname
`

func readTables(ctx context.Context, q Querier, schema string, includeViews bool) ([]ir.Table, error) {
	kinds := []string{"r", "p"}
	if includeViews {
		kinds = append(kinds, "v", "m")
	}

	rows, err := q.Query(ctx, tableQuery, schema, kinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ir.Table
	for rows.Next() {
		var (
			name, kind, comment string
			unlogged            bool
		)
		if err := rows.Scan(&name, &kind, &unlogged, &comment); err != nil {
			return nil, err
		}
		out = append(out, ir.Table{
			Name:     name,
			Kind:     tableKind(kind),
			Comment:  comment,
			Unlogged: unlogged,
		})
	}
	return out, rows.Err()
}

func tableKind(relkind string) ir.TableKind {
	switch relkind {
	case "v":
		return ir.TableKindView
	case "m":
		return ir.TableKindMaterializedView
	default:
		return ir.TableKindBase
	}
}

// columnQuery reads columns.
//
// format_type gives the type as a human would write it — "character varying(64)"
// rather than an oid — while typname gives the underlying name, which is what
// identifies an enum or an array element. rig needs both.
const columnQuery = `
SELECT
    c.relname                                          AS table_name,
    a.attname                                          AS name,
    format_type(a.atttypid, a.atttypmod)               AS sql_type,
    bt.typname                                         AS udt_name,
    NOT a.attnotnull                                   AS nullable,
    a.atthasdef                                        AS has_default,
    coalesce(pg_get_expr(d.adbin, d.adrelid), '')      AS default_expr,
    a.attidentity <> ''                                AS is_identity,
    a.attgenerated <> ''                               AS is_generated,
    a.attnum                                           AS ordinal,
    coalesce(col_description(c.oid, a.attnum), '')     AS comment,
    coalesce(information_schema._pg_char_max_length(a.atttypid, a.atttypmod), 0) AS char_max_length,
    coalesce(information_schema._pg_numeric_precision(a.atttypid, a.atttypmod), 0) AS numeric_precision,
    coalesce(information_schema._pg_numeric_scale(a.atttypid, a.atttypmod), 0)     AS numeric_scale
FROM pg_attribute a
JOIN pg_class c        ON c.oid = a.attrelid
JOIN pg_namespace n    ON n.oid = c.relnamespace
JOIN pg_type bt        ON bt.oid = a.atttypid
LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
WHERE n.nspname = $1
  AND a.attnum > 0
  AND NOT a.attisdropped
  AND c.relkind = ANY($2)
ORDER BY c.relname, a.attnum
`

func readColumns(ctx context.Context, q Querier, schema string, tables map[string]*ir.Table) error {
	rows, err := q.Query(ctx, columnQuery, schema, []string{"r", "p", "v", "m"})
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tableName string
			col       ir.Column
			hasDef    bool
			defExpr   string
		)
		if err := rows.Scan(
			&tableName, &col.Name, &col.SQLType, &col.UDTName,
			&col.Nullable, &hasDef, &defExpr,
			&col.Identity, &col.Generated, &col.Ordinal, &col.Comment,
			&col.CharMaxLength, &col.NumericPrec, &col.NumericScale,
		); err != nil {
			return err
		}

		t, ok := tables[tableName]
		if !ok {
			continue
		}

		// A generated column's "default" is its expression, which is not a
		// default at all: nobody can supply a value for it. Keeping the
		// distinction here stops downstream code from offering it as writable.
		if hasDef && !col.Generated {
			col.HasDefault = true
			col.Default = defExpr
		}
		if col.Comment != "" {
			col.CommentSource = ir.CommentSourceDatabase
		}

		t.Columns = append(t.Columns, col)
	}
	return rows.Err()
}

// constraintQuery reads primary keys, foreign keys, unique constraints, and
// check constraints in one pass.
//
// conkey and confkey are attribute-number arrays, so the column names are
// looked up positionally — and the position matters: it is the key's column
// order, which determines whether an index can serve a lookup.
const constraintQuery = `
SELECT
    c.relname AS table_name,
    con.conname AS name,
    con.contype AS kind,
    coalesce((
        SELECT array_agg(a.attname ORDER BY k.ord)
        FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
        JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
    ), '{}') AS columns,
    coalesce(fc.relname, '') AS foreign_table,
    coalesce((
        SELECT array_agg(a.attname ORDER BY k.ord)
        FROM unnest(con.confkey) WITH ORDINALITY AS k(attnum, ord)
        JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.attnum
    ), '{}') AS foreign_columns,
    con.confupdtype AS on_update,
    con.confdeltype AS on_delete,
    coalesce(pg_get_constraintdef(con.oid), '') AS definition
FROM pg_constraint con
JOIN pg_class c            ON c.oid = con.conrelid
JOIN pg_namespace n        ON n.oid = c.relnamespace
LEFT JOIN pg_class fc      ON fc.oid = con.confrelid
WHERE n.nspname = $1
  AND con.contype IN ('p', 'f', 'u', 'c')
ORDER BY c.relname, con.conname
`

func readConstraints(ctx context.Context, q Querier, schema string, tables map[string]*ir.Table) error {
	rows, err := q.Query(ctx, constraintQuery, schema)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tableName, name string
			kind            uint8
			columns         []string
			foreignTable    string
			foreignColumns  []string
			onUpdate        uint8
			onDelete        uint8
			definition      string
		)
		if err := rows.Scan(&tableName, &name, &kind, &columns,
			&foreignTable, &foreignColumns, &onUpdate, &onDelete, &definition); err != nil {
			return err
		}

		t, ok := tables[tableName]
		if !ok {
			continue
		}

		switch kind {
		case 'p':
			t.PrimaryKey = columns
		case 'u':
			t.Uniques = append(t.Uniques, columns)
		case 'c':
			t.Checks = append(t.Checks, ir.Check{Name: name, Expression: definition})
		case 'f':
			t.ForeignKeys = append(t.ForeignKeys, ir.ForeignKey{
				Name:           name,
				Columns:        columns,
				ForeignTable:   foreignTable,
				ForeignColumns: foreignColumns,
				OnUpdate:       referentialAction(onUpdate),
				OnDelete:       referentialAction(onDelete),
			})
		}
	}
	return rows.Err()
}

// referentialAction decodes the single-character action code Postgres stores.
func referentialAction(code uint8) string {
	switch code {
	case 'a':
		return "NO ACTION"
	case 'r':
		return "RESTRICT"
	case 'c':
		return "CASCADE"
	case 'n':
		return "SET NULL"
	case 'd':
		return "SET DEFAULT"
	default:
		return ""
	}
}

// indexQuery reads indexes with the two properties that decide whether an index
// is useful for a given lookup: its column order, and whether it is partial.
//
// Expression indexes report an empty column name for the expression's position.
// They are kept with that gap rather than dropped, so that an index leading with
// an expression is not mistaken for one leading with the column that follows it.
const indexQuery = `
SELECT
    c.relname   AS table_name,
    ic.relname  AS name,
    i.indisunique AS is_unique,
    am.amname   AS method,
    coalesce(pg_get_expr(i.indpred, i.indrelid), '') AS partial,
    coalesce((
        SELECT array_agg(coalesce(a.attname, '') ORDER BY k.ord)
        FROM unnest(i.indkey::int[]) WITH ORDINALITY AS k(attnum, ord)
        LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
    ), '{}') AS columns
FROM pg_index i
JOIN pg_class c     ON c.oid = i.indrelid
JOIN pg_class ic    ON ic.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_am am       ON am.oid = ic.relam
WHERE n.nspname = $1
  AND i.indislive
ORDER BY c.relname, ic.relname
`

func readIndexes(ctx context.Context, q Querier, schema string, tables map[string]*ir.Table) error {
	rows, err := q.Query(ctx, indexQuery, schema)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tableName string
			idx       ir.Index
			columns   []string
		)
		if err := rows.Scan(&tableName, &idx.Name, &idx.Unique, &idx.Method, &idx.Partial, &columns); err != nil {
			return err
		}

		t, ok := tables[tableName]
		if !ok {
			continue
		}
		idx.Columns = columns
		t.Indexes = append(t.Indexes, idx)
	}
	return rows.Err()
}

// walLevelQuery reads the setting logical replication cannot do without.
//
// It is its own query rather than a column on [publicationQuery] because a
// database with no publications still has a wal_level worth reporting: a
// project that has not published anything yet is the case where knowing it is
// "replica" saves the most time.
const walLevelQuery = `SELECT current_setting('wal_level')`

// publicationQuery reads every publication, carrying anything or not.
//
// An empty publication is a fact worth keeping: it is what the sync service
// leaves behind when it has connected but nothing has subscribed yet, and a
// diagnostic reads very differently when it can say so.
const publicationQuery = `
SELECT
    p.pubname      AS name,
    p.puballtables AS all_tables
FROM pg_publication p
ORDER BY p.pubname
`

// publicationTableQuery reads which publications carry which tables.
//
// pg_publication_tables is the view that already resolves FOR ALL TABLES, which
// is why membership is read from it rather than from pg_publication_rel alone.
// The union adds back the one thing it hides: a partitioned table published as a
// parent appears there as its leaves, and rig introspects the parent. Both
// halves are needed, and a table listed by both is listed once.
const publicationTableQuery = `
SELECT pubname, tablename
FROM pg_publication_tables
WHERE schemaname = $1
UNION
SELECT p.pubname, c.relname
FROM pg_publication_rel pr
JOIN pg_publication p ON p.oid = pr.prpubid
JOIN pg_class c       ON c.oid = pr.prrelid
JOIN pg_namespace n   ON n.oid = c.relnamespace
WHERE n.nspname = $1
ORDER BY 1, 2
`

// readReplication reads the server's replication settings and hangs each table's
// publications onto it.
//
// The result is never nil, because "this database has no publications" and "no
// database was read" are different answers, and a nil is how everything
// downstream tells them apart.
func readReplication(ctx context.Context, q Querier, schema string, tables map[string]*ir.Table) (*ir.Replication, error) {
	level, err := readWALLevel(ctx, q)
	if err != nil {
		return nil, err
	}
	publications, err := readPublications(ctx, q)
	if err != nil {
		return nil, err
	}
	out := &ir.Replication{WALLevel: level, Publications: publications}

	// Nothing to be a member of, so nothing to ask.
	if len(publications) == 0 {
		return out, nil
	}
	if err := readPublicationMembers(ctx, q, schema, tables); err != nil {
		return nil, err
	}
	return out, nil
}

func readWALLevel(ctx context.Context, q Querier) (string, error) {
	rows, err := q.Query(ctx, walLevelQuery)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var out string
	for rows.Next() {
		if err := rows.Scan(&out); err != nil {
			return "", err
		}
	}
	return out, rows.Err()
}

func readPublications(ctx context.Context, q Querier) ([]ir.Publication, error) {
	rows, err := q.Query(ctx, publicationQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ir.Publication
	for rows.Next() {
		var p ir.Publication
		if err := rows.Scan(&p.Name, &p.AllTables); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func readPublicationMembers(ctx context.Context, q Querier, schema string, tables map[string]*ir.Table) error {
	rows, err := q.Query(ctx, publicationTableQuery, schema)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var publication, table string
		if err := rows.Scan(&publication, &table); err != nil {
			return err
		}
		t, ok := tables[table]
		if !ok {
			continue
		}
		t.Publications = append(t.Publications, publication)
	}
	return rows.Err()
}
