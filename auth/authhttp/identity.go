package authhttp

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// The endpoints a person can reach before they belong to a tenant.
//
// All four take an identity token and none of them can touch application data,
// because there is no tenant to touch it in. That is not a restriction bolted on:
// the credential resolves to an identity and never to [tenancy.Claims], so there
// is nothing for a generated handler to scope a query by even if one were reached.

// identity resolves the identity session a request carries.
//
// A separate reader from [Handler.Claims] rather than a mode of it. The two
// answer different questions — "who is this" and "who is this, here" — and a
// function that could return either would eventually be used where only one of
// them is safe.
func (h *Handler) identity(r *http.Request) (*session.Identity, error) {
	if h.cfg.Identities == nil {
		return nil, rigerr.Unauthorized("identity sessions are not enabled")
	}
	presented, err := bearer(r)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(presented, session.PrefixIdentity) {
		// A tenant token presented here is refused by shape. It would often
		// name the right person, and accepting it would make the two credentials
		// interchangeable — which is the thing keeping them apart is for.
		return nil, session.ErrNoIdentitySession
	}
	return h.cfg.Identities.Verify(r.Context(), presented)
}

type registerRequest struct {
	EmailAddress string `json:"emailAddress"`
	DisplayName  string `json:"displayName"`
	Password     string `json:"password"`
}

// register creates a person with no tenant.
//
// Mounted only when the application allowed it. Whether a stranger may create an
// account is the same kind of decision as whether they may create a tenant:
// a product with an invite-only front door and one with public sign-up are both
// ordinary, and they need different code.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in registerRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	res, err := h.cfg.Accounts.Register(r.Context(), account.RegisterInput{
		EmailAddress: in.EmailAddress,
		DisplayName:  in.DisplayName,
		Password:     in.Password,
		IPAddress:    h.addrString(r),
		UserAgent:    r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, signInOf(res))
}

type invitationToMeView struct {
	ID uuid.UUID `json:"id"`
	// TenantName is the part that means anything to somebody who has not been
	// there. The identifier is for the accept call.
	TenantID   uuid.UUID `json:"tenantId"`
	TenantName string    `json:"tenantName"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// myInvitations lists the invitations waiting for the caller, in every tenant.
//
// The other side of [Handler.listInvitations], which is an administrator looking
// at their own tenant. This is somebody looking at where they have been asked
// to go, and it is most of what there is to see before they belong anywhere.
//
// It does not include the tokens. An invitation's token is the credential that
// redeems it and it was sent to an address; listing tokens here would turn "I can
// see my invitations" into "I can accept them", which is a different claim about
// who reached the mailbox.
func (h *Handler) myInvitations(w http.ResponseWriter, r *http.Request) {
	who, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	invites, err := h.cfg.Accounts.MyInvitations(r.Context(), who.IdentityID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := make([]invitationToMeView, 0, len(invites))
	for _, i := range invites {
		out = append(out, invitationToMeView{
			ID: i.ID, TenantID: i.TenantID, TenantName: i.TenantName,
			Role: string(i.Role), CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// myTenants lists the tenants the caller belongs to.
//
// The same list /auth/tenants answers, reached with an identity token instead of
// a session — which is what somebody has between signing in and picking one.
func (h *Handler) myTenants(w http.ResponseWriter, r *http.Request) {
	who, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	spaces, err := h.cfg.Accounts.MyTenants(r.Context(), who.IdentityID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := make([]tenantView, 0, len(spaces))
	for _, s := range spaces {
		out = append(out, tenantView{
			TenantID: s.TenantID, TenantName: s.TenantName, TenantSlug: s.TenantSlug,
			AccountID: s.AccountID, Role: string(s.Role),
			// Nothing is current: the caller is not in a tenant, which is the
			// reason they are asking.
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// endIdentitySession signs somebody out of the picker.
//
// Separate from /auth/logout, which ends a tenant session. Somebody part-way
// through choosing has only this credential, and leaving it alive because the
// other endpoint did not apply would be a session nobody could end.
func (h *Handler) endIdentitySession(w http.ResponseWriter, r *http.Request) {
	presented, err := bearer(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if h.cfg.Identities == nil {
		h.fail(w, r, rigerr.Unauthorized("identity sessions are not enabled"))
		return
	}
	if err := h.cfg.Identities.Revoke(r.Context(), presented); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type acceptAsMeRequest struct {
	InvitationID uuid.UUID `json:"invitationId"`
	Client       string    `json:"client"`
}

// acceptFromThePicker joins the tenant an invitation names.
//
// It answers with a full sign-in body, so the client swaps the credential it was
// holding for the one it now wants: a tenant session, and a tenant list with
// the new one in it. That is the last step of the flow this whole file exists for —
// signed in, nowhere to be, and now somewhere.
func (h *Handler) acceptFromThePicker(w http.ResponseWriter, r *http.Request) {
	who, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	var in acceptAsMeRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	pair, err := h.cfg.Accounts.AcceptAsMe(r.Context(), who.IdentityID, account.AcceptInput{
		InvitationID: in.InvitationID,
		Client:       clientOf(in.Client),
		IPAddress:    h.addrString(r),
		UserAgent:    r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.answerWithTenant(w, r, who.IdentityID, pair)
}

type createTenantRequest struct {
	Name   string `json:"name"`
	Client string `json:"client"`
}

// createTenant makes one and signs the caller into it.
//
// The work is [account.Service.CreateTenant]: the tenant row, the first account
// and whatever the application's OnCreated hook adds, in one transaction. What is
// left here is HTTP — read the name, issue the session, answer with the list.
func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	who, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	var in createTenantRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		h.fail(w, r, rigerr.Invalid("a tenant needs a name"))
		return
	}

	made, err := h.cfg.Accounts.CreateTenant(r.Context(), account.CreateTenantInput{
		IdentityID: who.IdentityID,
		Name:       in.Name,
		IPAddress:  h.addrString(r),
		UserAgent:  r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	pair, err := h.cfg.Sessions.Issue(r.Context(), session.IssueInput{
		TenantID:  made.TenantID,
		AccountID: made.AccountID,
		Client:    clientOf(in.Client),
		IPAddress: h.addrString(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.answerWithTenant(w, r, who.IdentityID, pair)
}

// answerWithTenant is the body both picker exits share: the session they just
// earned, the identity token they still hold, and the tenant list with the new
// one in it.
//
// The identity token is deliberately not revoked here. Somebody who just joined
// their second tenant is one click from switching to the first, and ending the
// picker's credential the moment it succeeded would make that a fresh sign-in.
func (h *Handler) answerWithTenant(
	w http.ResponseWriter, r *http.Request, identityID uuid.UUID, pair session.Pair,
) {
	spaces, err := h.cfg.Accounts.MyTenants(r.Context(), identityID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	tok, err := h.cfg.Sessions.Verify(r.Context(), pair.Access.Token)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := signInResponse{
		tokenPair: pairOf(pair),
		Tenants:   make([]tenantView, 0, len(spaces)),
	}
	for _, s := range spaces {
		out.Tenants = append(out.Tenants, tenantView{
			TenantID: s.TenantID, TenantName: s.TenantName, TenantSlug: s.TenantSlug,
			AccountID: s.AccountID, Role: string(s.Role),
			Current: s.TenantID == tok.TenantID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
