package account

import (
	"context"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// AcceptInput redeems an invitation.
type AcceptInput struct {
	// Token is the single-use secret from the invitation mail. It is how somebody
	// who is not signed in redeems one.
	Token string
	// InvitationID names the invitation instead, for [Service.AcceptAsMe], where
	// the caller is signed in as the person it was addressed to. Exactly one of
	// the two is used, decided by which method is called.
	InvitationID uuid.UUID

	// Password sets a first password, and is required only for somebody who has
	// none — an invitation to their first tenant. Somebody who already signs
	// in here is joining a second one, and their existing password is the one
	// that works; a field that quietly replaced it would let an invitation to any
	// tenant change the credential for all of them.
	Password string

	Client    session.Client
	IPAddress string
	UserAgent string
}

// AcceptInvitation redeems an invitation and returns a session for the account
// it was for.
//
// One round trip, which is the whole reason it exists. Without it an invitation
// is a verification link followed by the forgotten-password flow, and asking
// somebody to reset a password they have never had is a sign-up that loses people
// at the last step.
//
// It also confirms the address, because the link is the proof: it went to that
// address and came back.
func (s *Service) AcceptInvitation(ctx context.Context, in AcceptInput) (session.Pair, error) {
	v, ident, err := s.redeem(ctx, in.Token, KindInvitation)
	if err != nil {
		return session.Pair{}, err
	}
	if v.InvitedToTenantID == nil {
		// An invitation with no tenant is not an invitation. It cannot happen
		// through Provision, and if it is in the table anyway there is nothing
		// sensible to join.
		return session.Pair{}, rigerr.Internal(nil, "invitation %s names no tenant", v.ID)
	}

	return s.joinFromInvitation(ctx, v, ident, in)
}

// AcceptAsMe redeems an invitation for somebody already signed in.
//
// The picker's door, where the token-based one is the mailbox's. It takes the
// invitation's identifier and the identity behind an identity session, and that
// is a stronger claim than the token rather than a weaker one: a token proves
// somebody reached the address it was sent to, and a session proves who they are,
// established by a password. Requiring the emailed link from a caller already
// signed in as the person invited would add nothing — which is why a listing can
// safely hand out identifiers and never tokens.
//
// With [Config.Outbox] set this accepts an invitation whose mail has not gone out
// yet, because a queued link has no token and this door does not need one. That
// is the right answer: the person is signed in as themselves and is looking at
// the invitation in their own listing, and refusing until a cron job had run
// would be refusing on a detail they cannot see.
func (s *Service) AcceptAsMe(ctx context.Context, identityID uuid.UUID, in AcceptInput) (session.Pair, error) {
	if identityID == uuid.Nil || in.InvitationID == uuid.Nil {
		return session.Pair{}, rigerr.BadRequest("an identity and an invitation are required")
	}

	v, err := s.cfg.Store.VerificationByID(ctx, in.InvitationID)
	if err != nil {
		return session.Pair{}, err
	}
	// One answer for every way this can be wrong — unknown, somebody else's,
	// consumed, withdrawn, expired, the wrong kind. From the outside they are the
	// same thing, and telling them apart would let a caller probe the table.
	switch {
	case v == nil, v.Kind != KindInvitation, v.IdentityID != identityID,
		!v.Usable(s.now()), v.InvitedToTenantID == nil:
		return session.Pair{}, rigerr.BadRequest("that invitation is not valid any more")
	}

	ident, err := s.cfg.Store.FindIdentityByID(ctx, identityID)
	if err != nil {
		return session.Pair{}, err
	}
	if ident == nil || !ident.IsActive {
		return session.Pair{}, rigerr.Forbidden("this account has been disabled")
	}

	return s.joinFromInvitation(ctx, v, ident, in)
}

// joinFromInvitation is the half both doors share: check the account is still
// there, consume the link, set a first password if there is none, and issue the
// session for the tenant just joined.
func (s *Service) joinFromInvitation(
	ctx context.Context, v *Verification, ident *Identity, in AcceptInput,
) (session.Pair, error) {
	acct, err := s.cfg.Store.AccountForIdentity(ctx, *v.InvitedToTenantID, ident.ID)
	if err != nil {
		return session.Pair{}, err
	}
	if acct == nil {
		// The account was removed between the invitation and the click. Told as
		// an invalid link rather than as a missing account, because from the
		// outside those are the same thing and the difference is nobody's
		// business.
		return session.Pair{}, rigerr.BadRequest("this link is not valid or has expired")
	}
	if !acct.IsActive {
		return session.Pair{}, rigerr.Forbidden("this account has been disabled")
	}

	cred, err := s.cfg.Store.Credential(ctx, ident.ID)
	if err != nil {
		return session.Pair{}, err
	}
	if cred == nil {
		if err := s.cfg.Policy.Check(ctx, in.Password); err != nil {
			return session.Pair{}, err
		}
	}

	now := s.now()
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		consumed, err := s.consume(ctx, v)
		if err != nil {
			return err
		}
		if !consumed {
			// Two clicks on the same link, or a link forwarded to somebody else.
			return rigerr.BadRequest("this link has already been used")
		}
		if cred == nil {
			if err := s.storePassword(ctx, ident, in.Password); err != nil {
				return err
			}
		}
		if ident.Verified() {
			return nil
		}
		return s.cfg.Store.MarkIdentityVerified(ctx, ident.ID, now)
	}); err != nil {
		return session.Pair{}, err
	}

	pair, err := s.cfg.Sessions.Issue(ctx, session.IssueInput{
		TenantID:  acct.TenantID,
		AccountID: acct.ID,
		Client:    in.Client,
		IPAddress: in.IPAddress,
		UserAgent: in.UserAgent,
	})
	if err != nil {
		return session.Pair{}, err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventInvitationAccepted, Outcome: authlog.Succeeded,
		TenantID: &acct.TenantID, AccountID: &acct.ID,
		EmailAddress: normalizeEmail(ident.EmailAddress),
		IPAddress:    in.IPAddress, UserAgent: in.UserAgent,
		TokenRootID: &pair.RootTokenID,
		Detail:      map[string]any{"first_password": cred == nil},
	})
	return pair, nil
}

