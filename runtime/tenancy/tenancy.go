// Package tenancy carries who is making a request.
//
// Every generated query is scoped by tenant and every generated write is
// stamped with an actor, so the claims are not optional context — they are an
// argument the repository cannot work without. Requiring them explicitly is
// what makes a missing scope a failure rather than a leak.
package tenancy

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// Subject is what kind of caller a set of claims describes.
type Subject string

const (
	// SubjectAccount is a person, acting through a session.
	SubjectAccount Subject = "Account"
	// SubjectAPIKey is a machine, acting through a key.
	SubjectAPIKey Subject = "ApiKey"
	// SubjectSystem is rig itself: migrations, background work, and anything
	// else with no caller behind it.
	SubjectSystem Subject = "System"
)

// Claims describe the caller.
type Claims struct {
	TenantID  uuid.UUID
	AccountID uuid.UUID
	Subject   Subject

	// Roles and Permissions are what the caller may do, filled in by the
	// application before a handler runs — there is no model behind them here.
	// Permissions is what [Require] tests; Roles is for a coarse question an
	// application wants to ask itself.
	Roles       []string
	Permissions []string

	// ImpersonatedByAccountID is set when an administrator is acting as
	// someone else. It propagates through every audit entry, so the record
	// says who really did it.
	ImpersonatedByAccountID *uuid.UUID

	// Extra is the application's own context for this session, carried on the
	// credential and refreshed with it. Nil for a request that has none.
	//
	// Opaque here: the runtime stores and passes it and never looks inside.
	// [Extra] is the typed way to read it.
	//
	// It is only as fresh as the last refresh — up to the refresh lifetime, twelve
	// hours by default and thirty days for "remember me" — so nothing that decides
	// what a caller may do belongs in it. Permissions are resolved per request for
	// exactly that reason, and putting one here would mean revoking it took half a
	// day to bite.
	Extra json.RawMessage

	// APIKeyID is the key the request arrived with, when it arrived with one.
	//
	// It is separate from AccountID rather than folded into it because they
	// answer different questions: the account is whose change this is — a
	// service account for an integration, a person for their own key — and this
	// is which credential it came through. One service account can have several
	// keys, and when something has gone wrong the useful question is which key
	// to revoke.
	APIKeyID *uuid.UUID
}

// Actor is the account to stamp on a write, or nil when there is no person
// behind the request.
func (c Claims) Actor() *uuid.UUID {
	if c.AccountID == uuid.Nil {
		return nil
	}
	id := c.AccountID
	return &id
}

// ActorKey is the key to stamp an audit column with, or nil for a request that
// did not come through one.
func (c Claims) ActorKey() *uuid.UUID {
	if c.APIKeyID == nil || *c.APIKeyID == uuid.Nil {
		return nil
	}
	id := *c.APIKeyID
	return &id
}

// Can reports whether the caller holds a permission.
func (c Claims) Can(permission string) bool { return slices.Contains(c.Permissions, permission) }

// HasRole reports whether the caller holds a role.
func (c Claims) HasRole(role string) bool { return slices.Contains(c.Roles, role) }

// Permission is one thing a caller may be allowed to do.
//
// It is here, in the runtime, because both ends need it and neither should have
// to import the other: generated code produces the list, and the authentication
// foundation reconciles the database against it.
type Permission struct {
	// Key is what a check looks for and what a role grants.
	Key string
	// Name and Description are for the screen where somebody hands it out. They
	// come from the resource, so nobody writes them twice.
	Name        string
	Description string
}

// Require refuses a caller who does not hold a permission.
//
// It lives here rather than in the auth module because generated handlers call
// it: a project with no authentication at all still gets the generated check, and
// making that check depend on the auth module would make every project depend on
// argon2 and OAuth to serve a table of notes.
//
// This is the whole of rig's authorization model — a set of strings on the claims
// and a membership test. Where the strings come from is the application's, and
// there is no interface to implement: something fills in Claims.Permissions
// before a handler runs. rig derives the *keys* from the schema so that the check
// cannot be forgotten, and stops there, because who holds which is a product
// decision no framework gets right for everybody.
//
// The message names the permission. A caller who legitimately lacks it needs to
// know what to ask their administrator for, and one who does not learns nothing
// they could not read in the API documentation.
func Require(c Claims, permission string) error {
	switch {
	case !c.Valid():
		return rigerr.Unauthorized("this request is not authenticated")
	case permission == "":
		// Nothing asked for. An endpoint with no permission needs only a valid
		// caller, which is what a project that turned derivation off has and what
		// `public:` produces — and refusing over an empty string would refuse
		// every one of them.
		return nil
	case c.Can(permission):
		return nil
	default:
		return rigerr.Forbidden("this action requires the %q permission", permission)
	}
}

// Extra decodes a caller's session context into the application's own type.
//
// A function rather than a type parameter on [Claims]. A generic Claims[T] reads
// better in isolation and is the wrong trade here: generated code names
// tenancy.Claims in every handler, every service interface and every repository
// method, and the request envelope already carries three type parameters. One
// generic function gives the same type safety and costs nothing to ignore.
//
// A caller with no payload gets the zero value and no error, which is the
// ordinary case rather than a failure: a session issued before the application
// started setting one, or a request from an API key, both land here.
func Extra[T any](c Claims) (T, error) {
	var out T
	if len(c.Extra) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(c.Extra, &out); err != nil {
		return out, rigerr.Internal(err, "decode the session payload")
	}
	return out, nil
}

// Valid reports whether the claims can scope a query. A tenant is the minimum:
// without one there is nothing to filter by, and running the query anyway would
// return every tenant's rows.
func (c Claims) Valid() bool { return c.TenantID != uuid.Nil }

type contextKey struct{}

// NewContext returns a context carrying the claims.
func NewContext(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// FromContext returns the claims on a context.
//
// The error is deliberate rather than a zero value: a repository that silently
// proceeded with an empty tenant would return every tenant's rows, and a bug
// that leaks data across tenants should be impossible to write by forgetting
// something.
func FromContext(ctx context.Context) (Claims, error) {
	c, ok := ctx.Value(contextKey{}).(Claims)
	if !ok {
		return Claims{}, rigerr.Unauthorized("no claims on this request")
	}
	if !c.Valid() {
		return Claims{}, rigerr.Unauthorized("claims carry no tenant")
	}
	return c, nil
}

// System builds claims for rig's own work — migrations, background jobs — where
// there is no caller but a tenant still has to be named.
func System(tenantID uuid.UUID) Claims {
	return Claims{TenantID: tenantID, Subject: SubjectSystem}
}
