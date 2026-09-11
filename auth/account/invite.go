package account

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
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

	Client    session.Client
	IPAddress string
	UserAgent string
}

// AcceptInvitation redeems the link that was mailed and returns a session for
// the account it just created.
//
// The mailbox's door, for somebody who followed the link and is not signed in —
// which is most people most of the time, on a device this installation has never
// seen. [Service.PreviewInvitation] is what lets a page tell them what they are
// looking at before they get here.
//
// It also confirms the address, because the link is the proof: it went to that
// address and came back.
func (s *Service) AcceptInvitation(ctx context.Context, in AcceptInput) (session.Pair, error) {
	v, ident, err := s.redeem(ctx, in.Token, KindInvitation)
	if err != nil {
		return session.Pair{}, err
	}
	if v.InvitedToTenantID == nil || v.InvitedRole == nil {
		// A backstop rather than the rule: a CHECK on the table ties both to
		// the kind, so this cannot happen in the schema rig ships. It stays for
		// a schema where that constraint was dropped, because the alternative
		// is a nil dereference.
		return session.Pair{}, rigerr.Internal(nil, "invitation %s names no tenant or role", v.ID)
	}

	return s.joinFromInvitation(ctx, v, ident, in)
}

// PreviewInput asks what an invitation's link is for.
type PreviewInput struct {
	Token     string
	IPAddress string
	UserAgent string
}

// PreviewInvitation says what a link is for, without spending it.
//
// The one thing rig answers to somebody who has proved nothing but that they
// hold a link, and it exists because a mail's whole value is the sentence it
// lets the landing page say: Anna invited you to Skolan i Solna. Without it a
// front end holds a token it cannot interpret and can only show a bare sign-in
// box — which throws the mail away, and is the page most likely to be taken for
// phishing.
//
// It consumes nothing and extends nothing. Reading a link is not using it, and a
// preview that touched expires_at would let anybody who can see the URL keep an
// invitation alive forever.
//
// One answer for every way this can be wrong — unknown, consumed, withdrawn,
// expired, or the wrong kind of link. The same rule [Service.AcceptAsMe]
// applies, and for the same reason: from outside they are the same thing, and
// telling them apart would let a caller probe the table one token at a time.
//
// The address in what comes back is the invitation's, unmasked. Masking is the
// HTTP layer's, because a caller inside the process may well have a reason to
// see it and a caller over the wire has not proved they are its addressee —
// see [MaskEmail].
func (s *Service) PreviewInvitation(ctx context.Context, in PreviewInput) (*Invitation, error) {
	// Before the hash is computed and before the table is touched, for the
	// reason every limit in this package is checked first: a refused request
	// must not do the work.
	//
	// Nothing is written on a refusal, deliberately. The counted event is
	// InvitationPreviewed, so writing one here would let a locked-out source
	// keep its own window alive forever — the lockout would never end. A sign-in
	// gets away with writing on refusal because it writes a *different* event,
	// and there is no second event here worth inventing.
	decision, err := s.cfg.Limiter.Allow(ctx,
		throttle.Check{Limit: s.cfg.Limits.InvitationPreview, Key: throttle.IP(in.IPAddress)})
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, decision.Err()
	}

	notFound := rigerr.NotFound("this invitation is not valid or has expired")

	hash, ok := tokenHash(in.Token)
	if !ok {
		s.write(ctx, authlog.Entry{
			Event: authlog.EventInvitationPreviewed, Outcome: authlog.Failed,
			IPAddress: in.IPAddress, UserAgent: in.UserAgent,
			Detail: map[string]any{"reason": "malformed token"},
		})
		return nil, notFound
	}

	inv, err := s.cfg.Store.InvitationByToken(ctx, hash)
	if err != nil {
		return nil, err
	}
	if inv == nil || !s.now().Before(inv.ExpiresAt) {
		s.write(ctx, authlog.Entry{
			Event: authlog.EventInvitationPreviewed, Outcome: authlog.Failed,
			IPAddress: in.IPAddress, UserAgent: in.UserAgent,
			Detail: map[string]any{"reason": "no such invitation"},
		})
		return nil, notFound
	}

	tenantID := inv.TenantID
	s.write(ctx, authlog.Entry{
		Event: authlog.EventInvitationPreviewed, Outcome: authlog.Succeeded,
		TenantID: &tenantID, EmailAddress: normalizeEmail(inv.EmailAddress),
		IPAddress: in.IPAddress, UserAgent: in.UserAgent,
		Detail: map[string]any{"invitation_id": inv.ID.String()},
	})
	return inv, nil
}

