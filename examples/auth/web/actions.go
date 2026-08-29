package web

import (
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/services/authz"
)

// signUp creates a tenant and signs its owner in.
//
// Two API calls now, and nothing else: register, then create. It used to reach
// past the API into a service of its own, because rig created no tenants — that
// is the auth package's job now, so the form is a convenience over two endpoints
// rather than a second way in.
func (h *Handler) signUp(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	// The person first. What comes back is the tenant-less credential, which is
	// exactly what creating a tenant takes.
	body, _ := json.Marshal(map[string]any{
		"emailAddress": r.FormValue("email"),
		"displayName":  r.FormValue("name"),
		"password":     r.FormValue("password"),
	})

	req := r.Clone(r.Context())
	req.Header.Del("Cookie")

	status, out := h.call(req, http.MethodPost, "/auth/register", "", string(body))
	if status != http.StatusCreated {
		h.fail(w, r, status, out)
		return
	}
	var p pair
	if err := json.Unmarshal(out, &p); err != nil {
		h.redirect(w, r, "the response could not be read")
		return
	}

	// Then the tenant, with the credential the first call handed back.
	body, _ = json.Marshal(map[string]any{
		"name": r.FormValue("tenantName"), "client": "Web",
	})
	h.leavePicker(w, r, p.IdentityToken, "/auth/tenants", string(body),
		"tenant created — you are its Owner")
}

// login signs in with an address and a password, and nothing else.
//
// No tenant, deliberately: nobody knows which tenants an address belongs to
// until the password has been checked, so a sign-in page that asks first is asking
// a question the visitor cannot answer. The session lands in one of their
// tenants and the tabs under the header reach the rest.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}
	h.signInAs(w, r, uuid.Nil, r.FormValue("email"), r.FormValue("password"), "")
}

// signInAs posts to /auth/login and keeps the pair.
//
// A zero tenant means "wherever I belong", and the session that comes back says
// which — asked for afterwards, because the cookie's tenant is what every later
// request sends as its header.
func (h *Handler) signInAs(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, email, password, flash string) {
	body, _ := json.Marshal(map[string]any{
		"emailAddress": email, "password": password, "client": "Web",
	})

	// The header cannot come from the cookie here: there is no session yet. It is
	// set explicitly when a tenant was named, and left off when one was not.
	req := r.Clone(r.Context())
	req.Header.Del("Cookie")
	if tenantID != uuid.Nil {
		req.AddCookie(&http.Cookie{
			Name: cookieName, Value: base64(mustJSON(sessionCookie{TenantID: tenantID})),
		})
	}

	status, out := h.call(req, http.MethodPost, "/auth/login", "", string(body))
	if status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}

	var p pair
	if err := json.Unmarshal(out, &p); err != nil {
		h.redirect(w, r, "the sign-in response could not be read")
		return
	}

	if p.AccessToken == "" {
		// Signed in and belonging nowhere, which used to be a 403 and is now the
		// picker: their invitations, and the option of making a tenant. Only the
		// identity token is kept, because it is the only credential there is.
		h.setSession(w, p, uuid.Nil)
		h.redirect(w, r, "signed in — pick a tenant or make one")
		return
	}

	if tenantID == uuid.Nil {
		landed, err := h.tenantOfSession(r, p.AccessToken)
		if err != nil {
			h.redirect(w, r, err.Error())
			return
		}
		tenantID = landed
	}

	h.setSession(w, p, tenantID)
	h.redirect(w, r, flash)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if s, ok := h.session(r); ok {
		h.call(r, http.MethodPost, "/auth/logout", s.Access, "")
	}
	h.clearSession(w)
	h.redirect(w, r, "signed out — the token is dead on the next request")
}

// refresh rotates the pair, which is the thing worth watching: the presented
// token is consumed and replaying it revokes the whole family.
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	s, ok := h.session(r)
	if !ok {
		h.redirect(w, r, "not signed in")
		return
	}

	body, _ := json.Marshal(map[string]string{"refreshToken": s.Refresh})
	status, out := h.call(r, http.MethodPost, "/auth/refresh", "", string(body))
	if status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}

	var p pair
	if err := json.Unmarshal(out, &p); err != nil {
		h.redirect(w, r, "the refresh response could not be read")
		return
	}
	h.setSession(w, p, s.TenantID)
	h.redirect(w, r, "rotated — the old refresh token is spent")
}

// switchTenant moves to another tenant the same person belongs to.
func (h *Handler) switchTenant(w http.ResponseWriter, r *http.Request) {
	s, ok := h.session(r)
	if !ok {
		h.redirect(w, r, "not signed in")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	to, err := uuid.Parse(r.FormValue("tenant"))
	if err != nil {
		h.redirect(w, r, "which tenant?")
		return
	}

	status, out := h.call(r, http.MethodPost, "/auth/tenants/"+to.String()+"/switch", s.Access, "")
	if status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}

	var p pair
	if err := json.Unmarshal(out, &p); err != nil {
		h.redirect(w, r, "the switch response could not be read")
		return
	}
	h.setSession(w, p, to)
	h.redirect(w, r, "switched — a new session, for your account in that tenant")
}