// Tenants are the tenants a person belongs to.
//
// It takes an account rather than an identity because that is what a caller has —
// their claims name one account — and the answer is about the person behind it.
func (s *Service) Tenants(ctx context.Context, tenantID, accountID uuid.UUID) ([]Membership, error) {
	_, ident, err := s.person(ctx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.TenantsForIdentity(ctx, ident.ID)
}

// SwitchInput moves a session to another tenant.
type SwitchInput struct {
	// TenantID and AccountID are where the caller is now, from their claims.
	TenantID  uuid.UUID
	AccountID uuid.UUID
	// ToTenantID is where they want to be.
	ToTenantID uuid.UUID

	Client    session.Client
	IPAddress string
	UserAgent string
}

// Switch issues a session for another tenant the same person belongs to.
//
// No password: they have already proved who they are, and the new session is for
// a different account of the same identity. What it is not is a way to reach a
// tenant somebody does not belong to — that is the check below, and it is the
// only thing standing between a switcher and every customer's data.
//
// The old session is left alive. Somebody with two tenants open in two tabs
// has not asked to be signed out of either.
func (s *Service) Switch(ctx context.Context, in SwitchInput) (session.Pair, error) {
	_, ident, err := s.person(ctx, in.TenantID, in.AccountID)
	if err != nil {
		return session.Pair{}, err
	}

	target, err := s.cfg.Store.AccountForIdentity(ctx, in.ToTenantID, ident.ID)
	if err != nil {
		return session.Pair{}, err
	}
	if target == nil || !target.IsActive {
		// 403 and not 404: they are somebody, and what they are being told is
		// that this tenant is not theirs — which reveals nothing, since they
		// named it.
		return session.Pair{}, rigerr.Forbidden("you do not have access to this tenant")
	}
	if !ident.IsActive {
		return session.Pair{}, rigerr.Forbidden("this account has been disabled")
	}

	pair, err := s.cfg.Sessions.Issue(ctx, session.IssueInput{
		TenantID:  target.TenantID,
		AccountID: target.ID,
		Client:    in.Client,
		IPAddress: in.IPAddress,
		UserAgent: in.UserAgent,
	})
	if err != nil {
		return session.Pair{}, err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventTenantSwitched, Outcome: authlog.Succeeded,
		TenantID: &target.TenantID, AccountID: &target.ID,
		EmailAddress: normalizeEmail(ident.EmailAddress),
		IPAddress:    in.IPAddress, UserAgent: in.UserAgent,
		TokenRootID: &pair.RootTokenID,
		Detail:      map[string]any{"from_tenant_id": in.TenantID.String()},
	})
	return pair, nil
}

