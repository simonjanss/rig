package servergo

import (
	"strings"
	"time"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// corsModule is the generic half: a Policy and a Wrap, with no opinion about
// which headers this API reads.
const corsModule = runtimeModule + "/cors"

// corsFile emits the policy for this project's own front end.
//
// The lists are built here rather than defaulted in runtime/cors, because every
// entry in them is a fact about what *this* API reads and answers with, and that
// is what the document already describes. Three of the names vary per project —
// the tenant, revision and request-id headers — and they are constants in this
// same package, so the generated policy names them rather than their values.
//
// The alternative was a cors.RigDefaults() union in the runtime, and it cannot
// work: it would have to guess a renamed tenant header, and it would have to
// include the live-sync headers for every project including the ones with no
// shapes. Getting that list out of a project's own hand-written literal is how
// the header a sync cursor travels in ends up missing, and a subscription that
// stops after one response looks like a stream that died rather than like a
// CORS list with a hole in it.
func (e *emitter) corsFile() (gen.Artifact, error) {
	w := e.doc.API.Web
	b := gobuf.New(e.cfg.Package)
	corsPkg := b.Import(corsModule)

	b.Comment("AllowedOriginsEnv is where a deployment names the origins that may " +
		"call this API, as a comma-separated list.\n\n" +
		"Set, it **replaces** what rig.yaml said rather than adding to it — the " +
		"way the origin's own variable lets a deployment win. A deployment " +
		"overriding a list should not have to know what was in it.")
	b.L("const AllowedOriginsEnv = %s", gobuf.Quote(w.CORS.AllowedOriginsEnv))
	b.NL()

	e.allowedOriginsFunc(b, corsPkg)
	e.corsPolicyFunc(b, corsPkg)

	return artifact("cors.gen.go", b)
}

// allowedOriginsFunc emits the resolution: the environment, or the file.
func (e *emitter) allowedOriginsFunc(b *gobuf.Buf, corsPkg string) {
	w := e.doc.API.Web

	b.Comment("AllowedOrigins is who may call this API from a browser.\n\n" +
		"The front end's own origin is always one of them, which is why this can " +
		"fail: it comes from [WebOrigin], and a deployment that named only a " +
		"variable and set nothing has no front end. Anything in " +
		"`web.cors.allowed_origins` joins it — an administrative front end, a " +
		"preview deployment per branch.\n\n" +
		"[AllowedOriginsEnv] replaces the whole list when it is set. What this " +
		"cannot see is [OAuthHooks.WebOrigin]: a deployment that supplies its " +
		"origin in Go rather than in the environment has to set [Parts.CORS] as " +
		"well, or this refuses while naming a variable that deployment was never " +
		"going to use.")

	osPkg := b.Import("os")
	b.L("func AllowedOrigins() ([]string, error) {")
	b.L("if raw := %s.Getenv(AllowedOriginsEnv); raw != \"\" {", osPkg)
	b.L("return %s.Split(raw), nil", corsPkg)
	b.L("}")
	b.NL()
	b.L("origin, err := WebOrigin()")
	b.L("if err != nil {")
	b.L("return nil, err")
	b.L("}")

	if len(w.CORS.AllowedOrigins) == 0 {
		b.L("return []string{origin}, nil")
	} else {
		b.P("return append([]string{origin}, ")
		for i, o := range w.CORS.AllowedOrigins {
			if i > 0 {
				b.P(", ")
			}
			b.P("%s", gobuf.Quote(o))
		}
		b.L("), nil")
	}
	b.L("}")
	b.NL()
}

// corsPolicyFunc emits the policy itself.
func (e *emitter) corsPolicyFunc(b *gobuf.Buf, corsPkg string) {
	w := e.doc.API.Web

	b.Comment("CORS is the policy this API is answered under, for the origins " +
		"given.\n\n" +
		"The origins are a parameter rather than read in here so that the " +
		"dynamic case is the same function: a project whose allowed origins are " +
		"rows in a table passes what it has and sets " +
		"[github.com/simonjanss/rig/runtime/cors.Policy.AllowOrigin] on the " +
		"result.\n\n" +
		"There is no Access-Control-Allow-Credentials and " +
		"[github.com/simonjanss/rig/runtime/cors.Policy] will not write one: the " +
		"credential here is a bearer token in a header, so no cookie ever crosses " +
		"an origin. The one cookie rig does set for another origin is the sign-in " +
		"handoff, and that is read by script on the page it is delivered to " +
		"rather than sent back here.")

	b.L("func CORS(origins []string) %s.Policy {", corsPkg)
	b.L("return %s.Policy{", corsPkg)
	b.L("AllowedOrigins: origins,")

	b.L("AllowedMethods: []string{")
	e.corsMethods(b)
	b.L("},")

	b.L("AllowedHeaders: []string{")
	e.corsRequestHeaders(b)
	b.L("},")

	b.L("ExposedHeaders: []string{")
	e.corsResponseHeaders(b)
	b.L("},")

	if w.CORS.MaxAgeSeconds > 0 {
		b.L("MaxAge: %s,", genutil.GoDuration(b, ir.Duration(
			time.Duration(w.CORS.MaxAgeSeconds)*time.Second)))
	}
	b.L("}")
	b.L("}")
	b.NL()
}

// corsMethods is every method this API answers, plus OPTIONS for the preflight
// itself.
func (e *emitter) corsMethods(b *gobuf.Buf) {
	b.L("%s,", quoteList("DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"))

	if e.hasQueryRoute() {
		b.Comment("Search is a QUERY route on this API. Leaving it out is invisible " +
			"until it is not: the client sends QUERY and falls back to POST only on " +
			"a 405 or a 501, and a preflight that omits a method fails as a network " +
			"error — so there is no status for the fallback to read and search " +
			"fails with nothing in any log.")
		b.L("\"QUERY\",")
	}
}

// corsRequestHeaders is what a caller may send.
func (e *emitter) corsRequestHeaders(b *gobuf.Buf) {
	b.L("%s,", quoteList("Accept", "Authorization", "Content-Type", "If-None-Match"))

	b.Comment("Set by both SDKs on every unsafe method, so omitting it fails the " +
		"preflight on every POST rather than on the retried ones.")
	b.L("\"Idempotency-Key\",")

	if e.hasFiles() {
		b.Comment("A resumed or partial download asks for one.")
		b.L("\"Range\",")
	}

	b.Comment("The three that are this project's own spelling, named rather than " +
		"repeated so that renaming one in rig.yaml moves the policy with it.")
	b.L("RequestIDHeader,")
	b.L("RevisionHeader,")
	if e.hasTenantHeader() {
		b.L("TenantHeader,")
	}
}

// corsResponseHeaders is what a caller may read back.
//
// A browser hides every response header from script except a short safelist, so
// anything a client acts on has to be here. Each conditional entry below is a
// feature whose client half silently stops working without it.
func (e *emitter) corsResponseHeaders(b *gobuf.Buf) {
	b.L("RevisionHeader,")
	b.L("RequestIDHeader,")

	b.Comment("What a refused request tells a client to do about it. Without " +
		"these a 429 is a 429 with no schedule attached, and both SDKs fall back " +
		"to guessing one.")
	b.L("%s,", quoteList("Retry-After", "RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"))

	b.Comment("Whether a replayed idempotent write was replayed.")
	b.L("\"Idempotency-Replayed\",")

	if e.hasFiles() || e.servesOpenAPI() {
		b.L("\"ETag\",")
	}
	if e.hasFiles() {
		b.Comment("A download's filename, and the two a range response is read with.")
		b.L("%s,", quoteList("Content-Disposition", "Accept-Ranges", "Content-Range"))
	}

	if e.hasShapes() {
		b.Comment("The sync protocol's cursor travels in these. Until they are " +
			"exposed a browser hides them from the client, and the subscription " +
			"ends after one response — which looks like a stream that stopped " +
			"rather than like a policy with a hole in it.")
		b.L("%s,", quoteList("electric-handle", "electric-offset", "electric-schema"))
		b.L("%s,", quoteList("electric-cursor", "electric-up-to-date", "electric-has-data"))
		b.Comment("And whether this response came from the sync service or from " +
			"the fallback, which is the one thing a subscriber changes behaviour on.")
		b.L("\"X-Rig-Sync-Fallback\",")
	}
}

// hasQueryRoute reports whether any endpoint is reached with QUERY.
//
// Derived rather than read back off api.search_method, so that a route reaching
// QUERY some other way is covered the day it does.
func (e *emitter) hasQueryRoute() bool {
	for _, res := range e.resources() {
		for _, ep := range res.Endpoints {
			if strings.HasPrefix(ep.Pattern, "QUERY ") {
				return true
			}
			for _, alias := range ep.AliasPatterns {
				if strings.HasPrefix(alias, "QUERY ") {
					return true
				}
			}
		}
	}
	return false
}

// hasTenantHeader reports whether this project emits a TenantHeader constant,
// which is the same question as whether a caller may send one.
func (e *emitter) hasTenantHeader() bool {
	return e.hasAuth() && e.doc.API.Auth.Tenant.Uses(ir.TenantFromHeader)
}

// quoteList is one line of quoted strings, for a list whose entries are fixed
// and read as a group.
func quoteList(values ...string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, gobuf.Quote(v))
	}
	return strings.Join(quoted, ", ")
}

