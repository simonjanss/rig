package account

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// TenantOptions are what an application decides about making tenants.
//
// rig owns the mechanism — the tenant row, the first account, the slug, the
// transaction all of it happens in — because that part is the same everywhere and
// getting it half-right leaves a tenant nobody can reach. What it does not own is
// policy: who may make one, what a name is allowed to be, and what else a new
// tenant needs. Those are hooks, and every one of them is optional.
type TenantOptions struct {
	// Allow decides whether this person may create a tenant at all.
	//
	// Nil means anybody signed in may. It runs before anything is written and
	// before Validate, so a refusal costs one function call.
	//
	//	Allow: func(_ context.Context, by account.Creator) error {
	//		if !strings.HasSuffix(by.EmailAddress, "@rig.app") {
	//			return rigerr.Forbidden("only rig.app may create tenants")
	//		}
	//		return nil
	//	}
	Allow func(ctx context.Context, by Creator) error

	// Validate checks and may rewrite what is about to be created.
	//
	// It is handed a pointer, so a hook that wants to normalize rather than refuse
	// can: title-casing a name, trimming a suffix, filling in a slug. What it
	// leaves behind is what gets written.
	//
	//	Validate: func(_ context.Context, t *account.TenantDraft) error {
	//		if t.Name != strings.Title(t.Name) {
	//			return rigerr.Invalid("a tenant's name must be capitalised")
	//		}
	//		return nil
	//	}
	Validate func(ctx context.Context, t *TenantDraft) error

	// Slug derives the URL-safe name. Nil uses [DefaultSlug].
	Slug func(name string, id uuid.UUID) string

	// OnCreated runs inside the transaction that made the tenant, after the row
	// and the first account exist and before the commit.
	//
	// This is where an application's own tables get their rows: the roles a tenant
	// starts with, a default project, a settings record. Returning an error rolls
	// the whole thing back, which is the point of it being here rather than after —
	// a tenant whose roles failed to seed is a tenant whose Owner can do nothing.
	//
	// Reach the transaction with [github.com/simonjanss/rig/runtime/dbx.Tx],
	// which is the same way a generated repository reaches it:
	//
	//	OnCreated: func(ctx context.Context, made account.NewTenant) error {
	//		tx, ok := dbx.Tx(ctx)
	//		if !ok {
	//			return errors.New("expected a transaction")
	//		}
	//		return seedRoles(ctx, tx, made.TenantID, keys)
	//	}
	OnCreated func(ctx context.Context, made NewTenant) error

	// OwnerRole is the level the first account gets. Empty means [RoleOwner].
	OwnerRole Role
}

// Creator is who is asking to make a tenant.
type Creator struct {
	IdentityID uuid.UUID
	// EmailAddress is theirs, lowercased — which is what a domain rule wants.
	EmailAddress string
	DisplayName  string
}

// TenantDraft is the tenant about to exist, for a hook to check or adjust.
type TenantDraft struct {
	// ID is already decided, so a hook can use it — in a slug, or in a row of its
	// own keyed on it.
	ID uuid.UUID
	// Name is as typed, trimmed. A hook may rewrite it.
	Name string
	// Slug is empty unless a hook fills it in; otherwise it is derived after
	// Validate returns.
	Slug string
	// AllowedEmailDomains restricts who may be provisioned into this tenant
	// later. Empty means no restriction.
	AllowedEmailDomains []string

	// By is who is creating it, for a rule that depends on them.
	By Creator
}

// NewTenant is what was created.
type NewTenant struct {
	TenantID   uuid.UUID
	TenantName string
	TenantSlug string
	// AccountID is the first account, which holds OwnerRole.
	AccountID  uuid.UUID
	IdentityID uuid.UUID
}

// CreateTenantInput makes a tenant for somebody who already exists.
type CreateTenantInput struct {
	// IdentityID is the person who will own it, established by their identity
	// session.
	IdentityID uuid.UUID
	Name       string
	// AllowedEmailDomains restricts later provisioning into it.
	AllowedEmailDomains []string

	IPAddress string
	UserAgent string
}

