package authhttp

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

type acceptRequest struct {
	Token string `json:"token"`
	// Password is only read when the person has none yet. Somebody joining a
	// second tenant already has one, and it is not this endpoint's business.
	Password string `json:"password"`
	Client   string `json:"client"`
}

// acceptInvitation redeems an invitation and answers with a session.
//
// No tenant header: the link says which tenant it is for, and letting a header
// override that would be a way to join a tenant nobody invited you to.
func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in acceptRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	pair, err := h.cfg.Accounts.AcceptInvitation(r.Context(), account.AcceptInput{
		Token:     in.Token,
		Password:  in.Password,
		Client:    clientOf(in.Client),
		IPAddress: h.addrString(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pairOf(pair))
}

type invitationView struct {
	ID           uuid.UUID `json:"id"`
	EmailAddress string    `json:"emailAddress"`
	DisplayName  string    `json:"displayName"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// listInvitations answers with the invitations into the caller's tenant that are
// still live.
//
// It needs the same permission inviting does: who has been invited and not yet
// arrived is a list of people who do not work here yet, and that is administrative
// rather than public.
func (h *Handler) listInvitations(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := tenancy.Require(claims, account.PermissionProvision); err != nil {
		h.fail(w, r, err)
		return
	}

	pending, err := h.cfg.Accounts.Invitations(r.Context(), claims.TenantID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := make([]invitationView, 0, len(pending))
	for _, i := range pending {
		out = append(out, invitationView{
			ID: i.ID, EmailAddress: i.EmailAddress, DisplayName: i.DisplayName,
			Role: string(i.Role), CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// revokeInvitation withdraws one.
func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := tenancy.Require(claims, account.PermissionProvision); err != nil {
		h.fail(w, r, err)
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, rigerr.BadRequest("%q is not an invitation identifier", r.PathValue("id")))
		return
	}

	if err := h.cfg.Accounts.RevokeInvitation(r.Context(), account.RevokeInput{
		TenantID:     claims.TenantID,
		InvitationID: id,
		ByAccountID:  claims.Actor(),
		ByAPIKeyID:   claims.ActorKey(),
	}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type tenantView struct {
	TenantID   uuid.UUID `json:"tenantId"`
	TenantName string    `json:"tenantName"`
	TenantSlug string    `json:"tenantSlug"`
	AccountID  uuid.UUID `json:"accountId"`
	Role       string    `json:"role"`
	// Current marks the tenant this request was made in, so an interface can
	// show where somebody is without comparing identifiers itself.
	Current bool `json:"current"`
}

// listTenants answers with every tenant the caller belongs to.
func (h *Handler) listTenants(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	spaces, err := h.cfg.Accounts.Tenants(r.Context(), claims.TenantID, claims.AccountID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := make([]tenantView, 0, len(spaces))
	for _, s := range spaces {
		out = append(out, tenantView{
			TenantID: s.TenantID, TenantName: s.TenantName, TenantSlug: s.TenantSlug,
			AccountID: s.AccountID, Role: string(s.Role),
			Current: s.TenantID == claims.TenantID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// switchTenant issues a session for another tenant the caller belongs to.
func (h *Handler) switchTenant(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	to, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, rigerr.BadRequest("%q is not a tenant identifier", r.PathValue("id")))
		return
	}

	pair, err := h.cfg.Accounts.Switch(r.Context(), account.SwitchInput{
		TenantID:   claims.TenantID,
		AccountID:  claims.AccountID,
		ToTenantID: to,
		IPAddress:  h.addrString(r),
		UserAgent:  r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pairOf(pair))
}