// Invitations are the live invitations into a tenant.
//
// Live means minted and still redeemable: not accepted, not withdrawn, not
// expired. It is what an interface lists so that somebody can change their mind,
// and it comes from the database rather than from whatever the notifier
// remembers — a link that was accepted an hour ago is not pending, and only the
// row knows that.
func (s *Service) Invitations(ctx context.Context, tenantID uuid.UUID) ([]Invitation, error) {
	return s.cfg.Store.PendingInvitations(ctx, tenantID)
}

// RevokeInput withdraws an invitation.
type RevokeInput struct {
	// TenantID is the caller's, and it bounds what can be withdrawn: an
	// invitation belongs to one tenant, and naming one from another tenant has to
	// answer the same way as naming one that does not exist.
	TenantID     uuid.UUID
	InvitationID uuid.UUID

	// ByAccountID and ByAPIKeyID are who did it, for the audit columns.
	ByAccountID *uuid.UUID
	ByAPIKeyID  *uuid.UUID
}

// RevokeInvitation withdraws an invitation and removes the account it was for.
//
// Both halves, because either alone leaves something wrong. Killing only the link
// leaves an account nobody can ever use, listed among the people in the tenant
// and blocking a second invitation with a conflict. Removing only the account
// leaves a live link that would recreate nothing and fail confusingly.
//
// It can only withdraw an invitation that is still pending, which is what keeps
// it from being a way to delete a colleague: once somebody has accepted, their
// account is theirs and removing them is a different decision with a different
// name.
func (s *Service) RevokeInvitation(ctx context.Context, in RevokeInput) error {
	pending, err := s.cfg.Store.PendingInvitations(ctx, in.TenantID)
	if err != nil {
		return err
	}

	var found *Invitation
	for i := range pending {
		if pending[i].ID == in.InvitationID {
			found = &pending[i]
			break
		}
	}
	if found == nil {
		// Withdrawn already, accepted already, expired, or another tenant's. All
		// of them are "there is no such pending invitation", and telling them
		// apart would say whether an address belongs to somebody.
		return rigerr.NotFound("no pending invitation with that identifier")
	}

	now := s.now()
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		revoked, err := s.cfg.Store.RevokeVerification(ctx, found.ID, now)
		if err != nil {
			return err
		}
		if !revoked {
			// Somebody accepted it between the read and the write, which is the
			// race this exists to lose safely: their account stays.
			return rigerr.Conflict("that invitation was used a moment ago")
		}
		return s.cfg.Store.SoftDeleteAccount(ctx, DeleteAccountInput{
			TenantID:    found.TenantID,
			AccountID:   found.AccountID,
			At:          now,
			ByAccountID: in.ByAccountID,
			ByAPIKeyID:  in.ByAPIKeyID,
		})
	}); err != nil {
		return err
	}

	tenantID := found.TenantID
	s.write(ctx, authlog.Entry{
		Event: authlog.EventInvitationRevoked, Outcome: authlog.Succeeded,
		TenantID: &tenantID, AccountID: &found.AccountID,
		EmailAddress: normalizeEmail(found.EmailAddress),
		APIKeyID:     in.ByAPIKeyID,
		Detail:       map[string]any{"by_account_id": in.ByAccountID},
	})
	return nil
}
