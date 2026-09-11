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
// authenticate anybody with a Google account, so becoming somebody here is
// gated by [Config.AllowProvisioning], joining a tenant a sign-in named by
// [Config.AllowJoining], and both by the tenant's own list of allowed domains.
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
	"github.com/simonjanss/rig/runtime/httpx"
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
// rig_identity_oauth and rig_account, all of which `rig setup-project` creates.
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

	// LinkIdentity records the connection, and records the evidence for it.
	//
	// A link is only ever offered on an address the provider says is verified —
	// [Handler.identity] refuses otherwise — so an implementation must also
	// mark the identity's address verified when [Profile.EmailVerified] and it
	// is not already marked. That is the same evidence ProvisionIdentity acts
	// on, and writing one without the other leaves somebody who signed up with
	// a password and later signed in with Google unverified on the strength of
	// a column rather than of anything they did.
	//
	// Already verified is left alone rather than restamped: the question is
	// whether the address was ever proved, and the first answer is the true one.
	//
	// Called on every provider sign-in that carries a verified address, not
	// only the first, so it has to be idempotent: an implementation upserts on
	// (provider, subject) rather than inserting. Every sign-in rather than the
	// first because a link made before rig recorded the evidence has no other
	// occasion to be stamped, and because the address the provider reports is
	// worth keeping current.
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

	// Tenant resolves which tenant is signing in, at the start of the flow, and
	// may be left nil.
	//
	// Set it when the application already knows before the redirect — a host per
	// tenant is the case that does. Leave it nil, or answer uuid.Nil, and the
	// question is settled after the callback instead: the identity is resolved
	// globally, and where that person goes is answered from their own
	// memberships the way a password login answers it. That is the only answer
	// available to a deployment with more than one tenant and no host to read
	// it from — nobody can say which tenants an address belongs to before
	// somebody has proved they own the address.
	//
	// A tenant it does name is carried through the round trip rather than
	// resolved again; see the state cookie's own documentation for why it has
	// to be.
	Tenant func(*http.Request) (uuid.UUID, error)

	// OnSignIn finishes the request once an identity is resolved.
	//
	// It writes the response: set a cookie, redirect with a token, render a
	// page. rig does not choose, because the choice depends on whether the
	// client is a browser or a native application, and only the application
	// knows.
	OnSignIn func(w http.ResponseWriter, r *http.Request, in SignIn) error

	// OnError renders a failure, and it exists because these two routes are the
	// only ones rig serves that a person reaches with their address bar.
	//
	// Everything else here is called by script, which can read a status and a
	// body and decide what to do about it. A browser mid-navigation cannot: it
	// renders whatever came back as a document, so the default below — a bare
	// text/plain page on the API's own origin — is a dead end with no way back
	// to the application. An application serving a front end sends a redirect
	// to its own sign-in page instead, carrying copy it wrote for
	// [Failure.Reason].
	//
	// It takes a [*Failure] rather than an error, which is where it parts
	// company with
	// [github.com/simonjanss/rig/auth/authhttp.Config.OnError]: there is always
	// one to hand, and handing back a bare error every implementation would
	// open with an errors.As is a cost with no buyer. A Failure is still an
	// error, so passing it to an error writer works.
	//
	// It owns the response. Nothing is written after it returns, and nothing was
	// written before — except the authentication-log entry, which is written
	// either way and before this is called, so a hook cannot lose the record
	// that a sign-in was attempted and refused.
	//
	// Nil falls through to [Config.Fail], and then to text/plain with the status
	// the error carries.
	OnError func(w http.ResponseWriter, r *http.Request, f *Failure)

	// Fail is the API's own error writer, used when [Config.OnError] is nil.
	//
	// It is the shape every other route rig mounts takes for this —
	// notifyhttp.Options.Fail, presencehttp.Options.Fail, authhttp.Config.OnError
	// — which is the point of it: a generated server hands all four the same
	// closure, so a refused provider sign-in is classified, answered and *logged*
	// exactly as a refused login is. Without it these two routes were the only
	// ones rig serves that wrote no line at any level; a failed provider sign-in
	// existed in the authentication log and nowhere else.
	//
	// It takes an error rather than a [*Failure] because that is what an error
	// writer takes. A Failure is an error and unwraps to what it carries, so
	// nothing is lost passing one in.
	//
	// Second rather than first because [Config.OnError] answers a different
	// question — what a *browser* mid-navigation should be shown — and a project
	// that answered it meant it. An application serving a front end has one
	// installed for it, so in practice this is what a headless deployment gets.
	Fail func(w http.ResponseWriter, r *http.Request, err error)

	// AllowedReturnTo are the origins a sign-in may redirect to when it
	// finishes. A path on this origin is always allowed; anything else has to
	// be listed, because an unchecked returnTo is an open redirect and an open
	// redirect on a sign-in endpoint wears your domain in a phishing link.
	AllowedReturnTo []string

	// AllowProvisioning creates a person this application has never seen.
	//
	// Off by default. An open sign-in endpoint on a business application is a
	// way for anybody with a Google account to appear inside a customer's
	// tenant, which is rarely what anyone wants and never what they expect.
	//
	// It is the first of two doors, and it is the only one a sign-in that named
	// no tenant ever reaches — there is nowhere to join, and that question is
	// settled later from the person's own memberships. So for a deployment that
	// leaves [Config.Tenant] nil, this is the whole switch: "may a stranger
	// become somebody here at all".
	//
	// [Config.AllowJoining] is the second door, and follows this one unless it
	// is set.
	AllowProvisioning bool

	// AllowJoining creates an account in the tenant a sign-in named, for
	// somebody who is not in it yet. Nil follows [Config.AllowProvisioning],
	// which is what one switch did when it gated both doors.
	//
	// It is separate because the two doors are separate decisions, and a
	// deployment can want opposite answers to them. "A provider may create a
	// person, but only an invitation may put them in a tenant" is the ordinary
	// one — with a tenant read from a request, the join is what a crafted
	// /start link would abuse, and the person on its own reaches nothing. The
	// reverse is ordinary too: a host-per-tenant deployment whose people come
	// from a directory elsewhere may want to admit them to the tenant the host
	// named and never invent one.
	//
	// It only ever applies to a sign-in that named a tenant. Where none was
	// named, joining is the picker's job rather than the callback's, and this
	// field is not consulted.
	AllowJoining *bool

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
	//
	// **Both are uuid.Nil when the sign-in named no tenant.** That is not a
	// failure: it means the provider has said who this is and where they go has
	// not been decided yet, which is a question their own memberships answer.
	// [Config.Tenant] is what decides whether a sign-in can arrive this way.
	// The default OnSignIn handles it; a hand-written one that issues a session
	// straight from these two fields has to check first.
	TenantID  uuid.UUID
	AccountID uuid.UUID

	Profile  Profile
	Provider string
	// New reports that this sign-in created the account, so an application can
	// send a welcome message or run onboarding. It is true for somebody joining
	// their second tenant as well as their first: the account is new either
	// way, which is what onboarding is about.
	//
	// It is therefore always false when no tenant was named, because there was
	// no tenant to make an account in. [SignIn.NewIdentity] is the half of the
	// question that still has an answer there.
	New bool
	// NewIdentity reports that this sign-in created the *person* — nobody had
	// this provider account or this address before.
	//
	// The other half of New, and the half that survives a sign-in with no
	// tenant: somebody signing in for the first time anywhere is the case
	// onboarding is actually about, and it is answerable before there is
	// anywhere for them to be.
	NewIdentity bool
	// ReturnTo is where the caller asked to be sent afterwards, already
	// checked against the allow-list. Empty when none was asked for.
	ReturnTo string
	// Remember is the long-session request, from `?remember=` on the start
	// route and carried across the round trip in the sealed cookie. Feed it to
	// SignInIdentityInput.Remember, which the default ending does.
	//
	// It has no switch of its own, and does not need one: the two lengths it
	// chooses between are RefreshTTL and RememberTTL, both of which the
	// application configured. A deployment that does not want long provider
	// sessions sets them equal and this becomes a no-op.
	Remember bool
}

