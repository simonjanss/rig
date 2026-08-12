package authpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// AccountStore keeps identities, accounts, credentials, and single-use links.
//
// Two tables where there used to be one: identity is the person, global and
// unique on their address, and account is that person inside one tenant. Every
// query below belongs to one side or the other, and the ones that cross say so.
type AccountStore struct {
	db dbx.Conn
	tx dbx.Beginner
}

var _ account.Store = (*AccountStore)(nil)

const identityColumns = `id, email_address, display_name, is_active, email_verified_at`

const accountColumns = `id, tenant_id, identity_id, kind, role, email_address, display_name, time_zone, is_active`

// FindIdentityByEmail implements [account.Store].
//
// The comparison is lower(email_address) with no tenant in sight, which matches
// the unique index the foundation creates — so this is an index lookup rather
// than a scan, and it agrees with what the database will enforce on insert.
func (s *AccountStore) FindIdentityByEmail(ctx context.Context, lowercased string) (*account.Identity, error) {
	return s.oneIdentity(ctx, `
		SELECT `+identityColumns+` FROM identity
		WHERE lower(email_address) = $1 AND deleted_at IS NULL`, lowercased)
}

// FindIdentityByID implements [account.Store].
func (s *AccountStore) FindIdentityByID(ctx context.Context, id uuid.UUID) (*account.Identity, error) {
	return s.oneIdentity(ctx, `
		SELECT `+identityColumns+` FROM identity
		WHERE id = $1 AND deleted_at IS NULL`, id)
}

func (s *AccountStore) oneIdentity(ctx context.Context, sql string, args ...any) (*account.Identity, error) {
	rows, err := conn(ctx, s.db).Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("authpg: read identity: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, rows.Err()
	}

	var i account.Identity
	if err := rows.Scan(&i.ID, &i.EmailAddress, &i.DisplayName, &i.IsActive,
		&i.EmailVerifiedAt); err != nil {
		return nil, fmt.Errorf("authpg: scan identity: %w", err)
	}
	i.EmailVerifiedAt = dbx.UTCPtr(i.EmailVerifiedAt)
	return &i, nil
}

// InsertIdentity implements [account.Store].
func (s *AccountStore) InsertIdentity(ctx context.Context, i *account.Identity) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		INSERT INTO identity (id, created_at, created_by_account_id, created_by_api_key_id,
		                      email_address, display_name, is_active)
		VALUES ($1, now(), $2, $3, $4, $5, $6)`,
		i.ID, i.CreatedBy, i.CreatedByKey, i.EmailAddress, i.DisplayName, i.IsActive)
	if err != nil {
		return fmt.Errorf("authpg: insert identity: %w", err)
	}
	return nil
}

// MarkIdentityVerified implements [account.Store].
func (s *AccountStore) MarkIdentityVerified(ctx context.Context, identityID uuid.UUID, at time.Time) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		UPDATE identity SET email_verified_at = $2, updated_at = $2 WHERE id = $1`,
		identityID, at)
	if err != nil {
		return fmt.Errorf("authpg: mark verified: %w", err)
	}
	return nil
}

// Insert implements [account.Store].
//
// The audit columns are written from the input rather than defaulted, because
// the caller knows who asked and this does not: a provisioning request through
// an API key has both an account and a key to record, and a seed has neither.
func (s *AccountStore) Insert(ctx context.Context, a *account.Account) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		INSERT INTO account (id, tenant_id, identity_id, created_at, created_by_account_id,
		                     created_by_api_key_id, kind, role, email_address,
		                     display_name, time_zone, is_active)
		VALUES ($1, $2, $3, now(), $4, $5, $6, $7, $8, $9, $10, $11)`,
		a.ID, a.TenantID, a.IdentityID, a.CreatedBy, a.CreatedByKey, a.Kind, a.Role,
		a.EmailAddress, a.DisplayName, nullable(a.TimeZone), a.IsActive)
	if err != nil {
		return fmt.Errorf("authpg: insert account: %w", err)
	}
	return nil
}

// TenantDomains implements [account.Store].
func (s *AccountStore) TenantDomains(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
	var domains []string
	err := conn(ctx, s.db).QueryRow(ctx,
		`SELECT allowed_email_domains FROM tenant WHERE id = $1 AND deleted_at IS NULL`,
		tenantID).Scan(&domains)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No tenant is not "no restriction": it is a request naming something
		// that does not exist, and letting it through would create an account
		// under a tenant nobody can sign in to.
		return nil, rigerr.NotFound("no such tenant")
	case err != nil:
		return nil, fmt.Errorf("authpg: read tenant domains: %w", err)
	}
	return domains, nil
}

// FindByID implements [account.Store].
func (s *AccountStore) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*account.Account, error) {
	return s.oneAccount(ctx, `
		SELECT `+accountColumns+` FROM account
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
}

