package account

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Identity is a person who can sign in, independent of where they work.
//
// One address, one set of linked providers, no tenant. Somebody who belongs to
// two tenants has one identity and two accounts, and the identity is what a
// sign-in code, an invitation and a provider link all belong to.
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
	// this is a service account — which is nobody, so there is nothing to sign
	// in as.
	IdentityID *uuid.UUID

	// EmailAddress is a copy of the identity's, kept here so that listing the
	// people in a tenant is one query. For a service account it is a label.
	EmailAddress string
	DisplayName  string

	// Kind is whether this is a person or a service account an integration acts
	// as. A service account cannot sign in, which every sign-in path enforces
	// rather than leaving to whoever wrote the row.
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
	// KindPerson signs in with a provider or a mailed code.
	KindPerson Kind = "Person"
	// KindService is what an integration's key acts as. It has no identity, so
	// there is nothing to phish and nothing to sign in as.
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

// VerificationKind is what a single-use secret is for.
type VerificationKind string

const (
	// KindEmailVerification confirms an address belongs to whoever gave it.
	KindEmailVerification VerificationKind = "EmailVerification"
	// KindEmailCode is a short code mailed to an address and typed back, which
	// is how somebody with no provider account signs in.
	//
	// The one kind whose secret is guessable, which is why it is the only one
	// with an attempt ceiling and the only one not found by its hash — see
	// [Verification.Attempts] and [Store.LiveCodeFor].
	KindEmailCode VerificationKind = "EmailCode"
	// KindInvitation is a pending membership. Accepting it is what creates the
	// account in the tenant, so the row carries everything that account will be
	// made from.
	KindInvitation VerificationKind = "Invitation"
)

// Verification is a single-use secret.
//
// Only the hash is stored. A sign-in code is a live credential for the few
// minutes it lasts, and a database dump containing live ones is a database dump
// that hands over every account in it.
type Verification struct {
	ID         uuid.UUID
	IdentityID uuid.UUID

	// InvitedToTenantID is the tenant an invitation is into, and nil for a row
	// about the person rather than one tenant — confirming their address or
	// signing them in, both of which are global.
	InvitedToTenantID *uuid.UUID

	Kind      VerificationKind
	TokenHash []byte

	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time

	// RevokedAt is when the row was cancelled, which is not the same as used.
	// An invitation somebody withdrew and one somebody accepted are different
	// things to find in an audit trail, so the table keeps both — the same
	// distinction rig_account_token makes between a rotation and a revocation.
	RevokedAt *time.Time

	// Attempts is how many wrong codes have been offered for this row, and it
	// matters for [KindEmailCode] alone. Six digits are guessable in a way a
	// 32-byte token is not, so a code has to die after a handful of tries rather
	// than merely be slowed down by a rate limit.
	Attempts int

	// What an invitation carries, and nil for every other kind. It is what
	// accepting creates the account from, which is what makes an invitation a
	// pending membership rather than a mail about a membership that already
	// exists.
	//
	// A database CHECK ties InvitedRole and InvitedToTenantID to the kind, so
	// an invitation with neither cannot be written at all.
	InvitedRole        *Role
	InvitedDisplayName string
	// InvitedByAccountID and InvitedByAPIKeyID are who asked. They become the
	// created_by pair of the account accepting creates, so the provenance
	// survives the invitation being consumed.
	InvitedByAccountID *uuid.UUID
	InvitedByAPIKeyID  *uuid.UUID
}

