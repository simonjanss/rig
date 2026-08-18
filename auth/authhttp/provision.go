package authhttp

import (
	"net/http"
	"time"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// provision creates an account for somebody who has not got one.
//
// It is here rather than a POST on rig_account because an account created
// by plain CRUD would have no credential and nobody would have been told it
// exists. What this does instead is the part that is the same in every product:
// check the address, honour the tenant's allowed domains, refuse a second
// account for one address, record who asked, and — if asked — send the link that
// lets the person arrive.
//
// A key may call it, and that is the point of the audit columns: the row then
// says both which integration provisioned the account and through which
// credential.
func (h *Handler) provision(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := tenancy.Require(claims, account.PermissionProvision); err != nil {
		h.fail(w, r, err)
		return
	}

	var in authwire.ProvisionRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	// The actor comes from the credential, never from the body. A caller that
	// could name somebody else as the creator could forge an audit trail.
	acct, err := h.cfg.Accounts.Provision(r.Context(), account.ProvisionInput{
		TenantID:     claims.TenantID,
		EmailAddress: in.EmailAddress,
		DisplayName:  in.DisplayName,
		Kind:         account.Kind(in.Kind),
		Role:         account.Role(in.Role),
		TimeZone:     in.TimeZone,
		ByAccountID:  claims.Actor(),
		ByAPIKeyID:   claims.ActorKey(),
		Invite:       in.Invite,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, authwire.AccountView{
		ID:           acct.ID,
		TenantID:     acct.TenantID,
		EmailAddress: acct.EmailAddress,
		DisplayName:  acct.DisplayName,
		Kind:         string(acct.Kind),
		Role:         string(acct.Role),
		TimeZone:     acct.TimeZone,
		CreatedAt:    time.Now().UTC(),
	})
}