// DefaultStateTTL bounds a sign-in round trip.
const DefaultStateTTL = 10 * time.Minute

// Handler serves the sign-in routes.
type Handler struct {
	cfg       Config
	base      string
	providers map[string]Provider
	now       func() time.Time
	// joining is [Config.AllowJoining] with its default applied, resolved once
	// in [New] rather than at every callback — the same thing New does for the
	// log, the state TTL and the clock.
	joining bool
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

	joining := cfg.AllowProvisioning
	if cfg.AllowJoining != nil {
		joining = *cfg.AllowJoining
	}

	h := &Handler{
		cfg:       cfg,
		base:      strings.TrimRight(base, "/"),
		providers: make(map[string]Provider, len(cfg.Providers)),
		now:       cfg.Now,
		joining:   joining,
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

// resolve turns a profile into a sign-in.
//
// Two questions, and the order is the point. Who is this — answered globally,
// from the provider subject or the address, and always answered here. Then: do
// they belong to this tenant — answered from rig_account, and answered no unless
// provisioning is on and the tenant's domains say otherwise.
//
// Each question has a door of its own — [Config.AllowProvisioning] for the
// first, [Config.AllowJoining] for the second — and they refuse differently,
// because "we have never heard of you" and "you are not in this tenant" are
// different answers.
//
// The second question is only asked when a tenant was named. Otherwise it is not
// this handler's to answer and there is nowhere for the answer to come from: the
// only thing that could name a tenant at the callback is the sealed cookie, and
// a deployment with no host to read one from has nothing to seal. So the sign-in
// is handed on with the first question answered and the second left open, which
// is the state [account.Service.SignInIdentity] takes as its input.
func (h *Handler) resolve(ctx context.Context, tenantID uuid.UUID, p Provider, profile Profile) (SignIn, error) {
	if profile.Subject == "" {
		return SignIn{}, &Failure{
			Reason: ReasonInternal,
			Err:    rigerr.Internal(nil, "%s returned no subject", p.Name),
		}
	}

	link, created, err := h.identity(ctx, p, profile)
	if err != nil {
		return SignIn{}, err
	}

	if tenantID == uuid.Nil {
		return SignIn{
			Link: link, Profile: profile, Provider: p.Name, NewIdentity: created,
		}, nil
	}

	accountID, err := h.cfg.Store.FindAccount(ctx, tenantID, link.IdentityID)
	if err != nil {
		return SignIn{}, err
	}
	if accountID != uuid.Nil {
		return SignIn{
			Link: link, TenantID: tenantID, AccountID: accountID,
			Profile: profile, Provider: p.Name, NewIdentity: created,
		}, nil
	}

	// They are somebody, but not somebody here. Joining a tenant is a decision,
	// and an unchecked one would let anybody with a Google account appear inside
	// a customer's tenant.
	//
	// The refusal is the one a password login gives for this exact situation,
	// word for word. It used to be the sentence the identity lookup uses for
	// somebody nobody has ever heard of, so one message answered two different
	// facts — and left the person who was told it unable to tell whether to
	// register or to ask somebody for an invitation.
	if !h.joining {
		return SignIn{}, &Failure{
			Reason: ReasonNoTenantAccess,
			Err:    rigerr.Forbidden("you do not have access to this tenant"),
		}
	}

	accountID, err = h.cfg.Store.JoinTenant(ctx, JoinInput{
		TenantID: tenantID, IdentityID: link.IdentityID, Profile: profile,
	})
	if err != nil {
		return SignIn{}, err
	}
	return SignIn{
		Link: link, TenantID: tenantID, AccountID: accountID,
		Profile: profile, Provider: p.Name, New: true, NewIdentity: created,
	}, nil
}

// identity answers who is signing in, without reference to any tenant.
//
// The second return says the person did not exist until now, which is what
// [SignIn.NewIdentity] reports.
//
// Every path through it that ends in a verified address writes the link, the
// repeat sign-in included. Recording the evidence is not a thing the first
// sign-in does and the rest skip, because a link older than the recording would
// then have no occasion to catch up.
func (h *Handler) identity(ctx context.Context, p Provider, profile Profile) (*Link, bool, error) {
	// The subject, always. An address is a display detail here.
	link, err := h.cfg.Store.FindLink(ctx, p.Name, profile.Subject)
	if err != nil {
		return nil, false, err
	}
	if link != nil {
		if !profile.EmailVerified {
			// Nothing to record, and nothing to refuse either: the link is what
			// authorises this sign-in, and it was made on evidence. A provider
			// that has stopped asserting the address — GitHub, for somebody who
			// removed the verified one — does not undo that.
			return link, false, nil
		}
		// Recorded again, because the first time is not the only time it is
		// true. LinkIdentity is what stamps the identity's address verified,
		// and a link made before rig recorded that evidence would otherwise
		// never be stamped at all: this branch is the only one a repeat sign-in
		// takes, so the person it was meant to help — signed up with a
		// password, never confirmed, linked a provider — would stay refused by
		// RequireVerifiedEmail forever, on a column rather than on anything
		// they did.
		//
		// The upsert is the same one a first link runs, on the same conflict
		// target, so it lands on the row already here rather than a second one.
		// Stamping only when the address is already unstamped is the store's,
		// which is what keeps this from moving a timestamp on every sign-in.
		link, err = h.cfg.Store.LinkIdentity(ctx, LinkInput{
			IdentityID: link.IdentityID, Provider: p.Name, Profile: profile,
		})
		return link, false, err
	}

	email := strings.ToLower(strings.TrimSpace(profile.EmailAddress))
	if email == "" {
		return nil, false, &Failure{
			Reason: ReasonNoAddress,
			Err: rigerr.BadRequest(
				"%s did not share an email address, so there is no account to sign in to", p.Name),
		}
	}

	identityID, err := h.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return nil, false, err
	}
	if identityID != uuid.Nil {
		// The check the whole package turns on. Anybody can register any
		// address at some provider; only a verified one is evidence.
		if !profile.EmailVerified {
			return nil, false, &Failure{
				Reason: ReasonUnverifiedAddress,
				Err: rigerr.Forbidden(
					"%s has not verified this address, so it cannot be linked to an existing account", p.Name),
			}
		}
		link, err := h.cfg.Store.LinkIdentity(ctx, LinkInput{
			IdentityID: identityID, Provider: p.Name, Profile: profile,
		})
		return link, false, err
	}

	// Nobody has this address anywhere. Creating the person is gated by
	// AllowProvisioning wherever it happens, because on its own it would be a
	// way to fill rig_identity from a sign-in page — and it is the only half of
	// that switch a sign-in with no named tenant reaches, since there is no
	// tenant for the other half to be about.
	if !h.cfg.AllowProvisioning {
		return nil, false, &Failure{
			Reason: ReasonNoAccount,
			Err:    rigerr.Forbidden("there is no account for this address"),
		}
	}
	link, err = h.cfg.Store.ProvisionIdentity(ctx, ProvisionInput{Provider: p.Name, Profile: profile})
	if err != nil {
		return nil, false, err
	}
	return link, true, nil
}

// fail records a refused sign-in and answers it.
//
// The recording is here rather than at the sixteen places that call it, and that
// is the whole reason this function takes a [*Failure]. Two of those places used
// to write an entry and fourteen did not, so an expired state cookie or a
// refused code exchange left no evidence anywhere that anybody had tried — the
// plain-text page was the only record. Logging is no longer something a branch
// remembers to do, so a branch added later cannot forget it.
//
// The order matters: the entry is written before [Config.OnError] is given the
// response, so a hook that redirects cannot cost the audit trail an entry.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, f *Failure) {
	// The provider's own spelling, because that is what the Succeeded entry
	// records and what every entry before this function existed recorded.
	// [Failure.Provider] is lowercased for an application to switch on, and the
	// two spellings in one column would be one provider an operator has to
	// remember to query twice. A request naming a provider rig does not have
	// keeps what it was given, which is nothing.
	provider := f.Provider
	if p, ok := h.providers[f.Provider]; ok {
		provider = p.Name
	}

	detail := map[string]any{
		"provider": provider,
		"reason":   string(f.Reason),
		// The message rig would have answered with, which for an internal
		// failure is the part that never reaches the client and so the only
		// place it is written down at all.
		"error": f.Error(),
	}
	// The provider's own word for it, kept here and nowhere else: it is
	// attacker-controlled text, and a log line is the one place that is safe.
	if f.ProviderError != "" {
		detail["provider_error"] = f.ProviderError
	}

	entry := authlog.Entry{
		Event: authlog.EventOAuthSignIn, Outcome: authlog.Failed,
		EmailAddress: strings.ToLower(f.EmailAddress),
		IPAddress:    remoteAddr(r), UserAgent: r.UserAgent(),
		Detail: detail,
	}
	// Only when there was one. A pointer to the nil UUID is not "no tenant", it
	// is a tenant that does not exist, and it would go in the column. A copy,
	// because the hook below is handed this Failure and what was recorded should
	// not depend on what it does with it.
	if f.TenantID != uuid.Nil {
		tenantID := f.TenantID
		entry.TenantID = &tenantID
	}
	h.write(r.Context(), entry)

	if h.cfg.OnError != nil {
		h.cfg.OnError(w, r, f)
		return
	}
	if h.cfg.Fail != nil {
		h.cfg.Fail(w, r, f)
		return
	}

	// Nobody to answer. The guard is here rather than in front of the two hooks
	// above on purpose: they are what writes the log line, and a request that was
	// abandoned is still worth one. This is only in front of rig's own write.
	if rigerr.Aborted(f) {
		return
	}

	// The classification is [httpx.AnswerFor]'s, which is the same call the
	// generated mapper and every other mounted route make. It used to be a third
	// hand-written copy of it, and a hand-written copy of this exact mapper has
	// already drifted once — authhttp's dropped the per-field detail for long
	// enough that a client could not highlight the field somebody got wrong.
	//
	// The envelope is still text/plain, which is the part that is not shared and
	// should not be: these two routes are the only ones rig serves that a person
	// reaches with their address bar, and a browser mid-navigation renders what
	// comes back as a document. A JSON envelope is a worse dead end than a
	// sentence. An application with a front end sends a redirect instead, through
	// [Config.OnError].
	answer := httpx.AnswerFor(w, f)
	http.Error(w, answer.Message, answer.Status)
}

func (h *Handler) write(ctx context.Context, e authlog.Entry) {
	if e.At.IsZero() {
		e.At = h.now()
	}
	h.cfg.Log.Write(ctx, e)
}
