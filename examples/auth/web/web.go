// Package web is a server-rendered UI over this example's own HTTP API.
//
// It exists because authentication is hard to appreciate from a curl transcript.
// Sessions, API keys, invitations and an audit trail are all about who did what
// and with which credential, and that is easier to see in a page you can click
// than in a shell history.
//
// The one design decision worth knowing: **every button here makes a real HTTP
// request to this application's own API**, with a real Authorization header, and
// the request and response are shown in the panel on the right. Nothing in this
// package reaches past the API into a service or the database — with two
// deliberate exceptions, both marked below, for reading the auth log and the
// people in a tenant, which have no HTTP surface because rig generates nothing
// for the foundation's tables.
//
// That is what makes the interface trustworthy as a demonstration: if a panel
// shows something, an API did it, and the curl beside it is the request that
// would do the same thing from a terminal.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/auth/services/outbox"
)

//go:embed templates/*.gohtml
var files embed.FS

// cookieName holds the session pair.
//
// A browser needs a cookie and the API issues bearer tokens, so something has to
// bridge them. rig deliberately does not: which of the two a client wants depends
// on whether it is a single-page application, a server-rendered one, or a mobile
// app catching a deep link, and only the application knows. This is that choice,
// made once, in fifteen lines.
const cookieName = "rig_auth_demo"

// Handler serves the UI.
type Handler struct {
	// api is the application's own mux — the generated routes and the auth
	// endpoints, exactly as a client on the network reaches them. Calls go
	// through it rather than around it so that this interface cannot demonstrate
	// something the API cannot do.
	api http.Handler
	// pool is for the two reads with no HTTP surface: the auth log and the people
	// in a tenant. Both are the foundation's tables, which rig generates nothing
	// for — `auth.expose` would give them a REST resource, and plain SQL is the
	// other answer.
	pool *pgxpool.Pool

	mail *outbox.Box
	tpl  *template.Template

	// trace is the request log the curl panel shows.
	trace *tracer
	// lastKey holds a freshly minted secret for exactly one render.
	lastKey keyHolder
}

// New builds the UI.
func New(api http.Handler, pool *pgxpool.Pool, mail *outbox.Box) (*Handler, error) {
	tpl, err := template.New("").Funcs(helpers()).ParseFS(files, "templates/*.gohtml")
	if err != nil {
		return nil, err
	}
	return &Handler{
		api: api, pool: pool, mail: mail, tpl: tpl,
		trace: &tracer{limit: 40},
	}, nil
}

// Mount registers the routes.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui", h.page)
	mux.HandleFunc("GET /ui/", h.page)

	mux.HandleFunc("POST /ui/register", h.register)
	mux.HandleFunc("POST /ui/signup", h.signUp)
	// The picker's two exits: join a tenant you were invited to, or make one.
	mux.HandleFunc("POST /ui/join", h.join)
	mux.HandleFunc("POST /ui/tenants", h.createTenant)
	mux.HandleFunc("POST /ui/login", h.login)
	mux.HandleFunc("POST /ui/logout", h.logout)
	mux.HandleFunc("POST /ui/switch", h.switchTenant)
	mux.HandleFunc("POST /ui/refresh", h.refresh)

	mux.HandleFunc("POST /ui/invite", h.invite)
	mux.HandleFunc("POST /ui/accept", h.accept)
	mux.HandleFunc("POST /ui/invite/revoke", h.revokeInvite)

	mux.HandleFunc("POST /ui/notes", h.createNote)
	mux.HandleFunc("POST /ui/keys", h.createKey)
	mux.HandleFunc("POST /ui/keys/revoke", h.revokeKey)

	mux.HandleFunc("POST /ui/trace/clear", h.clearTrace)
}

// call makes a request to the application's own API.
//
// In process rather than over a socket — same mux, same handlers, same claims —
// so the UI needs no second port and no client. Every call is recorded for the
// curl panel, including the failures, because a 403 is the most interesting thing
// an authentication demonstration can show.
func (h *Handler) call(r *http.Request, method, path, token, body string) (int, []byte) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if tenant := tenantOf(r); tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}

	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req.WithContext(r.Context()))

	res := rec.Result()
	out := rec.Body.Bytes()
	h.trace.add(entry{
		Method: method, Path: path, Status: res.StatusCode,
		Body: redact(body), Response: redactSecrets(string(out)),
		Credential: describe(token), Tenant: tenantOf(r),
		At: time.Now().UTC(),
	})
	return res.StatusCode, out
}

