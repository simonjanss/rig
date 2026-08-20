// Package foundation carries the schema the auth module's tables need.
//
// The DDL is here rather than in the project that uses it because these tables
// are this module's: `authpg` writes SQL against every one of them, and a schema
// the code depends on should travel with the code. What a project decides is
// whether to vendor the set into its own migrations directory or leave it to be
// applied from here — see [github.com/simonjanss/rig/runtime/dbschema] for both
// readings.
//
// The set is append-only. A migration already shipped is never renumbered,
// rewritten or removed: somebody's database has already run it, and rewriting it
// would leave that database and this directory describing different schemas with
// no way to tell. A change to these tables is a new file at the next number.
//
// The package imports nothing but the standard library and dbschema. That is
// deliberate and load-bearing: notify's tables reference rig_account, which the
// tenancy migration here creates, so a project with notifications and no
// authentication has to be able to reach this set. It pays the SQL and nothing
// else — none of rig/auth's code comes with it.
package foundation

import (
	"embed"

	"github.com/simonjanss/rig/runtime/dbschema"
)

//go:embed *.sql
var sql embed.FS

// Table is where this set records how far it has been applied.
//
// Its own table, not the project's. The three foundation-carrying modules are
// tagged separately, so a shared table would mean a shared numbering sequence
// and a version collision the first time two of them shipped a migration in the
// same release.
const Table = "rig_auth_migrations"

// Set is the auth module's migrations, in the order they apply.
//
// Tenancy first because everything here references it. API keys before sessions
// because the token table and the log declare their key columns where they are
// created rather than altering them afterwards, so rig_api_key has to exist by
// then — the alternative is two shapes of the same table. OAuth last because
// nothing depends on it.
func Set() dbschema.Set {
	return dbschema.Set{
		Module: "rig/auth",
		FS:     sql,
		Dir:    ".",
		Table:  Table,
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "tenancy", Tables: []string{
				"rig_tenant", "rig_identity", "rig_identity_credential",
				"rig_identity_verification", "rig_account",
			}},
			{Number: 2, Name: "apikeys", Tables: []string{"rig_api_key"}},
			{Number: 3, Name: "sessions", Tables: []string{
				"rig_account_token", "rig_auth_log", "rig_identity_session",
			}},
			{Number: 4, Name: "oauth", Tables: []string{"rig_identity_oauth"}},
		},
	}
}
