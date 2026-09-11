package authhttp

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// acceptInvitation redeems an invitation and answers with a session.
//
// No tenant header: the link says which tenant it is for, and letting a header
// override that would be a way to join a tenant nobody invited you to.
func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in authwire.AcceptRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	pair, err := h.cfg.Accounts.AcceptInvitation(r.Context(), account.AcceptInput{
		Token:     in.Token,
		Client:    clientOf(in.Client),
		IPAddress: h.addrString(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pairOf(pair))
}

// inviteSomebody asks an address to join the caller's tenant, and creates
// nothing in it.
//
// Who invited them comes from the claims and never from the body, which is the
// same rule provisioning follows: a request that could name the inviter is a
// request that could name somebody else, and the name is what a landing page
// shows to somebody who has not signed in yet.
func (h *Handler) inviteSomebody(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := tenancy.Require(claims, account.PermissionProvision); err != nil {
		h.fail(w, r, err)
		return
	}

	var in authwire.InviteRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	inv, err := h.cfg.Accounts.Invite(r.Context(), account.InviteInput{
		TenantID:     claims.TenantID,
		EmailAddress: in.EmailAddress,
		DisplayName:  in.DisplayName,
		Role:         account.Role(in.Role),
		ByAccountID:  claims.Actor(),
		ByAPIKeyID:   claims.ActorKey(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, invitationViewOf(*inv))
}

// previewInvitation says what an invitation link is for, to whoever holds it.
//
// The token is in the query string rather than a body, which is what lets the
// link in the mail be the request: a landing page reads it out of its own URL
// and asks. That does put a live secret somewhere access logs and Referer
// headers reach — the link in the mail already was — and a page that cannot
// interpret the token it was handed is the failure this endpoint exists to fix,
// so the trade is worth taking and worth knowing about.
//
// No credential, and none read. Somebody following an invitation is, more often
// than not, signed out and on a device this installation has never seen. If they
// *are* signed in, the identifier this hands back is what takes them through the
// picker's door instead — see [github.com/simonjanss/rig/auth/account.Service.AcceptAsMe].
//
// An empty token goes through the service and gets the same 404 as a wrong one.
// Short-circuiting it with a 400 would give the endpoint two answers where it
// promised one.
func (h *Handler) previewInvitation(w http.ResponseWriter, r *http.Request) {
	inv, err := h.cfg.Accounts.PreviewInvitation(r.Context(), account.PreviewInput{
		Token:     r.URL.Query().Get("token"),
		IPAddress: h.addrString(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, authwire.InvitationPreview{
		ID:         inv.ID,
		TenantID:   inv.TenantID,
		TenantName: inv.TenantName,
		// Masked here rather than in the service: a caller inside the process
		// may well have a reason to see the address, and a caller over the wire
		// has not proved they are its addressee.
		EmailAddress: account.MaskEmail(inv.EmailAddress),
		Role:         string(inv.Role),
		InvitedBy:    inv.InvitedByName,
		ExpiresAt:    inv.ExpiresAt,
	})
}

// invitationViewOf is the administrator's view of one invitation.
func invitationViewOf(i account.Invitation) authwire.InvitationView {
	return authwire.InvitationView{
		ID: i.ID, EmailAddress: i.EmailAddress, DisplayName: i.DisplayName,
		Role: string(i.Role), InvitedBy: i.InvitedByName,
		CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt,
	}
}

// listInvitations answers with the invitations into the caller's tenant that are
// still live.
//
// It needs the same permission inviting does: who has been invited and not yet
// arrived is a list of people who do not work here yet — and now literally so,
// since none of them has an account. That is administrative rather than public.
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

	out := make([]authwire.InvitationView, 0, len(pending))
	for _, i := range pending {
		out = append(out, invitationViewOf(i))
	}
	httpx.WriteJSON(w, http.StatusOK, authwire.List[authwire.InvitationView]{Data: out})
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

	out := make([]authwire.TenantView, 0, len(spaces))
	for _, s := range spaces {
		out = append(out, authwire.TenantView{
			TenantID: s.TenantID, TenantName: s.TenantName, TenantSlug: s.TenantSlug,
			AccountID: s.AccountID, Role: string(s.Role),
			Current: s.TenantID == claims.TenantID,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, authwire.List[authwire.TenantView]{Data: out})
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
	httpx.WriteJSON(w, http.StatusOK, pairOf(pair))
}
