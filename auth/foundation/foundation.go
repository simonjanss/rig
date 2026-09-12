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
// Migration 1 was rewritten once anyway, and this is the record of it. Removing
// password authentication took out rig_identity_credential and a value of
// rig_identity_verification_kind, and making an invitation a pending membership
// added the columns an invitation carries to rig_identity_verification — none
// of which can be expressed as a forward migration without leaving the table
// that holds password hashes in every database that ever had one. It was done
// in place, on the grounds that rig is pre-v1 and no deployment outside this
// repository had applied it. That argument is spent: it cannot be made a second
// time, and the rule above is the rule.
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
// then — the alternative is two shapes of the same table. OAuth after that
// because nothing depends on it, and the two that follow it because they came
// later: the set is append-only, so arrival order and dependency order are the
// same order by construction.
func Set() dbschema.Set {
	return dbschema.Set{
		Module: "rig/auth",
		FS:     sql,
		Dir:    ".",
		Table:  Table,
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "tenancy", Tables: []string{
				"rig_tenant", "rig_identity",
				"rig_identity_verification", "rig_account",
			}},
			// Only the new table. It also alters rig_account, rig_identity and
			// rig_identity_verification to add the columns that name a key, and
			// those three are migration 1's — Set().Tables() must not list one
			// twice.
			{Number: 2, Name: "apikeys", Tables: []string{"rig_api_key"}},
			{Number: 3, Name: "sessions", Tables: []string{
				"rig_account_token", "rig_auth_log", "rig_identity_session",
			}},
			{Number: 4, Name: "oauth", Tables: []string{"rig_identity_oauth"}},
			// Only the new table. rig_identity_verification is already claimed by
			// migration 1, and Set().Tables() must not list it twice — this
			// migration alters it rather than creating it.
			{Number: 5, Name: "verification_delivery", Tables: []string{
				"rig_identity_verification_delivery",
			}},
			// No tables at all. What it adds is one index on rig_account_token,
			// which migration 3 created — the read behind "sign in and land back
			// where you were" has no other way to be cheap.
			{Number: 6, Name: "last_tenant"},
		},
	}
}
