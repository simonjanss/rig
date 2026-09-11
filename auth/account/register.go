package account

import (
	"context"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// Registered is somebody who has just come into existence, as
// [Config.OnRegistered] sees them.
//
// It arrives from one place: asking for a sign-in code with an address rig has
// never seen, in a deployment where [EmailCodeOptions.AllowProvisioning] says
// that is allowed. There is no registration endpoint — with no password there is
// nothing for one to take, and an endpoint that minted an identity token for
// anybody who typed an address would be a door rather than a form.
type Registered struct {
	IdentityID uuid.UUID
	// EmailAddress as typed, trimmed. Lowercase it yourself for comparisons —
	// the address is one person's name for themselves, and the cased form is
	// what they wrote.
	EmailAddress string
	// DisplayName as resolved: the address's local part, which is all anybody
	// knows about somebody who has typed nothing but an address.
	DisplayName string

	IPAddress string
	UserAgent string
}

// MyInvitations are the live invitations addressed to one person, in every
// tenant.
//
// Keyed on the identity rather than on a tenant and an account, because the
// caller has neither: this is what somebody sees before they belong anywhere. The
// administrator's view of the same table is [Service.Invitations], which is scoped
// to one tenant.
func (s *Service) MyInvitations(ctx context.Context, identityID uuid.UUID) ([]Invitation, error) {
	if identityID == uuid.Nil {
		return nil, rigerr.BadRequest("an identity is required")
	}
	return s.cfg.Store.InvitationsForIdentity(ctx, identityID)
}

// MyTenants are the tenants one person belongs to.
//
// The same list [Service.Tenants] answers, reached from an identity instead of
// from a tenant and an account — which is what a caller holding only an identity
// session has.
func (s *Service) MyTenants(ctx context.Context, identityID uuid.UUID) ([]Membership, error) {
	if identityID == uuid.Nil {
		return nil, rigerr.BadRequest("an identity is required")
	}
	return s.cfg.Store.TenantsForIdentity(ctx, identityID)
}
