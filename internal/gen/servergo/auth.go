package servergo

import (
	"fmt"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// authModule is the foundation the generated wiring hands its configuration to.
// Nothing about authentication is generated: what is generated is the call.
const authModule = "github.com/simonjanss/rig/auth"

// DefaultRequestIDHeader is where the generated error mapper looks for the
// identifier it puts in a failure body.
const DefaultRequestIDHeader = "X-Request-Id"

// authEmitter writes the authentication wiring. It is a type of its own rather
// than more methods on [emitter] because it reads a different part of the
// document — the auth block, not the resources — and every one of its methods
// is about the same call.
type authEmitter struct {
	doc  *ir.Document
	auth *ir.Auth
	cfg  Options
}

// authFile is all of the wiring, in one file, because every part of it is about
// the same call.
//
// It joins the package the rest of the API is generated into, so the error
// mapper it reaches for is the one every other endpoint already uses and there
// is no second package for a project to import and keep in step.
func (e *authEmitter) authFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.constants(b)
	e.hooks(b)
	e.newFunc(b)
	e.configFunc(b)
	e.tenantFunc(b)
	e.limitsFunc(b)
	if e.oauth() != nil {
		e.signingKeyFunc(b)
		e.providersFunc(b)
	}

	return artifact("auth.gen.go", b)
}

func (e *authEmitter) oauth() *ir.AuthOAuth { return e.auth.OAuth }

// failure prefixes a message raised by the generated wiring.
//
// Go's convention is the package the error came from, and the package the wiring
// is generated into is the project's own API package rather than one this
// generator names.
func (e *authEmitter) failure(msg string) string { return e.cfg.Package + ": " + msg }

// needsPermissions reports whether the generated handlers check one, which is
// what makes a missing Grants function a mistake rather than a choice.
func (e *authEmitter) needsPermissions() bool { return len(e.doc.API.Permissions) > 0 }

// constants are the configured values a caller may need to name: the paths, the
// header, the origin. They are constants rather than fields so that a page
// drawing a sign-in form and the server serving it cannot disagree.
func (e *authEmitter) constants(b *gobuf.Buf) {
	a := e.auth

	b.Comment("BasePath is where the authentication endpoints are mounted. Every " +
		"route below it comes from rig/auth: " + a.BasePath + "/login, " +
		a.BasePath + "/refresh, and the rest.")
	b.L("const BasePath = %s", gobuf.Quote(a.BasePath))
	b.NL()

	if a.Tenant.Uses(ir.TenantFromHeader) {
		b.Comment("TenantHeader is the header a request names its tenant in.")
		b.L("const TenantHeader = %s", gobuf.Quote(a.Tenant.Header))
		b.NL()
	}
	if a.Tenant.Uses(ir.TenantFromQuery) {
		b.Comment("TenantQuery is the query parameter a request may name its tenant in. " +
			"It travels in links and in logs, which is why it is rarely the right " +
			"source in a deployment.")
		b.L("const TenantQuery = %s", gobuf.Quote(a.Tenant.Query))
		b.NL()
	}
	if a.Tenant.Uses(ir.TenantFromHost) && a.Tenant.DefaultSlugEnv != "" {
		b.Comment("DefaultSlugEnv holds the tenant slug to fall back to when the host " +
			"names none, which is what lets a host-based deployment be tried on a " +
			"machine that has no subdomain to offer.")
		b.L("const DefaultSlugEnv = %s", gobuf.Quote(a.Tenant.DefaultSlugEnv))
		b.NL()
	}

	if o := e.oauth(); o != nil {
		b.Comment("Providers are the sign-in providers this configuration offers, in " +
			"order, for a page that draws a button per name. A provider whose " +
			"credentials are absent is not offered, so ask ConfiguredProviders for " +
			"what this process actually has.")
		b.P("var Providers = []string{")
		for i, p := range o.Providers {
			if i > 0 {
				b.P(", ")
			}
			b.P("%s", gobuf.Quote(p.Name))
		}
		b.L("}")
		b.NL()

		if o.SigningKeyEnv != "" {
			b.Comment("SigningKeyEnv holds the key that signs the state parameter. Empty " +
				"generates one per process, which is fine for one and wrong for several: " +
				"a callback may arrive at a different replica than the one that started " +
				"the sign-in.")
			b.L("const SigningKeyEnv = %s", gobuf.Quote(o.SigningKeyEnv))
			b.NL()
		}

		b.Comment("OriginScheme is the scheme a callback URL is built with. A provider " +
			"compares that URL exactly, so this follows the configured origin rather " +
			"than the request.")
		b.L("const OriginScheme = %s", gobuf.Quote(e.originScheme()))
		b.NL()

		e.baseURLFunc(b)
	}
}

