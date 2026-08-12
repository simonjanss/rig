// Package oauth signs people in through an external provider.
//
// Three decisions here are worth more than the rest of the package put together.
//
// A person is matched on the provider's subject, never on the email address.
// Subjects are stable for the life of an account; addresses change, and
// providers hand a released address to somebody else. Matching on the address
// is how one person ends up signed in as another.
//
// An existing person is only ever linked to a provider account when the provider
// says the address is verified. Without that check, anybody who can register your
// address at any supported provider owns your account here.
//
// And who somebody is, is a separate question from whether they belong here. The
// first is global — one provider link, one identity, however many tenants — and
// the second is per tenant and answered no by default. A provider will
// authenticate anybody with a Google account, so joining a tenant is gated by
// [Config.AllowProvisioning] and by the tenant's own list of allowed domains.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// Link is a provider account attached to a person.
//
// It hangs off the identity and carries no tenant: one Google account is one
// Google account, so linking it once means "sign in with Google" works for every
// tenant that person belongs to.
type Link struct {
	ID         uuid.UUID
	IdentityID uuid.UUID

	Provider     string
	Subject      string
	EmailAddress string
}

// Profile is what a provider says about somebody.
type Profile struct {
	// Subject is the provider's stable identifier. It is the only field an
	// identity is matched on.
	Subject      string
	EmailAddress string
	// EmailVerified is the provider's claim that the address belongs to this
	// person. An unverified address is never enough to link an existing
	// account, and this is the field that decides it.
	EmailVerified bool
	DisplayName   string
}

// LinkInput attaches a provider account to a person who already exists.
type LinkInput struct {
	IdentityID uuid.UUID
	Provider   string
	Profile    Profile
}

// ProvisionInput creates a person the application has not seen before, and links
// them, in one step.
type ProvisionInput struct {
	Provider string
	Profile  Profile
}

// JoinInput gives a person an account in a tenant.
type JoinInput struct {
	TenantID   uuid.UUID
	IdentityID uuid.UUID
	Profile    Profile
}

// Store is the persistence a sign-in needs.
//
// An application implements it over the generated repositories for identity,
// identity_oauth and account, all of which `rig setup-project` creates.
//
// The two halves are deliberately separate calls. Who somebody is, is global —
// one address, one provider link — and whether they belong here is per tenant, so
// a person signing in to their second tenant goes through the first half
// unchanged and only the second half decides anything.
type Store interface {
	// FindLink returns the link for a provider subject, or nil. It is not
	// scoped to a tenant: a provider account belongs to a person.
	FindLink(ctx context.Context, provider, subject string) (*Link, error)

	// FindIdentityByEmail returns the identifier of the person with that
	// address, or uuid.Nil. It is what makes "sign in with Google" reach the
	// person who already signed up with a password, rather than making a second
	// one beside them.
	FindIdentityByEmail(ctx context.Context, lowercased string) (uuid.UUID, error)

	// LinkIdentity records the connection.
	LinkIdentity(ctx context.Context, in LinkInput) (*Link, error)

	// ProvisionIdentity creates the person and links them in one step.
	ProvisionIdentity(ctx context.Context, in ProvisionInput) (*Link, error)

	// FindAccount returns a person's account in one tenant, or uuid.Nil when
	// they do not belong to it.
	FindAccount(ctx context.Context, tenantID, identityID uuid.UUID) (uuid.UUID, error)

	// JoinTenant creates an account for a person in a tenant. It must honour
	// the tenant's allowed email domains: a provider will authenticate anybody,
	// so this is the door.
	JoinTenant(ctx context.Context, in JoinInput) (uuid.UUID, error)
}

