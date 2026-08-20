// Package authhttp mounts the authentication endpoints.
//
// It is deliberately thin. Every decision worth arguing about was made in
// [github.com/simonjanss/rig/auth/account], [github.com/simonjanss/rig/auth/session],
// and [github.com/simonjanss/rig/auth/apikey]; what is here is JSON, status
// codes, and the one piece of real logic an HTTP layer owns — deciding what a
// request's credential is and turning it into claims.
//
// That last part, [Handler.Claims], is the integration point: it is what a
// generated server's Server.GetClaims is set to, and it is why an application
// never writes authentication middleware of its own.
package authhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
	"github.com/simonjanss/rig/runtime/throttle"
)

// Permissions the endpoints check.
const (
	// PermissionManageAPIKeys is required for a key that acts as a service
	// account — a separate identity with scopes of its own — and for seeing or
	// revoking anybody's key but your own.
	//
	// This is the administrative half, because minting a service key is creating
	// authority: the key acts as an account nobody signs in to, and what it may do
	// is whatever scopes it was given.
	PermissionManageAPIKeys = "apikey.manage"
	// PermissionOwnAPIKey is required to mint a key that acts as yourself.
	//
	// A separate, much cheaper permission, and the reason is in the verification
	// path: a personal key's permissions are intersected with its owner's every
	// time it is used, so it can never do anything its owner cannot. Minting one
	// grants nothing new — it is a second way to present authority somebody already
	// holds, like a password manager entry. Ordinary members can have this without
	// being able to create a service account.
	PermissionOwnAPIKey = "apikey.own"
	// PermissionImpersonate is required to act as somebody else.
	PermissionImpersonate = "account.impersonate"

	// The three wide keys. Each widens an endpoint that answers about the caller
	// into one that answers about the tenant, and each is checked *in addition*
	// to whatever the endpoint already required — see
	// [github.com/simonjanss/rig/runtime/tenancy.RequireScope]. A credential
	// holding only one of these can read nothing, which is the right shape: one
	// grant, one thing.
	//
	// Three rather than one, because they are three decisions. Reading who is
	// signed in is a support function; ending somebody else's session is an act;
	// reading the authentication trail is a compliance function, and a product
	// may well want an auditor who can do the last and neither of the others.

	// PermissionReadAuthLogAll is required to read the tenant's authentication
	// trail rather than only your own events.
	PermissionReadAuthLogAll = "authlog.read.all"
	// PermissionReadSessionsAll is required to see every session open in the
	// tenant rather than only your own.
	PermissionReadSessionsAll = "session.read.all"
	// PermissionRevokeSessionsAll is required to end somebody else's session.
	PermissionRevokeSessionsAll = "session.revoke.all"
)

// Permissions are the keys this package's own endpoints check.
//
// They belong beside the endpoints that check them rather than in a list an
// application maintains: minting a key and inviting somebody are things the auth
// package does, so the keys they require are its to declare. A reconcile unions
// them with the application's own, which is why a project passes only its
// generated catalogue and still ends up with a complete table.
func Permissions() []tenancy.Permission {
	return []tenancy.Permission{
		{
			Key:         PermissionManageAPIKeys,
			Name:        "Manage API keys",
			Description: "Mint keys for service accounts, and see or revoke anybody's key in the tenant.",
		},
		{
			Key:  PermissionOwnAPIKey,
			Name: "Own API keys",
			Description: "Mint a key that acts as you, and see or revoke your own. " +
				"It can never do more than you can: its permissions are intersected with yours on every request.",
		},
		{
			Key:         PermissionImpersonate,
			Name:        "Impersonate",
			Description: "Open a session as somebody else. Both ends are recorded.",
		},
		{
			Key:         account.PermissionProvision,
			Name:        "Invite people",
			Description: "Bring somebody into a tenant, and withdraw an invitation.",
		},
		{
			Key:  PermissionReadAuthLogAll,
			Name: "Read the authentication trail",
			Description: "See every sign-in, failure, lockout and key use in the tenant, " +
				"not only your own. Anybody can read their own without this.",
		},
		{
			Key:  PermissionReadSessionsAll,
			Name: "See who is signed in",
			Description: "List every session open in the tenant, with the device and address " +
				"each was opened from.",
		},
		{
			Key:  PermissionRevokeSessionsAll,
			Name: "End anybody's session",
			Description: "Sign somebody else out of a device, which is what a lost phone or a " +
				"departure needs. Ending your own needs nothing.",
		},
	}
}

