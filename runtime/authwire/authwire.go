// Package authwire is what the authentication endpoints send and receive.
//
// It exists because there are two ends of that conversation and only one of them
// used to have a name for it. The shapes lived unexported inside rig/auth's HTTP
// handlers, so a client — the generated Go SDK, a TypeScript one, anything
// somebody writes by hand — had to restate them, and a restatement drifts
// silently: a field renamed on the server is a field a client quietly stops
// sending, and nothing fails to compile on either side.
//
// So the shapes live here, in a package with no dependencies worth speaking of,
// and both ends import it. The server fills them in; a client reads them.
//
// Nothing here is generated, and nothing here is per-project. These endpoints are
// rig's own: the same routes with the same bodies in every application that turns
// authentication on. What differs from one project to the next is which of them
// are mounted and how long the credentials last, and that is in the document —
// see [github.com/simonjanss/rig/pkg/ir.Auth].
package authwire

import (
	"time"

	"github.com/google/uuid"
)

// List is the envelope every collection here comes back in.
//
// One member, so that a list can grow a sibling — a count, a cursor — without the
// response changing shape from an array into an object, which is the change that
// breaks every caller at once.
type List[T any] struct {
	Data []T `json:"data"`
}

// TokenPair is what every endpoint that starts or continues a session returns.
type TokenPair struct {
	AccessToken  string    `json:"accessToken,omitempty"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt,omitzero"`
	// RefreshExpiresAt is when the session itself ends. A client needs both:
	// one says when to refresh, the other says when to stop trying.
	RefreshExpiresAt time.Time `json:"refreshExpiresAt,omitzero"`
	SessionID        uuid.UUID `json:"sessionId,omitzero"`
}

// SignInResponse is what a sign-in answers with.
//
// The pair is embedded rather than nested, so accessToken and the rest stay where
// they have always been: a client written against the old shape keeps working, and
// the new fields are additions it can ignore.
//
// The token fields are omitted entirely when there is no session — somebody who
// belongs to no tenant yet. An empty accessToken would look like a token and
// fail on first use; an absent one says what happened.
type SignInResponse struct {
	TokenPair

	// IdentityToken proves who somebody is and names no tenant. It is what
	// the tenant picker runs on, and it is issued even alongside a session,
	// because switching tenant later is the same flow.
	IdentityToken     string    `json:"identityToken"`
	IdentityExpiresAt time.Time `json:"identityExpiresAt"`

	// Tenants is every one this person belongs to, so a client can draw the
	// picker without a second call. Empty means they belong to none yet, which is
	// an ordinary state and no longer a refusal.
	Tenants []TenantView `json:"tenants"`
}

// LoginRequest is the body of POST <base>/login.
type LoginRequest struct {
	EmailAddress string `json:"emailAddress"`
	Password     string `json:"password"`
	// Remember asks for the longer session lifetime.
	Remember bool `json:"remember"`
	// Client is web, mobile or machine. Anything else is read as web.
	Client string `json:"client"`
}

// RefreshRequest is the body of POST <base>/refresh.
//
// The token travels in the body rather than the Authorization header. It is not
// the credential for anything else, and a header is how it ends up in an access
// log next to a hundred access tokens that expire in ten minutes.
type RefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// ResetRequest is the body of POST <base>/password/reset.
type ResetRequest struct {
	EmailAddress string `json:"emailAddress"`
}

// ConfirmResetRequest is the body of POST <base>/password/reset/confirm.
type ConfirmResetRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

// ChangePasswordRequest is the body of POST <base>/password/change.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// VerifyEmailRequest is the body of POST <base>/email/verify.
type VerifyEmailRequest struct {
	Token string `json:"token"`
}

// RegisterRequest is the body of POST <base>/register, where a stranger creates
// an account that belongs to no tenant yet.
type RegisterRequest struct {
	// EmailAddress is the identity. It is what a second registration with the
	// same address collides with, and what verification is sent to.
	EmailAddress string `json:"emailAddress"`
	DisplayName  string `json:"displayName"`
	Password     string `json:"password"`
}

