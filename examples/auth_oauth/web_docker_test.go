//go:build docker

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Which tenant a request is for, decided by nothing but the host.
//
// Worth driving rather than eyeballing, because every interesting fact here is
// about which host something happened at, and a screenshot of one page cannot show
// that.
func TestTheHostDecidesTheTenant(t *testing.T) {
	ui := newBrowser(t)

	t.Run("each host names its own tenant", func(t *testing.T) {
		acme := ui.get(t, acmeHost, "/")
		if !strings.Contains(acme, "Acme") {
			t.Errorf("acme.localhost should be Acme:\n%s", excerpt(acme))
		}

		beta := ui.get(t, betaHost, "/")
		if !strings.Contains(beta, "Beta") {
			t.Errorf("beta.localhost should be Beta:\n%s", excerpt(beta))
		}

		// The same application, the same code, the same request — one label of the
		// host apart.
		if !strings.Contains(acme, "Continue with Demo") ||
			!strings.Contains(beta, "Continue with Demo") {
			t.Error("both hosts should offer the provider")
		}
	})

	t.Run("a host nobody is at says so", func(t *testing.T) {
		// A typo in a subdomain, which is what this looks like in practice. It is
		// answered rather than treated as no tenant at all, because a quiet
		// "nothing here" fails somewhere later and further away.
		page := ui.get(t, "nope.localhost", "/")
		if !strings.Contains(page, "No tenant") {
			t.Errorf("an unknown subdomain should refuse:\n%s", excerpt(page))
		}
		if strings.Contains(page, "Continue with Demo") {
			t.Error("there is nothing to sign in to at a host with no tenant")
		}
	})
}

