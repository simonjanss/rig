package authhttp_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/handoff"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/rigerr"
)

const frontEnd = "https://app.example.com"

// browser is the other ending, called the way oauth's callback calls it.
func (f *fixture) browser(t *testing.T, in oauth.SignIn) (*httptest.ResponseRecorder, error) {
	t.Helper()

	to, err := handoff.New(handoff.Config{
		Origin: frontEnd, APIHost: "https://api.example.com",
	})
	if err != nil {
		t.Fatalf("handoff.New: %v", err)
	}

	if in.Link == nil {
		in.Link = &oauth.Link{ID: uuid.New(), IdentityID: f.identity.ID, Provider: "Google"}
	}
	if in.Provider == "" {
		in.Provider = "Google"
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/callback", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0")
	return w, f.handler.SignInToBrowser(to)(w, r, in)
}

// takeHandoff reads the cookie the way the landing page will.
func takeHandoff(t *testing.T, w *httptest.ResponseRecorder) authwire.Handoff {
	t.Helper()

	cookies := (&http.Response{Header: w.Header()}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies, want 1", len(cookies))
	}
	if got := cookies[0].Name; got != authwire.HandoffCookie {
		t.Fatalf("cookie %q, want %q", got, authwire.HandoffCookie)
	}

	body, err := base64.RawURLEncoding.DecodeString(cookies[0].Value)
	if err != nil {
		t.Fatalf("the cookie is not base64url: %v", err)
	}
	var out authwire.Handoff
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func location(t *testing.T, w *httptest.ResponseRecorder) *url.URL {
	t.Helper()

	if got, want := w.Code, http.StatusSeeOther; got != want {
		t.Fatalf("status %d, want %d: %s", got, want, w.Body)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location %q: %v", w.Header().Get("Location"), err)
	}
	return u
}

// The same sign-in the JSON ending answers with a body, answered with a cookie
// and a redirect instead — and carrying the same session.
func TestTheBrowserEndingLeavesTheTokensInACookie(t *testing.T) {
	t.Parallel()

	f := setup(t)
	w, err := f.browser(t, oauth.SignIn{TenantID: f.tenant, AccountID: f.account.ID})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := location(t, w).String(), frontEnd+"/auth/callback"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	out := takeHandoff(t, w)
	switch {
	case out.AccessToken == "":
		t.Error("no access token")
	case out.RefreshToken == "":
		t.Error("no refresh token")
	case out.SessionID == uuid.Nil:
		t.Error("no session id")
	case out.IdentityToken == "":
		t.Error("no identity token, so the picker has nothing to run on")
	}
	// http.Redirect writes its own small anchor body for a GET, and the tokens
	// must not be anywhere in it: a body is not where a credential belongs, and
	// this one is a page the browser puts in its history.
	if strings.Contains(w.Body.String(), out.AccessToken) {
		t.Errorf("the access token is in the body as well: %s", w.Body)
	}
}

// The tenant list is the one thing this ending drops, and it drops it because a
// cookie has a size limit a tenant list does not.
func TestTheHandoffCarriesNoTenantList(t *testing.T) {
	t.Parallel()

	f := setup(t)
	w, err := f.browser(t, oauth.SignIn{TenantID: f.tenant, AccountID: f.account.ID})
	if err != nil {
		t.Fatal(err)
	}

	cookies := (&http.Response{Header: w.Header()}).Cookies()
	body, err := base64.RawURLEncoding.DecodeString(cookies[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["tenants"]; ok {
		t.Errorf("the handoff carries a tenant list: %s", body)
	}
}

// Somebody who belongs nowhere yet reaches the front end too, with the one
// token that works before there is a tenant. This is the flow #136 made
// reachable from a provider, and the ending must not lose it.
func TestTheBrowserEndingHandsOverAnIdentityWithNoTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	// The same way the JSON ending's test reaches this state: the one account
	// they had is not active, so they belong nowhere.
	f.account.IsActive = false
	f.store.Put(f.account)

	w, err := f.browser(t, oauth.SignIn{})
	if err != nil {
		t.Fatal(err)
	}

	out := takeHandoff(t, w)
	if out.IdentityToken == "" {
		t.Error("no identity token")
	}
	if out.AccessToken != "" || out.SessionID != uuid.Nil {
		t.Error("a session for somebody who belongs to no tenant")
	}
}

// A relative returnTo resolves against the front end, which is the bug this
// ending exists to fix: oauth lets a bare path through before the allow-list,
// and that path is a route on the application rather than on this API.
func TestTheBrowserEndingResolvesReturnToAgainstTheFrontEnd(t *testing.T) {
	t.Parallel()

	f := setup(t)
	w, err := f.browser(t, oauth.SignIn{
		TenantID: f.tenant, AccountID: f.account.ID,
		ReturnTo: "/auth/callback?redirect=%2Fprojects%2F7",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := location(t, w)
	if want := frontEnd + "/auth/callback?redirect=%2Fprojects%2F7"; got.String() != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	// And the cookie is scoped to the page that will read it rather than to the
	// whole front end.
	cookies := (&http.Response{Header: w.Header()}).Cookies()
	if cookies[0].Path != "/auth/callback" {
		t.Errorf("cookie Path = %q, want the destination's", cookies[0].Path)
	}
}

// A refusal is returned rather than redirected, so that oauth's own failure
// path — which writes the audit entry before it renders anything — is the one
// that runs. Nothing is written here.
func TestTheBrowserEndingReturnsARefusalRatherThanRedirecting(t *testing.T) {
	t.Parallel()

	f := setup(t)
	w, err := f.browser(t, oauth.SignIn{TenantID: uuid.New(), AccountID: uuid.New()})
	if err == nil {
		t.Fatal("a tenant they are not in was accepted")
	}
	if got, want := rigerr.CodeOf(err), rigerr.CodeForbidden; got != want {
		t.Errorf("code %v, want %v", got, want)
	}
	if set := w.Header().Values("Set-Cookie"); len(set) != 0 {
		t.Errorf("a refused sign-in wrote %v", set)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("a refused sign-in wrote a response: %d %s", w.Code, w.Body)
	}
}

// The regression a hand-written ending could not avoid: the address recorded
// against a sign-in comes from the handler's own TrustedProxies, which a hook
// has no way to reach. Both endings go through one constructor, so both get it.
func TestTheBrowserEndingRecordsTheClientAddressBehindAProxy(t *testing.T) {
	t.Parallel()

	proxy := netip.MustParsePrefix("10.0.0.0/8")
	f := setup(t, func(c *authhttp.Config) { c.TrustedProxies = []netip.Prefix{proxy} })

	to, err := handoff.New(handoff.Config{Origin: frontEnd, APIHost: "api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/callback", nil)
	r.RemoteAddr = "10.1.2.3:9999"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")

	w := httptest.NewRecorder()
	in := oauth.SignIn{
		Link:     &oauth.Link{ID: uuid.New(), IdentityID: f.identity.ID, Provider: "Google"},
		Provider: "Google", TenantID: f.tenant, AccountID: f.account.ID,
	}
	if err := f.handler.SignInToBrowser(to)(w, r, in); err != nil {
		t.Fatal(err)
	}

	entry, ok := f.log.last(authlog.EventLoginSucceeded)
	if !ok {
		t.Fatal("nothing recorded")
	}
	if got, want := entry.IPAddress, "203.0.113.7"; got != want {
		t.Errorf("recorded %s, want %s: the proxy's address rather than the "+
			"client's is what a hand-written ending had no way to avoid", got, want)
	}
}
