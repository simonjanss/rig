//go:build docker

package main

import (
	"context"
	"io"
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

// The interface, driven the way somebody would click it.
//
// It is worth testing rather than eyeballing because the interface is the only
// place several of these flows are joined up: an invitation is minted in one
// panel and redeemed in another, and a tenant switch is only interesting if
// the second tenant shows different things. A screenshot proves none of that
// stayed true.
//
// It also covers the wiring the UI depends on and nothing else exercises — a
// dead cookie is not a signed-in caller, and a note written by a key says so.
func TestTheInterface(t *testing.T) {
	ui := newBrowser(t)

	var invitation string

	t.Run("a stranger is offered a way in", func(t *testing.T) {
		page := ui.get(t, "/ui")
		for _, want := range []string{"Create a tenant", "Sign in", "Accept an invitation"} {
			if !strings.Contains(page, want) {
				t.Errorf("the welcome page should offer %q", want)
			}
		}
		if strings.Contains(page, "Sign out") {
			t.Error("a stranger is not signed in")
		}
		// And the sign-in form does not ask which tenant, because the visitor
		// cannot know: nobody can say which tenants an address belongs to until
		// the password has been checked.
		//
		// Scoped to that form. Naming the tenant you are creating is a different
		// field on a different form, and asserting over the whole page would catch
		// it.
		form, ok := between(page, `action="/ui/login"`, "</form>")
		if !ok {
			t.Fatal("no sign-in form")
		}
		if strings.Contains(form, `name="tenant"`) {
			t.Errorf("the sign-in form should not ask for a tenant:\n%s", form)
		}
	})

	tenant := "Acme " + uuid.NewString()[:8]
	owner := "ada-" + uuid.NewString()[:8] + "@acme.test"

	t.Run("creating a tenant signs its owner in", func(t *testing.T) {
		page := ui.post(t, "/ui/signup", url.Values{
			"tenantName": {tenant},
			"name":       {"Ada"},
			"email":      {owner},
			"password":   {"a long enough password"},
		})

		if !strings.Contains(page, "tenant created") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		// Owner of the tenant they just made, and signed in — the tenant, the
		// identity, the account and the role all landed together.
		if !strings.Contains(page, tenant) || !strings.Contains(page, "Owner") {
			t.Error("the header should name the tenant and the role")
		}
		if !strings.Contains(page, "Sign out") {
			t.Error("creating a tenant should sign you in")
		}
	})

	t.Run("a note records the session that wrote it", func(t *testing.T) {
		page := ui.post(t, "/ui/notes", url.Values{
			"title": {"Written by a session"},
		})
		if !strings.Contains(page, "note written with your session") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "by Ada") {
			t.Error("the note should be attributed to the person who wrote it")
		}
	})

	var secret string

	t.Run("a key can be minted and shown once", func(t *testing.T) {
		page := ui.post(t, "/ui/keys", url.Values{
			"name":  {"Nightly import"},
			"kind":  {"Integration"},
			"scope": {"note.write"},
		})
		if !strings.Contains(page, "Copy this now") {
			t.Fatalf("the secret should be shown once:\n%s", excerpt(page))
		}

		secret = findSecret(page)
		if secret == "" {
			t.Fatal("no secret in the page")
		}

		// And only once. A second render must not repeat it, because only its
		// hash is stored and the interface holds it for exactly one page.
		if again := ui.get(t, "/ui"); strings.Contains(again, secret) {
			t.Error("the secret should not survive a second render")
		}
	})

	t.Run("a note written with the key names the key", func(t *testing.T) {
		page := ui.post(t, "/ui/notes", url.Values{
			"title": {"Written by an integration"},
			"key":   {secret},
		})
		if !strings.Contains(page, "note written with an API key") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		// Both halves of the audit trail: the account it acted as, and the
		// credential it came through.
		if !strings.Contains(page, "through Nightly import") {
			t.Errorf("the row should name the key:\n%s", excerpt(page))
		}
	})

	guest := "grace-" + uuid.NewString()[:8] + "@acme.test"

	t.Run("inviting somebody mints a link", func(t *testing.T) {
		page := ui.post(t, "/ui/invite", url.Values{
			"email": {guest},
			"name":  {"Grace"},
			"role":  {"Admin"},
		})
		if !strings.Contains(page, "invited") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		// Invited and not yet arrived: the account exists, the address is not
		// confirmed, and there is no password.
		if !strings.Contains(page, guest) {
			t.Error("the invited person should appear in the list")
		}

		invitation = findToken(page)
		if invitation == "" {
			t.Fatalf("no invitation in the outbox:\n%s", excerpt(page))
		}
	})

	t.Run("a pending invitation can be withdrawn", func(t *testing.T) {
		page := ui.get(t, "/ui")
		if !strings.Contains(page, "Pending invitations") {
			t.Fatal("the panel should be there")
		}

		// Withdrawn, then invited again — which only works because withdrawing
		// removes the account as well as killing the link.
		id := invitationID(t, ui.pool, guest)
		page = ui.post(t, "/ui/invite/revoke", url.Values{"id": {id}})
		if !strings.Contains(page, "withdrawn") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Nobody is waiting") {
			t.Errorf("the withdrawn invitation should be off the list:\n%s", excerpt(page))
		}

		page = ui.post(t, "/ui/invite", url.Values{
			"email": {guest}, "name": {"Grace"}, "role": {"Admin"},
		})
		if !strings.Contains(page, "invited") {
			t.Fatalf("re-inviting should work after a withdrawal:\n%s", excerpt(page))
		}
		invitation = findToken(page)
		if invitation == "" {
			t.Fatal("no fresh invitation in the outbox")
		}
	})

	t.Run("the trail records it", func(t *testing.T) {
		page := ui.get(t, "/ui")

		// The panel reads GET /auth/audit?scope=all, which needs
		// authlog.read.all — so this is also the assertion that the endpoint
		// answers the question the hand-written query it replaced was written to
		// answer, and that the Owner role holds the key for it. Without this the
		// page would quietly render "Refused" and every check below would still
		// pass on the words being elsewhere.
		panel, ok := between(page, "Auth log", "Outbox")
		if !ok {
			t.Fatalf("no auth log panel:\n%s", excerpt(page))
		}
		if strings.Contains(panel, "Refused:") {
			t.Errorf("the owner holds authlog.read.all and was refused:\n%s", panel)
		}

		// No LoginSucceeded yet, and that is right: creating a tenant is register
		// then create, and neither is a sign-in. The sub-tests below do sign in and
		// the event turns up there.
		for _, want := range []string{
			"AccountProvisioned", "InvitationSent", "InvitationRevoked",
		} {
			if !strings.Contains(panel, want) {
				t.Errorf("the auth log should show %q", want)
			}
		}
	})

	t.Run("accepting it joins that tenant", func(t *testing.T) {
		// A fresh browser: the invited person is not the person who invited them.
		guestUI := newBrowser(t)

		page := guestUI.post(t, "/ui/accept", url.Values{
			"token":    {invitation},
			"password": {"grace picked this one"},
		})
		if !strings.Contains(page, "joined") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Admin") {
			t.Error("the role from the invitation should be in force")
		}
		// The link is the proof the address works, so it arrives confirmed.
		if !strings.Contains(page, "verified") {
			t.Error("redeeming an invitation should confirm the address")
		}

		// And it is spent. A link forwarded to somebody else is not a second
		// invitation.
		again := newBrowser(t).post(t, "/ui/accept", url.Values{
			"token":    {invitation},
			"password": {"somebody else entirely"},
		})
		if !strings.Contains(again, "already been used") &&
			!strings.Contains(again, "not valid") {
			t.Errorf("a consumed invitation should be refused:\n%s", excerpt(again))
		}
	})

	t.Run("signing in needs no tenant, and the tabs reach them all", func(t *testing.T) {
		fresh := newBrowser(t)
		page := fresh.post(t, "/ui/login", url.Values{
			"email":    {owner},
			"password": {"a long enough password"},
		})
		if !strings.Contains(page, "Sign out") {
			t.Fatalf("an address and a password should be enough:\n%s", excerpt(page))
		}
		if !strings.Contains(page, tenant) {
			t.Errorf("it should land in a tenant they belong to:\n%s", excerpt(page))
		}
		// The tab strip is how the rest are reached, and the current one is not a
		// button: switching to where you already are is not an action.
		if !strings.Contains(page, `aria-label="Tenants"`) {
			t.Error("the tenant tabs should be there")
		}
		if !strings.Contains(page, `aria-current="page"`) {
			t.Error("the current tenant should be marked")
		}
	})

	t.Run("one password reaches two tenants", func(t *testing.T) {
		guestUI := newBrowser(t)

		// Grace makes her own tenant, with the password she chose when she accepted
		// the invitation. One identity, two accounts, two roles.
		//
		// Signing in first rather than through the combined form: that form
		// registers and then creates, and she already exists. This is the path the
		// picker is for — somebody who is somewhere already, starting somewhere
		// else.
		second := "Grace " + uuid.NewString()[:8]
		if page := guestUI.post(t, "/ui/login", url.Values{
			"email": {guest}, "password": {"grace picked this one"},
		}); !strings.Contains(page, "Sign out") {
			t.Fatalf("she should sign in to the tenant she joined:\n%s", excerpt(page))
		}

		page := guestUI.post(t, "/ui/tenants", url.Values{"tenantName": {second}})
		if !strings.Contains(page, "tenant created") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}

		// Both are tabs, and the one you are in is not a button.
		if !strings.Contains(page, tenant) || !strings.Contains(page, second) {
			t.Errorf("both tenants should be tabs:\n%s", excerpt(page))
		}
		if !strings.Contains(page, `aria-current="page"`) {
			t.Error("the current tenant should be marked")
		}

		// Switching needs no password: she has already proved who she is.
		tenantID := tenantOf(t, ui.pool, tenant)
		page = guestUI.post(t, "/ui/switch", url.Values{"tenant": {tenantID.String()}})
		if !strings.Contains(page, "switched") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		// Admin there, Owner in her own — the role is per account, which the
		// single-table model could not express.
		if !strings.Contains(page, "Admin") {
			t.Error("her role in the first tenant is Admin")
		}
		// The notes panel is a narrow read, so she sees hers and not Ada's. This
		// is the owner scope working: she is an Admin in a tenant whose notes
		// somebody else wrote.
		if strings.Contains(page, "Written by an integration") {
			t.Error("the narrow read should not show another account's note")
		}

		// Admin holds note.read.all — see levels() in services/tenant — so the
		// wide view is hers to ask for, and the refusal path is somebody Basic's.
		wide := guestUI.get(t, "/ui?scope=all")
		if !strings.Contains(wide, "Written by an integration") {
			t.Errorf("scope=all should show the tenant's notes:\n%s", excerpt(wide))
		}
	})

	t.Run("signing out kills the session", func(t *testing.T) {
		if page := ui.post(t, "/ui/logout", nil); !strings.Contains(page, "signed out") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		if page := ui.get(t, "/ui"); strings.Contains(page, "Sign out") {
			t.Error("the cookie should be gone")
		}
	})

	t.Run("a dead cookie is not a signed-in caller", func(t *testing.T) {
		// The case a ten-minute token guarantees will happen: the cookie is
		// there and the session behind it is not. It has to send somebody back
		// to the sign-in page rather than render a dashboard of refusals.
		stale := newBrowser(t)
		stale.jar.SetCookies(stale.base, []*http.Cookie{{
			Name: "rig_auth_demo", Value: "NBSWY3DPEB3W64TMMQ", Path: "/",
		}})

		page := stale.get(t, "/ui")
		if strings.Contains(page, "Sign out") {
			t.Error("a cookie that does not resolve is not a session")
		}
		if !strings.Contains(page, "Sign in") {
			t.Error("it should offer the way back in")
		}
	})
}

