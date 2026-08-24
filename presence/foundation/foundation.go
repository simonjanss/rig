// Package foundation carries the schema the presence module's table needs.
//
// One migration, creating one table. Like
// [github.com/simonjanss/rig/notify/foundation] it has a dependency outside its
// own module — a presence row says which account is present and which tenant it
// is inside, so rig_presence references both rig_account and rig_tenant, which
// [github.com/simonjanss/rig/auth/foundation]'s tenancy migration creates. That
// set has to be applied first, and a project with presence and no
// authentication still needs it — which is why auth/foundation imports nothing
// but the standard library, so reaching it costs the SQL and none of rig/auth's
// code.
//
// Not sessions, though. Being present needs claims, and where those come from is
// not this schema's business.
//
// The set is append-only — see [github.com/simonjanss/rig/runtime/dbschema] for
// what that means and why.
package foundation

import (
	"embed"

	"github.com/simonjanss/rig/runtime/dbschema"
)

//go:embed *.sql
var sql embed.FS

// Table is where this set records how far it has been applied. Its own table, so
// that presence can be released without agreeing a migration number with auth,
// files or notify.
const Table = "rig_presence_migrations"

// Set is the presence module's migrations, in the order they apply.
func Set() dbschema.Set {
	return dbschema.Set{
		Module: "rig/presence",
		FS:     sql,
		Dir:    ".",
		Table:  Table,
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "presence", Tables: []string{"rig_presence"}},
		},
	}
}
