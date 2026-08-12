package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/examples/auth_oauth/internal/api"
)

// pages is the whole interface: one page signed out, one page signed in.
//
// Deliberately thin. examples/auth has the dashboard — sessions, keys, invitations,
// a request transcript — and building a second one here would bury the one thing
// this example is about.
type pages struct {
	pool *pgxpool.Pool
	// api is this application's own mux. The signed-in page reads its bookmarks
	// through it, with a real Authorization header, rather than querying the
	// database beside it: what is on the screen is what the API says.
	api       http.Handler
	providers []string
	// demo is whether the provider on offer is the stand-in this example serves
	// itself, which is worth saying on the page: the sign-in is real, and the thing
	// answering it is not.
	demo bool

	tpl *template.Template
}

const cookieName = "rig_oauth_example"

func (p *pages) Mount(mux *http.ServeMux) {
	p.tpl = template.Must(template.New("page").Funcs(template.FuncMap{
		"lower": strings.ToLower,
	}).Parse(pageHTML))

	mux.HandleFunc("GET /{$}", p.show)
	mux.HandleFunc("POST /sign-out", p.signOut)
	mux.HandleFunc("POST /bookmarks", p.addBookmark)
}

// view is what the page renders from.
type view struct {
	Flash string
	// Tenant is which tenant this host is, and Host is the host it came from —
	// shown together because seeing them side by side is the demonstration.
	Tenant string
	Host   string

	Providers []string
	// Demo is whether Providers is the stand-in.
	Demo bool

	SignedIn  bool
	Who       string
	Bookmarks []bookmarkView

	// OtherHost is the same page at the other tenant, so switching is a link.
	OtherHost string

	Refused string
}

type bookmarkView struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
}

func (p *pages) show(w http.ResponseWriter, r *http.Request) {
	v := view{
		Flash:     r.URL.Query().Get("flash"),
		Host:      r.Host,
		Providers: p.providers,
		Demo:      p.demo,
		OtherHost: otherHost(r.Host),
	}

	// The tenant this host is. A page that cannot name it says so, which is what a
	// typo in a subdomain looks like.
	if name, ok := p.tenantName(r); ok {
		v.Tenant = name
	} else if v.OtherHost != "" {
		v.Refused = "no tenant is served at this host — try " + v.OtherHost
	} else {
		v.Refused = "no tenant is served at this host"
	}

	if token, ok := currentSession(r); ok {
		// The API answers 401 once the access token expires, which is ten minutes.
		// A page that rendered a signed-in shell around a refusal would be lying.
		status, body := p.call(r, http.MethodGet, "/api/v1/bookmarks", token, "")
		switch {
		case status == http.StatusOK:
			v.SignedIn = true
			var page struct{ Data []bookmarkView }
			if err := json.Unmarshal(body, &page); err == nil {
				v.Bookmarks = page.Data
			}
			v.Who = p.whoami(r, token)
		case status == http.StatusUnauthorized:
			clearSession(w)
			v.Flash = "that session has expired — sign in again"
		default:
			v.SignedIn = true
			v.Refused = string(body)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = p.tpl.Execute(w, v)
}

// whoami names the caller, from the tenant list every session can read.
//
// There is no /auth/me — a session's claims are what every other endpoint already
// acts on — so the tenant list is what names the account.
func (p *pages) whoami(r *http.Request, token string) string {
	status, body := p.call(r, http.MethodGet, "/auth/tenants", token, "")
	if status != http.StatusOK {
		return ""
	}
	var out struct {
		Data []struct {
			TenantName string `json:"tenantName"`
			Role       string `json:"role"`
			Current    bool   `json:"current"`
		}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	for _, t := range out.Data {
		if t.Current {
			return t.Role + " of " + t.TenantName
		}
	}
	return ""
}

func (p *pages) addBookmark(w http.ResponseWriter, r *http.Request) {
	token, ok := currentSession(r)
	if !ok {
		http.Redirect(w, r, "/?flash="+flash("sign in first"), http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/?flash="+flash("could not read the form"), http.StatusFound)
		return
	}

	body, _ := json.Marshal(map[string]any{
		"title": r.FormValue("title"),
		"url":   r.FormValue("url"),
	})
	status, out := p.call(r, http.MethodPost, "/api/v1/bookmarks", token, string(body))

	note := "saved — and it belongs to this tenant only"
	if status != http.StatusCreated {
		note = string(out)
	}
	http.Redirect(w, r, "/?flash="+flash(note), http.StatusFound)
}

func (p *pages) signOut(w http.ResponseWriter, r *http.Request) {
	if token, ok := currentSession(r); ok {
		p.call(r, http.MethodPost, "/auth/logout", token, "")
	}
	clearSession(w)
	http.Redirect(w, r, "/?flash="+flash("signed out"), http.StatusFound)
}

// call makes a request to this application's own API, in process.
//
// The host is preserved, because the host is what names the tenant — an in-process
// call that dropped it would reach a different tenant than the page it came from,
// which is the one mistake this example could make that would look like it worked.
func (p *pages) call(r *http.Request, method, path, token, body string) (int, []byte) {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}

	req := httptest.NewRequest(method, "http://"+r.Host+path, reader)
	req.Host = r.Host
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	p.api.ServeHTTP(rec, req.WithContext(r.Context()))
	return rec.Code, rec.Body.Bytes()
}

func (p *pages) tenantName(r *http.Request) (string, bool) {
	slug := api.TenantSlug(r.Host)
	if slug == "" {
		return "", false
	}

	var name string
	if err := p.pool.QueryRow(context.Background(),
		`SELECT name FROM rig_tenant WHERE lower(slug) = $1 AND deleted_at IS NULL AND is_active`,
		slug).Scan(&name); err != nil {
		return "", false
	}
	return name, true
}

// otherHost is the same page at the other seeded tenant, so switching is a link and
// the point lands without anybody editing a URL.
//
// Empty when this host has no tenant label to swap — a single-host run, where there
// is nowhere else to go.
func otherHost(host string) string {
	name, port, hasPort := strings.Cut(host, ":")
	slug, rest, found := strings.Cut(name, ".")
	if !found || rest == "" || slug == "" || net.ParseIP(name) != nil {
		return ""
	}

	next := acmeSlug
	if strings.EqualFold(slug, acmeSlug) {
		next = betaSlug
	}
	out := next + "." + rest
	if hasPort {
		out += ":" + port
	}
	return out
}

// The cookie. HttpOnly because a token a script can read is a token an injected
// script can steal; not Secure because this runs on plain HTTP against localhost,
// where that flag is not optional anywhere real.
func setSession(w http.ResponseWriter, tenantID uuid.UUID, pair session.Pair) {
	raw, _ := json.Marshal(sessionPair{Access: pair.Access.Token, TenantID: tenantID})
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((12 * time.Hour).Seconds()),
	})
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

// currentSession reads the access token out of the cookie.
func currentSession(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return "", false
	}
	var p sessionPair
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false
	}
	return p.Access, p.Access != ""
}

// sessionPair is the cookie's payload. Only the access token is used here —
// refreshing is examples/auth's subject.
type sessionPair struct {
	Access   string    `json:"a"`
	TenantID uuid.UUID `json:"t"`
}

// flash escapes a message for the redirect that carries it.
func flash(s string) string { return url.QueryEscape(s) }