// invite provisions somebody into this tenant and mints an invitation.
func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	s, ok := h.session(r)
	if !ok {
		h.redirect(w, r, "not signed in")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	body, _ := json.Marshal(map[string]any{
		"emailAddress": r.FormValue("email"),
		"displayName":  r.FormValue("name"),
		"role":         r.FormValue("role"),
		"invite":       true,
	})

	// With a key when one was chosen, which is the point of the selector: the
	// same endpoint, a different credential, and an audit trail that says which.
	token := s.Access
	if key := strings.TrimSpace(r.FormValue("key")); key != "" {
		token = key
	}

	status, out := h.call(r, http.MethodPost, "/auth/accounts", token, string(body))
	if status != http.StatusCreated && status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}

	// Provisioning gave them a level and no grants: the auth package has no idea
	// what "Admin" means here, so an invited Admin could sign in and do nothing
	// until the application says. This is the application saying.
	var made struct {
		ID   uuid.UUID `json:"id"`
		Role string    `json:"role"`
	}
	if err := json.Unmarshal(out, &made); err == nil && made.ID != uuid.Nil {
		grants := append(api.PermissionKeys(), authz.AuthKeys()...)
		if err := authz.GrantLevel(
			r.Context(), h.pool, s.TenantID, made.ID, made.Role, grants, h.grantsCache,
		); err != nil {
			h.redirect(w, r, "invited, but the role could not be granted: "+err.Error())
			return
		}
	}

	h.redirect(w, r, "invited — the link is in the outbox below")
}

// revokeInvite withdraws an invitation, which also removes the account it made.
func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	s, ok := h.session(r)
	if !ok {
		h.redirect(w, r, "not signed in")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	id := r.FormValue("id")
	status, out := h.call(r, http.MethodDelete, "/auth/invitations/"+id, s.Access, "")
	if status != http.StatusNoContent && status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}
	h.redirect(w, r, "invitation withdrawn — the link is dead and the account is gone")
}

// accept redeems an invitation and signs the caller in as the invited account.
func (h *Handler) accept(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	body, _ := json.Marshal(map[string]any{
		"token":    r.FormValue("token"),
		"password": r.FormValue("password"),
		"client":   "Web",
	})

	// Unauthenticated, and with no tenant header: the link says which tenant
	// it is for, and letting a header override that would be a way to join one
	// nobody invited you to.
	plain := r.Clone(r.Context())
	plain.Header.Del("Cookie")

	status, out := h.call(plain, http.MethodPost, "/auth/invitations/accept", "", string(body))
	if status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}

	var p pair
	if err := json.Unmarshal(out, &p); err != nil {
		h.redirect(w, r, "the accept response could not be read")
		return
	}

	// The session says which tenant it is for, and the interface needs to agree:
	// the cookie's tenant is what every later request sends as its header.
	tenantID, err := h.tenantOfSession(r, p.AccessToken)
	if err != nil {
		h.redirect(w, r, err.Error())
		return
	}
	h.setSession(w, p, tenantID)
	h.redirect(w, r, "joined — you are signed in to that tenant")
}