// AcceptAsMe redeems an invitation for somebody already signed in.
//
// The picker's door, where the token-based one is the mailbox's. It takes the
// invitation's identifier and the identity behind an identity session, and that
// is a stronger claim than the token rather than a weaker one: a token proves
// somebody reached the address it was sent to, and a session proves who they
// are. Requiring the emailed link from a caller already signed in as the person
// invited would add nothing — which is why a listing can safely hand out
// identifiers and never tokens, and why [Service.PreviewInvitation] hands out
// the identifier too.
//
// That is also the answer to the fourth case, somebody signed in *and* holding
// the link: preview it, then come here with the identifier. The stronger of two
// claims is the one to prefer when both are present, and this one refuses an
// invitation that is not the caller's own — which the token door cannot, because
// there the token is the whole of the claim.
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
		!v.Usable(s.now()), v.InvitedToTenantID == nil, v.InvitedRole == nil:
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

// joinFromInvitation is the half both doors share, and it is where somebody
// becomes a member.
//
// The account is created here rather than when the invitation was sent, and that
// is the difference between an invitation and an announcement: until this runs,
// the tenant's people list does not contain the person, nobody counts them, and
// nothing is scoped to them. It is one transaction with consuming the link and
// confirming the address, because an account with a live link beside it is
// somebody who can join twice, and a consumed link with no account is somebody
// who can never join at all.
//
// Consuming comes first inside that transaction, and the order is the
// concurrency: two simultaneous accepts both reach the insert otherwise, and
// what a double-click deserves is one account and one refusal rather than a
// unique-index violation.
func (s *Service) joinFromInvitation(
	ctx context.Context, v *Verification, ident *Identity, in AcceptInput,
) (session.Pair, error) {
	if !ident.IsActive {
		return session.Pair{}, rigerr.Forbidden("this account has been disabled")
	}

	tenantID := *v.InvitedToTenantID

	acct, err := s.cfg.Store.AccountForIdentity(ctx, tenantID, ident.ID)
	if err != nil {
		return session.Pair{}, err
	}
	if acct != nil && !acct.IsActive {
		return session.Pair{}, rigerr.Forbidden("this account has been disabled")
	}

	// Already a member, having been added by some other route between the
	// invitation going out and the click. Not a refusal: the link said it would
	// put them in this tenant and they are in it, so it is consumed and a
	// session is issued. Refusing would be refusing on a detail they cannot see.
	joined := acct == nil
	if joined {
		// The tenant may have tightened its domain list since the invitation
		// went out, and honouring that late is cheaper than explaining why it
		// was not honoured at all. Told as an invalid link rather than as a
		// domain rule, because the holder of the link is not yet proven to be
		// its addressee and which domains a tenant allows is the tenant's
		// business.
		domains, err := s.cfg.Store.TenantDomains(ctx, tenantID)
		if err != nil {
			return session.Pair{}, err
		}
		if !DomainAllowed(normalizeEmail(ident.EmailAddress), domains) {
			return session.Pair{}, errInvalidLink
		}

		id, err := uuid.NewV7()
		if err != nil {
			return session.Pair{}, fmt.Errorf("account: generate id: %w", err)
		}
		acct = &Account{
			ID:           id,
			TenantID:     tenantID,
			IdentityID:   &ident.ID,
			Kind:         KindPerson,
			Role:         *v.InvitedRole,
			EmailAddress: ident.EmailAddress,
			DisplayName:  orName(v.InvitedDisplayName, ident.DisplayName),
			IsActive:     true,
			// Who brought them in, kept on the row itself so that the
			// provenance survives the invitation being consumed.
			CreatedBy:    v.InvitedByAccountID,
			CreatedByKey: v.InvitedByAPIKeyID,
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

		if joined {
			if err := s.cfg.Store.Insert(ctx, acct); err != nil {
				return err
			}
			// The same event Provision writes, so that "every account that
			// exists has an AccountProvisioned row" stays true now that an
			// account can come into existence here.
			s.write(ctx, authlog.Entry{
				TenantID:     &tenantID,
				Event:        authlog.EventAccountProvisioned,
				Outcome:      authlog.Succeeded,
				AccountID:    &acct.ID,
				EmailAddress: normalizeEmail(ident.EmailAddress),
				Detail: map[string]any{
					"from_invitation": v.ID.String(),
					"kind":            string(acct.Kind),
				},
			})
		}

		if !ident.Verified() {
			if err := s.cfg.Store.MarkIdentityVerified(ctx, ident.ID, now); err != nil {
				return err
			}
		}

		if joined && s.cfg.OnJoined != nil {
			return s.cfg.OnJoined(ctx, Joined{
				TenantID:     acct.TenantID,
				AccountID:    acct.ID,
				IdentityID:   ident.ID,
				Role:         acct.Role,
				EmailAddress: ident.EmailAddress,
				DisplayName:  acct.DisplayName,
				InvitedBy:    v.InvitedByAccountID,
			})
		}
		return nil
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
		Detail: map[string]any{
			"invitation_id":    v.ID.String(),
			"already_a_member": !joined,
		},
	})
	return pair, nil
}

