//go:build docker

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/auth/services/outbox"
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
		for _, want := range []string{"Ask for a code", "Type the code back", "Land on an invitation"} {
			if !strings.Contains(page, want) {
				t.Errorf("the welcome page should offer %q", want)
			}
		}
		if strings.Contains(page, "Sign out") {
			t.Error("a stranger is not signed in")
		}
		// Nothing anywhere asks for a password, because there is nothing to ask
		// for. This is the assertion #165 is about: the surface is gone, not
		// merely unused.
		if strings.Contains(page, `type="password"`) {
			t.Error("no form should ask for a password")
		}
		// And the sign-in form does not ask which tenant, because the visitor
		// cannot know: nobody can say which tenants an address belongs to until
		// the code has come back.
		//
		// Scoped to that form. Naming the tenant you are creating is a different
		// field on a different form, and asserting over the whole page would catch
		// it.
		form, ok := between(page, `action="/ui/signin"`, "</form>")
		if !ok {
			t.Fatal("no sign-in form")
		}
		if strings.Contains(form, `name="tenant"`) {
			t.Errorf("the sign-in form should not ask for a tenant:\n%s", form)
		}
	})

	tenant := "Acme " + uuid.NewString()[:8]
	owner := "ada-" + uuid.NewString()[:8] + "@acme.test"

	t.Run("a code signs somebody in, and they make a tenant", func(t *testing.T) {
		// The address is new, so asking for a code creates the person: that is
		// allow_provisioning, and it is what makes this the front door.
		page := ui.signIn(t, owner)
		if !strings.Contains(page, "pick a tenant or make one") {
			t.Fatalf("a newcomer should land in the picker:\n%s", excerpt(page))
		}

		page = ui.post(t, "/ui/tenants", url.Values{"tenantName": {tenant}})
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

		// Invited and not yet arrived, which now means exactly that: there is no
		// account, so she is not in the tenant's people list. This is the
		// assertion #164 is about, and it used to say the opposite.
		var members int
		if err := ui.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM rig_account
			 WHERE lower(email_address) = lower($1) AND deleted_at IS NULL`,
			guest).Scan(&members); err != nil {
			t.Fatal(err)
		}
		if members != 0 {
			t.Errorf("%d accounts for somebody who has not accepted, want 0", members)
		}
		// She is listed as a pending invitation, which is a different panel and
		// a different claim.
		if !strings.Contains(page, "Pending invitations") || !strings.Contains(page, guest) {
			t.Error("she should be waiting rather than present")
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

		// Withdrawn, then invited again. It used to be that this only worked
		// because withdrawing removed the account too; there is no account, so
		// withdrawing is one write and re-inviting is unremarkable.
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

		for _, want := range []string{
			"EmailCodeRequested", "LoginSucceeded",
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

		// Look first. This is what the mail is for: a page that can say where
		// somebody has been invited before asking them to prove anything.
		looked := guestUI.post(t, "/ui/invite/preview", url.Values{"token": {invitation}})
		if !strings.Contains(looked, tenant) {
			t.Errorf("the preview should name the tenant:\n%s", excerpt(looked))
		}
		if !strings.Contains(looked, "Ada") {
			t.Errorf("the preview should name who invited them:\n%s", excerpt(looked))
		}
		// Masked, because holding a forwarded link does not prove you are its
		// addressee.
		if strings.Contains(looked, guest) {
			t.Errorf("the preview should mask the address:\n%s", excerpt(looked))
		}

		page := guestUI.post(t, "/ui/accept", url.Values{"token": {invitation}})
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
		again := newBrowser(t).post(t, "/ui/accept", url.Values{"token": {invitation}})
		if !strings.Contains(again, "already been used") &&
			!strings.Contains(again, "not valid") {
			t.Errorf("a consumed invitation should be refused:\n%s", excerpt(again))
		}
	})

	t.Run("signing in needs no tenant, and the tabs reach them all", func(t *testing.T) {
		fresh := newBrowser(t)
		page := fresh.signIn(t, owner)
		if !strings.Contains(page, "Sign out") {
			t.Fatalf("an address and a code should be enough:\n%s", excerpt(page))
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

	t.Run("one address reaches two tenants", func(t *testing.T) {
		guestUI := newBrowser(t)

		// Grace makes her own tenant. One identity, two accounts, two roles —
		// and nothing to remember for either.
		second := "Grace " + uuid.NewString()[:8]
		if page := guestUI.signIn(t, guest); !strings.Contains(page, "Sign out") {
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

		// Switching needs no secret at all: she has already proved who she is.
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
	// mail is this run's mailbox, which is where a sign-in code exists in
	// plaintext. The interface shows it too; reading it here is the same act
	// with less parsing.
	mail *outbox.Box
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

	// The origin this run answers at, read off the listener before the server
	// starts — which is the whole reason newAPI takes it rather than reading the
	// environment. A provider compares the callback URL exactly, and the one
	// rig.yaml names is a port nothing here is listening on.
	origin := "http://" + srv.Listener.Addr().String()

	handler, front, _, mail, err := newAPI(context.Background(), pool, origin, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	closeAuth(t, front)
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
		jar:    jar, base: base, pool: pool, srv: srv.URL, mail: mail,
	}
}

// signIn is the whole flow through the interface: ask for a code, read it out
// of the mailbox, and type it back.
//
// It is also how somebody rig has never seen arrives, because this example sets
// allow_provisioning: the first request creates the person.
func (b *browser) signIn(t *testing.T, email string) string {
	t.Helper()

	if page := b.post(t, "/ui/code", url.Values{"email": {email}}); !strings.Contains(page, "a code is in the outbox") {
		t.Fatalf("asking for a code:\n%s", excerpt(page))
	}
	return b.post(t, "/ui/signin", url.Values{
		"email": {email}, "code": {b.codeFor(t, email)},
	})
}

// codeFor is the newest code mailed to an address.
func (b *browser) codeFor(t *testing.T, email string) string {
	t.Helper()

	for _, m := range b.mail.Messages() {
		if m.Kind == outbox.KindEmailCode && strings.EqualFold(m.To, email) {
			return m.Token
		}
	}
	t.Fatalf("no code was mailed to %s", email)
	return ""
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

// Signing in with a provider when nothing anywhere names a tenant.
//
// This is the half examples/auth_oauth cannot show. That one is `from: [host]`,
// so every sign-in in it names a real tenant before the redirect — which is what
// makes it the check that the default did not move, and what makes it the wrong
// place for this. Here nobody knows the tenant: `/start` is an anonymous browser
// GET, the resolver answers uuid.Nil, and where somebody goes is settled after
// the callback from their own memberships, exactly as a code sign-in settles
// it.
//
// The interesting half is the page that comes back. A 200 with an identity
// token, an empty tenant list and no session is a state a front end has to draw,
// and until there was a provider button nothing landed anybody in it.
func TestSigningInWithAProvider(t *testing.T) {
	t.Run("the page offers a button and names no tenant", func(t *testing.T) {
		ui := newBrowser(t)
		page := ui.get(t, "/ui")

		if !strings.Contains(page, "/auth/oauth/demo/start") {
			t.Fatalf("expected a provider button:\n%s", excerpt(page))
		}
		if strings.Contains(page, "tenant=") {
			t.Error("the button must not name a tenant; that is the whole point")
		}
	})

	t.Run("a stranger lands in the picker", func(t *testing.T) {
		ui := newBrowser(t)

		stranger := "ada-" + uuid.NewString()[:8] + "@example.com"
		page := ui.signInWithProvider(t, consent{
			subject: "subject-" + uuid.NewString()[:8],
			email:   stranger,
			name:    "Ada",
			// The provider vouches for the address.
			verified: true,
		})

		// A person, a provider link, and an account nowhere. Not a refusal.
		if !strings.Contains(page, "signed in with Demo") {
			t.Fatalf("expected the sign-in to have completed:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Where do you want to be?") {
			t.Fatalf("expected the picker:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Nobody has invited you anywhere") {
			t.Errorf("a brand new person has no invitations:\n%s", excerpt(page))
		}

		var accounts int
		if err := ui.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM rig_account a
			JOIN rig_identity i ON i.id = a.identity_id
			WHERE lower(i.email_address) = lower($1)`, stranger).Scan(&accounts); err != nil {
			t.Fatal(err)
		}
		if accounts != 0 {
			t.Errorf("%d accounts, want 0 — nothing named a tenant to make one in", accounts)
		}

		// And the picker's exit works from here, which is what makes the state a
		// state rather than a dead end.
		out := ui.post(t, "/ui/tenants", url.Values{"tenantName": {"Adas"}})
		if strings.Contains(out, "Where do you want to be?") {
			t.Errorf("making a tenant should leave the picker:\n%s", excerpt(out))
		}
		if !strings.Contains(out, "Adas") || !strings.Contains(out, "Owner") {
			t.Errorf("the new tenant should be the current tab:\n%s", excerpt(out))
		}
	})

	// The second sign-in, which is where #142's most-recently-used rule shows
	// itself: no tenant is named, and they land back in the one they were in
	// rather than in the picker again.
	t.Run("signing in again lands where they were", func(t *testing.T) {
		ui := newBrowser(t)

		address := "grace-" + uuid.NewString()[:8] + "@example.com"
		subject := "subject-" + uuid.NewString()[:8]
		ui.signInWithProvider(t, consent{
			subject: subject, email: address, name: "Grace", verified: true,
		})
		ui.post(t, "/ui/tenants", url.Values{"tenantName": {"Graces"}})
		ui.post(t, "/ui/logout", url.Values{})

		page := ui.signInWithProvider(t, consent{
			subject: subject, email: address, name: "Grace", verified: true,
		})
		if strings.Contains(page, "Where do you want to be?") {
			t.Fatalf("they belong somewhere now; the picker is the wrong answer:\n%s", excerpt(page))
		}
		if !strings.Contains(page, "Graces") {
			t.Errorf("expected to land back in the tenant they made:\n%s", excerpt(page))
		}
	})

	// Somebody rig has seen but who never finished — they asked for a code and
	// never used it — arriving through a provider instead. Two things have to be
	// true: the two are one person, and the address counts as confirmed from
	// here on, because the provider checked it and that is the same evidence a
	// code is allowed on.
	t.Run("an existing identity is linked, and its address is now verified", func(t *testing.T) {
		ui := newBrowser(t)

		address := "linus-" + uuid.NewString()[:8] + "@example.com"
		// The request creates the person and nothing else. Not typing the code
		// back is what leaves the address unconfirmed, which is the state this
		// test needs and the one a code sign-in would have removed.
		ui.post(t, "/ui/code", url.Values{"email": {address}})

		var before *time.Time
		if err := ui.pool.QueryRow(context.Background(),
			`SELECT email_verified_at FROM rig_identity WHERE lower(email_address) = lower($1)`,
			address).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if before != nil {
			t.Fatal("a self-registered person starts unconfirmed")
		}

		ui.signInWithProvider(t, consent{
			subject: "subject-" + uuid.NewString()[:8],
			email:   address, name: "Linus", verified: true,
		})

		var identities int
		if err := ui.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM rig_identity WHERE lower(email_address) = lower($1)`,
			address).Scan(&identities); err != nil {
			t.Fatal(err)
		}
		if identities != 1 {
			t.Errorf("%d identities, want 1 — the provider should reach the person who exists", identities)
		}

		var after *time.Time
		if err := ui.pool.QueryRow(context.Background(),
			`SELECT email_verified_at FROM rig_identity WHERE lower(email_address) = lower($1)`,
			address).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after == nil {
			t.Error("linking a verified provider address should record it as verifying the identity")
		}
	})

	// The check the whole OAuth package turns on, on the deferred path.
	t.Run("an unverified address will not link an existing account", func(t *testing.T) {
		ui := newBrowser(t)

		address := "mallory-" + uuid.NewString()[:8] + "@example.com"
		ui.post(t, "/ui/code", url.Values{"email": {address}})

		page := ui.signInWithProvider(t, consent{
			subject: "an-attacker-" + uuid.NewString()[:8],
			email:   address, name: "Not Mallory",
			// The provider will not vouch for it.
			verified: false,
		})
		if strings.Contains(page, "signed in with Demo") {
			t.Fatalf("an unverified address must not reach an account that exists:\n%s", excerpt(page))
		}

		var links int
		if err := ui.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM rig_identity_oauth o
			JOIN rig_identity i ON i.id = o.identity_id
			WHERE lower(i.email_address) = lower($1)`, address).Scan(&links); err != nil {
			t.Fatal(err)
		}
		if links != 0 {
			t.Errorf("%d links, want 0", links)
		}
	})

	// allow_joining is off here, and this is why: `tenant.from` includes `query`,
	// so a crafted start link can name any tenant. Without the second switch the
	// callback would make whoever clicked it an account there.
	t.Run("a crafted link cannot join somebody to a tenant", func(t *testing.T) {
		ui := newBrowser(t)

		// A tenant that exists and has nothing to do with the visitor.
		owner := newBrowser(t)
		owner.signInWithProvider(t, consent{
			subject: "subject-" + uuid.NewString()[:8],
			email:   "owner-" + uuid.NewString()[:8] + "@example.com",
			name:    "Owner", verified: true,
		})
		owner.post(t, "/ui/tenants", url.Values{"tenantName": {"Private"}})

		var victimTenant uuid.UUID
		if err := owner.pool.QueryRow(context.Background(),
			`SELECT id FROM rig_tenant WHERE name = 'Private' ORDER BY created_at DESC LIMIT 1`).
			Scan(&victimTenant); err != nil {
			t.Fatal(err)
		}

		stranger := "eve-" + uuid.NewString()[:8] + "@example.com"
		ui.signInWithProviderAt(t, "/auth/oauth/demo/start?returnTo=/ui&tenant="+victimTenant.String(),
			consent{
				subject: "subject-" + uuid.NewString()[:8],
				email:   stranger, name: "Eve", verified: true,
			})

		var accounts int
		if err := ui.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM rig_account a
			JOIN rig_identity i ON i.id = a.identity_id
			WHERE lower(i.email_address) = lower($1) AND a.tenant_id = $2`,
			stranger, victimTenant).Scan(&accounts); err != nil {
			t.Fatal(err)
		}
		if accounts != 0 {
			t.Errorf("%d accounts in a tenant nobody chose, want 0", accounts)
		}
	})
}

// consent is what the stand-in provider will say about somebody. The last field
// is the one a real provider does not let you choose, and the one the whole
// linking rule turns on.
type consent struct {
	subject, email, name string
	verified             bool
}

// signInWithProvider drives the round trip the way a browser does: follow the
// button, approve at the consent screen, come back, and land on whatever the
// application rendered.
//
// Nothing here names a tenant, which is the assertion.
func (b *browser) signInWithProvider(t *testing.T, in consent) string {
	t.Helper()
	return b.signInWithProviderAt(t, "/auth/oauth/demo/start?returnTo=/ui", in)
}

// signInWithProviderAt is the same round trip from a start link the caller
// wrote, for the test that crafts one.
func (b *browser) signInWithProviderAt(t *testing.T, start string, in consent) string {
	t.Helper()

	page := b.get(t, start)

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
	return b.post(t, "/idp/approve", form)
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

// The four-step flow, through the interface: sign in, look at where you could
// go, join or make a tenant, and be in it.
func TestTheFlowThroughThePicker(t *testing.T) {
	t.Run("a newcomer lands in the picker", func(t *testing.T) {
		ui := newBrowser(t)
		address := "grace-" + uuid.NewString()[:8] + "@example.com"
		page := ui.signIn(t, address)

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
		ui.signIn(t, "hopper-"+uuid.NewString()[:8]+"@example.com")

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
		// Signed in first, so they are somebody already. Somebody invited before
		// they had ever been here takes the emailed link instead, which is the
		// other door and is tested above.
		joiner := newBrowser(t)
		guest := "linus-" + uuid.NewString()[:8] + "@example.com"
		joiner.signIn(t, guest)

		// An owner, elsewhere, who invites them.
		owner := newBrowser(t)
		space := "Acme " + uuid.NewString()[:8]
		owner.signIn(t, "ada-"+uuid.NewString()[:8]+"@example.com")
		owner.post(t, "/ui/tenants", url.Values{"tenantName": {space}})
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