// tenantOfSession asks the API which tenant a token belongs to.
//
// /auth/tenants marks the current one, which is how the interface learns where a
// freshly accepted invitation put it without being told.
func (h *Handler) tenantOfSession(r *http.Request, access string) (uuid.UUID, error) {
	status, out := h.call(r, http.MethodGet, "/auth/tenants", access, "")
	if status != http.StatusOK {
		return uuid.Nil, fmt.Errorf("could not read the new session: %s", message(status, out))
	}

	var body struct {
		Data []struct {
			TenantID uuid.UUID `json:"tenantId"`
			Current  bool      `json:"current"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		return uuid.Nil, err
	}
	for _, s := range body.Data {
		if s.Current {
			return s.TenantID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("the new session names no tenant")
}

// createNote writes a note, as the session or as a key.
//
// This is the one the whole interface is arranged around: the same endpoint, two
// credentials, and a row that records which. The list shows the difference.
func (h *Handler) createNote(w http.ResponseWriter, r *http.Request) {
	s, ok := h.session(r)
	if !ok {
		h.redirect(w, r, "not signed in")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	body, _ := json.Marshal(map[string]any{
		"title": r.FormValue("title"),
		"body":  r.FormValue("body"),
	})

	token, as := s.Access, "your session"
	if key := strings.TrimSpace(r.FormValue("key")); key != "" {
		token, as = key, "an API key"
	}

	status, out := h.call(r, http.MethodPost, "/api/v1/notes", token, string(body))
	if status != http.StatusCreated {
		h.fail(w, r, status, out)
		return
	}
	h.redirect(w, r, "note written with "+as)
}

// createKey mints an API key. The secret comes back once and is shown once.
func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	s, ok := h.session(r)
	if !ok {
		h.redirect(w, r, "not signed in")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	scopes := r.Form["scope"]
	body, _ := json.Marshal(map[string]any{
		"name":   r.FormValue("name"),
		"kind":   r.FormValue("kind"),
		"scopes": scopes,
	})

	status, out := h.call(r, http.MethodPost, "/auth/api-keys", s.Access, string(body))
	if status != http.StatusCreated && status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}

	var made struct {
		// The secret is a field of its own, beside the key it belongs to: the
		// key is a row anybody with the permission can list, and this is the one
		// response that will ever contain the other half.
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(out, &made)

	// Kept for the length of this page render and no longer. Only the hash is
	// stored, so this is the one moment the secret exists — which is exactly why
	// a real interface shows it once and tells you to copy it.
	h.lastKey.Store(made.Secret)
	h.redirect(w, r, "key created — the secret is shown once, below")
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	s, ok := h.session(r)
	if !ok {
		h.redirect(w, r, "not signed in")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	id := r.FormValue("id")
	status, out := h.call(r, http.MethodDelete, "/auth/api-keys/"+id, s.Access, "")
	if status != http.StatusNoContent && status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}
	h.redirect(w, r, "key revoked — it stops working on the next request")
}

// base64 and unbase64 keep the cookie printable. base32 without padding, because
// a cookie value with an = in it is a cookie some proxy eventually mangles.
var cookieEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func base64(raw []byte) string { return cookieEncoding.EncodeToString(raw) }

func unbase64(s string) ([]byte, error) { return cookieEncoding.DecodeString(s) }

func mustJSON(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}

// register creates a person and lands them in the picker.
//
// The first of the two doors a stranger has. It makes no tenant: what comes
// back is an identity token and a look at the invitations waiting for them, which
// is the state the picker exists to render.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	body, _ := json.Marshal(map[string]any{
		"emailAddress": r.FormValue("email"),
		"displayName":  r.FormValue("name"),
		"password":     r.FormValue("password"),
	})

	// No cookie and no tenant header: there is nothing to be in yet.
	req := r.Clone(r.Context())
	req.Header.Del("Cookie")

	status, out := h.call(req, http.MethodPost, "/auth/register", "", string(body))
	if status != http.StatusCreated {
		h.fail(w, r, status, out)
		return
	}

	var p pair
	if err := json.Unmarshal(out, &p); err != nil {
		h.redirect(w, r, "the response could not be read")
		return
	}
	h.setSession(w, p, uuid.Nil)
	h.redirect(w, r, "account created — pick a tenant or make one")
}

// join accepts an invitation from the picker.
//
// It sends the invitation's identifier and nothing else. The emailed token is the
// other way in, for somebody who is not signed in; a caller already signed in as
// the person invited has made the stronger claim of the two, so the identifier is
// enough — which is why the listing hands out identifiers and never tokens.
func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	token, ok := h.picker(r)
	if !ok {
		h.redirect(w, r, "sign in first")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	body, _ := json.Marshal(map[string]any{
		"invitationId": r.FormValue("invitation"), "client": "Web",
	})
	h.leavePicker(w, r, token, "/auth/me/invitations/accept", string(body), "joined")
}

// createTenant makes one instead of joining one.
func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	token, ok := h.picker(r)
	if !ok {
		h.redirect(w, r, "sign in first")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "could not read the form")
		return
	}

	body, _ := json.Marshal(map[string]any{
		"name": r.FormValue("tenantName"), "client": "Web",
	})
	h.leavePicker(w, r, token, "/auth/tenants", string(body),
		"tenant created — you are its Owner")
}

// leavePicker swaps the identity credential for a tenant session.
//
// Both exits answer with the same body a sign-in does, so both end the same way:
// keep the pair, and remember which tenant it is for — the cookie's tenant is
// what every later request sends as its header.
func (h *Handler) leavePicker(w http.ResponseWriter, r *http.Request, token, path, body, flash string) {
	req := r.Clone(r.Context())
	req.Header.Del("Cookie")

	status, out := h.call(req, http.MethodPost, path, token, body)
	if status != http.StatusOK {
		h.fail(w, r, status, out)
		return
	}

	var p pair
	if err := json.Unmarshal(out, &p); err != nil {
		h.redirect(w, r, "the response could not be read")
		return
	}

	// Which tenant it landed in comes from the list that came back with it,
	// rather than from a second call.
	var landed uuid.UUID
	for _, ws := range p.Tenants {
		if ws.Current {
			landed = ws.TenantID
		}
	}
	if landed == uuid.Nil {
		h.redirect(w, r, "the session came back naming no tenant")
		return
	}

	// The identity token is kept: switching tenant is one click away, and the
	// picker's endpoints are what answer "where else could I go".
	p.IdentityToken = token
	h.setSession(w, p, landed)
	h.redirect(w, r, flash)
}