// AccountForIdentity implements [account.Store].
func (s *AccountStore) AccountForIdentity(ctx context.Context, tenantID, identityID uuid.UUID) (*account.Account, error) {
	return s.oneAccount(ctx, `
		SELECT `+accountColumns+` FROM account
		WHERE tenant_id = $1 AND identity_id = $2 AND deleted_at IS NULL`,
		tenantID, identityID)
}

// AccountsForIdentity implements [account.Store].
//
// The one query in this package with no tenant predicate, deliberately: its
// whole purpose is to reach the tenants the caller is not in. It is unexported
// business — nothing here is returned over HTTP — and what it feeds is revoking
// every session a person has.
func (s *AccountStore) AccountsForIdentity(ctx context.Context, identityID uuid.UUID) ([]*account.Account, error) {
	rows, err := conn(ctx, s.db).Query(ctx, `
		SELECT `+accountColumns+` FROM account
		WHERE identity_id = $1 AND deleted_at IS NULL
		ORDER BY created_at`, identityID)
	if err != nil {
		return nil, fmt.Errorf("authpg: read accounts: %w", err)
	}
	defer rows.Close()

	var out []*account.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TenantsForIdentity implements [account.Store].
//
// The one join in this package. It has no tenant predicate for the same reason
// [AccountStore.AccountsForIdentity] has none — the question is which tenants a
// person belongs to — and a deleted tenant is left out, because being a member of
// something that no longer exists is not somewhere anybody can go.
func (s *AccountStore) TenantsForIdentity(ctx context.Context, identityID uuid.UUID) ([]account.Membership, error) {
	rows, err := conn(ctx, s.db).Query(ctx, `
		SELECT tenant.id, tenant.name, tenant.slug, account.id, account.role, account.is_active
		  FROM account
		  JOIN tenant ON tenant.id = account.tenant_id
		 WHERE account.identity_id = $1
		   AND account.deleted_at IS NULL
		   AND tenant.deleted_at IS NULL
		   AND tenant.is_active
		 ORDER BY tenant.name`, identityID)
	if err != nil {
		return nil, fmt.Errorf("authpg: read tenants: %w", err)
	}
	defer rows.Close()

	var out []account.Membership
	for rows.Next() {
		var w account.Membership
		if err := rows.Scan(&w.TenantID, &w.TenantName, &w.TenantSlug,
			&w.AccountID, &w.Role, &w.IsActive); err != nil {
			return nil, fmt.Errorf("authpg: scan tenant: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *AccountStore) oneAccount(ctx context.Context, sql string, args ...any) (*account.Account, error) {
	rows, err := conn(ctx, s.db).Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("authpg: read account: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanAccount(rows)
}

func scanAccount(rows pgx.Rows) (*account.Account, error) {
	var (
		a    account.Account
		zone *string
	)
	if err := rows.Scan(&a.ID, &a.TenantID, &a.IdentityID, &a.Kind, &a.Role,
		&a.EmailAddress, &a.DisplayName, &zone, &a.IsActive); err != nil {
		return nil, fmt.Errorf("authpg: scan account: %w", err)
	}
	if zone != nil {
		a.TimeZone = *zone
	}
	return &a, nil
}

// Credential implements [account.Store].
func (s *AccountStore) Credential(ctx context.Context, identityID uuid.UUID) (*account.Credential, error) {
	rows, err := conn(ctx, s.db).Query(ctx, `
		SELECT id, identity_id, password_hash, algorithm, params, created_at, updated_at
		FROM identity_credential WHERE identity_id = $1`, identityID)
	if err != nil {
		return nil, fmt.Errorf("authpg: read credential: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		// Somebody with no credential is not broken: a person who only ever
		// signed in through a provider has none.
		return nil, rows.Err()
	}

	var (
		c   account.Credential
		raw []byte
	)
	if err := rows.Scan(&c.ID, &c.IdentityID, &c.PasswordHash,
		&c.Algorithm, &raw, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, fmt.Errorf("authpg: scan credential: %w", err)
	}
	if err := json.Unmarshal(raw, &c.Params); err != nil {
		return nil, fmt.Errorf("authpg: read credential parameters: %w", err)
	}
	c.CreatedAt = dbx.UTC(c.CreatedAt)
	c.UpdatedAt = dbx.UTCPtr(c.UpdatedAt)
	return &c, nil
}

// SaveCredential implements [account.Store].
//
// One credential per identity, so a change is an upsert rather than a delete and
// an insert: the second form has a window in which the person has no password
// at all, and a crash inside it locks somebody out permanently.
func (s *AccountStore) SaveCredential(ctx context.Context, c *account.Credential) error {
	params, err := json.Marshal(c.Params)
	if err != nil {
		return fmt.Errorf("authpg: encode credential parameters: %w", err)
	}

	_, err = conn(ctx, s.db).Exec(ctx, `
		INSERT INTO identity_credential
			(id, identity_id, password_hash, algorithm, params, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (identity_id) DO UPDATE SET
			password_hash = excluded.password_hash,
			algorithm     = excluded.algorithm,
			params        = excluded.params,
			updated_at    = excluded.created_at`,
		c.ID, c.IdentityID, c.PasswordHash, c.Algorithm, params, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("authpg: save credential: %w", err)
	}
	return nil
}

// CreateVerification implements [account.Store].
func (s *AccountStore) CreateVerification(ctx context.Context, v *account.Verification) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		INSERT INTO identity_verification
			(id, identity_id, invited_to_tenant_id, kind, token_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		v.ID, v.IdentityID, v.InvitedToTenantID, string(v.Kind), v.TokenHash,
		v.CreatedAt, v.ExpiresAt)
	if err != nil {
		return fmt.Errorf("authpg: create verification: %w", err)
	}
	return nil
}

// PendingInvitations implements [account.Store].
//
// Live means all four conditions at once: not consumed, not revoked, not expired,
// and for an account that is still there. The join is what makes it useful — an
// interface listing invitations wants to say who, not which token hash.
func (s *AccountStore) PendingInvitations(ctx context.Context, tenantID uuid.UUID) ([]account.Invitation, error) {
	rows, err := conn(ctx, s.db).Query(ctx, `
		SELECT v.id, v.identity_id, account.id, v.invited_to_tenant_id, tenant.name,
		       identity.email_address, identity.display_name, account.role,
		       v.created_at, v.expires_at
		  FROM identity_verification v
		  JOIN identity ON identity.id = v.identity_id
		  JOIN tenant   ON tenant.id = v.invited_to_tenant_id
		  JOIN account  ON account.identity_id = v.identity_id
		                AND account.tenant_id = v.invited_to_tenant_id
		 WHERE v.invited_to_tenant_id = $1
		   AND v.kind = 'Invitation'
		   AND v.consumed_at IS NULL
		   AND v.revoked_at IS NULL
		   AND v.expires_at > now()
		   AND account.deleted_at IS NULL
		 ORDER BY v.created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("authpg: read invitations: %w", err)
	}
	defer rows.Close()

	var out []account.Invitation
	for rows.Next() {
		var i account.Invitation
		if err := rows.Scan(&i.ID, &i.IdentityID, &i.AccountID, &i.TenantID, &i.TenantName,
			&i.EmailAddress, &i.DisplayName, &i.Role,
			&i.CreatedAt, &i.ExpiresAt); err != nil {
			return nil, fmt.Errorf("authpg: scan invitation: %w", err)
		}
		i.CreatedAt = dbx.UTC(i.CreatedAt)
		i.ExpiresAt = dbx.UTC(i.ExpiresAt)
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeVerification implements [account.Store].
//
// The two IS NULL clauses are what make it safe under concurrency: somebody
// accepting an invitation at the same moment sets consumed_at, and exactly one of
// the two statements affects a row.
func (s *AccountStore) RevokeVerification(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := conn(ctx, s.db).Exec(ctx, `
		UPDATE identity_verification SET revoked_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, id, at)
	if err != nil {
		return false, fmt.Errorf("authpg: revoke verification: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SoftDeleteAccount implements [account.Store].
//
// Soft, because the foundation says the table is: account carries deleted_at and
// a restore window, so a withdrawal that turns out to be a mistake is one update
// away from being undone.
func (s *AccountStore) SoftDeleteAccount(ctx context.Context, in account.DeleteAccountInput) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		UPDATE account
		   SET deleted_at = $3, deleted_by_account_id = $4, deleted_by_api_key_id = $5
		 WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`,
		in.TenantID, in.AccountID, in.At, in.ByAccountID, in.ByAPIKeyID)
	if err != nil {
		return fmt.Errorf("authpg: delete account: %w", err)
	}
	return nil
}

// VerificationByHash implements [account.Store].
func (s *AccountStore) VerificationByHash(ctx context.Context, hash []byte) (*account.Verification, error) {
	return s.verification(ctx, `token_hash = $1`, hash)
}

// VerificationByID implements [account.Store].
func (s *AccountStore) VerificationByID(ctx context.Context, id uuid.UUID) (*account.Verification, error) {
	return s.verification(ctx, `id = $1`, id)
}

func (s *AccountStore) verification(ctx context.Context, where string, arg any) (*account.Verification, error) {
	rows, err := conn(ctx, s.db).Query(ctx, `
		SELECT id, identity_id, invited_to_tenant_id, kind, token_hash,
		       created_at, expires_at, consumed_at, revoked_at
		FROM identity_verification WHERE `+where, arg)
	if err != nil {
		return nil, fmt.Errorf("authpg: read verification: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, rows.Err()
	}

	var (
		v    account.Verification
		kind string
	)
	if err := rows.Scan(&v.ID, &v.IdentityID, &v.InvitedToTenantID, &kind, &v.TokenHash,
		&v.CreatedAt, &v.ExpiresAt, &v.ConsumedAt, &v.RevokedAt); err != nil {
		return nil, fmt.Errorf("authpg: scan verification: %w", err)
	}
	v.Kind = account.VerificationKind(kind)
	v.CreatedAt = dbx.UTC(v.CreatedAt)
	v.ExpiresAt = dbx.UTC(v.ExpiresAt)
	v.ConsumedAt = dbx.UTCPtr(v.ConsumedAt)
	v.RevokedAt = dbx.UTCPtr(v.RevokedAt)
	return &v, nil
}

// ConsumeVerification implements [account.Store].
//
// The `consumed_at IS NULL` clause is what makes it single-use under
// concurrency: two requests racing to redeem one link both run the UPDATE, and
// exactly one of them affects a row.
func (s *AccountStore) ConsumeVerification(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := conn(ctx, s.db).Exec(ctx, `
		UPDATE identity_verification SET consumed_at = $2
		WHERE id = $1 AND consumed_at IS NULL`, id, at)
	if err != nil {
		return false, fmt.Errorf("authpg: consume verification: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// InTx implements [account.Store].
func (s *AccountStore) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return dbx.InTx(ctx, s.tx, func(ctx context.Context, _ dbx.Conn) error { return fn(ctx) })
}

// InvitationsForIdentity implements [account.Store].
//
// No tenant predicate, deliberately, and it is the one query in this package
// where that is the whole point: somebody who belongs to no tenant is asking
// which ones have asked for them. Scoping it by tenant would be scoping it to a
// tenant they are not in yet.
func (s *AccountStore) InvitationsForIdentity(ctx context.Context, identityID uuid.UUID) ([]account.Invitation, error) {
	rows, err := conn(ctx, s.db).Query(ctx, `
		SELECT v.id, v.identity_id, account.id, v.invited_to_tenant_id, tenant.name,
		       identity.email_address, identity.display_name, account.role,
		       v.created_at, v.expires_at
		  FROM identity_verification v
		  JOIN identity ON identity.id = v.identity_id
		  JOIN tenant   ON tenant.id = v.invited_to_tenant_id
		  JOIN account  ON account.identity_id = v.identity_id
		                AND account.tenant_id = v.invited_to_tenant_id
		 WHERE v.identity_id = $1
		   AND v.kind = 'Invitation'
		   AND v.consumed_at IS NULL
		   AND v.revoked_at IS NULL
		   AND v.expires_at > now()
		   AND account.deleted_at IS NULL
		 ORDER BY v.created_at DESC`, identityID)
	if err != nil {
		return nil, fmt.Errorf("authpg: read invitations for identity: %w", err)
	}
	defer rows.Close()

	var out []account.Invitation
	for rows.Next() {
		var i account.Invitation
		if err := rows.Scan(&i.ID, &i.IdentityID, &i.AccountID, &i.TenantID, &i.TenantName,
			&i.EmailAddress, &i.DisplayName, &i.Role,
			&i.CreatedAt, &i.ExpiresAt); err != nil {
			return nil, fmt.Errorf("authpg: scan invitation: %w", err)
		}
		i.CreatedAt = dbx.UTC(i.CreatedAt)
		i.ExpiresAt = dbx.UTC(i.ExpiresAt)
		out = append(out, i)
	}
	return out, rows.Err()
}

// InsertTenant implements [account.Store].
func (s *AccountStore) InsertTenant(ctx context.Context, t *account.Tenant) error {
	// The column is NOT NULL with an empty-array default, and a nil slice reaches
	// Postgres as NULL rather than as that default. "No restriction" is an empty
	// list, so it is written as one.
	domains := t.AllowedEmailDomains
	if domains == nil {
		domains = []string{}
	}

	_, err := conn(ctx, s.db).Exec(ctx, `
		INSERT INTO tenant (id, created_at, name, slug, is_active, allowed_email_domains)
		VALUES ($1, now(), $2, $3, $4, $5)`,
		t.ID, t.Name, t.Slug, t.IsActive, domains)
	if err != nil {
		return fmt.Errorf("authpg: insert tenant: %w", err)
	}
	return nil
}
