// Package foundation carries the schema runtime's own tables need.
//
// One migration, creating rig_idempotency: the record of a write that carried
// an Idempotency-Key, so that a client which had to send the same write twice
// gets one row and the same answer both times.
//
// It is the one set that references nothing. An idempotency record is useful in
// a project with no authentication and no files, and it is worth having before
// rig_tenant exists — so like rig_file it carries a tenant_id with no foreign
// key on it, and unlike every other set it can be applied on its own.
//
// It lives under runtime rather than in a module of its own because runtime is
// what every generated application already imports, and a table the generated
// handlers write to should not be a fourth dependency to take on. The cost is
// that runtime, which is meant to stay small, now carries SQL; the alternative
// was a module whose whole content is one table.
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
// so that runtime can be released without agreeing a migration number with
// auth, files or notify.
const Table = "rig_runtime_migrations"

// Set is the runtime module's migrations, in the order they apply.
func Set() dbschema.Set {
	return dbschema.Set{
		Module: "rig/runtime",
		FS:     sql,
		Dir:    ".",
		Table:  Table,
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "idempotency", Tables: []string{"rig_idempotency"}},
		},
	}
}