// SessionView is one of the caller's sessions.
type SessionView struct {
	ID         uuid.UUID `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	IPAddress  string    `json:"ipAddress,omitempty"`
	UserAgent  string    `json:"userAgent,omitempty"`
	Client     string    `json:"client"`
	// Current marks the session making this request, so an interface can label
	// it rather than inviting somebody to revoke the tab they are looking at
	// without warning.
	Current bool `json:"current"`
}

// TenantView is one tenant somebody belongs to.
type TenantView struct {
	TenantID   uuid.UUID `json:"tenantId"`
	TenantName string    `json:"tenantName"`
	TenantSlug string    `json:"tenantSlug"`
	AccountID  uuid.UUID `json:"accountId"`
	Role       string    `json:"role"`
	// Current marks the tenant this request was made in, so an interface can
	// show where somebody is without comparing identifiers itself.
	Current bool `json:"current"`
}

// CreateTenantRequest is the body of POST <base>/tenants.
type CreateTenantRequest struct {
	Name string `json:"name"`
	// Client is web, mobile or machine, for the session the new tenant is
	// entered with.
	Client string `json:"client"`
}

// InvitationView is one invitation into the caller's tenant, seen by whoever
// administers it.
//
// It carries no token, for the same reason [InvitationToMeView] does not: the
// token is the credential that redeems the invitation, and an administrator who
// can list invitations is not thereby somebody who can accept one on another
// person's behalf.
type InvitationView struct {
	ID           uuid.UUID `json:"id"`
	EmailAddress string    `json:"emailAddress"`
	DisplayName  string    `json:"displayName"`
	// Role is the role the invitation grants on acceptance, not one the invited
	// person holds yet.
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	// ExpiresAt is when the token stops working. A listed invitation past it is
	// history, not something still waiting to be accepted.
	ExpiresAt time.Time `json:"expiresAt"`
}

// InvitationToMeView is one invitation waiting for the caller, seen by the person
// who was invited.
//
// It carries no token. An invitation's token is the credential that redeems it
// and it was sent to an address; listing tokens would turn "I can see my
// invitations" into "I can accept them", which is a different claim about who
// reached the mailbox.
type InvitationToMeView struct {
	ID uuid.UUID `json:"id"`
	// TenantName is the part that means anything to somebody who has not been
	// there. The identifier is for the accept call.
	TenantID   uuid.UUID `json:"tenantId"`
	TenantName string    `json:"tenantName"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// AcceptRequest is the body of POST <base>/invitations/accept, where the token
// from the invitation mail is the credential.
type AcceptRequest struct {
	Token string `json:"token"`
	// Password is only read when the person has none yet. Somebody joining a
	// second tenant already has one, and it is not this endpoint's business.
	Password string `json:"password"`
	Client   string `json:"client"`
}

// AcceptAsMeRequest is the body of POST <base>/me/invitations/accept, where the
// caller is already signed in and names the invitation by identifier.
type AcceptAsMeRequest struct {
	InvitationID uuid.UUID `json:"invitationId"`
	Client       string    `json:"client"`
}

// ImpersonateRequest is the body of POST <base>/impersonate.
type ImpersonateRequest struct {
	AccountID uuid.UUID `json:"accountId"`
}

// ProvisionRequest is the body of POST <base>/accounts.
type ProvisionRequest struct {
	EmailAddress string `json:"emailAddress"`
	DisplayName  string `json:"displayName"`

	// Kind and Role are optional: a person and Basic unless the caller says
	// otherwise. They are strings on the wire so that a client sends the same
	// words the database stores.
	Kind string `json:"kind"`
	Role string `json:"role"`

	TimeZone string `json:"timeZone"`

	// Invite sends a verification link so the person can set a password. It is
	// a request rather than the default, because provisioning during an import
	// of four thousand employees should not send four thousand emails.
	Invite bool `json:"invite"`
}

// AccountView is what a provisioning call comes back with. It is deliberately not
// the whole row: an account's audit trail is administration, and this answers "it
// exists now".
type AccountView struct {
	// ID names the account — the person inside this tenant. It is not the
	// identity: somebody in two tenants has one identity and two accounts, and
	// this is the one that was just created.
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenantId"`
	EmailAddress string    `json:"emailAddress"`
	DisplayName  string    `json:"displayName"`
	// Kind and Role are the values the database stores, as strings, so that a
	// client sends back the same words it was given.
	Kind string `json:"kind"`
	Role string `json:"role"`
	// TimeZone is an IANA name, for example Europe/Stockholm. Empty means UTC.
	TimeZone  string    `json:"timeZone,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// APIKeyView is one key, without the secret — which nothing stored can produce
// again.
type APIKeyView struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	KeyID string    `json:"keyId"`
	// Kind is Integration or Personal. A client cannot tell them apart without
	// it, and they behave differently enough that an administration screen
	// showing a list of keys has to say which is which.
	Kind          string     `json:"kind"`
	Scopes        []string   `json:"scopes"`
	CIDRAllowList []string   `json:"cidrAllowList,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt    *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
}

// CreateKeyRequest is the body of POST <base>/api-keys.
type CreateKeyRequest struct {
	Name          string     `json:"name"`
	Scopes        []string   `json:"scopes"`
	CIDRAllowList []string   `json:"cidrAllowList"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	// Kind is Integration or Personal, and defaults to Integration.
	//
	// A personal key acts as its creator, so it cannot name a service account —
	// the manager refuses that combination and so does a CHECK on the table. An
	// integration key is the default because it is the one whose writes stay
	// attributable after the person who set it up has left.
	Kind string `json:"kind"`
	// ServiceAccountID is who the key acts as. It defaults to the caller, which
	// is the common case and the one that keeps the writes attributable.
	ServiceAccountID *uuid.UUID `json:"serviceAccountId"`
}

// CreateKeyResponse is the only response that will ever contain the secret.
type CreateKeyResponse struct {
	Key APIKeyView `json:"key"`
	// Secret is shown exactly once. Nothing stored can produce it again, which
	// is what makes storing only a hash safe.
	Secret string `json:"secret"`
}
