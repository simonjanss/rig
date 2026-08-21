package rigclient

import (
	"context"
	"iter"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/simonjanss/rig/runtime/authwire"
)

// Auth is rig's own authentication endpoints.
//
// Nothing here is generated, because none of it varies: these are the same
// routes with the same bodies in every project that turns authentication on.
// What varies is which of them are mounted and how long the credentials last,
// and that arrives in the [AuthProfile] the generator emitted.
//
// Reached as Client.Auth on a generated client, and nil for a project with no
// authentication at all.
type Auth struct {
	rt      *Runtime
	profile AuthProfile
}

// Profile is what the document says about this API's authentication.
func (a *Auth) Profile() AuthProfile { return a.profile }

// path builds a route under the authentication base path.
func (a *Auth) path(rest string) string { return a.profile.BasePath + rest }

// WithTenant names the tenant a sign-in is for.
//
// A sign-in is the one call that cannot read the tenant from a credential —
// there is not one yet — so it is named here. Which header or parameter carries
// it is the project's decision, and it is in the profile.
func (a *Auth) WithTenant(tenantID uuid.UUID) CallOption {
	header := a.profile.TenantHeader
	if header == "" {
		header = "X-Tenant-Id"
	}
	return WithHeader(header, tenantID.String())
}

// withBearer presents a specific token instead of the client's credential.
//
// The identity-session endpoints need it: the credential that reaches them is
// the token somebody holds between signing in and picking a tenant, which is
// deliberately not the same thing as a session.
func withBearer(token string) CallOption {
	return func(c *call) {
		c.anonymous = true
		c.header.Set("Authorization", "Bearer "+token)
	}
}

// SignIn signs in and installs the resulting session on the client.
//
// It is [Auth.Login] plus the bookkeeping every caller would otherwise write:
// from here on every request carries the access token, and the session refreshes
// itself before that token expires.
//
// A person who belongs to no tenant gets a response with no session in it and an
// identity token instead — that is not a failure, it is the tenant picker. Check
// SignInResponse.AccessToken before assuming there is one.
func (a *Auth) SignIn(
	ctx context.Context, in authwire.LoginRequest, opts ...CallOption,
) (*authwire.SignInResponse, error) {
	res, err := a.Login(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	if res.AccessToken != "" {
		a.rt.Use(NewSession(res.TokenPair))
	}
	return res, nil
}

// Login signs in and returns the answer without installing anything.
func (a *Auth) Login(
	ctx context.Context, in authwire.LoginRequest, opts ...CallOption,
) (*authwire.SignInResponse, error) {
	// Anonymous deliberately: presenting an expired token to the endpoint that
	// would have replaced it is how a client gets stuck refusing to sign in.
	opts = append([]CallOption{Anonymous()}, opts...)
	return Do[authwire.SignInResponse](ctx, a.rt, Op{
		Name:   "authLogin",
		Method: http.MethodPost, Root: true, Path: a.path("/login"), Body: in,
	}, opts...)
}

// Logout ends the session the client is holding, and forgets it.
func (a *Auth) Logout(ctx context.Context, opts ...CallOption) error {
	if err := DoNoContent(ctx, a.rt, Op{
		Name:   "authLogout",
		Method: http.MethodPost, Root: true, Path: a.path("/logout"),
	}, opts...); err != nil {
		return err
	}
	a.rt.Use(nil)
	return nil
}

// Refresh exchanges a refresh token for a new pair.
//
// [Session] calls it; a caller rarely needs to. It is anonymous because the
// refresh token in the body is the credential, and the access token it is being
// swapped for is by then expired.
func (a *Auth) Refresh(
	ctx context.Context, refreshToken string, opts ...CallOption,
) (*authwire.TokenPair, error) {
	opts = append([]CallOption{Anonymous()}, opts...)
	return Do[authwire.TokenPair](ctx, a.rt, Op{
		Name:   "authRefresh",
		Method: http.MethodPost, Root: true, Path: a.path("/refresh"),
		Body: authwire.RefreshRequest{RefreshToken: refreshToken},
	}, opts...)
}

// Register creates an account that belongs to no tenant yet, and installs the
// session when the answer carries one.
func (a *Auth) Register(
	ctx context.Context, in authwire.RegisterRequest, opts ...CallOption,
) (*authwire.SignInResponse, error) {
	if !a.profile.HasRegistration {
		return nil, notMounted("POST "+a.path("/register"),
			"set auth.allow_registration in rig.yaml to open it")
	}

	opts = append([]CallOption{Anonymous()}, opts...)
	res, err := Do[authwire.SignInResponse](ctx, a.rt, Op{
		Name:   "authRegister",
		Method: http.MethodPost, Root: true, Path: a.path("/register"), Body: in,
	}, opts...)
	if err != nil {
		return nil, err
	}
	if res.AccessToken != "" {
		a.rt.Use(NewSession(res.TokenPair))
	}
	return res, nil
}

// Provision creates an account for somebody else, in the caller's tenant.
func (a *Auth) Provision(
	ctx context.Context, in authwire.ProvisionRequest, opts ...CallOption,
) (*authwire.AccountView, error) {
	return Do[authwire.AccountView](ctx, a.rt, Op{
		Name:   "authProvision",
		Method: http.MethodPost, Root: true, Path: a.path("/accounts"), Body: in,
	}, opts...)
}

// RequestPasswordReset asks for a reset link.
//
// It always succeeds, whether or not the address is registered: any difference
// in answer is the account enumeration this endpoint is most often used for.
func (a *Auth) RequestPasswordReset(
	ctx context.Context, emailAddress string, opts ...CallOption,
) error {
	opts = append([]CallOption{Anonymous()}, opts...)
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authRequestPasswordReset",
		Method: http.MethodPost, Root: true, Path: a.path("/password/reset"),
		Body: authwire.ResetRequest{EmailAddress: emailAddress},
	}, opts...)
}

