// Package authz is this application's authorization model. rig ships none.
//
// rig derives the permission *keys* from the schema — api.PermissionKeys() is the
// whole catalogue — and generates the check that reads them off a caller's claims.
// It stops there, deliberately: who holds which is a product decision, and every
// product answers it differently. A role table like this one, a switch on the
// account's level, a claim on a federated token, a group in a directory.
//
// This is one answer, and the whole of it: three roles matching the three coarse
// levels, seeded from the derived keys so a new table lands in them without
// anybody editing this, and a Grants function handed to auth.Config.
//
// It is separate from the tenant package next door because they are separate
// jobs. That one makes tenants, which rig cannot do for you; this one decides what
// somebody may do once they are in one. They were the same file for a while and
// the seam kept showing.
package authz

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/runtime/dbx"
)

// Levels maps a coarse level to the permissions it holds.
//
// The example's policy, not rig's. An Owner does everything; an Admin does
// everything except mint credentials, which is the one thing worth keeping to the
// person who owns the tenant; somebody Basic reads and writes but does not
// delete or administer.
//
// It is a function of the derived keys rather than a list, so a new table lands in
// all three levels without anybody editing this.
//
// A widening key — note.read.all — matches neither suffix, so it stays with Owner
// and Admin and never reaches Basic. That is the intent and it is worth saying out
// loud: somebody Basic reads the notes they wrote, and reading everybody's is an
// administrative thing to be able to do.
//
// apikey.own is named for Basic on purpose, and apikey.manage is not. A personal
// key is intersected with its owner's grants on every request, so it can never do
// more than they can — letting anybody make one grants nothing. A service key is a
// new account with scopes of its own, which is authority creation, and that stays
// with the people who administer the tenant.
func Levels(all []string) map[string][]string {
	var reads, writes []string
	for _, key := range all {
		switch {
		case strings.HasSuffix(key, ".read"):
			reads = append(reads, key)
		case strings.HasSuffix(key, ".write"):
			writes = append(writes, key)
		}
	}

	admin := make([]string, 0, len(all))
	for _, key := range all {
		if key != "apikey.manage" {
			admin = append(admin, key)
		}
	}

	basic := append(reads, writes...)
	basic = append(basic, authhttp.PermissionOwnAPIKey)

	return map[string][]string{
		string(account.RoleOwner): all,
		string(account.RoleAdmin): admin,
		string(account.RoleBasic): basic,
	}
}

// SeedRoles creates the three level roles and their grants.
//
// Idempotent: joining a tenant that already has them is the common case, and
// ON CONFLICT is cheaper than asking first.
func SeedRoles(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, all []string) error {
	// Every key any level grants, not just the ones passed in. Levels() adds two
	// of its own — apikey.own is not derived from a table — and a grant pointing at
	// a permission row nobody inserted is a foreign-key violation at the last
	// statement of the transaction, which is a confusing way to find out.
	byLevel := Levels(all)
	wanted := make([]string, 0, len(all))
	seen := map[string]bool{}
	for _, keys := range byLevel {
		for _, key := range keys {
			if !seen[key] {
				seen[key], wanted = true, append(wanted, key)
			}
		}
	}
	for _, key := range all {
		if !seen[key] {
			seen[key], wanted = true, append(wanted, key)
		}
	}
	slices.Sort(wanted)

	ids := make(map[string]uuid.UUID, len(wanted))
	for _, key := range wanted {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO permission (id, created_at, key, name, description)
			VALUES ($1, now(), $2, $2, 'Declared by the application.')
			ON CONFLICT (key) DO UPDATE SET key = excluded.key
			RETURNING id`, uuid.New(), key).Scan(&id); err != nil {
			return fmt.Errorf("tenant: permission %s: %w", key, err)
		}
		ids[key] = id
	}

	for level, keys := range byLevel {
		var roleID uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO role (id, tenant_id, created_at, key, name, description, is_system)
			VALUES ($1, $2, now(), $3, $4, 'Created with the tenant.', true)
			ON CONFLICT (tenant_id, key) DO UPDATE SET name = excluded.name
			RETURNING id`,
			uuid.New(), tenantID, strings.ToLower(level), level).Scan(&roleID); err != nil {
			return fmt.Errorf("tenant: role %s: %w", level, err)
		}

		for _, key := range keys {
			if _, err := tx.Exec(ctx, `
				INSERT INTO role_permission (role_id, permission_id) VALUES ($1, $2)
				ON CONFLICT DO NOTHING`, roleID, ids[key]); err != nil {
				return fmt.Errorf("tenant: grant %s to %s: %w", key, level, err)
			}
		}
	}
	return nil
}