// originScheme is decided here rather than at runtime, because a redirect URI is
// registered with a provider exactly and must not depend on how one request
// happened to arrive.
func (e *authEmitter) originScheme() string {
	o := e.oauth()
	switch {
	case strings.HasPrefix(o.BaseURL, "http://"):
		return "http"
	case strings.HasPrefix(o.BaseURL, "https://"):
		return "https"
	case o.Insecure:
		return "http"
	default:
		return "https"
	}
}

// baseURLFunc emits the origin a provider redirects back to.
func (e *authEmitter) baseURLFunc(b *gobuf.Buf) {
	o := e.oauth()

	b.Comment("BaseURL is this application's own origin, which a provider has " +
		"registered as the prefix of its callback URL.\n\n" +
		"It is a function rather than a constant because the environment gets the " +
		"last word: the same binary is deployed at more than one origin, and the " +
		"configuration cannot know which.")
	b.L("func BaseURL() string {")
	switch {
	case o.BaseURLEnv != "" && o.BaseURL != "":
		strPkg := b.Import("strings")
		cmpPkg := b.Import("cmp")
		osPkg := b.Import("os")
		b.L("return %s.TrimRight(%s.Or(%s.Getenv(%s), %s), \"/\")",
			strPkg, cmpPkg, osPkg, gobuf.Quote(o.BaseURLEnv), gobuf.Quote(o.BaseURL))
	case o.BaseURLEnv != "":
		strPkg := b.Import("strings")
		osPkg := b.Import("os")
		b.L("return %s.TrimRight(%s.Getenv(%s), \"/\")", strPkg, osPkg, gobuf.Quote(o.BaseURLEnv))
	default:
		b.L("return %s", gobuf.Quote(o.BaseURL))
	}
	b.L("}")
	b.NL()
}

// hooks is the one struct a project fills in.
func (e *authEmitter) hooks(b *gobuf.Buf) {
	a := e.auth
	var (
		httpPkg  = b.Import("net/http")
		authhttp = b.Import(authModule + "/authhttp")
		account  = b.Import(authModule + "/account")
	)

	b.Comment("Hooks are what a configuration file cannot hold: the functions this " +
		"application has to supply, and nothing else.\n\n" +
		"Everything with a fixed answer is in rig.yaml and already applied — the " +
		"lifetimes, the rotation leeway, the rate limits, the password policy, which " +
		"providers exist. What is left here is behaviour: who holds a permission, " +
		"where mail goes, what a new tenant needs. Each is optional unless the " +
		"configuration makes it necessary, and Config says so rather than failing " +
		"later.")
	b.L("type Hooks struct {")

	if e.needsPermissions() {
		b.Comment("Grants answers what an account may do, and is required: every " +
			"generated handler checks a permission, so without this nobody holds one " +
			"and every endpoint answers 403.\n\n" +
			"rig derives the permission keys from the schema — PermissionKeys(), beside " +
			"this, is " +
			"the whole catalogue — and generates the check. Deciding who holds which is " +
			"this application's own model.")
	} else {
		b.Comment("Grants answers what an account may do. This project generates no " +
			"permission checks, so it is only read by an endpoint that carries one of " +
			"the foundation's own keys.")
	}
	b.L("Grants %s.Grants", authhttp)
	b.NL()

	b.Comment("Notifier sends the mail a flow needs: a reset link, a verification " +
		"link, an invitation. Nil sends none, which makes those flows unusable rather " +
		"than silently broken — the token is in the response for a test to read, and " +
		"nothing reaches a person.")
	b.L("Notifier %s.Notifier", account)
	b.NL()

	if a.AllowTenantCreation {
		b.Comment("Tenants is this application's policy for making one: who may, what a " +
			"name may be, how a slug is derived, and what else a new tenant needs in the " +
			"transaction that made it. The zero value lets anybody signed in make one " +
			"called anything.\n\n" +
			"rig owns the mechanism — the row, the first account, the transaction — " +
			"because that part is the same everywhere and getting it half right leaves a " +
			"tenant nobody can reach.")
		b.L("Tenants %s.TenantOptions", account)
		b.NL()
	}

	if a.Tenant.Uses(ir.TenantFromHook) {
		uuidPkg := b.Import("github.com/google/uuid")
		b.Comment("Tenant resolves which tenant a request is for, and is required " +
			"because auth.tenant.from names the hook source. Answer uuid.Nil for a " +
			"request that names no tenant: that is an ordinary answer, not a failure.")
		b.L("Tenant func(*%s.Request) (%s.UUID, error)", httpPkg, uuidPkg)
		b.NL()
	}

	b.Comment("OnSessionRefresh replaces a session's payload every time it is " +
		"refreshed. Nil carries the previous one forward unchanged.\n\n" +
		"Not for permissions: it is written at sign-in and again only at refresh, so " +
		"anything here outlives its own revocation by up to the refresh lifetime. " +
		"Grants answers that question, per request.")
	b.L("OnSessionRefresh func(%s.Context, *%s.Token) (%s.RawMessage, error)",
		b.Import("context"), b.Import(authModule+"/session"), b.Import("encoding/json"))
	b.NL()

	b.Comment("OnError renders a failure. Nil uses this package's own error mapper, " +
		"so an authentication failure is shaped like every other failure this API " +
		"returns.")
	b.L("OnError func(w %s.ResponseWriter, r *%s.Request, err error)", httpPkg, httpPkg)
	b.NL()

	if e.oauth() != nil {
		b.Comment("OAuth is what a provider sign-in needs beyond the configuration.")
		b.L("OAuth OAuthHooks")
		b.NL()
	}

	b.Comment("Now is the clock, for tests.")
	b.L("Now func() %s.Time", b.Import("time"))
	b.L("}")
	b.NL()

	if e.oauth() != nil {
		e.oauthHooks(b)
	}
}

