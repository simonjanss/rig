// Package foundation carries the schema the notify module's tables need.
//
// One migration, creating the five notification tables. It is the one set with a
// dependency outside its own module: an inbox line names an account, so
// rig_notification_recipient references rig_account, which
// [github.com/simonjanss/rig/auth/foundation]'s tenancy migration creates. That
// set has to be applied first, and a project with notifications and no
// authentication still needs it — which is why auth/foundation imports nothing
// but the standard library, so reaching it costs the SQL and none of rig/auth's
// code.
//
// Not sessions, though. Reading your own inbox needs claims, and where those come
// from is not this schema's business.
//
// The set is append-only — see
// [github.com/simonjanss/rig/runtime/dbschema] for what that means and why.
package foundation

import (
	"embed"

	"github.com/simonjanss/rig/runtime/dbschema"
)

//go:embed *.sql
var sql embed.FS

// Table is where this set records how far it has been applied. Its own table,
// so that notify can be released without agreeing a migration number with auth
// or files.
const Table = "rig_notify_migrations"

// Set is the notify module's migrations, in the order they apply.
func Set() dbschema.Set {
	return dbschema.Set{
		Module: "rig/notify",
		FS:     sql,
		Dir:    ".",
		Table:  Table,
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "notifications", Tables: []string{
				"rig_notification", "rig_notification_recipient",
				"rig_notification_device", "rig_notification_setting",
				"rig_notification_delivery",
			}},
		},
	}
}
