package authpg

import (
	"context"
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
		SELECT `+identityColumns+` FROM rig_identity
		WHERE lower(email_address) = $1 AND deleted_at IS NULL`, lowercased)
}

// FindIdentityByID implements [account.Store].
func (s *AccountStore) FindIdentityByID(ctx context.Context, id uuid.UUID) (*account.Identity, error) {
	return s.oneIdentity(ctx, `
		SELECT `+identityColumns+` FROM rig_identity
		WHERE id = $1 AND deleted_at IS NULL`, id)
}

func (s *AccountStore) oneIdentity(ctx context.Context, sql string, args ...any) (*account.Identity, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, sql, args...)
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
	_, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_identity (id, created_at, created_by_account_id, created_by_api_key_id,
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
	_, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		UPDATE rig_identity SET email_verified_at = $2, updated_at = $2 WHERE id = $1`,
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
	_, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_account (id, tenant_id, identity_id, created_at, created_by_account_id,
		                     created_by_api_key_id, kind, role, email_address,
		                     display_name, time_zone, is_active)
		VALUES ($1, $2, $3, now(), $4, $5, $6, $7, $8, $9, $10, $11)`,
		a.ID, a.TenantID, a.IdentityID, a.CreatedBy, a.CreatedByKey, a.Kind, a.Role,
		a.EmailAddress, a.DisplayName, dbx.Null(a.TimeZone), a.IsActive)
	if err != nil {
		return fmt.Errorf("authpg: insert account: %w", err)
	}
	return nil
}

// TenantDomains implements [account.Store].
func (s *AccountStore) TenantDomains(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
	var domains []string
	err := dbx.ConnFor(ctx, s.db).QueryRow(ctx,
		`SELECT allowed_email_domains FROM rig_tenant WHERE id = $1 AND deleted_at IS NULL`,
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
		SELECT `+accountColumns+` FROM rig_account
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
}

// AccountForIdentity implements [account.Store].
func (s *AccountStore) AccountForIdentity(ctx context.Context, tenantID, identityID uuid.UUID) (*account.Account, error) {
	return s.oneAccount(ctx, `
		SELECT `+accountColumns+` FROM rig_account
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
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		SELECT `+accountColumns+` FROM rig_account
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

// LastAccountForIdentity implements [account.Store].
//
// Session roots only — `root_token_id = id` — because a root is one per sign-in
// where the rest of the family is one per rotation, and "where was I last" is a
// question about sign-ins. That predicate is also what the partial index in the
// last_tenant migration is on, so this is an index probe per account rather than
// a sort over every token an account has ever held. Consumed tokens are kept
// rather than deleted, so that difference only grows.
//
// The two joins filter what the caller would have to filter anyway: an account
// or a tenant that has gone away is not somewhere to land, and answering with
// one would land somebody where [AccountStore.TenantsForIdentity] does not list.
func (s *AccountStore) LastAccountForIdentity(
	ctx context.Context, identityID uuid.UUID,
) (*account.Account, error) {
	return s.oneAccount(ctx, `
		SELECT `+prefixed("a.", accountColumns)+`
		  FROM rig_account_token t
		  JOIN rig_account a ON a.id = t.account_id
		  JOIN rig_tenant  n ON n.id = a.tenant_id
		 WHERE a.identity_id = $1
		   AND t.root_token_id = t.id
		   AND a.deleted_at IS NULL AND a.is_active
		   AND n.deleted_at IS NULL AND n.is_active
		 ORDER BY t.created_at DESC
		 LIMIT 1`, identityID)
}

// TenantsForIdentity implements [account.Store].
//
// The one join in this package. It has no tenant predicate for the same reason
// [AccountStore.AccountsForIdentity] has none — the question is which tenants a
// person belongs to — and a deleted tenant is left out, because being a member of
// something that no longer exists is not somewhere anybody can go.
func (s *AccountStore) TenantsForIdentity(ctx context.Context, identityID uuid.UUID) ([]account.Membership, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		SELECT rig_tenant.id, rig_tenant.name, rig_tenant.slug,
		       rig_account.id, rig_account.role, rig_account.is_active
		  FROM rig_account
		  JOIN rig_tenant ON rig_tenant.id = rig_account.tenant_id
		 WHERE rig_account.identity_id = $1
		   AND rig_account.deleted_at IS NULL
		   AND rig_tenant.deleted_at IS NULL
		   AND rig_tenant.is_active
		 ORDER BY rig_tenant.name`, identityID)
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
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, sql, args...)
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

// CreateVerification implements [account.Store].
func (s *AccountStore) CreateVerification(ctx context.Context, v *account.Verification) error {
	var role *string
	if v.InvitedRole != nil {
		r := string(*v.InvitedRole)
		role = &r
	}

	_, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_identity_verification
			(id, identity_id, invited_to_tenant_id, kind, token_hash, created_at, expires_at,
			 invited_role, invited_display_name, invited_by_account_id, invited_by_api_key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		v.ID, v.IdentityID, v.InvitedToTenantID, string(v.Kind), v.TokenHash,
		v.CreatedAt, v.ExpiresAt,
		role, dbx.Null(v.InvitedDisplayName), v.InvitedByAccountID, v.InvitedByAPIKeyID)
	if err != nil {
		return fmt.Errorf("authpg: create verification: %w", err)
	}
	return nil
}

// PendingInvitations implements [account.Store].
//
// Live means not consumed, not revoked and not expired. It used to also mean
// "and for an account that is still there", because an account existed from the
// moment somebody was invited; there is none now until they accept, which is
// what makes this a list of people who are *not* in the tenant.
func (s *AccountStore) PendingInvitations(ctx context.Context, tenantID uuid.UUID) ([]account.Invitation, error) {
	return s.invitations(ctx,
		`v.invited_to_tenant_id = $1 AND v.expires_at > now()`, tenantID)
}

// InvitationsForIdentity implements [account.Store].
//
// No tenant predicate, deliberately, and it is the one query in this package
// where that is the whole point: somebody who belongs to no tenant is asking
// which ones have asked for them. Scoping it by tenant would be scoping it to a
// tenant they are not in yet.
func (s *AccountStore) InvitationsForIdentity(ctx context.Context, identityID uuid.UUID) ([]account.Invitation, error) {
	return s.invitations(ctx, `v.identity_id = $1 AND v.expires_at > now()`, identityID)
}

// InvitationByID implements [account.Store].
func (s *AccountStore) InvitationByID(ctx context.Context, id uuid.UUID) (*account.Invitation, error) {
	return s.oneInvitation(ctx, `v.id = $1`, id)
}

// InvitationByToken implements [account.Store].
func (s *AccountStore) InvitationByToken(ctx context.Context, hash []byte) (*account.Invitation, error) {
	return s.oneInvitation(ctx, `v.token_hash = $1`, hash)
}

// oneInvitation is [AccountStore.invitations] where at most one row can match.
//
// Neither of its callers filters on expiry, and that is deliberate: the service
// compares against its own clock — which a test can move — and a second opinion
// in SQL is how a double and the database come apart about the property under
// test.
func (s *AccountStore) oneInvitation(ctx context.Context, where string, args ...any) (*account.Invitation, error) {
	out, err := s.invitations(ctx, where, args...)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// invitations is the shared read behind every invitation query.
//
// The joins are what make a row useful — an interface listing invitations wants
// to say who and where, not which token hash. The inviter is a LEFT JOIN because
// two ordinary things make it absent: a key sent the invitation, or the person
// who sent it has since been removed. Either way the row still has to render.
func (s *AccountStore) invitations(ctx context.Context, where string, args ...any) ([]account.Invitation, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		SELECT v.id, v.identity_id, v.invited_to_tenant_id, rig_tenant.name,
		       rig_identity.email_address,
		       coalesce(nullif(v.invited_display_name, ''), rig_identity.display_name),
		       v.invited_role, v.invited_by_account_id, inviter.display_name,
		       v.created_at, v.expires_at
		  FROM rig_identity_verification v
		  JOIN rig_identity ON rig_identity.id = v.identity_id
		  JOIN rig_tenant   ON rig_tenant.id = v.invited_to_tenant_id
		  LEFT JOIN rig_account inviter ON inviter.id = v.invited_by_account_id
		                               AND inviter.deleted_at IS NULL
		 WHERE v.kind = 'Invitation'
		   AND v.consumed_at IS NULL
		   AND v.revoked_at IS NULL
		   AND `+where+`
		 ORDER BY v.created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("authpg: read invitations: %w", err)
	}
	defer rows.Close()

	var out []account.Invitation
	for rows.Next() {
		var (
			i  account.Invitation
			by *string
		)
		if err := rows.Scan(&i.ID, &i.IdentityID, &i.TenantID, &i.TenantName,
			&i.EmailAddress, &i.DisplayName, &i.Role,
			&i.InvitedByAccountID, &by,
			&i.CreatedAt, &i.ExpiresAt); err != nil {
			return nil, fmt.Errorf("authpg: scan invitation: %w", err)
		}
		i.InvitedByName = dbx.Deref(by)
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
	tag, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		UPDATE rig_identity_verification SET revoked_at = $2
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, id, at)
	if err != nil {
		return false, fmt.Errorf("authpg: revoke verification: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// VerificationByHash implements [account.Store].
func (s *AccountStore) VerificationByHash(ctx context.Context, hash []byte) (*account.Verification, error) {
	return s.verification(ctx, `token_hash = $1`, hash)
}

// VerificationByID implements [account.Store].
func (s *AccountStore) VerificationByID(ctx context.Context, id uuid.UUID) (*account.Verification, error) {
	return s.verification(ctx, `id = $1`, id)
}

func (s *AccountStore) verification(ctx context.Context, where string, args ...any) (*account.Verification, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		SELECT id, identity_id, invited_to_tenant_id, kind, token_hash,
		       created_at, expires_at, consumed_at, revoked_at, attempts,
		       invited_role, invited_display_name, invited_by_account_id,
		       invited_by_api_key_id
		FROM rig_identity_verification WHERE `+where, args...)
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
		role *string
		name *string
	)
	if err := rows.Scan(&v.ID, &v.IdentityID, &v.InvitedToTenantID, &kind, &v.TokenHash,
		&v.CreatedAt, &v.ExpiresAt, &v.ConsumedAt, &v.RevokedAt, &v.Attempts,
		&role, &name, &v.InvitedByAccountID, &v.InvitedByAPIKeyID); err != nil {
		return nil, fmt.Errorf("authpg: scan verification: %w", err)
	}
	v.Kind = account.VerificationKind(kind)
	if role != nil {
		r := account.Role(*role)
		v.InvitedRole = &r
	}
	if name != nil {
		v.InvitedDisplayName = *name
	}
	v.CreatedAt = dbx.UTC(v.CreatedAt)
	v.ExpiresAt = dbx.UTC(v.ExpiresAt)
	v.ConsumedAt = dbx.UTCPtr(v.ConsumedAt)
	v.RevokedAt = dbx.UTCPtr(v.RevokedAt)
	return &v, nil
}