// ConfirmPasswordReset sets a new password using the token from the mail.
func (a *Auth) ConfirmPasswordReset(
	ctx context.Context, token, newPassword string, opts ...CallOption,
) error {
	opts = append([]CallOption{Anonymous()}, opts...)
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authConfirmPasswordReset",
		Method: http.MethodPost, Root: true, Path: a.path("/password/reset/confirm"),
		Body: authwire.ConfirmResetRequest{Token: token, NewPassword: newPassword},
	}, opts...)
}

// ChangePassword changes the caller's own, and installs the pair that comes
// back — the old one was revoked along with the password.
func (a *Auth) ChangePassword(
	ctx context.Context, in authwire.ChangePasswordRequest, opts ...CallOption,
) (*authwire.TokenPair, error) {
	pair, err := Do[authwire.TokenPair](ctx, a.rt, Op{
		Name:   "authChangePassword",
		Method: http.MethodPost, Root: true, Path: a.path("/password/change"), Body: in,
	}, opts...)
	if err != nil {
		return nil, err
	}
	a.adopt(pair)
	return pair, nil
}

// VerifyEmail confirms an address with the token from the mail.
func (a *Auth) VerifyEmail(ctx context.Context, token string, opts ...CallOption) error {
	opts = append([]CallOption{Anonymous()}, opts...)
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authVerifyEmail",
		Method: http.MethodPost, Root: true, Path: a.path("/email/verify"),
		Body: authwire.VerifyEmailRequest{Token: token},
	}, opts...)
}

// ResendVerification sends the verification mail again.
func (a *Auth) ResendVerification(ctx context.Context, opts ...CallOption) error {
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authResendVerification",
		Method: http.MethodPost, Root: true, Path: a.path("/email/verify/resend"),
	}, opts...)
}

// Tenants lists every tenant the caller belongs to, reached with a session.
func (a *Auth) Tenants(ctx context.Context, opts ...CallOption) ([]authwire.TenantView, error) {
	return list[authwire.TenantView](ctx, a.rt, a.path("/tenants"), opts)
}

// SwitchTenant issues a session for another tenant the caller belongs to, and
// installs it.
func (a *Auth) SwitchTenant(
	ctx context.Context, tenantID uuid.UUID, opts ...CallOption,
) (*authwire.TokenPair, error) {
	pair, err := Do[authwire.TokenPair](ctx, a.rt, Op{
		Name:   "authSwitchTenant",
		Method: http.MethodPost, Root: true,
		Path: a.path("/tenants/" + PathValue(tenantID.String()) + "/switch"),
	}, opts...)
	if err != nil {
		return nil, err
	}
	a.adopt(pair)
	return pair, nil
}