func (e *authEmitter) oauthHooks(b *gobuf.Buf) {
	var (
		httpPkg   = b.Import("net/http")
		oauthPkg  = b.Import(authModule + "/oauth")
		hasOrigin = !e.oauth().OriginFromHost
	)

	b.Comment("OAuthHooks are the provider decisions that are code.")
	b.L("type OAuthHooks struct {")

	b.Comment("OnSignIn finishes a provider sign-in. Nil issues a session and answers " +
		"with the same body a password login does.\n\n" +
		"A browser flow usually wants a cookie and a redirect instead, and that " +
		"depends on what the client is — a single-page application, a server-rendered " +
		"one, a mobile app catching a deep link — which is why rig will not choose.")
	b.L("OnSignIn func(w %s.ResponseWriter, r *%s.Request, in %s.SignIn) error", httpPkg, httpPkg, oauthPkg)
	b.NL()

	if hasOrigin {
		b.Comment("Origin overrides the configured origin for one request, for an " +
			"application served at several. Setting auth.oauth.origin_from_host in " +
			"rig.yaml is the declarative form of the usual answer.")
		b.L("Origin func(r *%s.Request) string", httpPkg)
		b.NL()
	}

	b.Comment("Extra are providers this application builds itself: an in-house " +
		"identity server, or a stand-in served by the application during " +
		"development. They are appended to the configured ones.")
	b.L("Extra []%s.Provider", oauthPkg)
	b.NL()

	b.Comment("ReturnTo are further origins a finished sign-in may land on, added to " +
		"the configured auth.oauth.allowed_return_to.\n\n" +
		"For the deployment whose set is not fixed. An application with a tenant per " +
		"subdomain has one origin per tenant, and a list in a file cannot name a " +
		"tenant that was created this morning.")
	b.L("ReturnTo []string")
	b.L("}")
	b.NL()
}

// newFunc is the call a main function makes.
func (e *authEmitter) newFunc(b *gobuf.Buf) {
	var (
		authPkg = b.Import(authModule)
		poolPkg = b.Import("github.com/jackc/pgx/v5/pgxpool")
	)

	b.Comment("New assembles the authentication foundation over a pool.\n\n" +
		"Hand the result to the generated API as api.Server.Auth and both halves are " +
		"wired at once: every handler identifies its caller with the same " +
		"verification that issued the token, and " + e.auth.BasePath + "/* is mounted " +
		"on the same mux.")
	b.L("func New(pool *%s.Pool, h Hooks) (*%s.Auth, error) {", poolPkg, authPkg)
	b.L("cfg, err := Config(pool, h)")
	b.L("if err != nil {")
	b.L("return nil, err")
	b.L("}")
	b.L("return %s.New(cfg)", authPkg)
	b.L("}")
	b.NL()
}

