package rigtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Identity is a person, across every tenant.
type Identity struct {
	ID              uuid.UUID
	EmailAddress    string
	DisplayName     string
	IsActive        bool
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
}

// Verified reports whether the address has been confirmed, which is what
// auth.Config.RequireVerifiedEmail gates a sign-in on.
func (i Identity) Verified() bool { return i.EmailVerifiedAt != nil }

// Identity resolves an address to the person who has it, or nil.
//
// The address is global rather than per tenant, which is the fact worth knowing
// before asserting on anything else here: one address is one person, and the
// same person holds an account in each tenant they belong to.
func (r *Rig) Identity(tb testing.TB, emailAddress string) *Identity {
	tb.Helper()

	var i Identity
	err := r.Pool.QueryRow(context.Background(), `
		SELECT id, email_address, display_name, is_active, email_verified_at, created_at
		  FROM rig_identity
		 WHERE lower(email_address) = lower($1) AND deleted_at IS NULL`,
		emailAddress).Scan(&i.ID, &i.EmailAddress, &i.DisplayName,
		&i.IsActive, &i.EmailVerifiedAt, &i.CreatedAt)
	switch {
	case err == nil:
		return &i
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	default:
		tb.Fatalf("rigtest: %v", err)
		return nil
	}
}

// Identities counts every person this installation has, which is the assertion
// an identity gate is actually about: a refused stranger leaves no row.
func (r *Rig) Identities(tb testing.TB) int {
	tb.Helper()
	return one[int](tb, r, `SELECT count(*) FROM rig_identity WHERE deleted_at IS NULL`)
}

// Account is one person's membership of one tenant.
type Account struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	IdentityID   uuid.UUID
	EmailAddress string
	DisplayName  string
	Kind         string
	Role         string
	IsActive     bool
	CreatedAt    time.Time
}

// Accounts lists a tenant's people, newest last.
func (r *Rig) Accounts(tb testing.TB, tenantID uuid.UUID) []Account {
	tb.Helper()

	rows, err := r.Pool.Query(context.Background(), `
		SELECT id, tenant_id, identity_id, email_address, display_name,
		       kind::text, role::text, is_active, created_at
		  FROM rig_account
		 WHERE tenant_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at, id`, tenantID)
	if err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.TenantID, &a.IdentityID, &a.EmailAddress,
			&a.DisplayName, &a.Kind, &a.Role, &a.IsActive, &a.CreatedAt); err != nil {
			tb.Fatalf("rigtest: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	return out
}

// Account is one person's membership of one tenant, or nil when they have none.
//
// Nil is the state an invitation leaves somebody in: rig writes the identity and
// a pending membership, and accepting is what creates the account. So this
// answering nil while [Rig.Identity] answers a row is "invited and not yet
// arrived" rather than a mistake.
func (r *Rig) Account(tb testing.TB, tenantID, identityID uuid.UUID) *Account {
	tb.Helper()

	for _, a := range r.Accounts(tb, tenantID) {
		if a.IdentityID == identityID {
			return &a
		}
	}
	return nil
}

// Link is a provider account somebody signs in with.
type Link struct {
	IdentityID   uuid.UUID
	Provider     string
	Subject      string
	EmailAddress string
	CreatedAt    time.Time
}

// Links lists the providers one person has connected.
//
// A link is not scoped to a tenant, because a provider account belongs to a
// person. Two links on one identity is somebody who signed in with Google and
// later with GitHub, which is the case the address lookup exists to make work.
func (r *Rig) Links(tb testing.TB, identityID uuid.UUID) []Link {
	tb.Helper()

	rows, err := r.Pool.Query(context.Background(), `
		SELECT identity_id, provider::text, subject, email_address, created_at
		  FROM rig_identity_oauth
		 WHERE identity_id = $1
		 ORDER BY created_at, provider`, identityID)
	if err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	defer rows.Close()

	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.IdentityID, &l.Provider, &l.Subject,
			&l.EmailAddress, &l.CreatedAt); err != nil {
			tb.Fatalf("rigtest: %v", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	return out
}

// LastTenant is where somebody would land if they signed in now, or uuid.Nil.
//
// There is no column for this and it is the single read most likely to be
// written wrongly by hand: it is the tenant of the newest session *root* —
// `root_token_id = id` — because a root is one per sign-in and every refresh
// after it shares the root's family. Counting refreshes instead would make
// "where somebody was last" mean "where they were busiest".
func (r *Rig) LastTenant(tb testing.TB, identityID uuid.UUID) uuid.UUID {
	tb.Helper()

	id, ok := maybe[uuid.UUID](tb, r, `
		SELECT t.tenant_id
		  FROM rig_account_token t
		  JOIN rig_account a ON a.id = t.account_id
		 WHERE a.identity_id = $1
		   AND t.root_token_id = t.id
		 ORDER BY t.created_at DESC, t.id DESC
		 LIMIT 1`, identityID)
	if !ok {
		return uuid.Nil
	}
	return id
}

// AgeSessions moves every one of somebody's sessions back in time.
//
// It is here because the alternative is sleeping. "Where somebody was last" is
// decided by which session root is newest, so a test that signs in twice to prove
// the order needs the first sign-in to be older than the second — and two
// sign-ins in the same millisecond are not reliably ordered by a timestamp.
// Ageing the first is deterministic; a sleep is a slower way to be flaky.
func (r *Rig) AgeSessions(tb testing.TB, identityID uuid.UUID, by time.Duration) {
	tb.Helper()

	r.exec(tb, `
		UPDATE rig_account_token AS t
		   SET created_at = t.created_at - $2::interval
		  FROM rig_account a
		 WHERE a.id = t.account_id AND a.identity_id = $1`,
		identityID, by.String())
}