// CreateTenant makes a tenant and signs the holder of an identity token into it.
func (a *Auth) CreateTenant(
	ctx context.Context, identityToken string, in authwire.CreateTenantRequest, opts ...CallOption,
) (*authwire.SignInResponse, error) {
	if !a.profile.HasTenantCreation {
		return nil, notMounted("POST "+a.path("/tenants"),
			"set auth.allow_tenant_creation in rig.yaml to open it")
	}

	opts = append([]CallOption{withBearer(identityToken)}, opts...)
	res, err := Do[authwire.SignInResponse](ctx, a.rt, Op{
		Name:   "authCreateTenant",
		Method: http.MethodPost, Root: true, Path: a.path("/tenants"), Body: in,
	}, opts...)
	if err != nil {
		return nil, err
	}
	if res.AccessToken != "" {
		a.rt.Use(NewSession(res.TokenPair))
	}
	return res, nil
}

// MyTenants lists the tenants the holder of an identity token belongs to. It is
// the picker somebody sees between signing in and choosing where to be.
func (a *Auth) MyTenants(
	ctx context.Context, identityToken string, opts ...CallOption,
) ([]authwire.TenantView, error) {
	if err := a.needsIdentity(); err != nil {
		return nil, err
	}
	opts = append([]CallOption{withBearer(identityToken)}, opts...)
	return list[authwire.TenantView](ctx, a.rt, a.path("/me/tenants"), opts)
}

// MyInvitations lists the invitations waiting for the holder of an identity
// token, in every tenant.
func (a *Auth) MyInvitations(
	ctx context.Context, identityToken string, opts ...CallOption,
) ([]authwire.InvitationToMeView, error) {
	if err := a.needsIdentity(); err != nil {
		return nil, err
	}
	opts = append([]CallOption{withBearer(identityToken)}, opts...)
	return list[authwire.InvitationToMeView](ctx, a.rt, a.path("/me/invitations"), opts)
}

// AcceptMyInvitation joins the tenant an invitation names, and installs the
// session that comes back.
func (a *Auth) AcceptMyInvitation(
	ctx context.Context, identityToken string, invitationID uuid.UUID, client string,
	opts ...CallOption,
) (*authwire.SignInResponse, error) {
	if err := a.needsIdentity(); err != nil {
		return nil, err
	}

	opts = append([]CallOption{withBearer(identityToken)}, opts...)
	res, err := Do[authwire.SignInResponse](ctx, a.rt, Op{
		Name:   "authAcceptMyInvitation",
		Method: http.MethodPost, Root: true, Path: a.path("/me/invitations/accept"),
		Body: authwire.AcceptAsMeRequest{InvitationID: invitationID, Client: client},
	}, opts...)
	if err != nil {
		return nil, err
	}
	if res.AccessToken != "" {
		a.rt.Use(NewSession(res.TokenPair))
	}
	return res, nil
}

// EndIdentitySession signs somebody out of the picker.
func (a *Auth) EndIdentitySession(
	ctx context.Context, identityToken string, opts ...CallOption,
) error {
	if err := a.needsIdentity(); err != nil {
		return err
	}
	opts = append([]CallOption{withBearer(identityToken)}, opts...)
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authEndIdentitySession",
		Method: http.MethodDelete, Root: true, Path: a.path("/me/session"),
	}, opts...)
}

// AcceptInvitation redeems an invitation with the token from the mail, and
// installs the session it answers with.
//
// The token is the credential, for one use, so this needs none of its own.
func (a *Auth) AcceptInvitation(
	ctx context.Context, in authwire.AcceptRequest, opts ...CallOption,
) (*authwire.TokenPair, error) {
	opts = append([]CallOption{Anonymous()}, opts...)
	pair, err := Do[authwire.TokenPair](ctx, a.rt, Op{
		Name:   "authAcceptInvitation",
		Method: http.MethodPost, Root: true, Path: a.path("/invitations/accept"), Body: in,
	}, opts...)
	if err != nil {
		return nil, err
	}
	if pair.AccessToken != "" {
		a.rt.Use(NewSession(*pair))
	}
	return pair, nil
}

// Invitations lists the live invitations into the caller's tenant.
func (a *Auth) Invitations(
	ctx context.Context, opts ...CallOption,
) ([]authwire.InvitationView, error) {
	return list[authwire.InvitationView](ctx, a.rt, a.path("/invitations"), opts)
}