// configFunc builds the configuration itself. It is exported separately from New
// so that a project needing one field this generator cannot express can take the
// configuration, change that field, and call auth.New itself — rather than
// abandoning the generated wiring wholesale.
func (e *authEmitter) configFunc(b *gobuf.Buf) {
	a := e.auth
	var (
		authPkg  = b.Import(authModule)
		poolPkg  = b.Import("github.com/jackc/pgx/v5/pgxpool")
		errsPkg  = b.Import("errors")
		pwPkg    = b.Import(authModule + "/password")
		httpPkg  = b.Import("net/http")
		fail     = "return " + authPkg + ".Config{}, "
		hasProxy = len(a.TrustedProxies) > 0
	)

	b.Comment("Config is the configuration rig.yaml describes, with the hooks folded " +
		"in.\n\n" +
		"Every value in it was resolved when the configuration was read, so this " +
		"function makes no decisions: it is the same numbers the specification " +
		"documents and the same ones a generated client is built against.")
	b.L("func Config(pool *%s.Pool, h Hooks) (%s.Config, error) {", poolPkg, authPkg)

	b.L("if pool == nil {")
	b.L("%s%s.New(%q)", fail, errsPkg,
		e.failure("no pool: the authentication foundation lives in the database"))
	b.L("}")

	if e.needsPermissions() {
		b.Comment("Refused at construction rather than at the first request. Every " +
			"generated handler checks a permission, so a nil Grants is an API where " +
			"every endpoint answers 403 — a failure that looks like a policy decision.")
		b.L("if h.Grants == nil {")
		b.L("%s%s.New(%q)", fail, errsPkg,
			e.failure("no Grants: every endpoint checks a permission, so nobody would hold one"))
		b.L("}")
	}
	if a.Tenant.Uses(ir.TenantFromHook) {
		b.L("if h.Tenant == nil {")
		b.L("%s%s.New(%q)", fail, errsPkg,
			e.failure("no Tenant: auth.tenant.from names the hook source"))
		b.L("}")
	}
	if a.RequireVerifiedEmail {
		b.Comment("A verification link nobody sends is an account nobody can ever use, " +
			"and require_verified_email is what makes that fatal rather than merely " +
			"awkward.")
		b.L("if h.Notifier == nil {")
		b.L("%s%s.New(%q)", fail, errsPkg,
			e.failure("no Notifier, but auth.require_verified_email is set, so nobody could verify an address"))
		b.L("}")
	}
	b.NL()

	if hasProxy {
		netipPkg := b.Import("net/netip")
		fmtPkg := b.Import("fmt")
		b.Comment("The ranges whose X-Forwarded-For may be believed. rig validated " +
			"these when it read the configuration; parsing here is what turns them " +
			"into values rather than trusting that it did.")
		b.L("trusted := make([]%s.Prefix, 0, %d)", netipPkg, len(a.TrustedProxies))
		b.P("for _, cidr := range []string{")
		for i, cidr := range a.TrustedProxies {
			if i > 0 {
				b.P(", ")
			}
			b.P("%s", gobuf.Quote(cidr))
		}
		b.L("} {")
		b.L("prefix, err := %s.ParsePrefix(cidr)", netipPkg)
		b.L("if err != nil {")
		b.L("%s%s.Errorf(%q, cidr, err)", fail, fmtPkg, e.failure("trusted proxy %q: %w"))
		b.L("}")
		b.L("trusted = append(trusted, prefix)")
		b.L("}")
		b.NL()
	}

	b.L("cfg := %s.Config{", authPkg)
	b.L("Pool: pool,")
	b.L("BasePath: BasePath,")

	if e.tenantIsDefault() {
		b.L("// Tenant is left to rig/auth's default, which reads %s.", a.Tenant.Header)
	} else {
		b.L("Tenant: tenant(pool, h),")
	}

	b.NL()
	b.L("AccessTTL: %s,", genutil.GoDuration(b, a.Session.AccessTTL))
	b.L("RefreshTTL: %s,", genutil.GoDuration(b, a.Session.RefreshTTL))
	b.L("RememberTTL: %s,", genutil.GoDuration(b, a.Session.RememberTTL))
	b.L("RotationLeeway: %s,", genutil.GoDuration(b, a.Session.RotationLeeway))
	b.L("IdentitySessionTTL: %s,", genutil.GoDuration(b, a.Session.IdentityTTL))
	if a.Session.CacheTTL > 0 {
		b.L("// Verified access tokens are cached, so a revoked session keeps working")
		b.L("// for up to this long.")
		b.L("SessionCacheTTL: %s,", genutil.GoDuration(b, a.Session.CacheTTL))
	}
	b.NL()

	b.L("Policy: %s.Policy{MinLength: %d, MaxLength: %d},",
		pwPkg, a.Password.MinLength, a.Password.MaxLength)
	if a.Password.BreachCheck {
		b.L("// Only a hash prefix is sent, and the check fails open: a third party's")
		b.L("// outage must not stop somebody changing their password.")
		b.L("BreachChecker: %s.NewHIBP(),", pwPkg)
	}
	b.NL()

	b.L("AllowRegistration: %t,", a.AllowRegistration)
	b.L("AllowTenantCreation: %t,", a.AllowTenantCreation)
	b.L("RequireVerifiedEmail: %t,", a.RequireVerifiedEmail)
	if a.AllowTenantCreation {
		b.L("Tenants: h.Tenants,")
	}
	b.NL()

	b.L("Grants: h.Grants,")
	b.L("Notifier: h.Notifier,")
	b.L("OnSessionRefresh: h.OnSessionRefresh,")
	b.L("OnError: h.OnError,")
	b.L("Limits: limits(),")
	if hasProxy {
		b.L("TrustedProxies: trusted,")
	}
	b.L("Now: h.Now,")
	b.L("}")
	b.NL()

	if e.oauth() != nil {
		e.oauthConfig(b, fail)
	}

	b.Comment("So an authentication failure looks like every other failure this API " +
		"returns. The mapper is this package's own, which is the point of the " +
		"wiring being generated here rather than beside it.")
	b.L("if cfg.OnError == nil {")
	b.L("cfg.OnError = func(w %s.ResponseWriter, r *%s.Request, err error) {", httpPkg, httpPkg)
	b.L("DefaultErrorMapper(w, r, RequestContext{")
	b.L("RequestID: r.Header.Get(%s),", gobuf.Quote(e.cfg.RequestIDHeader))
	b.L("Method: r.Method,")
	b.L("Path: r.URL.Path,")
	b.L("}, err)")
	b.L("}")
	b.L("}")
	b.NL()

	b.L("return cfg, nil")
	b.L("}")
	b.NL()
}

