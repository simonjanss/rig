package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/examples/auth/services/outbox"
)

// data is everything the page shows.
type data struct {
	Flash string
	Base  string

	// SignedIn and what follows are only filled when there is a tenant session.
	SignedIn bool
	Me       me

	// InThePicker is the state between the two: they proved who they are and
	// belong to no tenant, so there is nothing to scope a read by. What renders
	// is their invitations and an offer to make one.
	InThePicker bool
	// MyInvitations are the ones addressed to the caller, across tenants, which
	// is a different list from Invitations — that one is an administrator looking
	// at their own tenant.
	MyInvitations []myInviteView

	// Tenants are the ones the caller belongs to, which the header draws as tabs.
	Tenants     []tenantView
	Invitations []inviteView
	Sessions    []sessionView
	Keys        []keyView
	Notes       []noteView
	People      []personView
	Log         []logView

	// NoteScope is how wide the notes panel asked to read: "own" or "all". The
	// note table declares `access: { scope: own }`, so the parameter exists and
	// widening it needs note.read.all.
	NoteScope string

	Outbox  []outbox.Message
	LastKey string

	// Refused records the panels whose API call was turned away, keyed by panel.
	//
	// A 403 is the most interesting answer an authentication demonstration can
	// give, and an empty panel is the worst way to report one: "no keys yet" and
	// "you may not read the keys" look identical and mean opposite things.
	Refused map[string]string

	Trace []entry
}

type me struct {
	AccountID   uuid.UUID
	TenantID    uuid.UUID
	Tenant      string
	Email       string
	DisplayName string
	Role        string
	Roles       []string
	Permissions []string
	Verified    bool

	// CanOwnKeys and CanManageKeys are the two API-key permissions, resolved once
	// so the template asks a question rather than searching a slice.
	CanOwnKeys    bool
	CanManageKeys bool
	ExpiresAt     time.Time
}

type tenantView struct {
	TenantID   uuid.UUID `json:"tenantId"`
	TenantName string    `json:"tenantName"`
	AccountID  uuid.UUID `json:"accountId"`
	Role       string    `json:"role"`
	Current    bool      `json:"current"`
}