// RevokeInvitation withdraws one.
func (a *Auth) RevokeInvitation(
	ctx context.Context, invitationID uuid.UUID, opts ...CallOption,
) error {
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authRevokeInvitation",
		Method: http.MethodDelete, Root: true,
		Path: a.path("/invitations/" + PathValue(invitationID.String())),
	}, opts...)
}

// Sessions lists the caller's own sessions, with the one making this request
// marked.
//
// Pass [Wide] for every session open in the tenant, which needs
// `session.read.all`. There is one method rather than two, for the reason the
// parameter exists at all: two endpoints would let a client written against the
// narrow one keep working, silently, when its credential was widened.
func (a *Auth) Sessions(ctx context.Context, opts ...CallOption) ([]authwire.SessionView, error) {
	return list[authwire.SessionView](ctx, a.rt, a.path("/sessions"), opts)
}

// RevokeSession ends one of them.
//
// Pass [Wide] to end somebody else's, which needs `session.revoke.all`. Without
// it — and with it, for a session in another tenant — a session that is not the
// caller's answers NotFound rather than Forbidden, so an identifier cannot be
// probed for existence.
func (a *Auth) RevokeSession(ctx context.Context, id uuid.UUID, opts ...CallOption) error {
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authRevokeSession",
		Method: http.MethodDelete, Root: true, Path: a.path("/sessions/" + PathValue(id.String())),
	}, opts...)
}

// AuditQuery narrows the authentication trail.
//
// Every field is optional and a nil one does not narrow. AccountID is only
// answerable alongside [Wide]: without it the trail is the caller's own and
// naming somebody else is refused rather than quietly ignored.
type AuditQuery struct {
	AccountID *uuid.UUID
	Event     *string
	Outcome   *string
	Since     *time.Time
	Until     *time.Time

	// Limit is the page size, defaulting to 50 and capped at 500. Offset is
	// where the page starts.
	Limit  *int
	Offset *int
}

// Values renders the query.
func (q AuditQuery) Values() url.Values {
	v := url.Values{}
	SetUUID(v, "accountId", q.AccountID)
	SetString(v, "event", q.Event)
	SetString(v, "outcome", q.Outcome)
	SetTime(v, "since", q.Since)
	SetTime(v, "until", q.Until)
	SetInt(v, "limit", q.Limit)
	SetInt(v, "offset", q.Offset)
	return v
}

// AuditLog reads one page of the authentication trail, newest first.
//
// The caller's own events by default, which needs no permission; the whole
// tenant's with [Wide], which needs `authlog.read.all`. What neither reaches is
// the attempts that resolved to no tenant — a sign-in that named none, or one
// against an address with no account anywhere. Those are recorded and no tenant
// has the standing to read them.
func (a *Auth) AuditLog(
	ctx context.Context, q AuditQuery, opts ...CallOption,
) (*authwire.Page[authwire.AuthLogEntryView], error) {
	return Do[authwire.Page[authwire.AuthLogEntryView]](ctx, a.rt, Op{
		Name:   "authAuditLog",
		Method: http.MethodGet, Root: true, Path: a.path("/audit"), Query: q.Values(),
	}, opts...)
}

// AuditLogAll walks the trail a page at a time.
//
// The query's own limit is the page size and its offset is where the walk
// starts. Iteration stops at the first failure, which arrives as the second
// value of the last pair — so a loop that ignores it is a loop that silently
// stops early.
func (a *Auth) AuditLogAll(
	ctx context.Context, q AuditQuery, opts ...CallOption,
) iter.Seq2[authwire.AuthLogEntryView, error] {
	start := 0
	if q.Offset != nil {
		start = *q.Offset
	}

	return Paginate(ctx, start, func(ctx context.Context, offset int) (Page[authwire.AuthLogEntryView], error) {
		q := q
		q.Offset = &offset

		res, err := a.AuditLog(ctx, q, opts...)
		if err != nil {
			return Page[authwire.AuthLogEntryView]{}, err
		}
		if res == nil {
			return Page[authwire.AuthLogEntryView]{}, nil
		}
		return Page[authwire.AuthLogEntryView]{
			Items:  res.Data,
			Total:  res.Pagination.Total,
			Offset: res.Pagination.Offset,
		}, nil
	})
}