// oauthConfig fills in the provider half, which is skipped entirely when this
// process has no credentials for any of them.
func (e *authEmitter) oauthConfig(b *gobuf.Buf, fail string) {
	o := e.oauth()
	var (
		authPkg = b.Import(authModule)
		httpPkg = b.Import("net/http")
	)

	b.Comment("Providers are wired only when this process has credentials for at " +
		"least one. A deployment that has none mounts no provider routes, which is " +
		"better than mounting a button that cannot work.")
	b.L("configured, err := ConfiguredProviders(h.OAuth)")
	b.L("if err != nil {")
	b.L("%serr", fail)
	b.L("}")
	b.L("if len(configured) > 0 {")
	if o.SigningKeyEnv != "" {
		b.L("key, err := signingKey()")
		b.L("if err != nil {")
		b.L("%serr", fail)
		b.L("}")
	}
	b.L("cfg.OAuth = %s.OAuth{", authPkg)
	b.L("Providers: configured,")
	b.L("BaseURL: BaseURL(),")

	if o.OriginFromHost {
		b.Comment("A callback comes back to the host it started at. Not a nicety: the " +
			"state cookie carrying the PKCE verifier is set on that host, and a browser " +
			"will not send it to a sibling subdomain — so with a tenant per host, every " +
			"one of these origins is registered with the provider.")
		b.L("Origin: func(r *%s.Request) string { return OriginScheme + \"://\" + r.Host },", httpPkg)
	} else {
		b.L("Origin: h.OAuth.Origin,")
	}

	if o.SigningKeyEnv != "" {
		b.L("SigningKey: key,")
	}
	b.L("StateTTL: %s,", genutil.GoDuration(b, o.StateTTL))
	b.L("AllowProvisioning: %t,", o.AllowProvisioning)
	b.P("AllowedReturnTo: append([]string{")
	for i, r := range o.AllowedReturnTo {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s", gobuf.Quote(r))
	}
	b.L("}, h.OAuth.ReturnTo...),")
	if o.Insecure {
		b.L("// Plain HTTP, so the __Host- cookie a browser would insist on is")
		b.L("// unavailable. Never set anywhere real.")
		b.L("Insecure: true,")
	}
	b.L("OnSignIn: h.OAuth.OnSignIn,")
	b.L("}")
	b.L("}")
	b.NL()
}

