package modelgo

// notifyModelImport is the package that ships the Go for rig's notification
// tables.
//
// Named as a string rather than imported, for the reason every runtime module
// is named that way here: importing it would put it in the CLI's binary, and
// what this generator needs from it is a path to write into an import block.
const notifyModelImport = "github.com/simonjanss/rig/notify/notifymodel"

// authModelImport is the same for rig_account.
//
// A module of its own rather than a package under rig/auth, because
// `auth.expose: [rig_account]` does not require `auth.enabled`: a project may
// read account rows without using rig's authentication, and an alias into the
// auth module would put argon2, OAuth and x/crypto in its go.mod for the sake of
// a struct. Notifications need no such care — a project with those tables
// already calls notify.NewRegistry, so that module was there either way.
const authModelImport = "github.com/simonjanss/rig/authmodel"

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
// The name is returned as well as the path, and that is not redundant: a
// project may rename the resource its table projects to, and rig's own
// configuration is only the default. The shipped package was compiled against
// one spelling, so the alias has to name both sides — the project's on the left,
// rig's on the right.
func shippedModel(table string) (importPath, name string, ok bool) {
	switch table {
	case "rig_notification":
		return notifyModelImport, "Notification", true
	case "rig_notification_recipient":
		return notifyModelImport, "NotificationRecipient", true
	case "rig_notification_device":
		return notifyModelImport, "NotificationDevice", true
	case "rig_notification_setting":
		return notifyModelImport, "NotificationSetting", true
	case "rig_notification_delivery":
		return notifyModelImport, "NotificationDelivery", true
	case "rig_account":
		return authModelImport, "Account", true
	}
	return "", "", false
}

// shippedEnum is [shippedModel] for a Postgres enum type.
//
// Separate because an enum is not a table and reaches this generator through
// [github.com/simonjanss/rig/pkg/ir.Enum] rather than through a resource. The
// Postgres type name is the key for the same reason the table name is: rig's
// own configuration fixes the Go name, but the type is what the migration
// created.
func shippedEnum(pgType string) (importPath, name string, ok bool) {
	switch pgType {
	case "rig_notification_state":
		return notifyModelImport, "NotificationState", true
	case "rig_notification_channel":
		return notifyModelImport, "NotificationChannel", true
	case "rig_notification_digest":
		return notifyModelImport, "NotificationDigest", true
	case "rig_notification_delivery_state":
		return notifyModelImport, "NotificationDeliveryState", true
	case "rig_account_role_level":
		return authModelImport, "AccountRoleLevel", true
	case "rig_account_kind":
		return authModelImport, "AccountKind", true
	}
	return "", "", false
}
