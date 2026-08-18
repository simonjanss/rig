package authhttp

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.cfg.Tenant(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	var in authwire.LoginRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	res, err := h.cfg.Accounts.Login(r.Context(), account.LoginInput{
		TenantID:     tenantID,
		EmailAddress: in.EmailAddress,
		Password:     in.Password,
		Remember:     in.Remember,
		Client:       clientOf(in.Client),
		IPAddress:    h.addrString(r),
		UserAgent:    r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, signInOf(res))
}

// logout ends the session the request is authenticated with.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	tok, err := h.Session(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.cfg.Accounts.Logout(r.Context(), tok.RootTokenID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refresh takes the token in the body rather than the header.
//
// It is not the credential for anything else, and putting it in an
// Authorization header is how it ends up in an access log next to a hundred
// access tokens that expire in ten minutes.
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var in authwire.RefreshRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	pair, err := h.cfg.Accounts.Refresh(r.Context(), in.RefreshToken)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pairOf(pair))
}

// requestReset always answers 202.
//
// Whether the address is registered is not the caller's business, and any
// difference in status, body, or timing is the enumeration this endpoint is
// most often used for.
func (h *Handler) requestReset(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.cfg.Tenant(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	var in authwire.ResetRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	// A rate-limit refusal is still reported: it is about the caller's
	// behavior, not about whether the address exists.
	if err := h.cfg.Accounts.RequestPasswordReset(r.Context(), tenantID, in.EmailAddress, h.addrString(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) confirmReset(w http.ResponseWriter, r *http.Request) {
	var in authwire.ConfirmResetRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	if err := h.cfg.Accounts.ConfirmPasswordReset(r.Context(), in.Token, in.NewPassword, h.addrString(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// changePassword returns a fresh pair, because it revoked the caller's own.
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	var in authwire.ChangePasswordRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	pair, err := h.cfg.Accounts.ChangePassword(r.Context(), account.ChangePasswordInput{
		TenantID:        claims.TenantID,
		AccountID:       claims.AccountID,
		CurrentPassword: in.CurrentPassword,
		NewPassword:     in.NewPassword,
		IPAddress:       h.addrString(r),
		UserAgent:       r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pairOf(pair))
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var in authwire.VerifyEmailRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.cfg.Accounts.VerifyEmail(r.Context(), in.Token); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resendVerification(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.cfg.Accounts.SendEmailVerification(r.Context(), claims.TenantID, claims.AccountID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// SignIn finishes a provider sign-in by issuing a session and answering with the
// same body a login does.
//
// It is the default [github.com/simonjanss/rig/auth/oauth.Config.OnSignIn], so
// that a project which only wants "sign in with Google" to work does not have to
// write the last step itself. A browser flow usually wants a cookie and a
// redirect instead, which is exactly the sort of decision that stays with the
// application.
func (h *Handler) SignIn(w http.ResponseWriter, r *http.Request, in oauth.SignIn) error {
	pair, err := h.cfg.Sessions.Issue(r.Context(), session.IssueInput{
		TenantID:  in.TenantID,
		AccountID: in.AccountID,
		Client:    session.ClientWeb,
		IPAddress: h.addrString(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pairOf(pair))
	return nil
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	tok, err := h.Session(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	families, err := h.cfg.Sessions.List(r.Context(), tok.TenantID, tok.AccountID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := make([]authwire.SessionView, 0, len(families))
	for _, f := range families {
		out = append(out, authwire.SessionView{
			ID:         f.Root.ID,
			CreatedAt:  f.Root.CreatedAt,
			LastUsedAt: f.LastUsedAt,
			ExpiresAt:  f.Root.ExpiresAt,
			IPAddress:  f.Root.IPAddress,
			UserAgent:  f.Root.UserAgent,
			Client:     string(f.Root.Client),
			Current:    f.Root.ID == tok.RootTokenID,
		})
	}
	writeJSON(w, http.StatusOK, authwire.List[authwire.SessionView]{Data: out})
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	tok, err := h.Session(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, rigerr.BadRequest("id is not a valid identifier"))
		return
	}

	// Only your own sessions, and 404 rather than 403 for anything else so a
	// session identifier cannot be probed.
	families, err := h.cfg.Sessions.List(r.Context(), tok.TenantID, tok.AccountID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	found := false
	for _, f := range families {
		if f.Root.ID == id {
			found = true
			break
		}
	}
	if !found {
		h.fail(w, r, rigerr.NotFound("no session with that identifier"))
		return
	}

	if err := h.cfg.Sessions.Revoke(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) impersonate(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Before the permission check, deliberately. An impersonating session must
	// never be able to impersonate again, whatever the account it is acting as
	// happens to hold — nesting would make the audit trail claim one person was
	// two.
	if claims.ImpersonatedByAccountID != nil {
		h.fail(w, r, rigerr.Conflict("this session is already an impersonation"))
		return
	}
	if err := tenancy.Require(claims, PermissionImpersonate); err != nil {
		h.fail(w, r, err)
		return
	}

	var in authwire.ImpersonateRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	pair, err := h.cfg.Accounts.Impersonate(r.Context(), account.ImpersonateInput{
		TenantID:        claims.TenantID,
		AdministratorID: claims.AccountID,
		AccountID:       in.AccountID,
		IPAddress:       h.addrString(r),
		UserAgent:       r.UserAgent(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, pairOf(pair))
}

func (h *Handler) endImpersonation(w http.ResponseWriter, r *http.Request) {
	tok, err := h.Session(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.cfg.Accounts.EndImpersonation(r.Context(), tok); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func viewOf(k *apikey.Key) authwire.APIKeyView {
	v := authwire.APIKeyView{
		ID: k.ID, Name: k.Name, KeyID: k.KeyID, Kind: string(k.Kind),
		Scopes:     k.Scopes,
		CreatedAt:  k.CreatedAt,
		ExpiresAt:  k.ExpiresAt,
		LastUsedAt: k.LastUsedAt,
		RevokedAt:  k.RevokedAt,
	}
	if v.Scopes == nil {
		v.Scopes = []string{}
	}
	for _, p := range k.CIDRAllowList {
		v.CIDRAllowList = append(v.CIDRAllowList, p.String())
	}
	return v
}

// kindOf reads the kind a caller asked for.
//
// Empty is Integration, because that is the one to default to: its writes stay
// attributable to the integration after the person who set it up has gone.
func kindOf(s string) (apikey.Kind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "integration":
		return apikey.KindIntegration, nil
	case "personal":
		return apikey.KindPersonal, nil
	default:
		return "", rigerr.Invalid("%q is not a kind of key; use Integration or Personal", s)
	}
}

func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in authwire.CreateKeyRequest
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}

	kind, err := kindOf(in.Kind)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// Which permission this needs depends on what is being made, and the split is
	// the whole point: a personal key is a second way to present authority the
	// caller already holds — intersected with their grants on every request — and a
	// service key is a new account with scopes of its own. The first grants
	// nothing; the second is authority creation.
	own := kind == apikey.KindPersonal &&
		(in.ServiceAccountID == nil || *in.ServiceAccountID == claims.AccountID)
	if own {
		// Either permission does: somebody who administers the tenant's keys
		// can obviously make their own.
		if !claims.Can(PermissionOwnAPIKey) && !claims.Can(PermissionManageAPIKeys) {
			h.fail(w, r, tenancy.Require(claims, PermissionOwnAPIKey))
			return
		}
	} else if err := tenancy.Require(claims, PermissionManageAPIKeys); err != nil {
		h.fail(w, r, err)
		return
	}

	allow := make([]netip.Prefix, 0, len(in.CIDRAllowList))
	for _, raw := range in.CIDRAllowList {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			h.fail(w, r, rigerr.Invalid("%q is not a valid network", raw))
			return
		}
		allow = append(allow, p)
	}

	// A key cannot be given authority its creator does not hold. Without this,
	// "manage API keys" would quietly be "grant yourself anything".
	for _, scope := range in.Scopes {
		if !claims.Can(scope) {
			h.fail(w, r, rigerr.Forbidden(
				"you cannot grant a key the %q permission, which you do not hold", scope))
			return
		}
	}

	serviceAccount := claims.AccountID
	if in.ServiceAccountID != nil {
		serviceAccount = *in.ServiceAccountID
	}

	minted, err := h.cfg.APIKeys.Mint(r.Context(), apikey.MintInput{
		TenantID:           claims.TenantID,
		AccountID:          serviceAccount,
		Kind:               kind,
		Name:               in.Name,
		Scopes:             in.Scopes,
		CIDRAllowList:      allow,
		ExpiresAt:          in.ExpiresAt,
		CreatedByAccountID: claims.Actor(),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated,
		authwire.CreateKeyResponse{Key: viewOf(minted.Key), Secret: minted.Secret})
}

// listAPIKeys answers with the tenant's keys, or with the caller's own.
//
// Narrow unless the caller administers keys, which is the same shape the scope
// parameter on a generated read has: somebody who may mint a key that acts as
// themselves has to be able to see and revoke it, and that is not a reason to show
// them everybody else's.
func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	all := claims.Can(PermissionManageAPIKeys)
	if !all && !claims.Can(PermissionOwnAPIKey) {
		h.fail(w, r, tenancy.Require(claims, PermissionOwnAPIKey))
		return
	}

	keys, err := h.cfg.APIKeys.List(r.Context(), claims.TenantID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := make([]authwire.APIKeyView, 0, len(keys))
	for _, k := range keys {
		if !all && k.AccountID != claims.AccountID {
			continue
		}
		out = append(out, viewOf(k))
	}
	writeJSON(w, http.StatusOK, authwire.List[authwire.APIKeyView]{Data: out})
}

// revokeAPIKey kills one, if it is the caller's or the caller administers keys.
func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	all := claims.Can(PermissionManageAPIKeys)
	if !all && !claims.Can(PermissionOwnAPIKey) {
		h.fail(w, r, tenancy.Require(claims, PermissionOwnAPIKey))
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, rigerr.BadRequest("id is not a valid identifier"))
		return
	}

	if !all {
		// Somebody else's key is answered as a key that is not there. A 403 would
		// confirm it exists to a caller who may not see it, which is the rule
		// every cross-tenant read follows.
		//
		// Through the list rather than a lookup by identifier, because that is the
		// only read this package needs and a tenant's keys are a handful of rows.
		keys, err := h.cfg.APIKeys.List(r.Context(), claims.TenantID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		var mine bool
		for _, k := range keys {
			if k.ID == id && k.AccountID == claims.AccountID {
				mine = true
			}
		}
		if !mine {
			h.fail(w, r, rigerr.NotFound("no API key with that identifier"))
			return
		}
	}

	if err := h.cfg.APIKeys.Revoke(r.Context(), claims.TenantID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
