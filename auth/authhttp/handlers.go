package authhttp

import (
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/query"
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
	httpx.WriteJSON(w, http.StatusOK, signInOf(res))
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
	httpx.WriteJSON(w, http.StatusOK, pairOf(pair))
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
	httpx.WriteJSON(w, http.StatusOK, pairOf(pair))
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
	httpx.WriteJSON(w, http.StatusOK, pairOf(pair))
	return nil
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	tok, err := h.Session(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	scope, err := h.sessionScope(r, tok, PermissionReadSessionsAll)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	var families []session.Family
	if scope == tenancy.ScopeAll {
		families, err = h.cfg.Sessions.ListTenant(r.Context(), tok.TenantID)
	} else {
		families, err = h.cfg.Sessions.List(r.Context(), tok.TenantID, tok.AccountID)
	}
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
			AccountID:  f.Root.AccountID,
			Client:     string(f.Root.Client),
			Current:    f.Root.ID == tok.RootTokenID,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, authwire.List[authwire.SessionView]{Data: out})
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	tok, err := h.Session(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// Before the identifier is even parsed. Asking to reach past your own
	// sessions without holding the permission is refused loudly, and it is
	// refused the same way whether the id that follows is real, malformed, or
	// invented — so the refusal says nothing about what exists.
	scope, err := h.sessionScope(r, tok, PermissionRevokeSessionsAll)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, rigerr.BadRequest("id is not a valid identifier"))
		return
	}

	// 404 rather than 403 for a session that is not the caller's to end, so a
	// session identifier cannot be probed. That rule survives the widening: a
	// caller who holds the permission gets the same 404 for an identifier in
	// another tenant as for one nobody has, and a caller who does not hold it
	// was already refused above.
	if err := h.revocable(r, tok, scope, id); err != nil {
		h.fail(w, r, err)
		return
	}

	// RevokeBy rather than Revoke, so the entry says who ended it. Without that
	// the trail records a session ending and never says by whom, which is the
	// one question asked about a session somebody else ended.
	if err := h.cfg.Sessions.RevokeBy(r.Context(), id, tok.AccountID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revocable reports whether this caller may end that session, as a 404 when it
// may not.
//
// The narrow case reads the caller's own families, which is the check that was
// here before. The wide case reads the token itself, because listing a whole
// tenant's sessions to look one up would get slower every day, and it holds the
// same two conditions the list would have: inside the caller's tenant, and the
// root of its family rather than some token within one.
func (h *Handler) revocable(r *http.Request, tok *session.Token, scope tenancy.Scope, id uuid.UUID) error {
	missing := rigerr.NotFound("no session with that identifier")

	if scope == tenancy.ScopeAll {
		found, err := h.cfg.Sessions.FindSession(r.Context(), tok.TenantID, id)
		if err != nil {
			return err
		}
		if found == nil {
			return missing
		}
		return nil
	}

	families, err := h.cfg.Sessions.List(r.Context(), tok.TenantID, tok.AccountID)
	if err != nil {
		return err
	}
	for _, f := range families {
		if f.Root.ID == id {
			return nil
		}
	}
	return missing
}

// scope reads the width the caller asked for and refuses one it does not hold.
//
// In one place, so that every endpoint here answers the parameter the way a
// generated endpoint does: an unrecognised value is a 400, and asking for more
// than you hold is a 403 rather than a quietly narrower answer. Narrowing
// instead would leave a caller unable to tell "you may not see that" from
// "there is nothing else".
func scope(r *http.Request, claims tenancy.Claims, wide string) (tenancy.Scope, error) {
	asked, err := tenancy.ParseScope(r.URL.Query().Get(tenancy.ScopeParam))
	if err != nil {
		return "", err
	}
	if err := tenancy.RequireScope(claims, asked, wide); err != nil {
		return "", err
	}
	return asked, nil
}

// sessionScope is [scope] for the two endpoints that hold a token already.
//
// They need the token for the session it names and the claims for the permission
// it carries, and resolving the request twice would verify it twice — so the
// claims are built from the token in hand rather than from the header again.
func (h *Handler) sessionScope(r *http.Request, tok *session.Token, wide string) (tenancy.Scope, error) {
	claims, err := h.claimsFor(r.Context(), tok)
	if err != nil {
		return "", err
	}
	return scope(r, claims, wide)
}

// audit answers the authentication trail.
//
// Both scopes come out of one endpoint on purpose. `scope=own` is "where have I
// signed in from, and did anything fail", which every product eventually wants
// and which would otherwise be a second endpoint with a second shape and its own
// bugs; `scope=all` is the tenant's trail and needs the permission for it.
//
// What no scope reaches is the entries that resolved to no tenant — an attempt
// that named none, or one against an address with no account anywhere. They are
// recorded, they are what the rate limiter most needs, and no tenant has the
// standing to read them. Reading those is an operator's job, against the table.
func (h *Handler) audit(w http.ResponseWriter, r *http.Request) {
	claims, err := h.Claims(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	asked, err := scope(r, claims, PermissionReadAuthLogAll)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	q, err := auditQuery(r, claims, asked)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	records, total, err := h.cfg.AuditLog.Read(r.Context(), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	out := make([]authwire.AuthLogEntryView, 0, len(records))
	for _, rec := range records {
		out = append(out, authwire.AuthLogEntryView{
			ID:           rec.ID,
			At:           rec.At,
			Event:        rec.Event,
			Outcome:      string(rec.Outcome),
			AccountID:    rec.AccountID,
			EmailAddress: rec.EmailAddress,
			IPAddress:    rec.IPAddress,
			UserAgent:    rec.UserAgent,
			APIKeyID:     rec.APIKeyID,
			APIKeyRef:    rec.APIKeyRef,
			SessionID:    rec.TokenRootID,
			Detail:       rec.Detail,
		})
	}

	httpx.WriteJSON(w, http.StatusOK, authwire.Page[authwire.AuthLogEntryView]{
		Data: out,
		Pagination: authwire.Pagination{
			Offset: q.Offset, Limit: q.Limit, Total: total,
		},
	})
}

// auditQuery reads the filters.
//
// Every one of them refuses a value it does not understand rather than returning
// fewer rows. A misspelled event that answered with an empty page would read as
// "that never happened", and there is no way to tell those apart from outside.
func auditQuery(r *http.Request, claims tenancy.Claims, asked tenancy.Scope) (authlog.Query, error) {
	params := r.URL.Query()
	q := authlog.Query{TenantID: claims.TenantID}

	switch asked {
	case tenancy.ScopeAll:
		if raw := params.Get("accountId"); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil {
				return q, rigerr.BadRequest("accountId is not a valid identifier")
			}
			q.AccountID = &id
		}
	default:
		// Narrow means the caller's own events and cannot be pointed elsewhere.
		// A credential with no account behind it — an integration key — has no
		// own events to read, and saying so beats answering with the empty page
		// that filtering on a nil identifier would produce.
		if claims.AccountID == uuid.Nil {
			return q, rigerr.Forbidden("this credential acts for no account, so it has no events of its own; "+
				"ask for %s=%s", tenancy.ScopeParam, tenancy.ScopeAll)
		}
		if raw := params.Get("accountId"); raw != "" && raw != claims.AccountID.String() {
			return q, rigerr.BadRequest("accountId names somebody else, which needs %s=%s",
				tenancy.ScopeParam, tenancy.ScopeAll)
		}
		mine := claims.AccountID
		q.AccountID = &mine
	}

	if event := params.Get("event"); event != "" {
		if !slices.Contains(authlog.Events(), event) {
			return q, rigerr.BadRequest("event %q is not one rig records", event)
		}
		q.Event = event
	}

	switch outcome := params.Get("outcome"); authlog.Outcome(outcome) {
	case "":
	case authlog.Succeeded, authlog.Failed:
		q.Outcome = authlog.Outcome(outcome)
	default:
		return q, rigerr.BadRequest("outcome must be %q or %q, not %q",
			authlog.Succeeded, authlog.Failed, outcome)
	}

	var err error
	if q.Since, err = instant(params.Get("since"), "since"); err != nil {
		return q, err
	}
	if q.Until, err = instant(params.Get("until"), "until"); err != nil {
		return q, err
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Until.After(q.Since) {
		return q, rigerr.BadRequest("until must be after since")
	}

	page, err := pageOf(params)
	if err != nil {
		return q, err
	}
	q.Limit, q.Offset = page.Limit, page.Offset
	return q, nil
}

// instant parses a timestamp filter.
func instant(raw, name string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, rigerr.BadRequest("%s must be an RFC 3339 timestamp, like 2026-08-20T09:00:00Z", name)
	}
	return at, nil
}

// pageOf reads limit and offset, clamped.
//
// A limit is always applied, and the ceiling is not negotiable: an unbounded
// list is a production incident waiting for the table to grow, and this is the
// table that grows with every login.
func pageOf(params url.Values) (query.Page, error) {
	var page query.Page

	for _, p := range []struct {
		name string
		into *int
	}{{"limit", &page.Limit}, {"offset", &page.Offset}} {
		raw := params.Get(p.name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return page, rigerr.BadRequest("%s must be a whole number", p.name)
		}
		*p.into = n
	}
	return page.Clamp(defaultPageLimit, maxPageLimit), nil
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
	httpx.WriteJSON(w, http.StatusCreated, pairOf(pair))
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
	httpx.WriteJSON(w, http.StatusCreated,
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
	httpx.WriteJSON(w, http.StatusOK, authwire.List[authwire.APIKeyView]{Data: out})
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