// browser is a client with a cookie jar, which is all a form-and-redirect
// interface needs.
type browser struct {
	client *http.Client
	jar    *cookiejar.Jar
	base   *url.URL
	pool   *pgxpool.Pool
	srv    string
}

func newBrowser(t *testing.T) *browser {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55442/rig?sslmode=disable"
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

	// The same function main uses, so what the test drives is what runs.
	srv := httptest.NewUnstartedServer(nil)

	handler, _, err := newAPI(context.Background(), pool)
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
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	return &browser{
		client: &http.Client{Jar: jar},
		jar:    jar, base: base, pool: pool, srv: srv.URL,
	}
}

// read is the body, which is what every assertion here looks at.
func read(t *testing.T, res *http.Response) string {
	t.Helper()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (b *browser) get(t *testing.T, path string) string {
	t.Helper()

	res, err := b.client.Get(b.srv + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return read(t, res)
}

// post submits a form and follows the redirect, which is what a browser does.
func (b *browser) post(t *testing.T, path string, form url.Values) string {
	t.Helper()

	res, err := b.client.PostForm(b.srv+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return read(t, res)
}

var (
	secretPattern = regexp.MustCompile(`rig_sk_[A-Z0-9]+_[A-Z0-9]+`)
	tokenPattern  = regexp.MustCompile(`<pre class="mono"[^>]*>([A-Z0-9]{40,})</pre>`)
)

func findSecret(page string) string { return secretPattern.FindString(page) }

func findToken(page string) string {
	m := tokenPattern.FindStringSubmatch(page)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// invitationID finds the live invitation for an address, the way the panel's own
// form does.
func invitationID(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()

	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
		SELECT v.id FROM rig_identity_verification v
		  JOIN rig_identity ON rig_identity.id = v.identity_id
		 WHERE lower(rig_identity.email_address) = lower($1)
		   AND v.kind = 'Invitation' AND v.consumed_at IS NULL AND v.revoked_at IS NULL
		 ORDER BY v.created_at DESC LIMIT 1`, email).Scan(&id); err != nil {
		t.Fatalf("find the invitation for %s: %v", email, err)
	}
	return id.String()
}

// tenantOf finds a tenant by name, for the switch.
func tenantOf(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM rig_tenant WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("find tenant %q: %v", name, err)
	}
	return id
}

// excerpt keeps a failure message readable: a whole page of HTML in a test log
// buries the assertion that failed.
func excerpt(page string) string {
	page = strings.Join(strings.Fields(stripTags(page)), " ")
	if len(page) > 400 {
		return page[:400] + "…"
	}
	return page
}

var (
	tagPattern   = regexp.MustCompile(`<[^>]*>`)
	blockPattern = regexp.MustCompile(`(?s)<(style|script)\b.*?</(style|script)>`)
)

// stripTags is for a failure message, so the stylesheet goes first: a page of CSS
// in a test log buries the sentence that would explain the failure.
func stripTags(s string) string {
	return tagPattern.ReplaceAllString(blockPattern.ReplaceAllString(s, " "), " ")
}

// The four-step flow, through the interface: create an account, look at where you
// could go, join or make a tenant, and be in it.
func TestTheFlowThroughThePicker(t *testing.T) {
	t.Run("registering lands in the picker", func(t *testing.T) {
		ui := newBrowser(t)
		address := "grace-" + uuid.NewString()[:8] + "@example.com"
		page := ui.post(t, "/ui/register", url.Values{
			"name":     {"Grace"},
			"email":    {address},
			"password": {"grace picked this one"},
		})

		if !strings.Contains(page, "pick a tenant or make one") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		// The picker, not the dashboard. There is no tenant, so there is nothing
		// a note panel could be scoped by.
		if !strings.Contains(page, "Where do you want to be?") {
			t.Errorf("expected the picker:\n%s", excerpt(page))
		}
		if strings.Contains(page, "the application&#39;s one table") {
			t.Error("the notes panel needs a tenant and there is none")
		}
		if !strings.Contains(page, "Nobody has invited you anywhere") {
			t.Errorf("a brand new person has no invitations:\n%s", excerpt(page))
		}
	})

	t.Run("making a tenant leaves it", func(t *testing.T) {
		ui := newBrowser(t)
		ui.post(t, "/ui/register", url.Values{
			"name": {"Hopper"}, "email": {"hopper-" + uuid.NewString()[:8] + "@example.com"},
			"password": {"hopper picked this one"},
		})

		page := ui.post(t, "/ui/tenants", url.Values{"tenantName": {"Hoppers"}})
		if !strings.Contains(page, "tenant created") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		// Out of the picker and into the dashboard, as the Owner of what they made.
		if strings.Contains(page, "Where do you want to be?") {
			t.Error("making a tenant should leave the picker")
		}
		if !strings.Contains(page, "Hoppers") || !strings.Contains(page, "Owner") {
			t.Errorf("the new tenant should be the current tab:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Notes") {
			t.Errorf("the dashboard should be rendering now:\n%s", excerpt(page))
		}
	})

	// The case the old 403 made impossible, and the reason for all of this: an
	// account here, no tenant, and an invitation waiting.
	t.Run("joining one you were invited to", func(t *testing.T) {
		// Registered first, so they have a password of their own. Somebody invited
		// before they ever signed up takes the emailed link instead, which is the
		// other door and is tested above.
		joiner := newBrowser(t)
		guest := "linus-" + uuid.NewString()[:8] + "@example.com"
		joiner.post(t, "/ui/register", url.Values{
			"name": {"Linus"}, "email": {guest},
			"password": {"linus picked this one"},
		})

		// An owner, elsewhere, who invites them.
		owner := newBrowser(t)
		space := "Acme " + uuid.NewString()[:8]
		owner.post(t, "/ui/signup", url.Values{
			"tenantName": {space}, "name": {"Ada"},
			"email":    {"ada-" + uuid.NewString()[:8] + "@example.com"},
			"password": {"ada picked this one"},
		})
		if page := owner.post(t, "/ui/invite", url.Values{
			"email": {guest}, "name": {"Linus"}, "role": {"Basic"},
		}); !strings.Contains(page, "invited") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}

		// It shows up in the picker, named — the tenant's name is the only part
		// somebody who has never been there recognises.
		page := joiner.get(t, "/ui")
		if !strings.Contains(page, space) {
			t.Fatalf("the invitation should name the tenant:\n%s", excerpt(page))
		}

		id := invitationID(t, joiner.pool, guest)
		page = joiner.post(t, "/ui/join", url.Values{"invitation": {id}})
		if !strings.Contains(page, "joined") {
			t.Fatalf("expected the flash:\n%s", excerpt(page))
		}
		if strings.Contains(page, "Where do you want to be?") {
			t.Error("joining should leave the picker")
		}
		if !strings.Contains(page, space) || !strings.Contains(page, "Basic") {
			t.Errorf("they should be in the tenant, at the role invited:\n%s", excerpt(page))
		}

		// A Basic member holds apikey.own and not apikey.manage, so the key form
		// offers the personal kind only.
		if !strings.Contains(page, "apikey.own") {
			t.Errorf("the panel should explain which permission they hold:\n%s", excerpt(page))
		}
		if strings.Contains(page, `<option value="Integration">`) {
			t.Error("a Basic member should not be offered the service kind")
		}
	})
}

// between is the substring between two markers, for an assertion about one form
// rather than the whole page.
func between(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}