// maxBodyBytes bounds a request body. Login is unauthenticated, so the limit is
// the only thing between a stranger and the server's memory.
const maxBodyBytes = 1 << 16

// The page bounds for the one paginated endpoint here. They are the numbers the
// generated endpoints use — DefaultLimit and MaxLimit in
// internal/compile/builtin.go — spelled again rather than imported, because this
// module cannot reach rig's internal packages. Two clients paging two of a
// project's endpoints should not find that they behave differently, so if those
// change, change these.
const (
	defaultPageLimit = 50
	maxPageLimit     = 500
)

// Config builds a handler.
type Config struct {
	Accounts *account.Service
	Sessions *session.Manager
	// APIKeys is optional. Without it the key endpoints are not mounted and a
	// key presented as a credential is refused, which is the right behavior for
	// a project that skipped that part of the foundation.
	APIKeys *apikey.Manager

	// AuditLog reads what was recorded. Nil leaves GET <base>/audit unmounted —
	// absent rather than answering 403, so there is nothing to probe, which is
	// the same choice registration and tenant creation make.
	//
	// A reader and not the [github.com/simonjanss/rig/auth/authlog.Log] every
	// other part of this package writes to. The write path discards its failures
	// on purpose and a read cannot, so they are two interfaces; one
	// implementation usually satisfies both.
	AuditLog authlog.Reader

	// Tenant resolves which tenant a request belongs to.
	//
	// There is no default, because there is no sensible one: a subdomain, a
	// header, and a path segment are all reasonable and only the application
	// knows which it uses. Guessing would mean guessing wrong for somebody.
	Tenant func(*http.Request) (uuid.UUID, error)

	// Identities verifies the tenant-less credential the tenant picker runs
	// on. Nil leaves the picker endpoints unmounted and refuses an identity
	// token, which is right for a project where every session names a tenant.
	Identities *session.IdentityManager

	// AllowTenantCreation mounts POST /auth/tenants, where somebody holding an
	// identity session makes one and is signed into it as its Owner.
	//
	// Off by default. What a new tenant consists of beyond the row and its first
	// account is [account.TenantOptions.OnCreated]; whether anybody may make one
	// at all is [account.TenantOptions.Allow]. This only decides whether the route
	// exists — off means absent rather than 403, so there is nothing to probe.
	AllowTenantCreation bool

	// AllowRegistration mounts POST /auth/register, where a stranger creates an
	// account with no tenant.
	//
	// Off by default, and it is the same kind of decision as who may create a
	// tenant: invite-only and public sign-up are both ordinary products, and
	// rig will not pick one. Off means the endpoint does not exist rather than
	// answering 403, so nothing is there to probe.
	AllowRegistration bool

	// Grants fills in a caller's roles and permissions. Nil means every
	// authenticated caller has none, which makes any endpoint with a permission
	// requirement answer 403 — correct, and confusing enough that it is worth
	// saying here.
	Grants Grants

	// BasePath prefixes every route. It defaults to /auth.
	BasePath string

	// OnError renders a failure. Nil uses a small default; an application that
	// already has an error shape should pass its own so that authentication
	// failures look like every other failure.
	OnError func(w http.ResponseWriter, r *http.Request, err error)

	// TrustedProxies are the networks whose X-Forwarded-For headers may be
	// believed.
	//
	// Empty means none, and that is the safe default: an address read out of a
	// header a client controls is an address a client chooses, and rate limits
	// keyed on it are rate limits an attacker can walk straight around.
	TrustedProxies []netip.Prefix
}

