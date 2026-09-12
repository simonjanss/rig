package account

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/identity"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// PermissionProvision is what a caller needs to create an account.
//
// The name is this package's because the endpoint is, but it is an ordinary
// permission key: grant it to a role, or put it in an API key's scopes, and the
// same check answers either.
const PermissionProvision = "account.provision"

// ProvisionInput describes somebody to create an account for.
type ProvisionInput struct {
	TenantID     uuid.UUID
	EmailAddress string
	DisplayName  string

	// Kind defaults to a person. Pass KindService for the account an
	// integration's key acts as — it gets no identity at all, so there is nothing
	// to sign in with and nothing to phish.
	Kind Kind
	// Role defaults to Basic, and applies to this tenant only. Whether the
	// caller may hand out a higher one is the application's rule: this package
	// will set what it is told.
	Role Role
	// TimeZone is an IANA name, optional.
	TimeZone string

	// ByAccountID and ByAPIKeyID are who asked, for the audit columns. The HTTP
	// layer fills them from the claims; a caller reaching this directly should
	// fill them too, or the row will say a change came from nobody.
	ByAccountID *uuid.UUID
	ByAPIKeyID  *uuid.UUID
}

// Provision brings somebody into a tenant, now.
//
// It finds the person by their address or creates them, then gives them an
// account in the tenant. An address that already belongs to somebody is reused
// rather than refused: a person who works at two of your customers is one
// person, and making them a second identity would mean a second of everything.
//
// **Provision adds a member; [Service.Invite] asks somebody to join.** That is
// the whole difference and it is worth being sure which one you want. This one
// writes the account immediately, which is what a bulk import wants and what a
// starter tenant in [Config.OnRegistered] wants — the person is in the tenant's
// people list from this call, whether or not they ever sign in. Invite writes no
// account at all, and accepting is what creates one.
//
// Nothing is mailed. Somebody provisioned this way needs a way to be told, and
// what that says is the application's — which is the argument for Invite when
// the mail is the point.
func (s *Service) Provision(ctx context.Context, in ProvisionInput) (*Account, error) {
	if in.TenantID == uuid.Nil {
		return nil, rigerr.BadRequest("a tenant is required")
	}

	email := normalizeEmail(in.EmailAddress)
	if email == "" || !strings.Contains(email, "@") {
		return nil, rigerr.Invalid("%q is not an email address", in.EmailAddress)
	}

	// The domain list governs joining rather than existing. Somebody may well
	// have an identity already — with a personal address, from another tenant —
	// and what this tenant controls is who works here.
	domains, err := s.cfg.Store.TenantDomains(ctx, in.TenantID)
	if err != nil {
		return nil, err
	}
	if !DomainAllowed(email, domains) {
		return nil, rigerr.Invalid("%s is not in a domain this tenant allows: %s",
			email, strings.Join(domains, ", "))
	}

	name := strings.TrimSpace(in.DisplayName)
	if name == "" {
		// Better than an empty row in every list: the local part is what people
		// call each other in a hurry anyway.
		name, _, _ = strings.Cut(email, "@")
	}

	kind := orKind(in.Kind)
	var ident *Identity
	if kind != KindService {
		// A service account gets no identity at all: nobody signs in as one, so
		// there is no person for it to be, and giving it one would put a row in
		// the global address space that no human owns.
		tenantID := in.TenantID
		ident, err = s.identityFor(ctx, newcomer{
			email: email, asTyped: in.EmailAddress, name: name,
			via: identity.SourceProvision, tenantID: &tenantID,
			byAccountID: in.ByAccountID, byAPIKeyID: in.ByAPIKeyID,
		})
		if err != nil {
			return nil, err
		}
	}

	if ident != nil {
		existing, err := s.cfg.Store.AccountForIdentity(ctx, in.TenantID, ident.ID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			// A conflict rather than a validation error: the request was
			// well-formed and the world disagrees with it.
			return nil, rigerr.Conflict("that person already has an account here")
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("account: generate id: %w", err)
	}

	acct := &Account{
		ID:           id,
		TenantID:     in.TenantID,
		Kind:         kind,
		Role:         orRole(in.Role),
		EmailAddress: strings.TrimSpace(in.EmailAddress),
		DisplayName:  name,
		TimeZone:     strings.TrimSpace(in.TimeZone),
		IsActive:     true,
		CreatedBy:    in.ByAccountID,
		CreatedByKey: in.ByAPIKeyID,
	}
	if ident != nil {
		acct.IdentityID = &ident.ID
	}

	// InTx is re-entrant, so this joins a caller's transaction — the
	// registration hook's, most often — rather than nesting inside one. That is
	// the whole reason a single write is wrapped: a hook that provisions and
	// then fails should leave no account behind.
	//
	// The log entry is inside it and unmoved by it: authpg writes the auth log
	// outside whatever transaction noticed the event, deliberately, because an
	// entry describing a failure has to survive that transaction's rollback.
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		if err := s.cfg.Store.Insert(ctx, acct); err != nil {
			return err
		}

		tenantID := in.TenantID
		s.write(ctx, authlog.Entry{
			TenantID:     &tenantID,
			Event:        authlog.EventAccountProvisioned,
			Outcome:      authlog.Succeeded,
			AccountID:    &acct.ID,
			EmailAddress: email,
			APIKeyID:     in.ByAPIKeyID,
			Detail:       map[string]any{"by_account_id": in.ByAccountID, "kind": string(acct.Kind)},
		})
		return nil
	}); err != nil {
		return nil, err
	}
	return acct, nil
}