// Impersonate issues a session acting as another account, and installs it. What
// the caller was holding is replaced, so [Auth.EndImpersonation] is how to get
// back.
func (a *Auth) Impersonate(
	ctx context.Context, accountID uuid.UUID, opts ...CallOption,
) (*authwire.TokenPair, error) {
	pair, err := Do[authwire.TokenPair](ctx, a.rt, Op{
		Name:   "authImpersonate",
		Method: http.MethodPost, Root: true, Path: a.path("/impersonate"),
		Body: authwire.ImpersonateRequest{AccountID: accountID},
	}, opts...)
	if err != nil {
		return nil, err
	}
	a.adopt(pair)
	return pair, nil
}

// EndImpersonation ends it. The administrator signs in again afterwards: the
// session that was impersonating is gone, and it was the credential in hand.
func (a *Auth) EndImpersonation(ctx context.Context, opts ...CallOption) error {
	if err := DoNoContent(ctx, a.rt, Op{
		Name:   "authEndImpersonation",
		Method: http.MethodDelete, Root: true, Path: a.path("/impersonate"),
	}, opts...); err != nil {
		return err
	}
	a.rt.Use(nil)
	return nil
}

// APIKeys lists the keys the caller may see: their own, or the tenant's when
// they administer keys.
func (a *Auth) APIKeys(ctx context.Context, opts ...CallOption) ([]authwire.APIKeyView, error) {
	if !a.profile.HasAPIKeys {
		return nil, notMounted("GET "+a.path("/api-keys"),
			"API keys are configured in main.go, by giving the handler an apikey manager")
	}
	return list[authwire.APIKeyView](ctx, a.rt, a.path("/api-keys"), opts)
}

// CreateAPIKey mints one. The secret in the answer is shown exactly once:
// nothing stored can produce it again.
func (a *Auth) CreateAPIKey(
	ctx context.Context, in authwire.CreateKeyRequest, opts ...CallOption,
) (*authwire.CreateKeyResponse, error) {
	if !a.profile.HasAPIKeys {
		return nil, notMounted("POST "+a.path("/api-keys"),
			"API keys are configured in main.go, by giving the handler an apikey manager")
	}
	return Do[authwire.CreateKeyResponse](ctx, a.rt, Op{
		Name:   "authCreateAPIKey",
		Method: http.MethodPost, Root: true, Path: a.path("/api-keys"), Body: in,
	}, opts...)
}

// RevokeAPIKey kills one.
func (a *Auth) RevokeAPIKey(ctx context.Context, id uuid.UUID, opts ...CallOption) error {
	if !a.profile.HasAPIKeys {
		return notMounted("DELETE "+a.path("/api-keys/{id}"),
			"API keys are configured in main.go, by giving the handler an apikey manager")
	}
	return DoNoContent(ctx, a.rt, Op{
		Name:   "authRevokeAPIKey",
		Method: http.MethodDelete, Root: true, Path: a.path("/api-keys/" + PathValue(id.String())),
	}, opts...)
}

// adopt hands a newly issued pair to the installed session, or installs one when
// there is none. A tenant switch, an impersonation and a password change all end
// with the client holding a different session than it started with, and a caller
// having to notice that themselves is a bug waiting to happen.
func (a *Auth) adopt(pair *authwire.TokenPair) {
	if pair == nil || pair.AccessToken == "" {
		return
	}
	if s, ok := a.rt.Session(); ok {
		s.replace(*pair)
		return
	}
	a.rt.Use(NewSession(*pair))
}

// needsIdentity refuses the picker endpoints on a project that mounts none.
func (a *Auth) needsIdentity() error {
	if a.profile.HasIdentitySessions {
		return nil
	}
	return notMounted("the "+a.profile.BasePath+"/me routes",
		"they exist where identity sessions are configured, which is what the tenant "+
			"picker runs on")
}

// list reads one of the collection endpoints, which all answer with the same
// one-member envelope.
func list[T any](
	ctx context.Context, rt *Runtime, path string, opts []CallOption,
) ([]T, error) {
	res, err := Do[authwire.List[T]](ctx, rt, Op{Method: http.MethodGet, Root: true, Path: path}, opts...)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.Data, nil
}
