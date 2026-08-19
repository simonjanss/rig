package compile

import (
	"slices"
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

	// The key columns say which credential a change came through, where the
	// account columns say whose it was.
	//
	// They are worth having separately because an integration's account answers
	// "who" with a service account that may be shared between several keys: the
	// account tells you the import did it, and the key tells you which
	// integration and which credential to revoke. A change made by a person
	// signed in normally leaves them null.
	ColCreatedByKey = "created_by_api_key_id"
	ColUpdatedByKey = "updated_by_api_key_id"
	ColDeletedByKey = "deleted_by_api_key_id"

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

// IsManagedColumn reports whether a column is set by the framework rather than
// by a client. Such columns never appear in a create or update input, and they
// are not configurable, so `rig sync` leaves them out of the files it writes.
func IsManagedColumn(table, name string) bool {
	switch name {
	case ColID, ColTenantID,
		ColCreatedAt, ColCreatedBy,
		ColUpdatedAt, ColUpdatedBy,
		ColDeletedAt, ColDeletedBy,
		ColCreatedByKey, ColUpdatedByKey, ColDeletedByKey,
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

// isInstantType reports whether a column holds a moment rather than a reading of
// a clock.
//
// Only timestamptz does. A bare timestamp is a wall-clock value with nothing to
// anchor it: two of them cannot be ordered across a daylight-saving change, and
// the same one means two different moments to two servers. That is a fine type
// for a birthday or for opening hours, and the wrong one for anything that
// happened.
func isInstantType(c *ir.Column) bool {
	switch strings.ToLower(c.SQLType) {
	case "timestamptz", "timestamp with time zone":
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

	CreatedByKey *ir.Column
	UpdatedByKey *ir.Column
	DeletedByKey *ir.Column

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
		l.DeletedAt != nil || l.DeletedBy != nil ||
		l.CreatedByKey != nil || l.UpdatedByKey != nil || l.DeletedByKey != nil
}

// scanLifecycle finds the convention columns on a table.
func scanLifecycle(t *ir.Table) lifecycle {
	return lifecycle{
		Tenant:    t.Column(ColTenantID),
		CreatedAt: t.Column(ColCreatedAt),
		CreatedBy: t.Column(ColCreatedBy),
		UpdatedAt: t.Column(ColUpdatedAt),
		UpdatedBy: t.Column(ColUpdatedBy),
		DeletedAt: t.Column(ColDeletedAt),
		DeletedBy: t.Column(ColDeletedBy),

		CreatedByKey: t.Column(ColCreatedByKey),
		UpdatedByKey: t.Column(ColUpdatedByKey),
		DeletedByKey: t.Column(ColDeletedByKey),

		VersionType: t.Column(ColVersionType),
		SnapshotID:  t.Column(SnapshotFromIDColumn(t.Name)),
		SnapshotAt:  t.Column(SnapshotFromAtColumn(t.Name)),
	}
}

// auditActorColumns are the audit columns that reference an account or a key.
//
// Both are actors in the sense that matters here: they are stamped from the
// claims rather than written by a caller, they are nullable because rig itself
// acts without either during a migration, and neither is a relation worth
// generating an endpoint around.
func auditActorColumns() []string {
	return []string{
		ColCreatedBy, ColUpdatedBy, ColDeletedBy,
		ColCreatedByKey, ColUpdatedByKey, ColDeletedByKey,
	}
}

// FileTable is the managed table every uploaded file has a row in.
const FileTable = "rig_file"

// fileColumnSuffix is what makes a column a file column.
const fileColumnSuffix = "_file_id"

// FileRole reports the role a file column names, and whether it is one.
//
// The column is the declaration. `profile_image_file_id` says there is one file
// attached to this row in the role "profile image", and the path segment, the
// endpoint names, the permission keys and the Go field all follow from it. This
// is the same kind of fact as `deleted_at` meaning soft-deletable: the migration
// says what the thing is, and no configuration key can disagree with it.
//
// A bare `file_id` has no role to name, and two of them on one table would be
// indistinguishable, so it is not one of these.
func FileRole(column string) (role string, ok bool) {
	role = strings.TrimSuffix(column, fileColumnSuffix)
	if role == column || role == "" {
		return "", false
	}
	return role, true
}

// isFileColumn reports whether a column is a file attachment pointing at the
// managed table.
//
// Both halves matter: the name says it is meant to be one, and the target says
// it is. A `cover_file_id` pointing at some other table is an ordinary foreign
// key that happens to be named confusingly, and it gets the ordinary naming
// advice rather than this exemption.
//
// The target is read from the table's constraints as well as from the column,
// because the shape rig recommends is the one [ir.Column.ForeignKey] cannot
// see. Tenant-safety wants a composite key — `(tenant_id, cover_file_id)`
// referencing `rig_file (tenant_id, id)` — so that attaching another tenant's
// file is a constraint violation rather than something a hook has to remember.
// Composite constraints live on the table, and a recognition that consulted
// only the convenient denormalized field would fail on precisely the schema
// this convention exists to encourage.
func isFileColumn(t *ir.Table, c *ir.Column) bool {
	if _, ok := FileRole(c.Name); !ok {
		return false
	}
	return fileKeyTarget(t, c.Name) == FileTable
}

// fileKeyTarget names the table a column's foreign key points at, looking at the
// column's own key first and then at every constraint on the table. Empty when
// the column is in no foreign key at all.
func fileKeyTarget(t *ir.Table, column string) string {
	if c := columnByName(t, column); c != nil && c.ForeignKey != nil {
		return c.ForeignKey.Table
	}
	for _, fk := range t.ForeignKeys {
		if slices.Contains(fk.Columns, column) {
			return fk.ForeignTable
		}
	}
	return ""
}

func columnByName(t *ir.Table, name string) *ir.Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// isAuditActorColumn reports whether a column names who or what performed an
// action.
func isAuditActorColumn(name string) bool {
	switch name {
	case ColCreatedBy, ColUpdatedBy, ColDeletedBy,
		ColCreatedByKey, ColUpdatedByKey, ColDeletedByKey:
		return true
	default:
		return false
	}
}

// The media types rig's own endpoints speak, spelled here for the compiler's
// own convenience.
//
// They are [ir.MediaJSON] and its neighbours, and they live there because a
// generator reads them back out of the document and no generator imports this
// package. Writing them and reading them from two different constants is how
// the two halves of one fact drift apart.
const (
	MediaJSON      = ir.MediaJSON
	MediaMultipart = ir.MediaMultipart
	MediaOctet     = ir.MediaOctet
)