// tenantIsDefault reports whether the configured resolution is exactly what
// rig/auth already does, in which case the generated file says so instead of
// emitting a function that reimplements it.
func (e *authEmitter) tenantIsDefault() bool {
	t := e.auth.Tenant
	return len(t.Sources) == 1 && t.Sources[0] == ir.TenantFromHeader &&
		t.Header == "X-Tenant-Id"
}

// tenantFunc emits the resolver: the sources in order, and the first that names
// a tenant wins.
func (e *authEmitter) tenantFunc(b *gobuf.Buf) {
	if e.tenantIsDefault() {
		return
	}

	a := e.auth
	var (
		httpPkg = b.Import("net/http")
		uuidPkg = b.Import("github.com/google/uuid")
		poolPkg = b.Import("github.com/jackc/pgx/v5/pgxpool")
		errPkg  = b.Import(runtimeModule + "/rigerr")
	)

	names := make([]string, 0, len(a.Tenant.Sources))
	for _, s := range a.Tenant.Sources {
		names = append(names, string(s))
	}

	b.Comment("tenant resolves which tenant a request is for, from " +
		strings.Join(names, ", then ") + ".\n\n" +
		"It is consulted only where the tenant cannot be known some other way: a " +
		"sign-in, a password reset, the start of a provider flow. Everything else " +
		"takes its tenant from the token.\n\n" +
		"A request that names none is an ordinary answer rather than an error. A " +
		"single sign-in page cannot know which tenants an address belongs to before " +
		"the password has been checked, so uuid.Nil means unspecified and login " +
		"resolves it.")
	b.L("func tenant(pool *%s.Pool, h Hooks) func(*%s.Request) (%s.UUID, error) {",
		poolPkg, httpPkg, uuidPkg)
	b.L("return func(r *%s.Request) (%s.UUID, error) {", httpPkg, uuidPkg)

	for _, source := range a.Tenant.Sources {
		switch source {
		case ir.TenantFromHeader:
			b.L("if raw := r.Header.Get(TenantHeader); raw != \"\" {")
			b.L("id, err := %s.Parse(raw)", uuidPkg)
			b.L("if err != nil {")
			b.L("// Present and malformed is a caller getting it wrong, which is")
			b.L("// different from leaving it out.")
			b.L("return %s.Nil, %s.BadRequest(\"%%s is not a valid identifier\", TenantHeader)",
				uuidPkg, errPkg)
			b.L("}")
			b.L("return id, nil")
			b.L("}")

		case ir.TenantFromQuery:
			b.L("if raw := r.URL.Query().Get(TenantQuery); raw != \"\" {")
			b.L("id, err := %s.Parse(raw)", uuidPkg)
			b.L("if err != nil {")
			b.L("return %s.Nil, %s.BadRequest(\"%%s is not a valid identifier\", TenantQuery)",
				uuidPkg, errPkg)
			b.L("}")
			b.L("return id, nil")
			b.L("}")

		case ir.TenantFromHost:
			b.L("if id, found, err := tenantForHost(r.Context(), pool, r.Host); err != nil || found {")
			b.L("return id, err")
			b.L("}")

		case ir.TenantFromHook:
			b.L("if id, err := h.Tenant(r); err != nil || id != %s.Nil {", uuidPkg)
			b.L("return id, err")
			b.L("}")
		}
	}

	b.L("return %s.Nil, nil", uuidPkg)
	b.L("}")
	b.L("}")
	b.NL()

	if a.Tenant.Uses(ir.TenantFromHost) {
		e.hostLookup(b)
	}
}

