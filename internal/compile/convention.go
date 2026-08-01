package compile

import (
	"strings"

	"github.com/simonjanss/rig/pkg/ir"
)

// The columns rig recognizes by name. A table opts into a behavior by having
// the column: there is no configuration key for "this table is soft-deletable",
// because the database already answers that question and two sources of truth
// would eventually disagree.
const (
	// ColID is the primary key of every table rig exposes.
	ColID = "id"
	// ColTenantID scopes every generated query.
	ColTenantID = "tenant_id"

	ColCreatedAt = "created_at"
	ColCreatedBy = "created_by_account_id"
	ColUpdatedAt = "updated_at"
	ColUpdatedBy = "updated_by_account_id"

	// ColDeletedAt makes a table soft-deletable.
	ColDeletedAt = "deleted_at"
	ColDeletedBy = "deleted_by_account_id"

	// ColVersionType is one third of the snapshot triple.
	ColVersionType = "version_type"
)

// The two labels the version_type enum must carry.
const (
	VersionOriginal = "Original"
	VersionSnapshot = "Snapshot"
)

// SnapshotFromIDColumn is the self-referencing column pointing at the row a
// snapshot was copied from.
func SnapshotFromIDColumn(table string) string { return "snapshot_from_" + table + "_id" }

// SnapshotFromAtColumn records the source row's version identity at copy time.
func SnapshotFromAtColumn(table string) string { return "snapshot_from_" + table + "_at" }

// managedColumns are set by the framework rather than by a client. They never
// appear in a create or update input, and they are not configurable.
func isManagedColumn(table, name string) bool {
	switch name {
	case ColID, ColTenantID,
		ColCreatedAt, ColCreatedBy,
		ColUpdatedAt, ColUpdatedBy,
		ColDeletedAt, ColDeletedBy,
		ColVersionType:
		return true
	}
	return name == SnapshotFromIDColumn(table) || name == SnapshotFromAtColumn(table)
}

// isSnapshotColumn reports whether a column is part of the snapshot triple.
func isSnapshotColumn(table, name string) bool {
	return name == ColVersionType ||
		name == SnapshotFromIDColumn(table) ||
		name == SnapshotFromAtColumn(table)
}

// booleanPrefixes are the prefixes that make a boolean column read as the
// question it answers.
var booleanPrefixes = []string{"is_", "has_", "can_", "should_", "was_", "allow_"}

func hasBooleanPrefix(name string) bool {
	for _, p := range booleanPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// isTimestampType reports whether a column holds a point in time.
func isTimestampType(c *ir.Column) bool {
	switch strings.ToLower(c.SQLType) {
	case "timestamptz", "timestamp with time zone", "timestamp", "timestamp without time zone":
		return true
	default:
		return false
	}
}

func isDateType(c *ir.Column) bool { return strings.ToLower(c.SQLType) == "date" }

func isUUIDType(c *ir.Column) bool { return strings.ToLower(c.SQLType) == "uuid" }

func isBoolType(c *ir.Column) bool {
	switch strings.ToLower(c.SQLType) {
	case "bool", "boolean":
		return true
	default:
		return false
	}
}

// lifecycle is what the convention scan found on one table.
type lifecycle struct {
	Tenant *ir.Column

	CreatedAt *ir.Column
	CreatedBy *ir.Column
	UpdatedAt *ir.Column
	UpdatedBy *ir.Column
	DeletedAt *ir.Column
	DeletedBy *ir.Column

	VersionType *ir.Column
	SnapshotID  *ir.Column
	SnapshotAt  *ir.Column
}

// SoftDeletable reports whether rows are retired rather than removed.
func (l lifecycle) SoftDeletable() bool { return l.DeletedAt != nil }

// SnapshotColumnsFound counts how many of the snapshot triple are present, so a
// partial triple can be reported as the mistake it is rather than silently
// treated as "not snapshotable".
func (l lifecycle) SnapshotColumnsFound() int {
	n := 0
	for _, c := range []*ir.Column{l.VersionType, l.SnapshotID, l.SnapshotAt} {
		if c != nil {
			n++
		}
	}
	return n
}

// Snapshotable reports whether updates keep prior versions.
func (l lifecycle) Snapshotable() bool { return l.SnapshotColumnsFound() == 3 }

// HasAudit reports whether any audit column is present.
func (l lifecycle) HasAudit() bool {
	return l.CreatedAt != nil || l.CreatedBy != nil ||
		l.UpdatedAt != nil || l.UpdatedBy != nil ||
		l.DeletedAt != nil || l.DeletedBy != nil
}

// scanLifecycle finds the convention columns on a table.
func scanLifecycle(t *ir.Table) lifecycle {
	return lifecycle{
		Tenant:      t.Column(ColTenantID),
		CreatedAt:   t.Column(ColCreatedAt),
		CreatedBy:   t.Column(ColCreatedBy),
		UpdatedAt:   t.Column(ColUpdatedAt),
		UpdatedBy:   t.Column(ColUpdatedBy),
		DeletedAt:   t.Column(ColDeletedAt),
		DeletedBy:   t.Column(ColDeletedBy),
		VersionType: t.Column(ColVersionType),
		SnapshotID:  t.Column(SnapshotFromIDColumn(t.Name)),
		SnapshotAt:  t.Column(SnapshotFromAtColumn(t.Name)),
	}
}

// auditActorColumns are the audit columns that reference an account.
func auditActorColumns() []string {
	return []string{ColCreatedBy, ColUpdatedBy, ColDeletedBy}
}

// isAuditActorColumn reports whether a column names who performed an action.
func isAuditActorColumn(name string) bool {
	switch name {
	case ColCreatedBy, ColUpdatedBy, ColDeletedBy:
		return true
	default:
		return false
	}
}