// newcomer is who identityFor is being asked about.
//
// A struct rather than six positional parameters, because the sixth was already
// one too many and this adds a seventh: two *uuid.UUID in a row, either of which
// may be nil, is a call nobody can read at the site.
type newcomer struct {
	// email is normalised and asTyped is not. Both are kept: the lookup is on
	// the first and the row records the second, so somebody who signs up as
	// Ada@Example.com is greeted the way they wrote it.
	email   string
	asTyped string
	name    string
	via     identity.Source
	// tenantID is the tenant they are being brought into, for the gate. Nil is
	// not a tenant rather than the nil tenant.
	tenantID    *uuid.UUID
	byAccountID *uuid.UUID
	byAPIKeyID  *uuid.UUID
}

// identityFor finds the person an address belongs to, creating them if this
// installation has never seen it.
//
// The gate is consulted on the second branch only, which is the contract
// identity.Gate states: somebody who already exists is not a stranger, and
// asking about them would make an application write a rule about people it has
// already admitted.
func (s *Service) identityFor(ctx context.Context, in newcomer) (*Identity, error) {
	ident, err := s.cfg.Store.FindIdentityByEmail(ctx, in.email)
	if err != nil {
		return nil, err
	}
	if ident != nil {
		return ident, nil
	}

	if err := identity.Allow(ctx, s.cfg.AllowIdentity, identity.Candidate{
		EmailAddress: in.asTyped,
		// Not verified, and not verifiable here: an administrator typed this
		// address in, which is evidence about the administrator rather than
		// about the address. The invitation mail is what proves it, later.
		DisplayName: in.name,
		Via:         in.via,
		TenantID:    in.tenantID,
	}); err != nil {
		return nil, err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("account: generate identity id: %w", err)
	}

	ident = &Identity{
		ID:           id,
		EmailAddress: strings.TrimSpace(in.asTyped),
		DisplayName:  in.name,
		IsActive:     true,
		CreatedBy:    in.byAccountID,
		CreatedByKey: in.byAPIKeyID,
	}
	if err := s.cfg.Store.InsertIdentity(ctx, ident); err != nil {
		return nil, err
	}
	return ident, nil
}

// InviteInput asks somebody to join a tenant.
type InviteInput struct {
	TenantID     uuid.UUID
	EmailAddress string

	// DisplayName is what to call them in this tenant. Empty falls back at
	// accept time to the name the person already has, rather than to a guess
	// made now: somebody who already signs in here has told you their name, and
	// an invitation should not overwrite it with the local part of an address.
	DisplayName string
	// Role defaults to Basic, and applies to this tenant only. Whether the
	// caller may hand out a higher one is the application's rule.
	Role Role

	// ByAccountID and ByAPIKeyID are who asked. They are not only audit columns
	// here: one of them becomes the created_by of the account accepting creates,
	// and the account's display name is what a landing page shows somebody who
	// has not signed in yet.
	ByAccountID *uuid.UUID
	ByAPIKeyID  *uuid.UUID
}

