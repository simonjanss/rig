package account

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/password"
)

// Identity is a person who can sign in, independent of where they work.
//
// One address, one password, one set of linked providers, no tenant. Somebody
// who belongs to two tenants has one identity and two accounts, and the identity
// is what a password, a reset link and a provider link all belong to.
type Identity struct {
	ID uuid.UUID

	// EmailAddress is stored as the person typed it and matched lowercased.
	// Showing somebody a differently-cased version of their own address is a
	// small rudeness with no upside. It is unique across every tenant.
	EmailAddress string
	// DisplayName is what to call the person before any tenant has an opinion.
	// An account may override it, and mail uses this one.
	DisplayName string

	// IsActive is whether the person may sign in anywhere at all. An account's
	// own IsActive is narrower: it removes somebody from one tenant.
	IsActive        bool
	EmailVerifiedAt *time.Time

	// CreatedBy and CreatedByKey are who asked for this identity, for the audit
	// columns. Both are optional: a seed creates identities with nobody to name.
	CreatedBy    *uuid.UUID
	CreatedByKey *uuid.UUID
}

// Verified reports whether the address has been confirmed.
func (i *Identity) Verified() bool { return i.EmailVerifiedAt != nil }

// Account is one person inside one tenant.
//
// It carries only what authentication needs. Everything else about somebody —
// their preferences, their avatar, whatever the application is actually for —
// lives in the generated model, which is the same row read through a different
// lens.
type Account struct {
	ID       uuid.UUID
	TenantID uuid.UUID

	// IdentityID is the person this account belongs to, and nil exactly when
	// this is a service account — which is nobody, has no credential, and cannot
	// sign in.
	IdentityID *uuid.UUID

	// EmailAddress is a copy of the identity's, kept here so that listing the
	// people in a tenant is one query. For a service account it is a label.
	EmailAddress string
	DisplayName  string

	// Kind is whether this is a person or a service account an integration acts
	// as. A service account has no credential and cannot sign in, which Login
	// enforces rather than leaving to whoever wrote the row.
	Kind Kind

	// Role is the coarse level in this tenant: Owner, Admin or Basic. Finer
	// grants come from roles and permissions, and both reach a caller through
	// their claims. It is per account rather than per identity, so somebody can
	// be an Owner in one tenant and Basic in another.
	Role Role

	// TimeZone is an IANA name, for example Europe/Stockholm. Empty means UTC.
	//
	// It lives on the account because it is a property of the person rather than
	// of a request: a report somebody schedules for 9am means 9am where they are,
	// and a browser's offset is not available when nobody is looking at one.
	TimeZone string

	// IsActive is whether the account may be used. It is narrower than the
	// identity's: this removes somebody from one tenant.
	IsActive bool

	// CreatedBy and CreatedByKey are who asked for this account, for the audit
	// columns. Both are optional: rig itself creates accounts during a seed with
	// nobody to name.
	CreatedBy    *uuid.UUID
	CreatedByKey *uuid.UUID
}

// Tenant is the isolation boundary every generated query scopes by.
//
// Only what creating one needs. A tenant is otherwise not this package's to
// describe: it is an ordinary table, and an application that wants to read or
// change one does it through its own code — `auth.expose: [tenant]` in rig.yaml
// puts it in the generated set for an administration screen.
type Tenant struct {
	ID   uuid.UUID
	Name string
	// Slug is the URL-safe name, unique across every tenant.
	Slug string
	// AllowedEmailDomains restricts who may be provisioned into it. Empty means
	// no restriction.
	AllowedEmailDomains []string
	IsActive            bool
}

// Kind is what an account is.
type Kind string

const (
	// KindPerson signs in with a password or a provider.
	KindPerson Kind = "Person"
	// KindService is what an integration's key acts as. It has no credential, so
	// there is nothing to phish and nothing to reset.
	KindService Kind = "Service"
)

// Role is an account's coarse level.
//
// Three, because almost every product ends up with these three and inventing a
// permission taxonomy on day one is how a project ends up with fourteen. The
// role and permission tables are there for the day three is not enough.
type Role string

const (
	// RoleOwner may do anything, including the things that end the account:
	// billing, deleting the tenant, removing the last other owner.
	RoleOwner Role = "Owner"
	// RoleAdmin administers the tenant — inviting, configuring, managing keys —
	// without the decisions that are the owner's to make.
	RoleAdmin Role = "Admin"
	// RoleBasic gets on with the work.
	RoleBasic Role = "Basic"
)

// Person reports whether an account belongs to somebody who can sign in.
func (a *Account) Person() bool { return a.Kind == KindPerson && a.IdentityID != nil }

