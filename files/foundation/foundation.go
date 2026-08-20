// Package foundation carries the schema the files module's table needs.
//
// One migration, creating rig_file. It references nothing: rig_file carries a
// tenant_id and deliberately no foreign key to rig_tenant, because uploads are
// the one part of rig's foundation that is useful in a project with no
// authentication at all. So this set applies on its own, in any order relative
// to the others.
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
// so that files can be released without agreeing a migration number with auth
// or notify.
const Table = "rig_files_migrations"

// Set is the files module's migrations, in the order they apply.
func Set() dbschema.Set {
	return dbschema.Set{
		Module: "rig/files",
		FS:     sql,
		Dir:    ".",
		Table:  Table,
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "files", Tables: []string{"rig_file"}},
		},
	}
}
