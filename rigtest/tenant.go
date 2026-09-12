package rigtest

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
)

// Tenant is a workspace made for one test.
type Tenant struct {
	ID   uuid.UUID
	Name string
	Slug string
}

// Tenant makes one, and it is the isolation model to build a suite on.
//
// rig scopes every generated query by tenant, so a test that leaks into another
// one's rows has found a bug rather than tripped over its own fixture. That only
// holds if each test has a tenant nobody else is in — which is why this exists
// rather than a shared fixture seeded once.
//
// Two things about the row are easy to get wrong and neither fails in a way that
// names itself. rig_tenant.slug is NOT NULL with a unique index on lower(slug),
// so a fixture that leaves it out is refused. And a slug built from the first
// characters of a UUIDv7 collides: those are the high bits of a millisecond
// timestamp, so every v7 minted inside about a minute shares them, and two
// parallel tests are a unique-constraint violation rather than a flake. The
// suffix here is from crypto/rand for that reason.
//
// Options are for the fields a test is actually about; the zero value is a
// tenant with no domain restrictions, which is the right default for a fixture
// that is not about domains.
func (r *Rig) Tenant(tb testing.TB, opts ...TenantOption) Tenant {
	tb.Helper()

	t := Tenant{ID: uuid.New(), Name: "rigtest", Slug: "rigtest-" + nonce()}
	settings := tenantSettings{}
	for _, opt := range opts {
		opt(&t, &settings)
	}

	// An empty slice rather than the nil one an unset option leaves: the column
	// is NOT NULL DEFAULT '{}', and nil goes in as NULL rather than as empty.
	if settings.domains == nil {
		settings.domains = []string{}
	}
	r.exec(tb, `
		INSERT INTO rig_tenant (id, name, slug, allowed_email_domains)
		VALUES ($1, $2, $3, $4)`,
		t.ID, t.Name, t.Slug, settings.domains)
	return t
}

// TenantOption changes something about the tenant a test is given.
type TenantOption func(*Tenant, *tenantSettings)

// tenantSettings are the columns that are not on [Tenant] because a caller has
// no reason to read them back off the fixture it just set them on.
type tenantSettings struct {
	domains []string
}

// Named gives the tenant a name and a slug derived from it.
//
// The slug keeps a random suffix even so: a name is what a test is asserting
// about, and two tests naming their tenant the same thing is ordinary rather
// than a mistake to refuse.
func Named(name string) TenantOption {
	return func(t *Tenant, _ *tenantSettings) {
		t.Name, t.Slug = name, slugify(name)+"-"+nonce()
	}
}

// AllowedEmailDomains restricts who may hold an account in this tenant, which is
// the column rig checks when somebody is provisioned, invited, or joins a tenant
// a provider sign-in named.
//
// It is a different question from whether somebody may become an identity at all
// — that one is the auth.AllowIdentity gate, and it is not per tenant.
func AllowedEmailDomains(domains ...string) TenantOption {
	return func(_ *Tenant, s *tenantSettings) { s.domains = domains }
}

// nonce is eight random hex characters, for the reason given on [Rig.Tenant].
func nonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("rigtest: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// slugify is enough for a fixture: lowercase, and anything that is not a letter
// or a digit becomes a hyphen.
func slugify(name string) string {
	out := make([]rune, 0, len(name))
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