// Handler serves the authentication endpoints.
type Handler struct {
	cfg  Config
	base string
}

// New builds a handler.
func New(cfg Config) (*Handler, error) {
	switch {
	case cfg.Accounts == nil:
		return nil, errors.New("authhttp: an account service is required")
	case cfg.Sessions == nil:
		return nil, errors.New("authhttp: a session manager is required")
	case cfg.Tenant == nil:
		return nil, errors.New("authhttp: a Tenant resolver is required; " +
			"only the application knows how its customers are addressed")
	}

	base := cfg.BasePath
	if base == "" {
		base = "/auth"
	}
	return &Handler{cfg: cfg, base: strings.TrimRight(base, "/")}, nil
}

// Mount registers every route.
func (h *Handler) Mount(mux *http.ServeMux) {
	route := func(pattern string, fn http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+h.base+path, fn)
	}

	route("POST /login", h.login)
	route("POST /logout", h.logout)
	route("POST /refresh", h.refresh)

	route("POST /password/reset", h.requestReset)
	route("POST /password/reset/confirm", h.confirmReset)
	route("POST /password/change", h.changePassword)

	route("POST /email/verify", h.verifyEmail)
	route("POST /email/verify/resend", h.resendVerification)

	// Creating an account is not CRUD on a table: see provision.
	route("POST /accounts", h.provision)

	// An invitation is redeemed without a credential, so it is unauthenticated —
	// the token is the credential, for one use.
	route("POST /invitations/accept", h.acceptInvitation)

	// Listing and withdrawing are administrative, and need the same permission
	// inviting does.
	route("GET /invitations", h.listInvitations)
	route("DELETE /invitations/{id}", h.revokeInvitation)

	// Before a tenant: what somebody holding only an identity session can
	// reach. Nothing here touches application data, because there is no tenant
	// to touch it in.
	if h.cfg.Identities != nil {
		if h.cfg.AllowRegistration {
			route("POST /register", h.register)
		}
		route("GET /me/invitations", h.myInvitations)
		route("POST /me/invitations/accept", h.acceptFromThePicker)
		route("GET /me/tenants", h.myTenants)
		route("DELETE /me/session", h.endIdentitySession)

		if h.cfg.AllowTenantCreation {
			route("POST /tenants", h.createTenant)
		}
	}

	route("GET /tenants", h.listTenants)
	route("POST /tenants/{id}/switch", h.switchTenant)

	route("GET /sessions", h.listSessions)
	route("DELETE /sessions/{id}", h.revokeSession)

	if h.cfg.AuditLog != nil {
		route("GET /audit", h.audit)
	}

	route("POST /impersonate", h.impersonate)
	route("DELETE /impersonate", h.endImpersonation)

	if h.cfg.APIKeys != nil {
		route("GET /api-keys", h.listAPIKeys)
		route("POST /api-keys", h.createAPIKey)
		route("DELETE /api-keys/{id}", h.revokeAPIKey)
	}
}