// Usable reports whether the secret may still be redeemed.
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
	TenantID   uuid.UUID
	// TenantName is which tenant it is into. An invitation listed to the
	// person receiving it is otherwise a row of identifiers: they have not been
	// there yet, so the name is the only part they recognise.
	TenantName string

	EmailAddress string
	// DisplayName is what the inviter called them, falling back to the name the
	// person already has when the inviter said nothing.
	DisplayName string
	// Role is what accepting will grant, which is worth showing: being invited
	// as an Admin is something to know before pressing the button.
	Role Role

	// InvitedByAccountID and InvitedByName are who sent it. The name is empty
	// when a key sent it, and when the person who did has since been removed —
	// both of which a landing page has to render rather than fail on.
	InvitedByAccountID *uuid.UUID
	InvitedByName      string

	CreatedAt time.Time
	ExpiresAt time.Time
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
	// It is what makes "sign me out everywhere" mean what it says: a person is
	// global and their sessions are not, so ending every one of them has to reach
	// the tenants the request was not made from.
	AccountsForIdentity(ctx context.Context, identityID uuid.UUID) ([]*Account, error)

	// LastAccountForIdentity is the account a person most recently held a
	// session for, or nil when there is no record of one.
	//
	// It is what makes a sign-in that names no tenant land where somebody was
	// rather than where they started — see [Service.SignInIdentity]. The
	// account rather than the tenant, because an account is a person in a
	// tenant and the caller needs the row either way.
	//
	// It must skip an account or a tenant that is no longer active, for the
	// same reason [Store.AccountsForIdentity]'s caller does: landing somebody
	// somewhere [Store.TenantsForIdentity] will not list is worse than landing
	// them nowhere.
	LastAccountForIdentity(ctx context.Context, identityID uuid.UUID) (*Account, error)

	// TenantsForIdentity is the same set with the tenant's name attached, for
	// showing somebody where they can go.
	//
	// Ordered for a person to read rather than to match whichever account a
	// sign-in landed in: which one that was is *marked* — see
	// [SignInResult.TenantID] — so the two orderings have nothing to disagree
	// about.
	TenantsForIdentity(ctx context.Context, identityID uuid.UUID) ([]Membership, error)

	// Insert writes a new account.
	//
	// It is separate from the rest because creating one is not a flow this
	// package owns end to end: who may join a tenant is a product decision,
	// and [Service.Provision] is the part that is the same everywhere — the
	// address is checked, the tenant's domains are honoured, and nothing is
	// written twice.
	Insert(ctx context.Context, a *Account) error

	// InsertTenant writes a tenant row.
	InsertTenant(ctx context.Context, t *Tenant) error

	// TenantDomains are the email domains this tenant's accounts may use, or
	// empty for no restriction.
	TenantDomains(ctx context.Context, tenantID uuid.UUID) ([]string, error)

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
	// InvitationByID and InvitationByToken are one invitation, assembled the
	// way the two listings assemble theirs.
	//
	// They exist for the two callers that hold one invitation rather than a
	// person's set of them: the mail dispatcher, which has a delivery and needs
	// the tenant's name to put in the mail, and the preview, which has a token
	// out of a URL and needs to say what it is for. Neither filters on expiry —
	// that is a comparison against the service's clock, and putting it in SQL is
	// how the in-memory double and the real store end up disagreeing.
	InvitationByID(ctx context.Context, id uuid.UUID) (*Invitation, error)
	InvitationByToken(ctx context.Context, hash []byte) (*Invitation, error)

	// RevokeVerification cancels a row. It must be a no-op on one that is
	// already consumed or revoked, and report whether it changed anything, so
	// two requests racing cannot both claim to have withdrawn it.
	RevokeVerification(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)
	// VerificationByHash finds a row by the hash of its token, or nil.
	VerificationByHash(ctx context.Context, hash []byte) (*Verification, error)
	// VerificationByID finds one by identifier, or nil.
	//
	// The by-identifier lookup exists for accepting an invitation from the
	// picker, where the caller is already signed in as the person it was
	// addressed to. The token proves somebody reached the mailbox; a session
	// proves who they are, which is the stronger claim of the two — so the
	// identifier is enough, and it is the only thing a listing hands out.
	VerificationByID(ctx context.Context, id uuid.UUID) (*Verification, error)
	// ConsumeVerification marks a row used. It must be a no-op on one that is
	// already consumed, so that two requests racing to redeem it cannot both
	// win.
	ConsumeVerification(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)

	// LiveVerification is the newest row of a kind for one person that may still
	// be redeemed, or nil.
	//
	// It is how a sign-in code is found, and the reason there are two lookups
	// rather than one: a request carries the address and the code, not a token,
	// and six digits cannot be a key. [Store.VerificationByHash] is for the
	// kinds whose secret is long enough to be one.
	LiveVerification(ctx context.Context, identityID uuid.UUID, kind VerificationKind) (*Verification, error)

	// ChargeVerificationAttempt charges one wrong guess against a row and
	// revokes it when that was its last, reporting the count after the charge
	// and whether the row is now dead.
	//
	// It must be one statement. A read followed by a write is a window two
	// concurrent guesses both fit through, which is a handful of free attempts
	// against a secret that only has a million values.
	//
	// Revoked rather than a state of its own: [Verification.Usable] already
	// refuses a revoked row and so does the mail queue, so nothing else has to
	// learn that a ceiling exists. A row burned by guessing and one an
	// administrator withdrew are told apart by [Verification.Attempts].
	ChargeVerificationAttempt(ctx context.Context, id uuid.UUID, max int, at time.Time) (attempts int, dead bool, err error)

	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Notifier delivers the secrets this package mints.
