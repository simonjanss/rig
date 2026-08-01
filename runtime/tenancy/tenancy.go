// Package tenancy carries who is making a request.
//
// Every generated query is scoped by tenant and every generated write is
// stamped with an actor, so the claims are not optional context — they are an
// argument the repository cannot work without. Requiring them explicitly is
// what makes a missing scope a failure rather than a leak.
package tenancy

import (
	"context"
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

	Roles       []string
	Permissions []string

	// ImpersonatedByAccountID is set when an administrator is acting as
	// someone else. It propagates through every audit entry, so the record
	// says who really did it.
	ImpersonatedByAccountID *uuid.UUID
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

// Can reports whether the caller holds a permission.
func (c Claims) Can(permission string) bool { return slices.Contains(c.Permissions, permission) }

// HasRole reports whether the caller holds a role.
func (c Claims) HasRole(role string) bool { return slices.Contains(c.Roles, role) }

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