// Claims resolves a request's credential.
//
// This is what a generated server's Server.GetClaims is set to. It accepts an
// access token or an API key in the Authorization header and tells them apart
// by prefix, which is why the prefixes exist.
//
// Roles are resolved here, per request, rather than baked into the token: a
// role taken away should stop working now, not whenever the session next
// happens to refresh.
func (h *Handler) Claims(r *http.Request) (tenancy.Claims, error) {
	presented, err := bearer(r)
	if err != nil {
		return tenancy.Claims{}, err
	}

	switch {
	case strings.HasPrefix(presented, apikey.Prefix):
		if h.cfg.APIKeys == nil {
			return tenancy.Claims{}, rigerr.Unauthorized("API keys are not enabled")
		}
		claims, key, err := h.cfg.APIKeys.Verify(r.Context(), presented, h.remoteAddr(r))
		if err != nil {
			return tenancy.Claims{}, err
		}

		// An integration key's scopes are its permissions outright. Enriching
		// them from the service account's roles would make a machine credential
		// grow new powers whenever somebody edited a role, which is the opposite
		// of what a machine credential is for.
		if key.Kind != apikey.KindPersonal {
			return claims, nil
		}

		// A personal key is its owner automating their own work, so it carries
		// what they carry — and no more, which is the part worth stating: their
		// grants bound it, and its scopes bound it further when it has any.
		// Without that a personal key would be either useless (no permissions at
		// all) or a way to hold on to access after somebody's role was reduced.
		owner, err := enrich(r.Context(), h.cfg.Grants, claims)
		if err != nil {
			return tenancy.Claims{}, err
		}
		if len(key.Scopes) > 0 {
			owner.Permissions = intersect(owner.Permissions, key.Scopes)
		}
		return owner, nil

	case strings.HasPrefix(presented, session.PrefixAccess):
		tok, err := h.cfg.Sessions.Verify(r.Context(), presented)
		if err != nil {
			return tenancy.Claims{}, err
		}
		return h.claimsFor(r.Context(), tok)

	default:
		// A refresh token lands here, which is right: it is a credential for
		// the refresh endpoint and nothing else.
		return tenancy.Claims{}, rigerr.Unauthorized("the credential is not valid")
	}
}

// claimsFor is what a verified session amounts to.
//
// Split out of [Handler.Claims] so a handler that needs the token *and* the
// claims — the session endpoints do, one for the family it belongs to and one
// for the permission it carries — can have both from one verification instead of
// paying for two.
func (h *Handler) claimsFor(ctx context.Context, tok *session.Token) (tenancy.Claims, error) {
	claims := tenancy.Claims{
		TenantID:                tok.TenantID,
		AccountID:               tok.AccountID,
		Subject:                 tenancy.SubjectAccount,
		ImpersonatedByAccountID: tok.ImpersonatedByAccountID,
		// Whatever the application put on this session, read from the same
		// row the token was verified against — so it costs nothing beyond the
		// lookup that was happening anyway.
		Extra: tok.Payload,
	}
	return enrich(ctx, h.cfg.Grants, claims)
}

// Grants answers what an account may do, in the tenant the request is for.
//
// A function, and the only thing rig asks of an application's authorization
// model. Where the answer comes from is not rig's business: a table of roles, a
// directory, a claim on a federated token, a switch on the account's level, or a
// constant while a project is young. rig derives the permission *keys* from the
// schema and generates the check that reads them; deciding who holds which is the
// application's, because no two products agree on it and a framework that picked
// one would be wrong for everybody else.
//
// It runs when a request arrives rather than at sign-in, which is what makes
// revoking access take effect on the next call instead of whenever the session
// happens to refresh. That also means it is on the hot path: cache it if the
// answer is expensive.
type Grants func(ctx context.Context, tenantID, accountID uuid.UUID) (roles, permissions []string, err error)

// enrich fills in a caller's grants, if the application supplied a way to.
func enrich(ctx context.Context, g Grants, c tenancy.Claims) (tenancy.Claims, error) {
	if g == nil || c.AccountID == uuid.Nil {
		return c, nil
	}
	roles, permissions, err := g(ctx, c.TenantID, c.AccountID)
	if err != nil {
		return tenancy.Claims{}, err
	}
	c.Roles, c.Permissions = roles, permissions
	return c, nil
}

