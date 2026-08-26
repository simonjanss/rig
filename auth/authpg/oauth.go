package authpg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// OAuthStore keeps provider links in rig_identity_oauth.
type OAuthStore struct {
	db dbx.Conn
	tx dbx.Beginner
	// now is swappable for tests; nothing else touches it.
	now func() time.Time
}

var _ oauth.Store = (*OAuthStore)(nil)

// OAuth builds the store a sign-in needs.
//
// It is separate from [New] because a project can skip the OAuth part of the
// foundation, and a store over a table that does not exist is a runtime error
// waiting for its first sign-in.
func (s *Stores) OAuth() *OAuthStore {
	return &OAuthStore{db: s.Accounts.db, tx: s.Accounts.tx, now: time.Now}
}

const linkColumns = `id, identity_id, provider, subject, email_address`

// FindLink implements [oauth.Store].
func (s *OAuthStore) FindLink(ctx context.Context, provider, subject string) (*oauth.Link, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		SELECT `+linkColumns+` FROM rig_identity_oauth
		WHERE provider = $1 AND subject = $2`, provider, subject)
	if err != nil {
		return nil, fmt.Errorf("authpg: read provider link: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, rows.Err()
	}

	var l oauth.Link
	if err := rows.Scan(&l.ID, &l.IdentityID, &l.Provider, &l.Subject,
		&l.EmailAddress); err != nil {
		return nil, fmt.Errorf("authpg: scan provider link: %w", err)
	}
	return &l, nil
}

// FindIdentityByEmail implements [oauth.Store].
func (s *OAuthStore) FindIdentityByEmail(ctx context.Context, lowercased string) (uuid.UUID, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		SELECT id FROM rig_identity
		WHERE lower(email_address) = $1 AND deleted_at IS NULL`, lowercased)
	if err != nil {
		return uuid.Nil, fmt.Errorf("authpg: find identity: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return uuid.Nil, rows.Err()
	}

	var id uuid.UUID
	if err := rows.Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("authpg: scan identity: %w", err)
	}
	return id, nil
}

// LinkIdentity implements [oauth.Store].
//
// The conflict target is (provider, subject), which the foundation makes
// unique. Two sign-ins racing on a first link both insert; the second updates
// the address instead of failing, and both end up with the same row.
func (s *OAuthStore) LinkIdentity(ctx context.Context, in oauth.LinkInput) (*oauth.Link, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("authpg: generate link id: %w", err)
	}

	now := s.now()
	var out oauth.Link
	err = dbx.ConnFor(ctx, s.db).QueryRow(ctx, `
		INSERT INTO rig_identity_oauth
			(id, identity_id, provider, subject, email_address, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (provider, subject) DO UPDATE SET
			email_address = excluded.email_address,
			updated_at    = $6
		RETURNING `+linkColumns,
		id, in.IdentityID, in.Provider, in.Profile.Subject,
		strings.ToLower(in.Profile.EmailAddress), now).
		Scan(&out.ID, &out.IdentityID, &out.Provider, &out.Subject, &out.EmailAddress)
	if err != nil {
		return nil, fmt.Errorf("authpg: link identity: %w", err)
	}
	return &out, nil
}

// ProvisionIdentity implements [oauth.Store].
//
// The person and the link go in together. Half of this — an identity with no
// link — is somebody who can never sign in and never shows up in a support
// query, which is the worst kind of leftover.
//
// No tenant is involved and no domain is checked here, because nothing has been
// granted yet: an identity on its own reaches nothing. [OAuthStore.JoinTenant]
// is where the door is.
func (s *OAuthStore) ProvisionIdentity(ctx context.Context, in oauth.ProvisionInput) (*oauth.Link, error) {
	identityID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("authpg: generate identity id: %w", err)
	}

	now := s.now()
	var link *oauth.Link
	err = dbx.InTx(ctx, s.tx, func(ctx context.Context, tx dbx.Conn) error {
		var verified *time.Time
		if in.Profile.EmailVerified {
			// The provider already confirmed it, and asking the person to
			// confirm an address they just proved they control is the kind of
			// thing that makes people give up on a sign-up.
			verified = &now
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO rig_identity
				(id, created_at, email_address, display_name, email_verified_at)
			VALUES ($1, $2, $3, $4, $5)`,
			identityID, now, in.Profile.EmailAddress,
			displayName(in.Profile), verified); err != nil {
			return fmt.Errorf("authpg: provision identity: %w", err)
		}

		link, err = s.LinkIdentity(ctx, oauth.LinkInput{
			IdentityID: identityID, Provider: in.Provider, Profile: in.Profile,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return link, nil
}

// FindAccount implements [oauth.Store].
func (s *OAuthStore) FindAccount(ctx context.Context, tenantID, identityID uuid.UUID) (uuid.UUID, error) {
	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, `
		SELECT id FROM rig_account
		WHERE tenant_id = $1 AND identity_id = $2 AND deleted_at IS NULL`,
		tenantID, identityID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("authpg: find account: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return uuid.Nil, rows.Err()
	}

	var id uuid.UUID
	if err := rows.Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("authpg: scan account: %w", err)
	}
	return id, nil
}

// JoinTenant implements [oauth.Store].
//
// This is the door, so the tenant's domain list is checked here. A provider will
// happily authenticate anybody with a Google account, so "sign in with Google"
// plus provisioning is an open door into a customer's tenant unless something
// says which addresses belong there.
func (s *OAuthStore) JoinTenant(ctx context.Context, in oauth.JoinInput) (uuid.UUID, error) {
	domains, err := (&AccountStore{db: s.db, tx: s.tx}).TenantDomains(ctx, in.TenantID)
	if err != nil {
		return uuid.Nil, err
	}
	if !account.DomainAllowed(strings.ToLower(in.Profile.EmailAddress), domains) {
		return uuid.Nil, rigerr.Forbidden("%s is not in a domain this tenant allows",
			in.Profile.EmailAddress)
	}

	accountID, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("authpg: generate account id: %w", err)
	}

	if _, err := dbx.ConnFor(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_account
			(id, tenant_id, identity_id, created_at, email_address, display_name)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		accountID, in.TenantID, in.IdentityID, s.now(),
		in.Profile.EmailAddress, displayName(in.Profile)); err != nil {
		return uuid.Nil, fmt.Errorf("authpg: join tenant: %w", err)
	}
	return accountID, nil
}

// displayName is what to call somebody when the provider was vague about it.
func displayName(p oauth.Profile) string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.EmailAddress
}