// The provider sign-in itself, through the real flow.
//
// The stand-in provider is not a mock: it hands back a single-use authorization
// code and verifies the PKCE challenge before exchanging it, so what runs here is
// rig's actual OAuth path. What it adds is a consent screen that can lie, which is
// how both branches of the verified-address check are reachable.
//
// Two of the facts below would be invisible in manual use until the day they broke:
// a session established at one subdomain is not a session at its sibling, and a
// callback delivered to the wrong host is refused rather than quietly signing
// somebody in somewhere else.
func TestSigningInWithAProvider(t *testing.T) {
	t.Run("a stranger joins the tenant the host named", func(t *testing.T) {
		ui := newBrowser(t)

		stranger := "grace-" + uuid.NewString()[:8] + "@example.com"
		page := ui.signIn(t, betaHost, consent{
			subject: "subject-" + uuid.NewString()[:8],
			email:   stranger,
			name:    "Grace",
			// The provider vouches for the address.
			verified: true,
		})

		if !strings.Contains(page, "with Demo") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Sign out") {
			t.Fatalf("a provider sign-in should end signed in:\n%s", excerpt(page))
		}
		// Beta, because the sign-in started at beta.localhost. Nothing named the
		// tenant anywhere else: not a form field, not a query parameter.
		//
		// Read off the signed-in panel, which names the tenant the session is for.
		// The header names the tenant the host is, and would say Beta whether the
		// session belonged to it or not.
		if !strings.Contains(page, "of Beta") {
			t.Errorf("the session should be in Beta:\n%s", excerpt(page))
		}

		// And the account landed in Beta rather than anywhere else.
		var tenants []string
		rows, err := ui.pool.Query(context.Background(), `
			SELECT t.slug FROM rig_account a
			JOIN rig_tenant t ON t.id = a.tenant_id
			JOIN rig_identity i ON i.id = a.identity_id
			WHERE lower(i.email_address) = lower($1)`, stranger)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var slug string
			if err := rows.Scan(&slug); err != nil {
				t.Fatal(err)
			}
			tenants = append(tenants, slug)
		}
		rows.Close()
		if len(tenants) != 1 || tenants[0] != betaSlug {
			t.Errorf("accounts in %v, want exactly [%s]", tenants, betaSlug)
		}
	})

	t.Run("a session belongs to the host it was established at", func(t *testing.T) {
		ui := newBrowser(t)

		ui.signIn(t, acmeHost, consent{
			subject:  "subject-" + uuid.NewString()[:8],
			email:    "grace-" + uuid.NewString()[:8] + "@example.com",
			name:     "Grace",
			verified: true,
		})

		// The same browser, one label of the host away. The cookie is host-only, so
		// there is nothing to be signed in with — which is the property that makes
		// a subdomain per tenant a boundary rather than a decoration.
		page := ui.get(t, betaHost, "/")
		if strings.Contains(page, "Sign out") {
			t.Errorf("an Acme session is not a Beta session:\n%s", excerpt(page))
		}
	})

	t.Run("an unverified address will not link an existing account", func(t *testing.T) {
		ui := newBrowser(t)

		// The seed gave this address a password in Acme. Arriving through a
		// provider that will not vouch for it is the attack the check exists for:
		// without it, whoever registers your address anywhere owns your account
		// here.
		page := ui.signIn(t, acmeHost, consent{
			subject:  "impostor-" + uuid.NewString()[:8],
			email:    SeedEmail,
			name:     "Not Ada",
			verified: false,
		})
		if !strings.Contains(page, "has not verified") {
			t.Fatalf("an unverified address should be refused:\n%s", excerpt(page))
		}

		// The same address with the provider vouching does link — one person, two
		// ways in, and the account they already had.
		page = ui.signIn(t, acmeHost, consent{
			subject:  "ada-subject-" + uuid.NewString()[:8],
			email:    SeedEmail,
			name:     "Ada",
			verified: true,
		})
		if !strings.Contains(page, "Sign out") {
			t.Fatalf("a verified address should link:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Owner of Acme") {
			t.Errorf("it should be the account they already had:\n%s", excerpt(page))
		}
	})

	// The one that would be invisible until it mattered.
	//
	// The state cookie is set on the host the sign-in started at, so a callback
	// delivered to a sibling arrives without it. That has to be a refusal: the
	// alternative is a code minted for one tenant being spent at another.
	t.Run("a callback delivered to the wrong host is refused", func(t *testing.T) {
		ui := newBrowser(t)

		back := ui.approve(t, acmeHost, consent{
			subject:  "subject-" + uuid.NewString()[:8],
			email:    "grace-" + uuid.NewString()[:8] + "@example.com",
			name:     "Grace",
			verified: true,
		})

		// The same code and state, aimed at the other tenant's callback.
		wrong := *back
		wrong.Host = betaHost
		res, body := ui.raw(t, http.MethodGet, wrong.String())
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400:\n%s", res.StatusCode, excerpt(body))
		}
		if !strings.Contains(body, "did not start here") {
			t.Errorf("unexpected refusal:\n%s", excerpt(body))
		}

		// And it did not sign anybody in at either host.
		for _, host := range []string{acmeHost, betaHost} {
			if page := ui.get(t, host, "/"); strings.Contains(page, "Sign out") {
				t.Errorf("%s should have nobody signed in:\n%s", host, excerpt(page))
			}
		}
	})

	// The shape a real provider forces, and the reason DEFAULT_TENANT exists.
	//
	// Google and Microsoft register a redirect URI exactly and neither takes plain
	// http for anything but localhost, so acme.localhost cannot be registered with
	// either. Running at one host with the tenant named by the environment is what
	// lets somebody try their own credentials on a laptop — and it has to reach the
	// same place the subdomain does, or the instructions in the README are a
	// different application.
	t.Run("a single host names its tenant from the environment", func(t *testing.T) {
		t.Setenv("DEFAULT_TENANT", acmeSlug)
		ui := newBrowserAt(t, "localhost")

		page := ui.get(t, "localhost", "/")
		if !strings.Contains(page, "Acme") {
			t.Fatalf("localhost should be Acme:\n%s", excerpt(page))
		}

		page = ui.signIn(t, "localhost", consent{
			subject:  "subject-" + uuid.NewString()[:8],
			email:    "grace-" + uuid.NewString()[:8] + "@example.com",
			name:     "Grace",
			verified: true,
		})
		if !strings.Contains(page, "Sign out") {
			t.Fatalf("a single-host sign-in should end signed in:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "of Acme") {
			t.Errorf("the session should be in Acme:\n%s", excerpt(page))
		}
	})
}

