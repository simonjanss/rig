package account

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// RegisterInput creates a person, and nothing else.
type RegisterInput struct {
	EmailAddress string
	DisplayName  string
	Password     string

	IPAddress string
	UserAgent string
}

// Registered is somebody who just signed themselves up, as
// [Config.OnRegistered] sees them.
type Registered struct {
	IdentityID uuid.UUID
	// EmailAddress as typed, trimmed. Lowercase it yourself for comparisons —
	// the address is one person's name for themselves, and the cased form is
	// what they wrote.
	EmailAddress string
	// DisplayName as resolved: what was sent, or the address's local part when
	// nothing was.
	DisplayName string

	IPAddress string
	UserAgent string
}

// Register creates an identity with a password and no tenant at all.
//
// The counterpart to [Service.Provision], and the difference is who is asking.
// Provision is an administrator or an integration adding somebody to a tenant
// they already run: it needs a permission, checks the tenant's allowed domains,
// and sets no password. This is a stranger signing themselves up, so there is no
// caller to check and nowhere to put them yet.
//
// What comes back is an identity session — the tenant-less credential — because
// the next thing that happens is a tenant picker: they look at the invitations
// waiting for them and either accept one or make a tenant of their own. No
// tenant session is issued, because there is no tenant.
//
// Whether a stranger may do this at all is the application's decision, the same
// way creating a tenant is. rig mounts the endpoint only when
// [github.com/simonjanss/rig/auth.Config.AllowRegistration] says so.
func (s *Service) Register(ctx context.Context, in RegisterInput) (SignInResult, error) {
	email := normalizeEmail(in.EmailAddress)
	if email == "" || !strings.Contains(email, "@") {
		return SignInResult{}, rigerr.Invalid("%q is not an email address", in.EmailAddress)
	}

	// Unauthenticated and it writes a row, so it is limited before anything else.
	// The address limit is the one that matters: without it, one script makes ten
	// thousand identities and the table is somebody else's problem.
	decision, err := s.cfg.Limiter.Allow(ctx,
		throttle.Check{Limit: s.cfg.Limits.PasswordReset, Key: throttle.Email(email)},
		throttle.Check{Limit: s.cfg.Limits.LoginByIP, Key: throttle.IP(in.IPAddress)},
	)
	if err != nil {
		return SignInResult{}, err
	}
	if !decision.Allowed {
		return SignInResult{}, decision.Err()
	}

	// The password is checked against the policy before the identity exists, so a
	// refused one leaves nothing behind.
	if err := s.cfg.Policy.Check(ctx, in.Password); err != nil {
		return SignInResult{}, err
	}

	existing, err := s.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return SignInResult{}, err
	}
	if existing != nil {
		// A plain conflict, which does tell a stranger that this address is
		// already registered. The alternative — answering success and sending a
		// "you already have an account" email — hides that, and needs a mail
		// path this cannot assume. A deployment that cares about address
		// enumeration should do it that way; this is the honest, simple one, and
		// the sign-in page leaks the same fact anyway.
		s.write(ctx, authlog.Entry{
			Event: authlog.EventLoginFailed, Outcome: authlog.Failed,
			EmailAddress: email, IPAddress: in.IPAddress, UserAgent: in.UserAgent,
			Detail: map[string]any{"reason": "already registered"},
		})
		return SignInResult{}, rigerr.Conflict("an account already exists for %s", in.EmailAddress)
	}

	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display, _, _ = strings.Cut(strings.TrimSpace(in.EmailAddress), "@")
	}

	ident := &Identity{
		ID:           uuid.New(),
		EmailAddress: strings.TrimSpace(in.EmailAddress),
		DisplayName:  display,
		IsActive:     true,
	}

	// One transaction around the person, their credential, and whatever the
	// application's OnRegistered adds — an invitation into a starter tenant,
	// most often. A hook error rolls the whole sign-up back, so a retry is a
	// clean retry rather than a conflict with a half-made account. The
	// credential goes through storePassword rather than SetPassword: the policy
	// was checked above, and a brand-new identity has no sessions to revoke.
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		if err := s.cfg.Store.InsertIdentity(ctx, ident); err != nil {
			return err
		}
		if err := s.storePassword(ctx, ident, in.Password); err != nil {
			return err
		}
		if s.cfg.OnRegistered == nil {
			return nil
		}
		return s.cfg.OnRegistered(ctx, s, Registered{
			IdentityID:   ident.ID,
			EmailAddress: ident.EmailAddress,
			DisplayName:  display,
			IPAddress:    in.IPAddress,
			UserAgent:    in.UserAgent,
		})
	}); err != nil {
		return SignInResult{}, err
	}

	token, err := s.cfg.Identities.Issue(ctx, session.IdentityIssueInput{
		IdentityID: ident.ID,
		IPAddress:  in.IPAddress,
		UserAgent:  in.UserAgent,
	})
	if err != nil {
		return SignInResult{}, err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventAccountProvisioned, Outcome: authlog.Succeeded,
		EmailAddress: email, IPAddress: in.IPAddress, UserAgent: in.UserAgent,
		Detail: map[string]any{"self_registered": true},
	})

	// Tenants is empty and Session is nil, which is the whole point of the
	// state: they exist, they can prove it, and they are nowhere yet.
	return SignInResult{IdentityID: ident.ID, Identity: token}, nil
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
