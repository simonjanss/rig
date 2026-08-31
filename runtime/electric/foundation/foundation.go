// Package foundation carries the Postgres role the sync service connects as.
//
// The one set in rig's foundation that creates no table. What it creates is a role and its
// grants, and the reason that is a migration rather than something a deployment does by
// hand is ALTER DEFAULT PRIVILEGES: it is recorded per grantor and per schema, so it has to
// be executed by the role that creates the tables, which is the role that runs migrations
// and nothing else. A grant made from anywhere else covers the tables that existed when it
// ran and none added after — and the failure that produces is a shape that comes back empty
// rather than one that says why.
//
// It applies after rig's other sets and before the project's own, which is what lets two
// statements cover the whole schema: ON ALL TABLES for what rig has already created, and
// the default privileges for everything the project adds later.
//
// The role is created with no password, so nothing in the SQL is a credential.
// [github.com/simonjanss/rig/runtime/electric.SetRolePassword] is the other half, and
// [github.com/simonjanss/rig/runtime/electric.Role] is the name both agree on.
//
// This set is not applied to every project. It arrives when a project streams — see
// [github.com/simonjanss/rig/internal/scaffold] — because a role with SELECT on everything
// is not something to create in a database nothing will ever sync from.
//
// The set is append-only — see [github.com/simonjanss/rig/runtime/dbschema] for what that
// means and why.
package foundation

import (
	"embed"

	"github.com/simonjanss/rig/runtime/dbschema"
)

//go:embed *.sql
var sql embed.FS

// Table is where this set records how far it has been applied. Its own table, so that the
// role can be added to a project without agreeing a migration number with auth, files,
// notify, presence or runtime.
const Table = "rig_electric_migrations"

// Set is the sync service role's migrations, in the order they apply.
func Set() dbschema.Set {
	return dbschema.Set{
		Module: "rig/runtime/electric",
		FS:     sql,
		Dir:    ".",
		Table:  Table,
		// No Tables. This migration creates a role and grants privileges, which is
		// the one thing in rig's foundation that is not a table — see
		// [dbschema.Migration.Tables], which documents an empty list as the ordinary
		// shape of a migration that creates nothing.
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "electric_role"},
		},
	}
}