// myInviteView is an invitation seen by the person it was sent to. The tenant's
// name is the part they recognise; the identifier is what accepting sends.
type myInviteView struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenantId"`
	TenantName string    `json:"tenantName"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type inviteView struct {
	ID           uuid.UUID `json:"id"`
	EmailAddress string    `json:"emailAddress"`
	DisplayName  string    `json:"displayName"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type sessionView struct {
	ID         uuid.UUID `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	IPAddress  string    `json:"ipAddress"`
	UserAgent  string    `json:"userAgent"`
	Client     string    `json:"client"`
	Current    bool      `json:"current"`
}

type keyView struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	KeyID      string     `json:"keyId"`
	Kind       string     `json:"kind"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

type noteView struct {
	ID                uuid.UUID  `json:"id"`
	Title             string     `json:"title"`
	Body              *string    `json:"body"`
	CreatedAt         time.Time  `json:"createdAt"`
	CreatedByAccount  *uuid.UUID `json:"createdByAccountId"`
	CreatedByAPIKeyID *uuid.UUID `json:"createdByApiKeyId"`

	// ByWhom and ByWhat are filled in from the people and keys already fetched,
	// so a row can say "Ada, through Nightly import" rather than two identifiers.
	ByWhom string
	ByWhat string
}

type personView struct {
	AccountID uuid.UUID
	Email     string
	Name      string
	Role      string
	Kind      string
	Verified  bool
	JoinedAt  time.Time
}

// logView is one line of the trail, decoded from what GET /auth/audit answers.
//
// The tags are the endpoint's own names. Detail arrives as an object and is
// rendered as whatever JSON it was, because what is in it depends on the event —
// the addresses a replayed token was used from, the administrator who ended a
// session — and a page that showed only the keys it knew about would hide exactly
// the entries worth reading.
type logView struct {
	At        time.Time      `json:"at"`
	Event     string         `json:"event"`
	Outcome   string         `json:"outcome"`
	Email     string         `json:"emailAddress"`
	IPAddress string         `json:"ipAddress"`
	KeyRef    string         `json:"apiKeyRef"`
	Detail    map[string]any `json:"detail"`
}

// lastKey holds a freshly minted secret for exactly one render.
//
// A field on the handler rather than the session, because it is not per-user
// state worth persisting: the secret exists once, is shown once, and is gone.
type keyHolder struct{ v atomic.Value }

func (k *keyHolder) Store(s string) { k.v.Store(s) }

func (k *keyHolder) Take() string {
	s, _ := k.v.Swap("").(string)
	return s
}

// page renders everything.
//
// One handler, one render, every panel — which costs a handful of API calls per
// page load and buys the property that matters in a demonstration: what is on the
// screen is what the API says right now.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	d := data{
		Flash:   r.URL.Query().Get("flash"),
		Base:    "http://" + r.Host,
		Trace:   h.trace.all(),
		Outbox:  h.mail.Messages(),
		LastKey: h.lastKey.Take(),
		Refused: map[string]string{},

		// From the page's own query string, so the two links in the panel are
		// ordinary links and the browser's back button does the right thing. It is
		// not validated here: the API validates it, and passing a bad value
		// straight through is how the panel shows what the API says about one.
		NoteScope: noteScope(r.URL.Query().Get("scope")),
	}

	// The picker first, because it is the narrower state: a cookie with an identity
	// token and no access token is somebody part-way through.
	if token, ok := h.picker(r); ok {
		if _, signedIn := h.session(r); !signedIn {
			if h.fillPicker(r, token, &d) {
				d.InThePicker = true
			} else {
				h.clearSession(w)
				d.Flash = "that sign-in has expired — sign in again"
			}
		}
	}

	if s, ok := h.session(r); ok {
		// A token expires after ten minutes and a database reset kills every one
		// of them, so "the cookie exists" is not "the session works". Finding out
		// is one call, and the alternative is a dashboard of refusals.
		if h.fill(r, s, &d) {
			d.SignedIn = true
		} else {
			h.clearSession(w)
			d.Flash = "that session has expired — sign in again"
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tpl.ExecuteTemplate(w, "page", d); err != nil {
		// The response has already begun, so there is nothing to do but say so
		// where somebody will see it.
		fmt.Fprintf(w, "\n<!-- template error: %v -->", err)
	}
}

// fillPicker gathers what somebody with no tenant can see.
//
// Two calls, and neither can reach application data: the credential resolves to a
// person and not to a tenant, so there is nothing for a query to be scoped by. It
// reports whether the credential is still alive.
func (h *Handler) fillPicker(r *http.Request, token string, d *data) bool {
	status, out := h.call(r, http.MethodGet, "/auth/me/invitations", token, "")
	if status == http.StatusUnauthorized {
		return false
	}
	if status == http.StatusOK {
		var body struct{ Data []myInviteView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.MyInvitations = body.Data
		}
	} else {
		d.Refused["myInvitations"] = message(status, out)
	}

	// Empty for somebody who just registered, and not for somebody who signed out
	// of a tenant they still belong to.
	if status, out := h.call(r, http.MethodGet, "/auth/me/tenants", token, ""); status == http.StatusOK {
		var body struct{ Data []tenantView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.Tenants = body.Data
		}
	} else {
		d.Refused["myTenants"] = message(status, out)
	}
	return true
}

// noteScope is what the panel asks the API for.
//
// Empty becomes "own" so the URL and the request agree — the page has to know
// which of the two links to mark as current, and reading that back off "whatever
// the API defaulted to" would be guessing. Anything else is passed through
// untouched: a nonsense value is a 400 worth seeing in the transcript rather than
// something to correct on the way in.
func noteScope(raw string) string {
	if raw == "" {
		return "own"
	}
	return raw
}

// fill gathers the panels for a signed-in caller.
//
// It reports whether the session is alive. The tenant list is the probe: it
// needs nothing but a valid token, so a 401 there means the credential is gone
// and every panel after it would be a refusal.
func (h *Handler) fill(r *http.Request, s sessionCookie, d *data) bool {
	d.Me.TenantID = s.TenantID
	d.Me.ExpiresAt = s.Expires

	// Who am I. There is no /auth/me — a session's claims are what every other
	// endpoint already acts on — so the tenant list is what names the account,
	// and the people list fills in the rest.
	status, out := h.call(r, http.MethodGet, "/auth/tenants", s.Access, "")
	if status == http.StatusUnauthorized {
		return false
	}
	if status == http.StatusOK {
		var body struct{ Data []tenantView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.Tenants = body.Data
			for _, ws := range body.Data {
				if ws.Current {
					d.Me.AccountID = ws.AccountID
					d.Me.Tenant = ws.TenantName
					d.Me.Role = ws.Role
				}
			}
		}
	}

	if status, out := h.call(r, http.MethodGet, "/auth/sessions", s.Access, ""); status == http.StatusOK {
		var body struct{ Data []sessionView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.Sessions = body.Data
		}
	} else {
		d.Refused["sessions"] = message(status, out)
	}

	if status, out := h.call(r, http.MethodGet, "/auth/api-keys", s.Access, ""); status == http.StatusOK {
		var body struct{ Data []keyView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.Keys = body.Data
		}
	} else {
		d.Refused["keys"] = message(status, out)
	}

	if status, out := h.call(r, http.MethodGet, "/auth/invitations", s.Access, ""); status == http.StatusOK {
		var body struct{ Data []inviteView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.Invitations = body.Data
		}
	} else {
		d.Refused["invitations"] = message(status, out)
	}

	// The one panel whose width the caller chooses. `?scope=all` is refused
	// without note.read.all, which is exactly what a demonstration should show:
	// the refusal is the answer, not a shorter list.
	if status, out := h.call(r, http.MethodGet, "/api/v1/notes?scope="+d.NoteScope, s.Access, ""); status == http.StatusOK {
		var body struct{ Data []noteView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.Notes = body.Data
		}
	} else {
		d.Refused["notes"] = message(status, out)
	}

	// The one read with no HTTP surface. rig_account is the foundation's table and
	// rig generates nothing for it: `auth.expose: [account]` in rig.yaml would
	// give it a REST resource with filters and paging, and this is the other
	// answer — a query, in the application, for a page that needs one.
	d.People = h.people(r.Context(), s.TenantID)

	// The trail, through the endpoint rig serves for it — which is why this page
	// is a regression test and not only a demonstration: the endpoint either
	// answers the question the query it replaced was written to answer, or this
	// stops working. `?scope=all` needs authlog.read.all, and the refusal is
	// shown rather than swallowed, the way the notes list shows its own.
	if status, out := h.call(r, http.MethodGet, "/auth/audit?scope=all&limit=40", s.Access, ""); status == http.StatusOK {
		var body struct{ Data []logView }
		if err := json.Unmarshal(out, &body); err == nil {
			d.Log = body.Data
		}
	} else {
		d.Refused["log"] = message(status, out)
	}

	for _, p := range d.People {
		if p.AccountID == d.Me.AccountID {
			d.Me.Email, d.Me.DisplayName, d.Me.Verified = p.Email, p.Name, p.Verified
		}
	}

	d.Me.Roles, d.Me.Permissions = h.grants(r.Context(), s.TenantID, d.Me.AccountID)
	for _, p := range d.Me.Permissions {
		switch p {
		case authhttp.PermissionOwnAPIKey:
			d.Me.CanOwnKeys = true
		case authhttp.PermissionManageAPIKeys:
			// Administering keys includes making your own, so the narrow form is
			// offered to somebody holding the wide permission too.
			d.Me.CanOwnKeys, d.Me.CanManageKeys = true, true
		}
	}
	h.attribute(d)
	return true
}

// attribute turns the identifiers on a note into names.
func (h *Handler) attribute(d *data) {
	people := map[uuid.UUID]string{}
	for _, p := range d.People {
		people[p.AccountID] = p.Name
	}
	keys := map[uuid.UUID]string{}
	for _, k := range d.Keys {
		keys[k.ID] = k.Name
	}

	for i := range d.Notes {
		n := &d.Notes[i]
		if n.CreatedByAccount != nil {
			if name, ok := people[*n.CreatedByAccount]; ok {
				n.ByWhom = name
			} else {
				n.ByWhom = n.CreatedByAccount.String()[:8]
			}
		}
		if n.CreatedByAPIKeyID != nil {
			switch name, ok := keys[*n.CreatedByAPIKeyID]; {
			case ok:
				n.ByWhat = name
			case d.Refused["keys"] != "":
				// The row records which key, and this caller may not read the
				// keys — so the honest answer is that there was one, not an
				// identifier that looks like a bug.
				n.ByWhat = "a key you cannot see"
			default:
				n.ByWhat = n.CreatedByAPIKeyID.String()[:8]
			}
		}
	}
}

// people lists the accounts in one tenant.
//
// Scoped by tenant_id in the WHERE clause, by hand. That is the difference worth
// noticing between this and a generated repository: there, the predicate is added
// for you and cannot be forgotten. Here it is one line, and forgetting it would
// list every customer's staff.
func (h *Handler) people(ctx context.Context, tenantID uuid.UUID) []personView {
	rows, err := h.pool.Query(ctx, `
		SELECT rig_account.id, rig_account.email_address, rig_account.display_name,
		       rig_account.role, rig_account.kind, rig_account.created_at,
		       rig_identity.email_verified_at IS NOT NULL
		  FROM rig_account
		  LEFT JOIN rig_identity ON rig_identity.id = rig_account.identity_id
		 WHERE rig_account.tenant_id = $1 AND rig_account.deleted_at IS NULL
		 ORDER BY rig_account.created_at`, tenantID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []personView
	for rows.Next() {
		var p personView
		if err := rows.Scan(&p.AccountID, &p.Email, &p.Name, &p.Role, &p.Kind,
			&p.JoinedAt, &p.Verified); err != nil {
			return out
		}
		out = append(out, p)
	}
	return out
}

// grants reads what the caller may do, the same way services/tenant does
// when it answers auth.Config.Grants.
func (h *Handler) grants(ctx context.Context, tenantID, accountID uuid.UUID) (roles, permissions []string) {
	if accountID == uuid.Nil {
		return nil, nil
	}
	err := h.pool.QueryRow(ctx, `
		SELECT coalesce(array_agg(DISTINCT role.key) FILTER (WHERE role.key IS NOT NULL), '{}'),
		       coalesce(array_agg(DISTINCT permission.key) FILTER (WHERE permission.key IS NOT NULL), '{}')
		  FROM account_role
		  JOIN role ON role.id = account_role.role_id AND role.tenant_id = $1
		  LEFT JOIN role_permission ON role_permission.role_id = role.id
		  LEFT JOIN permission ON permission.id = role_permission.permission_id
		 WHERE account_role.account_id = $2`, tenantID, accountID).Scan(&roles, &permissions)
	if err != nil {
		return nil, nil
	}
	return roles, permissions
}