// A bookmark is the ordinary half: it proves the session is a session, and that
// what it reaches is scoped by the tenant the host named.
func TestWhatASessionCanRead(t *testing.T) {
	ui := newBrowser(t)

	// One person, signing in at both hosts with the same provider identity. Two
	// accounts, because belonging is per tenant; one identity, because who you are
	// is not.
	subject := "subject-" + uuid.NewString()[:8]
	address := "grace-" + uuid.NewString()[:8] + "@example.com"
	in := consent{subject: subject, email: address, name: "Grace", verified: true}

	ui.signIn(t, acmeHost, in)
	page := ui.post(t, acmeHost, "/bookmarks", url.Values{
		"title": {"The Go blog"}, "url": {"https://go.dev/blog"},
	})
	if !strings.Contains(saved(t, page), "The Go blog") {
		t.Fatalf("the bookmark should be listed:\n%s", excerpt(page))
	}

	ui.signIn(t, betaHost, in)
	page = ui.get(t, betaHost, "/")
	if !strings.Contains(page, "Sign out") {
		t.Fatalf("the same identity should reach Beta too:\n%s", excerpt(page))
	}
	if strings.Contains(saved(t, page), "The Go blog") {
		t.Errorf("Acme's bookmark is not Beta's:\n%s", excerpt(page))
	}
	if !strings.Contains(page, "None yet") {
		t.Errorf("Beta should be empty:\n%s", excerpt(page))
	}

	// One identity behind both accounts.
	var identities int
	if err := ui.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT a.identity_id) FROM rig_account a
		JOIN rig_identity i ON i.id = a.identity_id
		WHERE lower(i.email_address) = lower($1)`, address).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if identities != 1 {
		t.Errorf("%d identities, want 1 — signing in is global, belonging is not", identities)
	}
}

// The browser. One cookie jar, several hosts — which is the whole point, so the
// helpers all take the host they are talking to.
type browser struct {
	client *http.Client
	pool   *pgxpool.Pool
	// port is the ephemeral port every host answers on.
	port string
	// provider is the origin the stand-in serves itself at, which is the primary
	// one — a real provider is somebody else's server entirely.
	provider string
}

// The hosts. Slugs, plus the suffix that resolves to this machine.
const (
	acmeHost = acmeSlug + ".localhost"
	betaHost = betaSlug + ".localhost"
)

func newBrowser(t *testing.T) *browser { return newBrowserAt(t, acmeHost) }

// newBrowserAt serves the example at one primary host, which is the origin it will
// tell a provider to come back to — and, when it has no tenant label in it, the
// shape a reader with real Google credentials has to run.
func newBrowserAt(t *testing.T, primary string) *browser {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55443/rig?sslmode=disable&TimeZone=UTC"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	t.Cleanup(pool.Close)

	// The two tenants, and the person with a password. The same function the
	// server runs at startup, and idempotent, so a repeated run is free.
	if err := seed(ctx, pool); err != nil {
		t.Fatal(err)
	}

	// Unstarted first, because the application has to be told its own origin
	// before it serves anything: a provider registers redirect URIs, and an
	// ephemeral port is not knowable until the listener exists.
	srv := httptest.NewUnstartedServer(nil)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	handler, err := newAPI(ctx, pool, "http://"+primary+":"+port)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = handler
	srv.Start()
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	return &browser{
		port:     port,
		pool:     pool,
		provider: primary,
		client: &http.Client{
			Jar: jar,
			// Every host reaches this one listener.
			//
			// *.localhost resolves to 127.0.0.1 on macOS and on glibc, but a test
			// that depended on the resolver would be a test that fails on
			// somebody's machine for a reason that has nothing to do with rig.
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
				},
			},
		},
	}
}

func (b *browser) url(host, path string) string {
	return "http://" + host + ":" + b.port + path
}

func (b *browser) get(t *testing.T, host, path string) string {
	t.Helper()

	_, body := b.raw(t, http.MethodGet, b.url(host, path))
	return body
}

// post submits a form and follows the redirect, which is what a browser does.
func (b *browser) post(t *testing.T, host, path string, form url.Values) string {
	t.Helper()

	res, err := b.client.PostForm(b.url(host, path), form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return read(t, res)
}

func (b *browser) raw(t *testing.T, method, target string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res, read(t, res)
}

// consent is what the stand-in provider will say about somebody.
type consent struct {
	subject, email, name string
	verified             bool
}

// signIn drives the whole round trip the way a browser does: start at a host,
// consent at the provider, come back, and land wherever the application sent us.
//
// Nothing here names a tenant. That is the assertion — the host is the only thing
// that says which tenant this sign-in is for, at the start, and the state cookie
// carries it from there.
func (b *browser) signIn(t *testing.T, host string, in consent) string {
	t.Helper()

	back := b.approve(t, host, in)
	_, body := b.raw(t, http.MethodGet, back.String())
	return body
}

// approve stops one step short of signIn and hands back the callback URL the
// provider redirected to, so a test can deliver it somewhere else.
func (b *browser) approve(t *testing.T, host string, in consent) *url.URL {
	t.Helper()

	page := b.get(t, host, "/auth/oauth/demo/start?returnTo=/")

	form := url.Values{}
	for _, name := range []string{"redirect_uri", "state", "code_challenge"} {
		v, ok := hiddenValue(page, name)
		if !ok {
			t.Fatalf("no %s on the consent screen:\n%s", name, excerpt(page))
		}
		form.Set(name, v)
	}
	form.Set("subject", in.subject)
	form.Set("email", in.email)
	form.Set("name", in.name)
	if in.verified {
		form.Set("verified", "on")
	}

	// The provider serves itself at one origin, whichever host the sign-in
	// started at — a real one is somebody else's server entirely. The redirect URI
	// on the form is what carries the sign-in back to where it began.
	client := &http.Client{Jar: b.client.Jar, Transport: b.client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	res, err := client.PostForm(b.url(b.provider, "/idp/approve"), form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusFound {
		t.Fatalf("the provider answered %d, want a redirect:\n%s",
			res.StatusCode, excerpt(read(t, res)))
	}
	back, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// The port is the listener's; the host is whichever one started the sign-in.
	back.Host = strings.Split(back.Host, ":")[0] + ":" + b.port
	return back
}

var hiddenPattern = regexp.MustCompile(
	`<input type="hidden" name="([a-z_]+)" value="([^"]*)"`)