// Invite asks somebody to join a tenant, and creates nothing in it.
//
// The difference from [Service.Provision] is the whole reason both exist.
// Provision adds a member; this adds nobody. What it writes is an identity, if
// this installation has never seen the address, and a row that says which
// tenant, what role, what to call them and who asked. **Accepting it is what
// makes somebody a member** — so an invitation that is never answered leaves the
// tenant's people list exactly as it was, and somebody who was invited and has
// not replied is not counted, not listed, and has nothing scoped to them.
//
// Inviting somebody who already has an invitation here supersedes it. The newest
// link is the one that should work, which is the same decision the mail queue
// already makes when it rotates a token before each attempt — and it means
// re-sending an invitation is something an administrator can just do.
//
// It returns the invitation so that whoever sent it can render the row they have
// just created, which is what saves a listing call.
func (s *Service) Invite(ctx context.Context, in InviteInput) (*Invitation, error) {
	if in.TenantID == uuid.Nil {
		return nil, rigerr.BadRequest("a tenant is required")
	}

	email := normalizeEmail(in.EmailAddress)
	if email == "" || !strings.Contains(email, "@") {
		return nil, rigerr.Invalid("%q is not an email address", in.EmailAddress)
	}

	// The domain list governs joining, so it governs being asked to: minting a
	// link the tenant's own rules would refuse at accept time is minting a link
	// that fails after somebody has followed it.
	domains, err := s.cfg.Store.TenantDomains(ctx, in.TenantID)
	if err != nil {
		return nil, err
	}
	if !DomainAllowed(email, domains) {
		return nil, rigerr.Invalid("%s is not in a domain this tenant allows: %s",
			email, strings.Join(domains, ", "))
	}

	tenantID := in.TenantID
	ident, err := s.identityFor(ctx, newcomer{
		email: email, asTyped: in.EmailAddress, name: displayNameFor(in.EmailAddress),
		via: identity.SourceInvitation, tenantID: &tenantID,
		byAccountID: in.ByAccountID, byAPIKeyID: in.ByAPIKeyID,
	})
	if err != nil {
		return nil, err
	}

	existing, err := s.cfg.Store.AccountForIdentity(ctx, in.TenantID, ident.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		// A conflict rather than a validation error: the request was
		// well-formed and the world disagrees with it. Inviting a colleague who
		// already works here is a mistake worth naming.
		return nil, rigerr.Conflict("that person already has an account here")
	}

	var (
		out  *Verification
		mail pendingMail
	)
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		// Whatever is in the slot comes out first. The unique index this
		// protects cannot include the expiry — now() is not immutable, so it
		// cannot be indexed on — which means an expired invitation still holds
		// the slot and something has to clear it. That is why the lookup is
		// InvitationSlot and not one of the listings: those hide an expired
		// row, and clearing the slot with a query that cannot see what is in it
		// leaves the index to refuse every later invitation to that address.
		live, err := s.cfg.Store.InvitationSlot(ctx, in.TenantID, ident.ID)
		if err != nil {
			return err
		}
		if live != nil {
			if _, err := s.cfg.Store.RevokeVerification(ctx, live.ID, s.now()); err != nil {
				return err
			}
		}

		v, send, err := s.deliver(ctx, ident, &pendingInvite{
			TenantID:    in.TenantID,
			Role:        orRole(in.Role),
			DisplayName: strings.TrimSpace(in.DisplayName),
			ByAccountID: in.ByAccountID,
			ByAPIKeyID:  in.ByAPIKeyID,
		}, KindInvitation, s.cfg.InvitationTTL)
		if err != nil {
			return err
		}
		out, mail = v, send

		// The entry stays here rather than moving to the dispatcher, and that is
		// deliberate: rig_auth_log is both the audit trail and what the rate
		// limits count, so a row written from a cron job an hour later is a row
		// the limiter reads at the wrong time.
		//
		// AccountID is the inviter's. It used to be the invitee's, because there
		// was an account to point at; there is not, and the FK to rig_account
		// means it has to be a real one or nothing. The inviter is also the
		// better answer: it puts the row in the trail of the person who acted.
		tenantID := in.TenantID
		s.write(ctx, authlog.Entry{
			Event: authlog.EventInvitationSent, Outcome: authlog.Succeeded,
			TenantID: &tenantID, AccountID: in.ByAccountID,
			EmailAddress: email,
			APIKeyID:     in.ByAPIKeyID,
			Detail:       map[string]any{"invitation_id": v.ID.String()},
		})
		return nil
	}); err != nil {
		return nil, err
	}

	// The mail after the commit, never inside it: see [Service.deliver]. A
	// failure here leaves the invitation standing, which is what the queued path
	// does too — and inviting the same address again supersedes it, so the way
	// out of a broken mail provider is to ask a second time.
	if err := post(ctx, mail); err != nil {
		return nil, err
	}

	inv, err := s.cfg.Store.InvitationByID(ctx, out.ID)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, rigerr.Internal(nil, "invitation %s vanished as it was created", out.ID)
	}
	return inv, nil
}

// DomainAllowed reports whether an address may be used in a tenant.
//
// This is the tenant's own list — rig_tenant.allowed_email_domains — and it
// answers whether somebody may hold an account *here*. Whether they may become
// an identity at all is a different question at a different time, asked once and
// not per tenant: see [github.com/simonjanss/rig/auth/identity.Gate].
//
// The rule is one rule, so it is written once. This is
// [github.com/simonjanss/rig/auth/identity.DomainAllowed], kept under this name
// because [Store] implementations in other packages call it.
func DomainAllowed(lowercasedEmail string, domains []string) bool {
	return identity.DomainAllowed(lowercasedEmail, domains)
}

func orKind(k Kind) Kind {
	if k == "" {
		return KindPerson
	}
	return k
}

func orRole(r Role) Role {
	if r == "" {
		return RoleBasic
	}
	return r
}