// LiveVerification implements [account.Store].
func (s *AccountStore) LiveVerification(
	ctx context.Context, identityID uuid.UUID, kind account.VerificationKind,
) (*account.Verification, error) {
	// Newest first and one row, so that a second code asked for while the first
	// was still live is the one that works — which is what [account.Service]
	// promises, and the reason it revokes the older row rather than relying on
	// this ordering alone.
	return s.verification(ctx,
		`identity_id = $1 AND kind = $2
		   AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC LIMIT 1`, identityID, string(kind))
}

// ChargeVerificationAttempt implements [account.Store].
//
// One statement, which is the whole requirement. A read followed by a write is a
// window two concurrent guesses both fit through, and against a six-digit secret
// that window is free attempts.
//
// The revocation is in the same UPDATE rather than a second one, so a code that
// has just run out of attempts is dead by the time the row is unlocked. Revoked
// rather than a state of its own: account.Verification.Usable already refuses a
// revoked row and so does the mail queue's rotation, so nothing else has to
// learn that a ceiling exists.
func (s *AccountStore) ChargeVerificationAttempt(
	ctx context.Context, id uuid.UUID, max int, at time.Time,
) (int, bool, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		UPDATE rig_identity_verification
		   SET attempts   = attempts + 1,
		       revoked_at = CASE WHEN attempts + 1 >= $2 THEN $3 ELSE revoked_at END
		 WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL
		RETURNING attempts, revoked_at IS NOT NULL`, id, max, at)
	if err != nil {
		return 0, false, fmt.Errorf("authpg: charge verification attempt: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		// Consumed or revoked between the read that found it and this write. It
		// is dead either way, which is what the caller needs to know.
		return 0, true, rows.Err()
	}

	var (
		attempts int
		dead     bool
	)
	if err := rows.Scan(&attempts, &dead); err != nil {
		return 0, false, fmt.Errorf("authpg: scan verification attempt: %w", err)
	}
	return attempts, dead, nil
}

// ConsumeVerification implements [account.Store].
//
// The `consumed_at IS NULL` clause is what makes it single-use under
// concurrency: two requests racing to redeem one link both run the UPDATE, and
// exactly one of them affects a row.
func (s *AccountStore) ConsumeVerification(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		UPDATE rig_identity_verification SET consumed_at = $2
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

// InsertTenant implements [account.Store].
func (s *AccountStore) InsertTenant(ctx context.Context, t *account.Tenant) error {
	// The column is NOT NULL with an empty-array default, and a nil slice reaches
	// Postgres as NULL rather than as that default. "No restriction" is an empty
	// list, so it is written as one.
	domains := t.AllowedEmailDomains
	if domains == nil {
		domains = []string{}
	}

	_, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_tenant (id, created_at, name, slug, is_active, allowed_email_domains)
		VALUES ($1, now(), $2, $3, $4, $5)`,
		t.ID, t.Name, t.Slug, t.IsActive, domains)
	if err != nil {
		return fmt.Errorf("authpg: insert tenant: %w", err)
	}
	return nil
}