// hiddenValue reads one hidden input out of a form.
func hiddenValue(page, name string) (string, bool) {
	for _, m := range hiddenPattern.FindAllStringSubmatch(page, -1) {
		if m[1] == name {
			return m[2], true
		}
	}
	return "", false
}

// saved is the bookmark list, and nothing else on the page.
//
// Scoped deliberately: the form beside it carries a placeholder that is also a
// title, so asserting over the whole page would pass whether the list held
// anything or not — and would pass at the tenant that saved nothing.
func saved(t *testing.T, page string) string {
	t.Helper()

	i := strings.Index(page, "<ul>")
	if i < 0 {
		t.Fatalf("no bookmark list on the page:\n%s", excerpt(page))
	}
	rest := page[i:]
	j := strings.Index(rest, "</ul>")
	if j < 0 {
		t.Fatalf("unterminated bookmark list:\n%s", excerpt(page))
	}
	return rest[:j]
}

func read(t *testing.T, res *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

var (
	tagPattern   = regexp.MustCompile(`<[^>]*>`)
	blockPattern = regexp.MustCompile(`(?s)<(style|script)\b.*?</(style|script)>`)
)

// excerpt is for a failure message, so the stylesheet goes first: a page of CSS in
// a test log buries the sentence that would explain the failure.
func excerpt(page string) string {
	page = tagPattern.ReplaceAllString(blockPattern.ReplaceAllString(page, " "), " ")
	page = strings.Join(strings.Fields(page), " ")
	if len(page) > 400 {
		return page[:400] + "…"
	}
	return page
}