// Joined is who just became a member, for [Config.OnJoined].
type Joined struct {
	TenantID   uuid.UUID
	AccountID  uuid.UUID
	IdentityID uuid.UUID
	Role       Role

	EmailAddress string
	DisplayName  string

	// InvitedBy is the account that sent the invitation, and nil when a key
	// sent it or when that person has since been removed.
	InvitedBy *uuid.UUID
}

// orName is the name the inviter chose, or the one the person already has.
func orName(chosen, own string) string {
	if chosen != "" {
		return chosen
	}
	return own
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

// RevokeInvitation withdraws an invitation.
//
// One write, where this used to be two. It killed the link and soft-deleted the
// account in the same transaction, because either alone left something wrong — a
// dead link beside an account nobody could ever use, or an account gone and a
// live link that would recreate nothing. There is no account now: nothing was
// created when the invitation went out, so withdrawing it removes nothing and
// the tenant's people list does not change.
//
// Which is the point. Before, "withdraw an invitation" was "delete a colleague
// who has not answered yet", and the only thing keeping that safe was that the
// invitation had to still be pending.
//
// It still refuses one that is not pending, and the reason is now about the link
// alone: a consumed link belongs to a member, and removing somebody from a
// tenant is a different decision with a different name — and no endpoint in rig,
// deliberately.
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

	// One statement, and no transaction around it: RevokeVerification is atomic
	// by its own IS NULL predicates, which is what makes the race below a race
	// it loses rather than one it has to hold a lock against.
	revoked, err := s.cfg.Store.RevokeVerification(ctx, found.ID, s.now())
	if err != nil {
		return err
	}
	if !revoked {
		// Somebody accepted it between the read and the write. Their account
		// exists now and has nothing to do with this call.
		return rigerr.Conflict("that invitation was used a moment ago")
	}

	// AccountID is the withdrawer's. It used to be the invitee's, which was
	// possible only because an account had been created for them up front.
	tenantID := found.TenantID
	s.write(ctx, authlog.Entry{
		Event: authlog.EventInvitationRevoked, Outcome: authlog.Succeeded,
		TenantID: &tenantID, AccountID: in.ByAccountID,
		EmailAddress: normalizeEmail(found.EmailAddress),
		APIKeyID:     in.ByAPIKeyID,
		Detail:       map[string]any{"invitation_id": found.ID.String()},
	})
	return nil
}
