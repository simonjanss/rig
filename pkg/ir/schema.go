package ir

import "slices"

// Schema is the physical layer: what Postgres actually contains, normalized.
//
// Tables and enums are sorted by name, columns by ordinal, and every other slice
// by a stable key, so two runs over the same database produce identical bytes.
type Schema struct {
	// Name is the Postgres schema, almost always "public".
	Name string `json:"name"`

	Tables []Table  `json:"tables"`
	Enums  []PgEnum `json:"enums"`

	// Replication is what the server says about logical replication.
	//
	// Nil means nobody looked: a schema dump written before rig read these
	// facts, or a hand-written fixture. A live introspection always sets it,
	// even when the database has no publication at all, which is what lets a
	// rule tell "looked and found none" from "never looked".
	Replication *Replication `json:"replication,omitempty"`
}

// Replication is the server-wide half of what live sync needs.
type Replication struct {
	// WALLevel is the server's wal_level. Logical replication needs "logical",
	// and it cannot be turned on after Postgres has started.
	WALLevel string `json:"wal_level,omitempty"`
	// Publications is every publication in the database, sorted by name,
	// whether or not it carries anything.
	Publications []Publication `json:"publications,omitempty"`
}

// Publication is one Postgres publication.
type Publication struct {
	Name string `json:"name"`
	// AllTables is a publication declared FOR ALL TABLES, which carries every
	// table in the database including ones created after it.
	AllTables bool `json:"all_tables,omitempty"`
	// Owned says this publication belongs to the role rig connected as, which is
	// the role that runs migrations.
	//
	// It is the one fact that tells a publication a migration wrote from one the
	// sync service created for itself, and the two are worth telling apart even
	// when they share a name. The sync service makes
	// `electric_publication_default` on first connection and owns it; a migration
	// that creates the same publication first owns it instead, and that is the
	// arrangement that makes live sync work for a role owning no tables. Only the
	// owner distinguishes them.
	Owned bool `json:"owned,omitempty"`
}

// ReplicaIdentity is a table's pg_class.relreplident, spelled out.
//
// Four values rather than a bool, because the three that are not
// [ReplicaIdentityFull] fail live sync for three different reasons and a rule
// worth reading says which: nothing carries no old row at all, index means
// somebody chose a covering index on purpose, and default — the Postgres default
// — carries the primary key and nothing else.
type ReplicaIdentity string

// The replica identities Postgres records.
const (
	// ReplicaIdentityDefault carries the primary key of the old row, which is
	// what a table has unless somebody changed it.
	ReplicaIdentityDefault ReplicaIdentity = "default"
	// ReplicaIdentityFull carries the whole old row, and is what live sync needs:
	// a subscriber matching an update against the row it replaces has only the
	// old values to match on.
	ReplicaIdentityFull ReplicaIdentity = "full"
	// ReplicaIdentityNothing carries nothing, so an update or a delete reaches a
	// subscriber with no way to say which row it was.
	ReplicaIdentityNothing ReplicaIdentity = "nothing"
	// ReplicaIdentityIndex carries the columns of a nominated unique index.
	ReplicaIdentityIndex ReplicaIdentity = "index"
)

// TableKind distinguishes real tables from the read-only relations that share
// their introspection shape.
type TableKind string

// The relation kinds rig introspects.
const (
	TableKindBase             TableKind = "Base"
	TableKindView             TableKind = "View"
	TableKindMaterializedView TableKind = "MaterializedView"
)

// Table is one relation in the schema.
type Table struct {
	Name    string    `json:"name"`
	Comment string    `json:"comment,omitempty"`
	Kind    TableKind `json:"kind"`

	Columns    []Column   `json:"columns"`
	PrimaryKey []string   `json:"primary_key,omitempty"`
	Uniques    [][]string `json:"uniques,omitempty"`

	Indexes     []Index      `json:"indexes,omitempty"`
	ForeignKeys []ForeignKey `json:"foreign_keys,omitempty"`
	Checks      []Check      `json:"checks,omitempty"`

	// Publications names every publication that carries this table, sorted,
	// including one declared FOR ALL TABLES. Empty on a table with live sync
	// means the stream would never emit a row.
	Publications []string `json:"publications,omitempty"`
	// Unlogged is a table whose relpersistence is 'u'. It writes no WAL, so
	// logical replication cannot see it at all.
	Unlogged bool `json:"unlogged,omitempty"`
	// ReplicaIdentity is what an UPDATE or a DELETE on this table puts in the
	// WAL for the row as it was. Live sync needs [ReplicaIdentityFull].
	//
	// Empty means nobody looked — a schema dump written before rig read this, or
	// a hand-written fixture — which is what lets the rule over it be an error
	// rather than a guess. Same convention as [Schema.Replication], one level
	// down.
	ReplicaIdentity ReplicaIdentity `json:"replica_identity,omitempty"`

	// LinkTable is set when this table is a pure many-to-many join: its primary
	// key is exactly two foreign-key columns. Such tables become relations on
	// the resources they join, never resources of their own.
	LinkTable *LinkTable `json:"link_table,omitempty"`
}

// CommentSource records where a comment came from, so a rule that requires
// documentation can tell an authored sentence from a generated placeholder.
type CommentSource string

const (
	// CommentSourceNone means the column has no comment at all.
	CommentSourceNone CommentSource = "none"
	// CommentSourceAuto means rig supplied it from a template for a well-known
	// column such as id or created_at.
	CommentSourceAuto CommentSource = "auto"
	// CommentSourceDatabase means it came from a Postgres COMMENT ON.
	CommentSourceDatabase CommentSource = "database"
	// CommentSourceConfig means a human wrote it in the table's YAML.
	CommentSourceConfig CommentSource = "config"
)