// AttachRole gives an account the role matching its level.
func AttachRole(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, level string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO account_role (id, account_id, role_id)
		SELECT $4, $2, role.id FROM role WHERE role.tenant_id = $1 AND role.key = $3
		ON CONFLICT (account_id, role_id) DO NOTHING`,
		tenantID, accountID, strings.ToLower(level), uuid.New())
	if err != nil {
		return fmt.Errorf("tenant: attach %s: %w", level, err)
	}
	return nil
}

// GrantLevel gives somebody the permissions their level implies.
//
// The invitation path calls it: provisioning creates an account with a level and
// no grants, because the auth package has no idea what a level means here — so
// an invited Admin can sign in and do nothing until this runs.
//
// A function taking a pool rather than a method, like everything else here: this
// package holds no state, it holds a policy.
func GrantLevel(
	ctx context.Context, pool *pgxpool.Pool,
	tenantID, accountID uuid.UUID, level string, all []string,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := SeedRoles(ctx, tx, tenantID, all); err != nil {
		return err
	}
	if err := AttachRole(ctx, tx, tenantID, accountID, level); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Grants answers what an account may do, over this example's role tables.
//
// This is the whole of the integration rig asks for. It derives the permission
// keys from the schema — api.PermissionKeys() is the catalogue — and generates a
// check that reads them off the caller's claims; where the claims get their
// strings is here, and it is deliberately a plain function rather than an
// interface rig defines. Hand it to auth.Config.Grants and every generated
// endpoint's permission check starts working.
//
// It happens to read a role table because that is what this example has. An
// application with a different model writes a different function: a switch on the
// account's level, a lookup in a directory, a claim off a federated token, or a
// constant while the product is young. None of that is rig's business, which is
// the point of the seam.
//
// It runs per request, not at sign-in, so a role taken away stops working on the
// next call rather than whenever the session happens to refresh. That also puts
// it on the hot path: a real deployment with an expensive answer should cache it
// here, keyed on the account, with a short time to live.
func Grants(pool *pgxpool.Pool) func(context.Context, uuid.UUID, uuid.UUID) ([]string, []string, error) {
	return func(ctx context.Context, tenantID, accountID uuid.UUID) (roles, permissions []string, err error) {
		// The account's coarse level and its assigned roles in one read.
		//
		// Both reach the caller as role names, so nothing checking one has to know
		// the difference: the level is the answer every product needs, the assigned
		// roles are the fine one, and a claim carries whichever apply. A second
		// query for the level would be a second chance for the two to disagree.
		//
		// Roles are scoped to the tenant and permissions are global, which is why
		// the join goes through role rather than starting at permission.
		rows, err := pool.Query(ctx, `
			SELECT
				rig_account.role::text,
				coalesce(array_remove(array_agg(DISTINCT role.key), NULL), '{}'),
				coalesce(array_remove(array_agg(DISTINCT permission.key), NULL), '{}')
			FROM rig_account
			LEFT JOIN account_role ON account_role.account_id = rig_account.id
			LEFT JOIN role ON role.id = account_role.role_id AND role.tenant_id = $1
			LEFT JOIN role_permission ON role_permission.role_id = role.id
			LEFT JOIN permission ON permission.id = role_permission.permission_id
			WHERE rig_account.tenant_id = $1 AND rig_account.id = $2 AND rig_account.deleted_at IS NULL
			GROUP BY rig_account.role`, tenantID, accountID)
		if err != nil {
			return nil, nil, fmt.Errorf("tenant: resolve grants: %w", err)
		}
		defer rows.Close()

		if rows.Next() {
			var level string
			if err := rows.Scan(&level, &roles, &permissions); err != nil {
				return nil, nil, fmt.Errorf("tenant: scan grants: %w", err)
			}
			roles = append(roles, level)
		}
		return roles, permissions, rows.Err()
	}
}

// SyncPermissions writes the keys the code checks into the permission table.
//
// Call it at startup, after the migrations. A key the handlers check and the
// table does not have is a key no role can hold, so every caller asking for it is
// refused until the row exists — which makes this the step that keeps the
// database from lagging behind a deploy that added a table.
//
// Nothing is ever deleted. A grant points at a permission row, so removing one
// silently drops every grant that pointed at it; a key nothing checks any more
// grants nothing anyway. If a table was renamed, rename the key in place —
// UPDATE permission SET key = '<new>' WHERE key = '<old>' — and the grants come
// with it.
//
// The application's job rather than rig's, because these are the application's
// tables: rig derives the keys and stops there. api.PermissionKeys() is the
// derived catalogue, and authhttp.Permissions() are the auth package's own — for
// inviting somebody and minting a key — which are unioned in here so a caller
// passes only its own and still ends up with a complete table.
func SyncPermissions(ctx context.Context, pool *pgxpool.Pool, keys []string) error {
	keys = append(keys, AuthKeys()...)
	for _, key := range keys {
		if _, err := pool.Exec(ctx, `
			INSERT INTO permission (id, created_at, key, name, description)
			VALUES ($1, now(), $2, $2, 'Declared by the application.')
			ON CONFLICT (key) DO NOTHING`, uuid.New(), key); err != nil {
			return fmt.Errorf("tenant: permission %s: %w", key, err)
		}
	}
	return nil
}

// AuthKeys is every permission the authentication endpoints check.
//
// Derived from authhttp.Permissions() rather than listed, which is the same
// argument api.PermissionKeys() makes for the application's own: rig adding an
// endpoint with a permission behind it — reading the authentication trail, ending
// somebody else's session — should land in this example's Owner role without
// anybody noticing it had to. A key nobody holds is an endpoint that answers 403
// and looks like a policy decision.
func AuthKeys() []string {
	out := make([]string, 0, len(authhttp.Permissions()))
	for _, p := range authhttp.Permissions() {
		out = append(out, p.Key)
	}
	return out
}

// SeedFor is the tenant hook: the roles a new tenant starts with.
//
// Handed to account.TenantOptions.OnCreated, so it runs inside the transaction
// that created the tenant. That matters more than it looks: a tenant whose roles
// failed to seed is a tenant whose Owner can sign in and do nothing, and rolling
// the tenant back with them is the only answer that leaves nothing broken.
//
// The keys come from the derived catalogue, so a new table lands in the roles of
// every tenant made after it without anybody editing this.
func SeedFor(keys []string) func(context.Context, account.NewTenant) error {
	return func(ctx context.Context, made account.NewTenant) error {
		tx, ok := dbx.Tx(ctx)
		if !ok {
			// The hook is documented as running inside one. Writing outside it
			// anyway would leave roles behind when the tenant rolled back.
			return fmt.Errorf("authz: OnCreated expected a transaction")
		}
		if err := SeedRoles(ctx, tx, made.TenantID, keys); err != nil {
			return err
		}
		return AttachRole(ctx, tx, made.TenantID, made.AccountID, string(account.RoleOwner))
	}
}