// Config builds a handler.
type Config struct {
	Store     Store
	Providers []Provider

	// BaseURL is the origin callbacks come back to, for example
	// https://app.example.com. It must match what is registered with the
	// provider, exactly.
	BaseURL string
	// BasePath defaults to /auth/oauth.
	BasePath string

	// Origin overrides BaseURL for one request, for an application served at more
	// than one origin — which is exactly what host-based tenancy is:
	// acme.example.com and beta.example.com are the same application, and a
	// sign-in that started at one has to come back to it. The state cookie is
	// host-only, so a callback that landed on the other host would arrive without
	// it and be refused.
	//
	// Nil means BaseURL, which is right for the single-origin deployment most
	// applications are.
	//
	// The constraint this has to live inside is the provider's, not rig's: a
	// redirect URI is registered exactly, and few providers accept a wildcard. So
	// every origin this returns has to be registered with every provider. A
	// deployment with a tenant per subdomain and a provider that will not take a
	// wildcard keeps the callback on one canonical host instead, and hands the
	// finished session on to the tenant's own host itself — which is what
	// OnSignIn is for.
	Origin func(r *http.Request) string

	// SigningKey signs the cookie that carries the state and the PKCE verifier
	// across the round trip.
	//
	// Required, and at least 32 bytes. The alternative is a table of pending
	// sign-ins to clean up; a signed cookie holds the same three values, needs
	// no storage, and cannot be forged.
	SigningKey []byte

	// Tenant resolves which tenant is signing in.
	Tenant func(*http.Request) (uuid.UUID, error)

	// OnSignIn finishes the request once an identity is resolved.
	//
	// It writes the response: set a cookie, redirect with a token, render a
	// page. rig does not choose, because the choice depends on whether the
	// client is a browser or a native application, and only the application
	// knows.
	OnSignIn func(w http.ResponseWriter, r *http.Request, in SignIn) error

	// AllowedReturnTo are the origins a sign-in may redirect to when it
	// finishes. A path on this origin is always allowed; anything else has to
	// be listed, because an unchecked returnTo is an open redirect and an open
	// redirect on a sign-in endpoint wears your domain in a phishing link.
	AllowedReturnTo []string

	// AllowProvisioning creates an account for somebody with no existing one.
	//
	// Off by default. An open sign-in endpoint on a business application is a
	// way for anybody with a Google account to appear inside a customer's
	// tenant, which is rarely what anyone wants and never what they expect.
	AllowProvisioning bool

	Log authlog.Log

	// StateTTL bounds how long a sign-in may take. Ten minutes is generous for
	// a redirect and short enough that a stolen state is useless by the time
	// anybody notices it.
	StateTTL time.Duration

	// Insecure allows the state cookie over plain HTTP. It is for local
	// development and nothing else.
	Insecure bool

	Now func() time.Time
}

// SignIn is what a completed sign-in hands to the application.
type SignIn struct {
	// Link is the provider account that was used, and Link.IdentityID is who it
	// belongs to.
	Link *Link
	// TenantID and AccountID are the session to issue. The account is the
	// person's row in this tenant, which is what claims are made of.
	TenantID  uuid.UUID
	AccountID uuid.UUID

	Profile  Profile
	Provider string
	// New reports that this sign-in created the account, so an application can
	// send a welcome message or run onboarding. It is true for somebody joining
	// their second tenant as well as their first: the account is new either
	// way, which is what onboarding is about.
	New bool
	// ReturnTo is where the caller asked to be sent afterwards, already
	// checked against the allow-list. Empty when none was asked for.
	ReturnTo string
}

// DefaultStateTTL bounds a sign-in round trip.
const DefaultStateTTL = 10 * time.Minute

// Handler serves the sign-in routes.
type Handler struct {
	cfg       Config
	base      string
	providers map[string]Provider
	now       func() time.Time
}

// New builds a handler.
func New(cfg Config) (*Handler, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("oauth: a Store is required")
	case len(cfg.Providers) == 0:
		return nil, errors.New("oauth: at least one provider is required")
	case cfg.BaseURL == "":
		return nil, errors.New("oauth: a BaseURL is required; it must match what the provider has registered")
	case len(cfg.SigningKey) < 32:
		return nil, errors.New("oauth: a SigningKey of at least 32 bytes is required")
	case cfg.Tenant == nil:
		return nil, errors.New("oauth: a Tenant resolver is required")
	case cfg.OnSignIn == nil:
		return nil, errors.New("oauth: an OnSignIn is required; rig does not decide how a sign-in ends")
	}

	if cfg.Log == nil {
		cfg.Log = authlog.Noop{}
	}
	if cfg.StateTTL == 0 {
		cfg.StateTTL = DefaultStateTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	base := cfg.BasePath
	if base == "" {
		base = "/auth/oauth"
	}

	h := &Handler{
		cfg:       cfg,
		base:      strings.TrimRight(base, "/"),
		providers: make(map[string]Provider, len(cfg.Providers)),
		now:       cfg.Now,
	}
	for _, p := range cfg.Providers {
		if p.Name == "" {
			return nil, errors.New("oauth: every provider needs a name")
		}
		if _, dup := h.providers[strings.ToLower(p.Name)]; dup {
			return nil, fmt.Errorf("oauth: provider %q is configured twice", p.Name)
		}
		h.providers[strings.ToLower(p.Name)] = p
	}
	return h, nil
}

// Mount registers the routes.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+h.base+"/{provider}/start", h.start)
	mux.HandleFunc("GET "+h.base+"/{provider}/callback", h.callback)
}

// Providers lists the configured providers, for a sign-in page.
func (h *Handler) Providers() []string {
	out := make([]string, 0, len(h.cfg.Providers))
	for _, p := range h.cfg.Providers {
		out = append(out, p.Name)
	}
	return out
}