// Authored reports whether a human wrote this comment, as opposed to rig or the
// database supplying one.
func (s CommentSource) Authored() bool { return s == CommentSourceConfig }

// Column is one column of a [Table]. Every field here is a fact read out of
// Postgres; none of it is configurable.
type Column struct {
	Name          string        `json:"name"`
	Comment       string        `json:"comment,omitempty"`
	CommentSource CommentSource `json:"comment_source"`

	// SQLType is the type as written, for example "text", "timestamptz",
	// "lesson_status" or "text[]". UDTName is the underlying pg_type name,
	// which differs for arrays and domains.
	SQLType string `json:"sql_type"`
	UDTName string `json:"udt_name,omitempty"`

	Nullable   bool   `json:"nullable"`
	HasDefault bool   `json:"has_default,omitempty"`
	Default    string `json:"default,omitempty"`

	// Identity is set for GENERATED { ALWAYS | BY DEFAULT } AS IDENTITY.
	Identity bool `json:"identity,omitempty"`
	// Generated is set for GENERATED ALWAYS AS (expr) STORED. Such columns can
	// never be written, so they are read-only everywhere above this layer.
	Generated bool `json:"generated,omitempty"`

	CharMaxLength int `json:"char_max_length,omitempty"`
	NumericPrec   int `json:"numeric_precision,omitempty"`
	NumericScale  int `json:"numeric_scale,omitempty"`

	// Ordinal is the 1-based position in the table, preserved so generated
	// structs read in the same order as the CREATE TABLE.
	Ordinal int `json:"ordinal"`

	IsPrimaryKey bool   `json:"is_primary_key,omitempty"`
	ForeignKey   *FKRef `json:"foreign_key,omitempty"`

	// EnumType is the Postgres enum type name when this column is an enum.
	EnumType string `json:"enum_type,omitempty"`
	// ArrayElem is the element type name when this column is an array.
	ArrayElem string `json:"array_elem,omitempty"`
}

// Writable reports whether a client could ever supply a value for this column.
func (c Column) Writable() bool { return !c.Generated && !c.Identity }

// FKRef is the single-column foreign key a column participates in, denormalized
// onto the column because that is how nearly every generator needs it. Composite
// foreign keys appear only in [Table.ForeignKeys].
type FKRef struct {
	Table    string `json:"table"`
	Column   string `json:"column"`
	OnDelete string `json:"on_delete,omitempty"`
	OnUpdate string `json:"on_update,omitempty"`
}

// ForeignKey is a full constraint, including composite ones.
type ForeignKey struct {
	Name           string   `json:"name"`
	Columns        []string `json:"columns"`
	ForeignTable   string   `json:"foreign_table"`
	ForeignColumns []string `json:"foreign_columns"`
	OnDelete       string   `json:"on_delete,omitempty"`
	OnUpdate       string   `json:"on_update,omitempty"`
}

// Index is one index on a table. Column order matters: rules that ask whether a
// column is "covered by an index leading with it" read the first element.
type Index struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Method  string   `json:"method,omitempty"` // btree, gin, gist, hash, brin
	// Partial holds the WHERE predicate of a partial index, empty otherwise.
	Partial string `json:"partial,omitempty"`
}

// LeadsWith reports whether the index's first column is name. A partial index
// does not count: it cannot serve the general lookup the rules care about.
func (i Index) LeadsWith(name string) bool {
	return i.Partial == "" && len(i.Columns) > 0 && i.Columns[0] == name
}

// Covers reports whether name appears anywhere in the index.
func (i Index) Covers(name string) bool { return slices.Contains(i.Columns, name) }

// Check is a CHECK constraint, carried so generators can surface it in
// documentation and so rules can recognize the constraints rig itself scaffolds.
type Check struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

// LinkTable describes a pure many-to-many join table.
type LinkTable struct {
	// Table is the join table itself. It is carried here because a relation
	// derived from a join table is otherwise unable to name the thing it
	// travels through, and that name is how configuration refers to it.
	Table string `json:"table"`

	LeftTable   string `json:"left_table"`
	LeftColumn  string `json:"left_column"`
	RightTable  string `json:"right_table"`
	RightColumn string `json:"right_column"`
}

// PgEnum is a Postgres enum type. Values are in enumsortorder, which is the
// order clients will see them in documentation and generated code.
type PgEnum struct {
	Name    string        `json:"name"`
	Comment string        `json:"comment,omitempty"`
	Values  []PgEnumValue `json:"values"`
}

// PgEnumValue is one label of a [PgEnum].
type PgEnumValue struct {
	Value string `json:"value"`
	// Description comes from the table configuration; Postgres cannot comment
	// on individual enum labels.
	Description string `json:"description,omitempty"`
}

// HasValue reports whether the enum contains the given label.
func (e PgEnum) HasValue(v string) bool {
	for _, ev := range e.Values {
		if ev.Value == v {
			return true
		}
	}
	return false
}

// Column returns the named column, or nil. The lookup is linear; for repeated
// lookups across a whole document use [Document.Column].
func (t *Table) Column(name string) *Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// HasColumn reports whether the table has the named column.
func (t *Table) HasColumn(name string) bool { return t.Column(name) != nil }

// IsLinkTable reports whether this table is a pure many-to-many join.
func (t *Table) IsLinkTable() bool { return t.LinkTable != nil }

// SinglePrimaryKey returns the primary key column name when the table has
// exactly one, and false otherwise.
func (t *Table) SinglePrimaryKey() (string, bool) {
	if len(t.PrimaryKey) != 1 {
		return "", false
	}
	return t.PrimaryKey[0], true
}
