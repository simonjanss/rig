package modelgo

// notifyModelImport is the package that ships the Go for rig's notification
// tables.
//
// Named as a string rather than imported, for the reason every runtime module
// is named that way here: importing it would put it in the CLI's binary, and
// what this generator needs from it is a path to write into an import block.
const notifyModelImport = "github.com/simonjanss/rig/notify/notifymodel"

// shippedModel names the package that already declares a table's row, its write
// inputs and its enums, or reports false for a table this project has to
// generate.
//
// The five notification tables are rig's own: their DDL comes from
// notify/foundation, a project cannot change them, and the Go over them was the
// same five thousand lines in every repository that turned notifications on. So
// it is written once, in the module that owns the schema, and what a project
// gets here is an alias.
//
// What is *not* shipped, and why there is still a file per table to write: the
// filter grammar. A project's own table that points at rig_notification adds a
// member to NotificationFilter, so that type is this project's and the
// repository that renders its subqueries is too.
//
// Keyed by physical table name, which is the one thing a configuration file
// cannot move.
func shippedModel(table string) (importPath string, ok bool) {
	switch table {
	case "rig_notification",
		"rig_notification_recipient",
		"rig_notification_device",
		"rig_notification_setting",
		"rig_notification_delivery":
		return notifyModelImport, true
	}
	return "", false
}

// shippedEnum is [shippedModel] for a Postgres enum type.
//
// Separate because an enum is not a table and reaches this generator through
// [github.com/simonjanss/rig/pkg/ir.Enum] rather than through a resource. The
// Postgres type name is the key for the same reason the table name is: rig's
// own configuration fixes the Go name, but the type is what the migration
// created.
func shippedEnum(pgType string) (importPath string, ok bool) {
	switch pgType {
	case "rig_notification_state",
		"rig_notification_channel",
		"rig_notification_digest",
		"rig_notification_delivery_state":
		return notifyModelImport, true
	}
	return "", false
}