// hostLookup emits the slug lookup that makes a subdomain name a tenant.
func (e *authEmitter) hostLookup(b *gobuf.Buf) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		poolPkg = b.Import("github.com/jackc/pgx/v5/pgxpool")
		pgxPkg  = b.Import("github.com/jackc/pgx/v5")
		errsPkg = b.Import("errors")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		strPkg  = b.Import("strings")
		netPkg  = b.Import("net")
	)

	b.Comment("tenantForHost reads the tenant out of the Host.\n\n" +
		"This is the source that survives a provider redirect. A callback URL is " +
		"registered with the provider and fixed, so it carries no header and no " +
		"parameter of yours — and a host does.\n\n" +
		"One indexed lookup per request that names a tenant. A cache is the obvious " +
		"next thing and is deliberately not here: a slug changing has to take effect, " +
		"and this is the sort of query Postgres answers from memory anyway.")
	b.L("func tenantForHost(ctx %s.Context, pool *%s.Pool, host string) (%s.UUID, bool, error) {",
		ctxPkg, poolPkg, uuidPkg)
	b.L("slug := TenantSlug(host)")
	b.L("if slug == \"\" {")
	b.L("return %s.Nil, false, nil", uuidPkg)
	b.L("}")
	b.NL()
	b.L("var id %s.UUID", uuidPkg)
	b.L("err := pool.QueryRow(ctx,")
	b.L("`SELECT id FROM rig_tenant WHERE lower(slug) = $1 AND deleted_at IS NULL AND is_active`,")
	b.L("slug).Scan(&id)")
	b.L("switch {")
	b.L("case %s.Is(err, %s.ErrNoRows):", errsPkg, pgxPkg)
	b.L("// A host nobody is at. Refused rather than quietly becoming \"no tenant\",")
	b.L("// because a typo in a subdomain should say so here instead of failing")
	b.L("// somewhere later.")
	b.L("return %s.Nil, false, %s.BadRequest(\"no tenant is served at %%s\", slug)", uuidPkg, errPkg)
	b.L("case err != nil:")
	b.L("return %s.Nil, false, %s.Internal(err, \"resolve the tenant\")", uuidPkg, errPkg)
	b.L("}")
	b.L("return id, true, nil")
	b.L("}")
	b.NL()

	b.Comment("TenantSlug is which tenant a host names: the leftmost label. An address " +
		"is not a name with a subdomain in front of it, so the leading label of " +
		"127.0.0.1 names nothing.\n\n" +
		"Exported because more than the resolver asks. A page that names the tenant " +
		"on screen has to agree with the request that resolved it, and two " +
		"implementations of one question eventually disagree.")
	b.L("func TenantSlug(host string) string {")
	b.L("if h, _, found := %s.Cut(host, \":\"); found {", strPkg)
	b.L("host = h")
	b.L("}")
	b.L("if %s.ParseIP(host) == nil {", netPkg)
	b.L("if slug, rest, found := %s.Cut(host, \".\"); found && rest != \"\" && slug != \"\" {", strPkg)
	b.L("return %s.ToLower(slug)", strPkg)
	b.L("}")
	b.L("}")
	if e.auth.Tenant.DefaultSlugEnv != "" {
		osPkg := b.Import("os")
		b.L("// The host names no tenant, so the environment may.")
		b.L("return %s.ToLower(%s.TrimSpace(%s.Getenv(DefaultSlugEnv)))", strPkg, strPkg, osPkg)
	} else {
		b.L("return \"\"")
	}
	b.L("}")
	b.NL()
}

// limitsFunc emits the rate limits.
//
// Only the numbers come from the configuration. The name, the counted event and
// what clears it stay rig's, because a limit counting a different event would
// not be the same limit under the same name.
func (e *authEmitter) limitsFunc(b *gobuf.Buf) {
	thr := b.Import(runtimeModule + "/throttle")
	l := e.auth.Limits

	b.Comment("limits are the rate limits from rig.yaml, counted in the database so " +
		"two replicas cannot disagree about how many times a password has been tried " +
		"and a restart does not clear somebody's lockout.\n\n" +
		"The configuration sets how many and over how long. Which event each limit " +
		"counts, and what clears it, is rig's — a limit counting something else under " +
		"the same name would not be the same limit.")
	b.L("func limits() %s.Defaults {", thr)
	b.L("d := %s.Standard()", thr)
	for _, pair := range []struct {
		field string
		limit ir.AuthLimit
	}{
		{"LoginByEmail", l.LoginByEmail},
		{"LoginByIP", l.LoginByIP},
		{"PasswordReset", l.PasswordReset},
		{"VerificationResend", l.VerificationResend},
		{"Refresh", l.Refresh},
		{"APIKeyFailures", l.APIKeyFailures},
	} {
		b.L("d.%s.Max, d.%s.Window = %d, %s",
			pair.field, pair.field, pair.limit.Max, genutil.GoDuration(b, pair.limit.Window))
	}
	b.L("return d")
	b.L("}")
	b.NL()
}