func (h *Handler) provider(r *http.Request) (Provider, error) {
	p, ok := h.providers[strings.ToLower(r.PathValue("provider"))]
	if !ok {
		return Provider{}, rigerr.NotFound("no such sign-in provider")
	}
	return p, nil
}

// redirectURI is where a provider sends somebody back to.
//
// It is built rather than configured because it has to match the route Mount
// registered, and two places to write one URL is one place to get it wrong.
func (h *Handler) redirectURI(r *http.Request, p Provider) string {
	base := h.cfg.BaseURL
	if h.cfg.Origin != nil {
		// The same request answers at start and at callback, so both halves of a
		// sign-in name the same redirect URI — which providers compare.
		if got := h.cfg.Origin(r); got != "" {
			base = got
		}
	}
	return strings.TrimRight(base, "/") + h.base + "/" + strings.ToLower(p.Name) + "/callback"
}

// resolve turns a profile into a session to issue.
//
// Two questions in order, and the order is the point. Who is this — answered
// globally, from the provider subject or the address. Then: do they belong to
// this tenant — answered from the account table, and answered no unless
// provisioning is on and the tenant's domains say otherwise.
func (h *Handler) resolve(ctx context.Context, tenantID uuid.UUID, p Provider, profile Profile) (SignIn, error) {
	if profile.Subject == "" {
		return SignIn{}, rigerr.Internal(nil, "%s returned no subject", p.Name)
	}

	link, err := h.identity(ctx, p, profile)
	if err != nil {
		return SignIn{}, err
	}

	accountID, err := h.cfg.Store.FindAccount(ctx, tenantID, link.IdentityID)
	if err != nil {
		return SignIn{}, err
	}
	if accountID != uuid.Nil {
		return SignIn{
			Link: link, TenantID: tenantID, AccountID: accountID,
			Profile: profile, Provider: p.Name,
		}, nil
	}

	// They are somebody, but not somebody here. Joining a tenant is a decision,
	// and an unchecked one would let anybody with a Google account appear inside
	// a customer's tenant.
	if !h.cfg.AllowProvisioning {
		return SignIn{}, rigerr.Forbidden("there is no account for this address")
	}

	accountID, err = h.cfg.Store.JoinTenant(ctx, JoinInput{
		TenantID: tenantID, IdentityID: link.IdentityID, Profile: profile,
	})
	if err != nil {
		return SignIn{}, err
	}
	return SignIn{
		Link: link, TenantID: tenantID, AccountID: accountID,
		Profile: profile, Provider: p.Name, New: true,
	}, nil
}

// identity answers who is signing in, without reference to any tenant.
func (h *Handler) identity(ctx context.Context, p Provider, profile Profile) (*Link, error) {
	// The subject, always. An address is a display detail here.
	link, err := h.cfg.Store.FindLink(ctx, p.Name, profile.Subject)
	if err != nil {
		return nil, err
	}
	if link != nil {
		return link, nil
	}

	email := strings.ToLower(strings.TrimSpace(profile.EmailAddress))
	if email == "" {
		return nil, rigerr.BadRequest(
			"%s did not share an email address, so there is no account to sign in to", p.Name)
	}

	identityID, err := h.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if identityID != uuid.Nil {
		// The check the whole package turns on. Anybody can register any
		// address at some provider; only a verified one is evidence.
		if !profile.EmailVerified {
			return nil, rigerr.Forbidden(
				"%s has not verified this address, so it cannot be linked to an existing account", p.Name)
		}
		return h.cfg.Store.LinkIdentity(ctx, LinkInput{
			IdentityID: identityID, Provider: p.Name, Profile: profile,
		})
	}

	// Nobody has this address anywhere. Creating the person is gated by the same
	// switch that gates joining a tenant, because on its own it would be a way to
	// fill the identity table from a sign-in page.
	if !h.cfg.AllowProvisioning {
		return nil, rigerr.Forbidden("there is no account for this address")
	}
	return h.cfg.Store.ProvisionIdentity(ctx, ProvisionInput{Provider: p.Name, Profile: profile})
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := rigerr.CodeOf(err)
	message := err.Error()

	var typed *rigerr.Error
	if errors.As(err, &typed) {
		message = typed.Message
	}
	if code == rigerr.CodeInternal {
		message = "something went wrong"
	}

	http.Error(w, message, code.HTTPStatus())
}

func (h *Handler) write(ctx context.Context, e authlog.Entry) {
	if e.At.IsZero() {
		e.At = h.now()
	}
	h.cfg.Log.Write(ctx, e)
}