// CreateTenant makes a tenant and puts one account in it as its Owner.
//
// One transaction for the tenant, the account and whatever OnCreated adds, so a
// failure leaves nothing behind: a tenant with no owner is unreachable, and an
// owner with no role can sign in and do nothing.
//
// It does not create the person. That is [Service.Register] or an invitation, and
// keeping them apart is what makes "sign in, then choose where to be" a flow
// rather than one form that does everything.
func (s *Service) CreateTenant(ctx context.Context, in CreateTenantInput) (NewTenant, error) {
	if in.IdentityID == uuid.Nil {
		return NewTenant{}, rigerr.BadRequest("an identity is required")
	}

	ident, err := s.cfg.Store.FindIdentityByID(ctx, in.IdentityID)
	if err != nil {
		return NewTenant{}, err
	}
	if ident == nil || !ident.IsActive {
		return NewTenant{}, rigerr.Forbidden("this account has been disabled")
	}

	by := Creator{
		IdentityID:   ident.ID,
		EmailAddress: normalizeEmail(ident.EmailAddress),
		DisplayName:  ident.DisplayName,
	}

	// Before anything is written, and before the name is even looked at: whether
	// this person may do this at all is the cheapest question to answer.
	if allow := s.cfg.Tenants.Allow; allow != nil {
		if err := allow(ctx, by); err != nil {
			s.write(ctx, authlog.Entry{
				Event: authlog.EventLoginFailed, Outcome: authlog.Failed,
				EmailAddress: by.EmailAddress, IPAddress: in.IPAddress,
				UserAgent: in.UserAgent,
				Detail:    map[string]any{"reason": "may not create a tenant"},
			})
			return NewTenant{}, err
		}
	}

	draft := TenantDraft{
		ID:                  uuid.New(),
		Name:                strings.TrimSpace(in.Name),
		AllowedEmailDomains: in.AllowedEmailDomains,
		By:                  by,
	}
	if draft.Name == "" {
		return NewTenant{}, rigerr.Invalid("a tenant needs a name")
	}
	if validate := s.cfg.Tenants.Validate; validate != nil {
		if err := validate(ctx, &draft); err != nil {
			return NewTenant{}, err
		}
		// A hook that emptied the name would otherwise write one, and a NOT NULL
		// column is a worse place to find out.
		if strings.TrimSpace(draft.Name) == "" {
			return NewTenant{}, rigerr.Invalid("a tenant needs a name")
		}
	}

	if draft.Slug == "" {
		slug := s.cfg.Tenants.Slug
		if slug == nil {
			slug = DefaultSlug
		}
		draft.Slug = slug(draft.Name, draft.ID)
	}

	role := s.cfg.Tenants.OwnerRole
	if role == "" {
		role = RoleOwner
	}

	out := NewTenant{
		TenantID: draft.ID, TenantName: draft.Name, TenantSlug: draft.Slug,
		AccountID: uuid.New(), IdentityID: ident.ID,
	}

	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		if err := s.cfg.Store.InsertTenant(ctx, &Tenant{
			ID:                  draft.ID,
			Name:                draft.Name,
			Slug:                draft.Slug,
			AllowedEmailDomains: draft.AllowedEmailDomains,
			IsActive:            true,
		}); err != nil {
			return err
		}

		// The account's copy of the address and name comes from the identity
		// rather than from the request: the request said which tenant to make, not
		// who to be.
		if err := s.cfg.Store.Insert(ctx, &Account{
			ID:           out.AccountID,
			TenantID:     draft.ID,
			IdentityID:   &ident.ID,
			EmailAddress: ident.EmailAddress,
			DisplayName:  ident.DisplayName,
			Kind:         KindPerson,
			Role:         role,
			IsActive:     true,
			// Nobody. The audit column is a foreign key to account, and this is
			// the first account there is — the person asking exists as an identity,
			// which is a different table. Their own row cannot name itself.
			CreatedBy: nil,
		}); err != nil {
			return err
		}

		if hook := s.cfg.Tenants.OnCreated; hook != nil {
			return hook(ctx, out)
		}
		return nil
	}); err != nil {
		return NewTenant{}, err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventAccountProvisioned, Outcome: authlog.Succeeded,
		TenantID: &out.TenantID, AccountID: &out.AccountID,
		EmailAddress: by.EmailAddress,
		IPAddress:    in.IPAddress, UserAgent: in.UserAgent,
		Detail: map[string]any{"created_tenant": true, "role": string(role)},
	})
	return out, nil
}

// slugUnsafe is every run of characters a slug cannot contain.
var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// DefaultSlug is the derivation an application gets without asking.
//
// Lowercased, non-alphanumerics collapsed to a hyphen, and the tenant's own
// identifier appended. The suffix is not decoration: the column is unique, two
// customers called Acme are ordinary, and a slug that collided would fail the
// insert of the second one.
func DefaultSlug(name string, id uuid.UUID) string {
	base := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		base = "tenant"
	}
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	return fmt.Sprintf("%s-%s", base, id.String()[:8])
}