// intersect is the permissions in both, so that a scope can narrow what a person
// may do and never widen it.
func intersect(held, scopes []string) []string {
	allowed := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		allowed[s] = struct{}{}
	}

	out := make([]string, 0, min(len(held), len(scopes)))
	for _, p := range held {
		if _, ok := allowed[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Session returns the token behind a request, for a handler that needs the
// session itself rather than the claims — ending an impersonation, for one.
func (h *Handler) Session(r *http.Request) (*session.Token, error) {
	presented, err := bearer(r)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(presented, session.PrefixAccess) {
		return nil, rigerr.Unauthorized("this endpoint needs a session, not a key")
	}
	return h.cfg.Sessions.Verify(r.Context(), presented)
}

// bearer reads the Authorization header.
func bearer(r *http.Request) (string, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return "", rigerr.Unauthorized("this request carries no credential")
	}

	scheme, token, ok := strings.Cut(raw, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", rigerr.Unauthorized("the Authorization header should be `Bearer <token>`")
	}
	return strings.TrimSpace(token), nil
}

// remoteAddr is where the request came from.
//
// The forwarded header is believed only when the immediate peer is a proxy the
// application named. Trusting it unconditionally would let anybody set their own
// address, and every limit keyed on one would become decorative.
func (h *Handler) remoteAddr(r *http.Request) netip.Addr {
	peer := parseAddr(r.RemoteAddr)

	if len(h.cfg.TrustedProxies) == 0 || !peer.IsValid() {
		return peer
	}
	trusted := false
	for _, p := range h.cfg.TrustedProxies {
		if p.Contains(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return peer
	}

	// The left-most entry is the original client. Everything after it was added
	// by a hop, and everything before the first untrusted hop is a claim.
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return peer
	}
	first, _, _ := strings.Cut(forwarded, ",")
	if addr, err := netip.ParseAddr(strings.TrimSpace(first)); err == nil {
		return addr
	}
	return peer
}

func parseAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func (h *Handler) addrString(r *http.Request) string {
	if a := h.remoteAddr(r); a.IsValid() {
		return a.String()
	}
	return ""
}

// decode reads a JSON body.
func decode(r *http.Request, into any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return rigerr.BadRequest("the request body is empty")
		}
		return rigerr.BadRequest("cannot read the request body: %v", err)
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if h.cfg.OnError != nil {
		h.cfg.OnError(w, r, err)
		return
	}

	code := rigerr.CodeOf(err)
	message := err.Error()

	var typed *rigerr.Error
	if errors.As(err, &typed) {
		message = typed.Message
	}
	if code == rigerr.CodeInternal {
		// An internal failure's detail is exactly what leaks a table name.
		message = "something went wrong"
	}

	// A 429 with no Retry-After leaves a client with nothing to do but guess,
	// and clients that guess retry immediately.
	if refusal, ok := throttle.RefusalOf(err); ok {
		refusal.Decision().SetHeaders(w.Header())
	}

	writeJSON(w, code.HTTPStatus(), map[string]string{
		"code": string(code), "message": message,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// clientOf reads the client kind a caller declared.
func clientOf(s string) session.Client {
	switch strings.ToLower(s) {
	case "mobile":
		return session.ClientMobile
	case "machine":
		return session.ClientMachine
	default:
		return session.ClientWeb
	}
}

func signInOf(res account.SignInResult) authwire.SignInResponse {
	out := authwire.SignInResponse{
		IdentityToken:     res.Identity.Token,
		IdentityExpiresAt: res.Identity.ExpiresAt,
		Tenants:           make([]authwire.TenantView, 0, len(res.Tenants)),
	}
	if res.Session != nil {
		out.TokenPair = pairOf(*res.Session)
	}
	for _, w := range res.Tenants {
		out.Tenants = append(out.Tenants, authwire.TenantView{
			TenantID: w.TenantID, TenantName: w.TenantName, TenantSlug: w.TenantSlug,
			AccountID: w.AccountID, Role: string(w.Role),
			// Which one the session landed in, so a picker can mark it without
			// comparing identifiers itself.
			Current: res.TenantID != uuid.Nil && w.TenantID == res.TenantID,
		})
	}
	return out
}

func pairOf(p session.Pair) authwire.TokenPair {
	return authwire.TokenPair{
		AccessToken:      p.Access.Token,
		RefreshToken:     p.Refresh.Token,
		ExpiresAt:        p.Access.ExpiresAt,
		RefreshExpiresAt: p.Refresh.ExpiresAt,
		SessionID:        p.RootTokenID,
	}
}
