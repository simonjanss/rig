package account

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
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

	// Invite sends a verification link, so the person can set a password and
	// arrive. Without a Notifier it does nothing, which is why it is a request
	// rather than an assumption.
	Invite bool
}

// Provision brings somebody into a tenant.
//
// It finds the person by their address or creates them, then gives them an
// account in the tenant. An address that already belongs to somebody is reused
// rather than refused: a person who works at two of your customers is one person,
// and making them a second identity would mean a second password.
//
// No credential is created, which is the whole point of it being here rather
// than a POST on the table: somebody who can sign in needs a password set or a
// provider linked, and both of those are flows with their own rules. What this
// does is the part that is the same everywhere — check the address, honour the
// tenant's domain list, refuse a second account in the same tenant, and record
// who asked.
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
	ident, err := s.identityFor(ctx, in, email, name, kind)
	if err != nil {
		return nil, err
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
	if err := s.cfg.Store.Insert(ctx, acct); err != nil {
		return nil, err
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

	// A service account has no person to invite, and inviting one would send
	// mail to an address nobody reads.
	if in.Invite && ident != nil {
		if err := s.invite(ctx, ident, acct); err != nil {
			// The account exists; the mail is a best effort. Failing the request
			// would leave the caller unsure whether to retry, and a retry would
			// then be refused as a duplicate.
			return acct, nil
		}
	}
	return acct, nil
}

// invite mints the link that brings somebody in.
//
// An invitation rather than a verification, which is a real difference and not a
// label: the link says which tenant it is for, and redeeming it sets a first
// password. Sending "confirm your email address" to somebody who has never had
// an account here, and then asking them to use the forgotten-password flow to
// get in, is a sign-up nobody finishes.
func (s *Service) invite(ctx context.Context, ident *Identity, acct *Account) error {
	tenantID := acct.TenantID
	token, err := s.mintVerification(ctx, ident, &tenantID, KindInvitation, s.cfg.InvitationTTL)
	if err != nil {
		return err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventInvitationSent, Outcome: authlog.Succeeded,
		TenantID: &tenantID, AccountID: &acct.ID,
		EmailAddress: normalizeEmail(ident.EmailAddress),
	})
	return s.cfg.Notifier.SendInvitation(ctx, ident, acct, token)
}

// identityFor finds the person an account is for, creating them if this is the
// first tenant they have been brought into.
//
// A service account gets no identity at all: nobody signs in as one, so there is
// no person for it to be, and giving it one would put a row in the global
// address space that no human owns.
func (s *Service) identityFor(ctx context.Context, in ProvisionInput, email, name string, kind Kind) (*Identity, error) {
	if kind == KindService {
		return nil, nil
	}

	ident, err := s.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if ident != nil {
		return ident, nil
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("account: generate identity id: %w", err)
	}

	ident = &Identity{
		ID:           id,
		EmailAddress: strings.TrimSpace(in.EmailAddress),
		DisplayName:  name,
		IsActive:     true,
		CreatedBy:    in.ByAccountID,
		CreatedByKey: in.ByAPIKeyID,
	}
	if err := s.cfg.Store.InsertIdentity(ctx, ident); err != nil {
		return nil, err
	}
	return ident, nil
}

// DomainAllowed reports whether an address may be used in a tenant.
//
// An empty list allows anything, because most tenants are not one company and a
// list that has to be filled in before anybody can be added would be a worse
// default than no list at all. A listed domain matches its subdomains too:
// somebody who allows example.com means the company, and mail.example.com is
// the company.
func DomainAllowed(lowercasedEmail string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}

	_, host, found := strings.Cut(lowercasedEmail, "@")
	if !found || host == "" {
		return false
	}

	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(d, "@")))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
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