// pair is the session a sign-in returns.
type pair struct {
	AccessToken      string    `json:"accessToken"`
	RefreshToken     string    `json:"refreshToken"`
	ExpiresAt        time.Time `json:"expiresAt"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
	SessionID        uuid.UUID `json:"sessionId"`

	// IdentityToken proves who somebody is and names no tenant. It comes back
	// from every sign-in, and it is the only credential there is when somebody
	// belongs to no tenant yet.
	IdentityToken string `json:"identityToken"`
	// Tenants is where they could go, so the picker needs no second call.
	Tenants []tenantView `json:"tenants"`
}

// session is what the cookie holds.
type sessionCookie struct {
	Access   string    `json:"a"`
	Refresh  string    `json:"r"`
	TenantID uuid.UUID `json:"t"`
	Expires  time.Time `json:"e"`

	// Identity is the tenant-less credential. It is kept alongside a tenant
	// session rather than instead of it: switching tenant is one click away and
	// the picker's endpoints are what answer "where else could I go".
	//
	// Access empty and this set is the picker: signed in, nowhere to be.
	Identity string `json:"i"`
}

func (h *Handler) setSession(w http.ResponseWriter, p pair, tenantID uuid.UUID) {
	setCookie(w, sessionCookie{
		Access: p.AccessToken, Refresh: p.RefreshToken,
		TenantID: tenantID, Expires: p.ExpiresAt,
		Identity: p.IdentityToken,
	})
}

// setCookie writes the session cookie.
func setCookie(w http.ResponseWriter, s sessionCookie) {
	raw, _ := json.Marshal(s)
	http.SetCookie(w, &http.Cookie{
		Name:  cookieName,
		Value: base64(raw),
		Path:  "/",
		// HttpOnly because a token a script can read is a token an injected
		// script can steal. Not Secure, because this example runs on plain HTTP
		// against localhost; in production that flag is not optional.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((12 * time.Hour).Seconds()),
	})
}

func (h *Handler) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

func (h *Handler) session(r *http.Request) (sessionCookie, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return sessionCookie{}, false
	}
	raw, err := unbase64(c.Value)
	if err != nil {
		return sessionCookie{}, false
	}
	var s sessionCookie
	if err := json.Unmarshal(raw, &s); err != nil {
		return sessionCookie{}, false
	}
	return s, s.Access != ""
}

// picker is the credential somebody has before they belong anywhere.
//
// A second reader rather than a mode of session, matching the split in the auth
// package itself: one answers "who is this, here" and the other only "who is
// this", and a function that could return either would eventually be used where
// only one of them is safe.
func (h *Handler) picker(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	raw, err := unbase64(c.Value)
	if err != nil {
		return "", false
	}
	var s sessionCookie
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s.Identity, s.Identity != ""
}

// tenantOf is the tenant header every call carries.
//
// The interface knows it from the cookie. A real deployment would take it from
// the subdomain, which is why the auth package asks the application rather than
// guessing.
func tenantOf(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	raw, err := unbase64(c.Value)
	if err != nil {
		return ""
	}
	var s sessionCookie
	if err := json.Unmarshal(raw, &s); err != nil || s.TenantID == uuid.Nil {
		return ""
	}
	return s.TenantID.String()
}

// describe names the credential a request used, for the trace.
func describe(token string) string {
	switch {
	case token == "":
		return "none"
	case strings.HasPrefix(token, "rig_at_"):
		return "session"
	case strings.HasPrefix(token, "rig_sk_"):
		return "api key"
	default:
		return "unknown"
	}
}

// entry is one recorded request.
type entry struct {
	Method     string
	Path       string
	Status     int
	Body       string
	Response   string
	Credential string
	Tenant     string
	At         time.Time
}

// Curl renders the request as a command somebody can paste.
//
// The token is abbreviated for the same reason the response is: a transcript on a
// screen is a transcript in a screenshot.
func (e entry) Curl(base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "curl -i -X %s %s%s", e.Method, base, e.Path)
	switch e.Credential {
	case "session":
		b.WriteString(" \\\n  -H 'Authorization: Bearer rig_at_…'")
	case "api key":
		b.WriteString(" \\\n  -H 'Authorization: Bearer rig_sk_…'")
	}
	if e.Tenant != "" {
		fmt.Fprintf(&b, " \\\n  -H 'X-Tenant-Id: %s'", e.Tenant)
	}
	if e.Body != "" {
		fmt.Fprintf(&b, " \\\n  -d '%s'", redact(e.Body))
	}
	return b.String()
}

// redact keeps a password out of a transcript.
func redact(body string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body
	}
	for _, key := range []string{"password", "newPassword", "currentPassword"} {
		if _, ok := m[key]; ok {
			m[key] = "…"
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(out)
}

// secretPattern matches a minted key.
var secretPattern = regexp.MustCompile(`rig_(sk|at|rt)_[A-Z0-9]+[._][A-Z0-9]+`)

// redactSecrets keeps a live credential out of the transcript.
//
// The transcript is the most useful panel in this interface and it was also the
// one leaking: minting a key answers with the secret, and a panel that keeps
// response bodies keeps that one too — on the screen, in a screenshot, for as
// long as the page is open. The secret is shown once, deliberately, in one place
// that says to copy it now; everywhere else it is stars.
func redactSecrets(body string) string {
	return secretPattern.ReplaceAllStringFunc(body, func(m string) string {
		prefix, _, _ := strings.Cut(m, "_")
		kind, _, _ := strings.Cut(strings.TrimPrefix(m, prefix+"_"), "_")
		return prefix + "_" + kind + "_…"
	})
}

// Pretty is the response body, indented when it is JSON.
func (e entry) Pretty() string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(e.Response), "", "  "); err != nil {
		return e.Response
	}
	return buf.String()
}

// OK reports whether the status was a success, for styling.
func (e entry) OK() bool { return e.Status >= 200 && e.Status < 300 }

// tracer keeps the last few requests.
type tracer struct {
	mu      sync.Mutex
	entries []entry
	limit   int
}

func (t *tracer) add(e entry) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.entries = append([]entry{e}, t.entries...)
	if len(t.entries) > t.limit {
		t.entries = t.entries[:t.limit]
	}
}

func (t *tracer) all() []entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]entry, len(t.entries))
	copy(out, t.entries)
	return out
}

func (t *tracer) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = nil
}

func (h *Handler) clearTrace(w http.ResponseWriter, r *http.Request) {
	h.trace.clear()
	h.redirect(w, r, "")
}

// redirect sends the browser back to the page.
//
// Every action is a form post followed by a fresh render rather than a fragment
// swap. It costs a round trip and buys something worth more in a demonstration:
// what you see is always the state of the database, never a patch applied to a
// stale page.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, flash string) {
	target := "/ui"
	if flash != "" {
		target += "?flash=" + url.QueryEscape(flash)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// fail reports what went wrong, in the flash.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	h.redirect(w, r, message(status, body))
}

// message pulls the readable part out of an API error.
func message(status int, body []byte) string {
	var out struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &out)

	switch {
	case out.Message != "":
		return fmt.Sprintf("%d — %s", status, out.Message)
	case out.Error.Message != "":
		return fmt.Sprintf("%d — %s", status, out.Error.Message)
	default:
		return fmt.Sprintf("%d", status)
	}
}

func helpers() template.FuncMap {
	return template.FuncMap{
		"short": func(id uuid.UUID) string {
			s := id.String()
			return s[:8]
		},
		"shortstr": func(s string) string {
			if len(s) <= 8 {
				return s
			}
			return s[:8]
		},
		"ago": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return t.Format("2 Jan")
			}
		},
		"clock": func(t time.Time) string { return t.Format("15:04:05") },
		"initial": func(s string) string {
			if s == "" {
				return "?"
			}
			return s[:1]
		},
		"lower": strings.ToLower,
		"sorted": func(in []string) []string {
			out := append([]string(nil), in...)
			sort.Strings(out)
			return out
		},
	}
}
