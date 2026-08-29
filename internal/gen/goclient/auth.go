package goclient

import (
	"time"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// authFile emits what the client knows about this project's authentication.
//
// The endpoints themselves are not generated — they are the same in every
// project that turns authentication on — but the numbers are, and they are the
// server's own: a client that knows the access lifetime and the rotation leeway
// refreshes ahead of expiry instead of finding out through a refused request.
// That is the reason the configuration lives in rig.yaml rather than in a Go
// literal in main.go.
func (e *emitter) authFile(auth *ir.Auth) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)
	rig := e.client(b)

	b.Comment("AuthProfile is this API's authentication, as the project " +
		"configured it.\n\n" +
		"Every value here is resolved rather than optional: a zero means somebody " +
		"wrote a zero, not \"use the default\". The client reads it so that the " +
		"lifetimes it acts on are the ones the server enforces.")
	b.L("var AuthProfile = %s.AuthProfile{", rig)
	b.L("BasePath: %s,", gobuf.Quote(auth.BasePath))
	b.NL()
	b.L("AccessTTL: %s,", genutil.GoDuration(b, auth.Session.AccessTTL))
	b.L("RefreshTTL: %s,", genutil.GoDuration(b, auth.Session.RefreshTTL))
	b.L("RememberTTL: %s,", genutil.GoDuration(b, auth.Session.RememberTTL))
	b.L("RotationLeeway: %s,", genutil.GoDuration(b, auth.Session.RotationLeeway))
	b.L("IdentityTTL: %s,", genutil.GoDuration(b, auth.Session.IdentityTTL))
	// The `cache:` block rather than anything under `auth:`, because it is one
	// number covering both of the reads rig caches. Zero for a project that
	// caches neither, which is the honest answer to "how stale can this be": not
	// at all, every request reads the row.
	b.L("CacheTTL: %s,", genutil.GoDuration(b, cacheBackstop(e.doc)))
	b.NL()

	if auth.Tenant.Uses(ir.TenantFromHeader) && auth.Tenant.Header != "" {
		b.L("TenantHeader: %s,", gobuf.Quote(auth.Tenant.Header))
	}
	if auth.Tenant.Uses(ir.TenantFromQuery) && auth.Tenant.Query != "" {
		b.L("TenantQuery: %s,", gobuf.Quote(auth.Tenant.Query))
	}
	b.NL()

	// Which routes exist follows from the configuration, the same way the
	// server's own mounting does. A client that asked for one that is not there
	// would get a 404 saying only that the URL is wrong.
	b.L("HasRegistration: %t,", auth.AllowRegistration)
	b.L("HasTenantCreation: %t,", auth.AllowTenantCreation)
	b.L("HasIdentitySessions: true,")
	b.L("HasAPIKeys: true,")

	if auth.OAuth != nil && len(auth.OAuth.Providers) > 0 {
		b.NL()
		b.P("OAuthProviders: []string{")
		for i, p := range auth.OAuth.Providers {
			if i > 0 {
				b.P(", ")
			}
			b.P("%s", gobuf.Quote(p.Name))
		}
		b.L("},")
	}
	b.L("}")
	b.NL()

	e.permissionConstants(b)

	return e.artifact("auth.gen.go", b)
}

// permissionConstants name every permission the API's endpoints require, so a
// caller minting an API key can ask for one by name rather than by string.
func (e *emitter) permissionConstants(b *gobuf.Buf) {
	if len(e.doc.API.Permissions) == 0 {
		return
	}

	b.Comment("The permissions this API's endpoints require. They are what an " +
		"API key's scopes are drawn from, and what a role grants.")
	n := naming.New(naming.Config{})

	b.L("const (")
	for _, p := range e.doc.API.Permissions {
		if p.Description != "" {
			b.Comment(p.Description)
		}
		b.L("Permission%s = %s", n.Go(p.Key), gobuf.Quote(p.Key))
	}
	b.L(")")
	b.NL()
}

// cacheBackstop is how long a lost invalidation could go unnoticed, which is the
// only thing about rig's own caching a client has any business being told.
//
// It cannot act on it — the number describes the server's memory, not the
// client's — and it is in the profile so that somebody reading the generated
// document can say how quickly a revocation takes effect in the worst case. In
// the ordinary case the answer is "at once": the invalidation is published on
// the transaction that revoked something.
func cacheBackstop(doc *ir.Document) ir.Duration {
	c := doc.API.Cache
	if c == nil || !c.Enabled {
		return 0
	}
	return ir.Duration(float64(time.Second) * c.TTLSeconds)
}