//
// Sending mail is the application's business: it knows the templates, the
// sender, the locale, and whether it uses a queue. What rig knows is when a
// link exists and what it says.
// It takes an identity rather than an account because that is what a link is
// about: the address being confirmed and the code being sent belong to the
// person, not to one of the tenants they work in.
//
// An invitation is the exception and takes the invitation too, because it is
// about one tenant: the mail has to say which tenant somebody is being invited
// to, who asked, and as what — "you have been invited" with no answer to
// "invited where" is a mail nobody can act on. It is the invitation rather than
// an account because there is no account yet, and that is the point of the
// flow: accepting is what creates one.
//
// **Where this is called from depends on [Config.Outbox].** Nil and it is the
// request that asked for the link, so a slow provider is a slow page. Set and it
// is [Service.DispatchMail], so a slow provider costs a lease and nothing else.
// The signature is the same either way, and turning the queue on changes no mail
// code.
//
// **The context carries a deadline when the queue is on** —
// [MailOptions.SendTimeout], thirty seconds by default — and honouring it is this
// method's job. rig cannot enforce it: what happens inside these calls is
// application code. Pass ctx to the request, and do not hand a provider SDK an
// http.Client with no timeout of its own, which is Go's default.
//
// **Do not deduplicate these at the provider.** It is otherwise good advice, and
// it is wrong here: every attempt carries a *different* token, because the
// queue rotates the secret before each send and the previous one stops working.
// Suppressing the second mail as a duplicate delivers a link that does not work.
// This is the one seam in rig where an idempotency key is the wrong answer —
// notify.Delivery.ID says the opposite, and means it.
type Notifier interface {
	// SendEmailCode delivers a short numeric code somebody types back to sign
	// in. It is the one secret here that a person reads out rather than clicks,
	// so the mail wants it legible and on its own line.
	SendEmailCode(ctx context.Context, i *Identity, code string) error
	SendEmailVerification(ctx context.Context, i *Identity, token string) error
	SendInvitation(ctx context.Context, i *Identity, inv *Invitation, token string) error
}

// NoNotifier drops every secret, silently and successfully.
//
// It is the default so that a manager can be built in one line during
// development. In production it means nobody can confirm an address and nothing
// anywhere says so — this type is substituted for a nil Notifier without a
// warning, which is a thing to know rather than a thing to rely on. Compare
// notify.NoSender, which refuses instead, having been written after somebody met
// this failure.
//
// [New] refuses two combinations rather than dropping them. A [Config.Outbox]
// with this as the notifier is a queue whose rows are written and then thrown
// away, which is a table that grows forever behind mail that never goes. And
// [EmailCodeOptions.Enabled] with this as the notifier is a sign-in nobody can
// ever complete — the only way in, minting codes into a void.
type NoNotifier struct{}

// SendEmailCode implements [Notifier].
func (NoNotifier) SendEmailCode(context.Context, *Identity, string) error { return nil }

// SendEmailVerification implements [Notifier].
func (NoNotifier) SendEmailVerification(context.Context, *Identity, string) error { return nil }

// SendInvitation implements [Notifier].
func (NoNotifier) SendInvitation(context.Context, *Identity, *Invitation, string) error {
	return nil
}