// signingKeyFunc emits the read of the key that signs the state parameter.
//
// It is a function rather than an expression because the failure is worth a
// sentence: a missing key is the one configuration mistake here that produces a
// sign-in which works on a laptop and fails behind a load balancer.
func (e *authEmitter) signingKeyFunc(b *gobuf.Buf) {
	o := e.oauth()
	if o == nil || o.SigningKeyEnv == "" {
		return
	}
	var (
		osPkg   = b.Import("os")
		fmtPkg  = b.Import("fmt")
		errsPkg = b.Import("errors")
	)

	b.Comment("signingKey is the key that signs the cookie carrying the state and the " +
		"PKCE verifier across a sign-in's round trip.\n\n" +
		"At least 32 bytes, from the environment, because it is a secret. It has to " +
		"be the same key in every replica: a callback may arrive at a different one " +
		"than the one that started the sign-in, and a key invented per process is a " +
		"sign-in that fails whenever a load balancer is doing its job.")
	b.L("func signingKey() ([]byte, error) {")
	b.L("key := []byte(%s.Getenv(SigningKeyEnv))", osPkg)
	b.L("switch {")
	b.L("case len(key) >= 32:")
	b.L("return key, nil")

	if o.Insecure {
		randPkg := b.Import("crypto/rand")
		b.L("case len(key) == 0:")
		b.L("// auth.oauth.insecure is set, which says this is local development: one")
		b.L("// process serves everything and there is no replica to share a key with.")
		b.L("// A deployment reaches the error below instead.")
		b.L("key = make([]byte, 32)")
		b.L("if _, err := %s.Read(key); err != nil {", randPkg)
		b.L("return nil, %s.Errorf(%q, err)", fmtPkg, e.failure("generate a development signing key: %w"))
		b.L("}")
		b.L("return key, nil")
	}

	b.L("}")
	b.L("return nil, %s.New(%s)", errsPkg, gobuf.Quote(e.failure(fmt.Sprintf(
		"%s must hold at least 32 bytes: it signs the OAuth state parameter, "+
			"and every replica has to use the same one", o.SigningKeyEnv))))
	b.L("}")
	b.NL()
}

// providersFunc emits provider construction from the environment.
func (e *authEmitter) providersFunc(b *gobuf.Buf) {
	o := e.oauth()
	var (
		oauthPkg = b.Import(authModule + "/oauth")
		osPkg    = b.Import("os")
		errsPkg  = b.Import("errors")
	)

	b.Comment("ConfiguredProviders are the providers this process has credentials for.\n\n" +
		"A client secret is not configuration: it is a secret, so rig.yaml names the " +
		"environment variable and this reads it. A provider whose pair is absent is " +
		"skipped rather than mounted broken, which is what lets one binary offer " +
		"Google in a deployment and nothing at all on a laptop. A provider marked " +
		"required refuses to start instead.\n\n" +
		"Exported so a sign-in page can draw a button per provider that actually " +
		"works, rather than one per provider somebody hoped for.")
	b.L("func ConfiguredProviders(h OAuthHooks) ([]%s.Provider, error) {", oauthPkg)
	b.L("out := make([]%s.Provider, 0, %d)", oauthPkg, len(o.Providers)+1)

	for _, p := range o.Providers {
		b.NL()
		b.L("if id, secret := %s.Getenv(%s), %s.Getenv(%s); id != \"\" && secret != \"\" {",
			osPkg, gobuf.Quote(p.ClientIDEnv), osPkg, gobuf.Quote(p.ClientSecretEnv))
		switch p.Name {
		case ir.ProviderMicrosoft:
			if p.TenantEnv != "" {
				b.L("// Microsoft's own idea of a tenant, which has nothing to do with")
				b.L("// rig's: empty means common, which accepts any account.")
				b.L("out = append(out, %s.Microsoft(id, secret, %s.Getenv(%s)))",
					oauthPkg, osPkg, gobuf.Quote(p.TenantEnv))
			} else {
				b.L("out = append(out, %s.Microsoft(id, secret, \"\"))", oauthPkg)
			}
		case ir.ProviderGitHub:
			b.L("out = append(out, %s.GitHub(id, secret))", oauthPkg)
		default:
			b.L("out = append(out, %s.Google(id, secret))", oauthPkg)
		}
		if p.Required {
			b.L("} else {")
			b.L("// Marked required, so a deployment missing its credentials refuses to")
			b.L("// start rather than quietly offering one provider fewer.")
			b.L("return nil, %s.New(%s)", errsPkg,
				gobuf.Quote(e.failure(fmt.Sprintf("provider %s is required, but %s and %s are not both set",
					p.Name, p.ClientIDEnv, p.ClientSecretEnv))))
		}
		b.L("}")
	}

	b.NL()
	b.L("return append(out, h.Extra...), nil")
	b.L("}")
	b.NL()
}