// corsWrap is the last thing mountWith does, and it is why Parts.Handler is an
// http.Handler rather than the mux it probably is.
//
// The whole handler, because which origins may read this API is a decision about
// the API and a policy with a route left out is a hole only a browser finds. It
// happens inside serve.withProbes, so the readiness check every second does not
// pay for it.
func (e *emitter) corsWrap(b *gobuf.Buf) {
	b.Comment("Cross-origin, last, around everything the application returned. " +
		"Parts.CORS is what an application supplies instead; nil is the policy " +
		"the `web:` block describes.")

	b.L("policy := %s.Policy{}", b.Import(corsModule))
	b.L("switch {")
	b.L("case parts.CORS != nil:")
	b.L("policy = *parts.CORS")
	b.L("default:")
	b.L("origins, err := AllowedOrigins()")
	b.L("if err != nil {")
	b.L("return nil, err")
	b.L("}")
	b.L("policy = CORS(origins)")
	b.L("}")
	b.NL()

	b.Comment("One line either way, beside \"serving the OpenAPI document\": a " +
		"front end that cannot reach this API is the commonest thing to be " +
		"looking for in a startup log, and an empty policy is a decision worth " +
		"seeing rather than a silence.")
	b.L("if policy.Enabled() {")
	b.L("app.Logger.InfoContext(ctx, %s, %s, policy.AllowedOrigins)",
		gobuf.Quote("answering cross-origin requests"), gobuf.Quote("origins"))
	b.L("} else {")
	b.L("app.Logger.InfoContext(ctx, %s, %s, %s)",
		gobuf.Quote("not answering cross-origin requests"), gobuf.Quote("cost"),
		gobuf.Quote("a browser on another origin cannot read any response from this API"))
	b.L("}")
	b.L("parts.Handler = policy.Wrap(parts.Handler)")
	b.NL()
}