// Location is where the account is, for formatting a time.
//
// UTC when the zone is empty or unknown, rather than an error: a bad zone name
// is a data problem and refusing to render a page over one helps nobody.
func (a *Account) Location() *time.Location {
	if a.TimeZone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(a.TimeZone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Credential is a person's password.
//
// One per identity rather than one per account, so somebody in three tenants has
// one password to remember and one password to change.
type Credential struct {
	ID         uuid.UUID
	IdentityID uuid.UUID

	PasswordHash string
	Algorithm    string
	Params       password.Params

	CreatedAt time.Time
	UpdatedAt *time.Time
}

// VerificationKind is what a single-use link is for.
type VerificationKind string

const (
	// KindEmailVerification confirms an address belongs to whoever gave it.
	KindEmailVerification VerificationKind = "EmailVerification"
	// KindPasswordReset lets somebody set a password without knowing the old one.
	KindPasswordReset VerificationKind = "PasswordReset"
	// KindInvitation brings a person into a tenant, whether or not they already
	// have an identity.
	KindInvitation VerificationKind = "Invitation"
)

// Verification is a single-use link.
//
// Only the hash is stored, for the same reason a password is not stored: a
// reset link is a credential for the few minutes it lives, and a database dump
// that contains live ones is a database dump that hands over every account.
type Verification struct {
	ID         uuid.UUID
	IdentityID uuid.UUID

	// InvitedToTenantID is the tenant an invitation is into, and nil for a link
	// about the person rather than one tenant — confirming their address or
	// resetting their password, both of which are global.
	InvitedToTenantID *uuid.UUID

	Kind      VerificationKind
	TokenHash []byte

	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time

	// RevokedAt is when the link was cancelled, which is not the same as used.
	// An invitation somebody withdrew and one somebody accepted are different
	// things to find in an audit trail, so the table keeps both — the same
	// distinction rig_account_token makes between a rotation and a revocation.
	RevokedAt *time.Time
}

// Usable reports whether the link may still be redeemed.
func (v *Verification) Usable(now time.Time) bool {
	return v.ConsumedAt == nil && v.RevokedAt == nil && now.Before(v.ExpiresAt)
}

// Membership is one tenant a person belongs to, and who they are in it.
//
// The pair is the point: the tenant is where, and the account is who — the same
// person is an Owner in one tenant and Basic in another, and a session is
// issued for one account rather than for the person.
//
// Named for the relationship rather than for either end, because [Tenant] is the
// row and this is not: two people in one tenant are two memberships.
type Membership struct {
	TenantID   uuid.UUID
	TenantName string
	TenantSlug string

	AccountID uuid.UUID
	Role      Role
	IsActive  bool
}

// Invitation is a live invitation, with enough about the person to show it.
type Invitation struct {
	ID         uuid.UUID
	IdentityID uuid.UUID
	AccountID  uuid.UUID
	TenantID   uuid.UUID
	// TenantName is which tenant it is into. An invitation listed to the
	// person receiving it is otherwise a row of identifiers: they have not been
	// there yet, so the name is the only part they recognise.
	TenantName string

	EmailAddress string
	DisplayName  string
	Role         Role

	CreatedAt time.Time
	ExpiresAt time.Time
}

// DeleteAccountInput removes somebody from a tenant.
type DeleteAccountInput struct {
	TenantID  uuid.UUID
	AccountID uuid.UUID

	At time.Time
	// ByAccountID and ByAPIKeyID are who did it, for the audit columns — who,
	// and through what.
	ByAccountID *uuid.UUID
	ByAPIKeyID  *uuid.UUID
}

// Store is the persistence the flows need.
//
// An application implements it over the generated repositories, which is a
// short adapter: every method here is one query against a table
// `rig setup-project` created.
type Store interface {
	// FindIdentityByEmail returns the person with a lowercased address, or nil
	// when there is none. It looks across every tenant, because an address is one
	// person. A missing identity is not an error: it is the ordinary answer to
	// somebody mistyping their address.
	FindIdentityByEmail(ctx context.Context, lowercased string) (*Identity, error)
	// FindIdentityByID returns a person, or nil.
	FindIdentityByID(ctx context.Context, id uuid.UUID) (*Identity, error)
	// InsertIdentity writes a new person.
	InsertIdentity(ctx context.Context, i *Identity) error
	// MarkIdentityVerified records that an address was confirmed. The address
	// belongs to the person, so confirming it counts in every tenant at once.
	MarkIdentityVerified(ctx context.Context, identityID uuid.UUID, at time.Time) error

	// FindByID returns an account, or nil.
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*Account, error)
	// AccountForIdentity returns a person's account in one tenant, or nil when
	// they do not belong to it.
	AccountForIdentity(ctx context.Context, tenantID, identityID uuid.UUID) (*Account, error)
	// AccountsForIdentity returns every account a person has, in every tenant.
	//
	// It is what makes a password change mean what it says: the credential is
	// global, so ending "every session" has to reach the tenants the request was
	// not made from.
	AccountsForIdentity(ctx context.Context, identityID uuid.UUID) ([]*Account, error)

	// TenantsForIdentity is the same set with the tenant's name attached, for
	// showing somebody where they can go.
	TenantsForIdentity(ctx context.Context, identityID uuid.UUID) ([]Membership, error)

	// Insert writes a new account.
	//
	// It is separate from the rest because creating one is not a flow this
	// package owns end to end: who may join a tenant is a product decision,
	// and Provision is the part that is the same everywhere — the address is
	// checked, the tenant's domains are honoured, nothing is written twice, and
	// no credential comes into existence by accident.
	Insert(ctx context.Context, a *Account) error

	// InsertTenant writes a tenant row.
	InsertTenant(ctx context.Context, t *Tenant) error

	// TenantDomains are the email domains this tenant's accounts may use, or
	// empty for no restriction.
	TenantDomains(ctx context.Context, tenantID uuid.UUID) ([]string, error)

	// Credential returns a person's password, or nil when they have none —
	// which is the case for somebody who only ever signed in through a
	// provider.
	Credential(ctx context.Context, identityID uuid.UUID) (*Credential, error)
	// SaveCredential creates or replaces a person's password.
	SaveCredential(ctx context.Context, c *Credential) error

	CreateVerification(ctx context.Context, v *Verification) error
	// PendingInvitations are the live invitations into one tenant: minted, not
	// redeemed, not withdrawn, not expired. It is what an interface lists so
	// somebody can change their mind.
	PendingInvitations(ctx context.Context, tenantID uuid.UUID) ([]Invitation, error)
	// InvitationsForIdentity are the live invitations addressed to one person,
	// across every tenant. The other side of PendingInvitations: that one is an
	// administrator looking at their tenant, this is somebody looking at
	// where they have been asked to go — which is the only thing they can see
	// before they belong anywhere.
	InvitationsForIdentity(ctx context.Context, identityID uuid.UUID) ([]Invitation, error)
	// RevokeVerification cancels a link. It must be a no-op on one that is
	// already consumed or revoked, and report whether it changed anything, so
	// two requests racing cannot both claim to have withdrawn it.
	RevokeVerification(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)
	// SoftDeleteAccount removes somebody from a tenant, recording who did it.
	SoftDeleteAccount(ctx context.Context, in DeleteAccountInput) error
	// VerificationByHash finds a link by the hash of its token, or nil.
	VerificationByHash(ctx context.Context, hash []byte) (*Verification, error)
	// VerificationByID finds one by identifier, or nil.
	//
	// The by-identifier lookup exists for accepting an invitation from the
	// picker, where the caller is already signed in as the person it was
	// addressed to. The token proves somebody reached the mailbox; a session
	// proves who they are, which is the stronger claim of the two — so the
	// identifier is enough, and it is the only thing a listing hands out.
	VerificationByID(ctx context.Context, id uuid.UUID) (*Verification, error)
	// ConsumeVerification marks a link used. It must be a no-op on a link that
	// is already consumed, so that two requests racing to redeem one cannot
	// both win.
	ConsumeVerification(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)

	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Notifier delivers the links this package mints.
//
// Sending mail is the application's business: it knows the templates, the
// sender, the locale, and whether it uses a queue. What rig knows is when a
// link exists and what it says.
// It takes an identity rather than an account because that is what a link is
// about: the address being confirmed and the password being reset belong to the
// person, not to one of the tenants they work in.
//
// An invitation is the exception and takes the account too, because it is about
// one tenant: the mail has to say which tenant somebody is being invited to,
// and "you have been invited" with no answer to "invited where" is a mail nobody
// can act on.
type Notifier interface {
	SendPasswordReset(ctx context.Context, i *Identity, token string) error
	SendEmailVerification(ctx context.Context, i *Identity, token string) error
	SendInvitation(ctx context.Context, i *Identity, a *Account, token string) error
}

// NoNotifier drops every link.
//
// It is the default so that a manager can be built in one line during
// development. In production it means nobody can ever reset a password, which
// is why [Service] says so out loud when it is used.
type NoNotifier struct{}

// SendPasswordReset implements [Notifier].
func (NoNotifier) SendPasswordReset(context.Context, *Identity, string) error { return nil }

// SendEmailVerification implements [Notifier].
func (NoNotifier) SendEmailVerification(context.Context, *Identity, string) error { return nil }

// SendInvitation implements [Notifier].
func (NoNotifier) SendInvitation(context.Context, *Identity, *Account, string) error { return nil }
