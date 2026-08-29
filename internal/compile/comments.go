package compile

import "github.com/simonjanss/rig/pkg/ir"

// autoComments documents the columns rig manages itself.
//
// Requiring a comment on every column is a good rule, but making someone write
// "the time this row was created" forty times is not. These fill in the columns
// whose meaning rig already knows; anything a human writes wins, and the
// distinction is kept in the column's comment source so the missing-comment
// rule can tell a real description from a generated one.
var autoComments = map[string]string{
	ColID:       "Unique identifier for this row.",
	ColTenantID: "Tenant this row belongs to. Every query is scoped by it.",

	ColCreatedAt: "When this row was created. Set automatically and never changes.",
	ColCreatedBy: "Account that created this row, taken from the request's claims.",
	ColUpdatedAt: "When this row was last updated. Set automatically on every update.",
	ColUpdatedBy: "Account that last updated this row, taken from the request's claims.",
	ColDeletedAt: "When this row was soft-deleted. Null while the row is live.",
	ColDeletedBy: "Account that soft-deleted this row, taken from the request's claims.",

	ColCreatedByKey: "API key this row was created through, when it was an integration rather than a person.",
	ColUpdatedByKey: "API key this row was last updated through, when it was an integration rather than a person.",
	ColDeletedByKey: "API key this row was soft-deleted through, when it was an integration rather than a person.",

	ColVersionType: "Whether this row is the live version or an immutable snapshot of a previous one.",
}

// autoComment returns the generated comment for a column, if rig has one.
func autoComment(table, column string) (string, bool) {
	if c, ok := autoComments[column]; ok {
		return c, true
	}
	switch column {
	case SnapshotFromIDColumn(table):
		return "The row this snapshot was copied from. Null on live rows.", true
	case SnapshotFromAtColumn(table):
		return "The source row's last-updated time at the moment this snapshot was taken. " +
			"This identifies the version captured, not when the copy was made.", true
	}
	return "", false
}

// applyAutoComments fills in comments for managed columns that have none.
// A comment already present — from the configuration or from a Postgres
// COMMENT ON — is left alone.
func applyAutoComments(t *ir.Table) {
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Comment != "" {
			continue
		}
		if text, ok := autoComment(t.Name, c.Name); ok {
			c.Comment = text
			c.CommentSource = ir.CommentSourceAuto
		}
	}
}
