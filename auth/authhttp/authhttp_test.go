package authhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/tenancy"
	"github.com/simonjanss/rig/runtime/throttle"
)

const goodPassword = "correct horse battery staple"

type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

type recorder struct {
	entries []authlog.Entry
	counter *throttle.Memory
}

func (r *recorder) Write(_ context.Context, e authlog.Entry) {
	r.entries = append(r.entries, e)
	if e.EmailAddress != "" {
		r.counter.Record(e.Event, throttle.Email(e.EmailAddress), e.At)
	}
	if e.IPAddress != "" {
		r.counter.Record(e.Event, throttle.IP(e.IPAddress), e.At)
	}
	if e.AccountID != nil {
		r.counter.Record(e.Event, throttle.Account(e.AccountID.String()), e.At)
	}
}

type notifier struct{ reset, verify, invite string }

func (n *notifier) SendPasswordReset(_ context.Context, _ *account.Identity, token string) error {
	n.reset = token
	return nil
}

func (n *notifier) SendInvitation(_ context.Context, _ *account.Identity, _ *account.Account, token string) error {
	n.invite = token
	return nil
}

func (n *notifier) SendEmailVerification(_ context.Context, _ *account.Identity, token string) error {
	n.verify = token
	return nil
}

type fixture struct {
	srv        *httptest.Server
	handler    *authhttp.Handler
	store      *account.MemoryStore
	keys       *apikey.Manager
	notify     *notifier
	clock      *clock
	grants     map[uuid.UUID]grants
	tenant     uuid.UUID
	identity   *account.Identity
	account    *account.Account
	sessions   *session.Manager
	identities *session.IdentityManager
}

func setup(t *testing.T) *fixture {
	t.Helper()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	log := &recorder{counter: throttle.NewMemory()}
	store := account.NewMemoryStore()
	notify := &notifier{}

	// One store for both credentials, the way the Postgres one is.
	tokens := session.NewMemoryStore()
	sessions, err := session.New(session.Config{Store: tokens, Log: log, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := session.NewIdentity(session.IdentityConfig{Store: tokens, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := apikey.New(apikey.Config{Store: apikey.NewMemoryStore(), Log: log, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	accounts, err := account.New(account.Config{
		Store:      store,
		Sessions:   sessions,
		Identities: identities,
		Hasher:     password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}),
		Log:        log,
		Notifier:   notify,
		Limiter:    throttle.New(log.counter).WithClock(c.now),
		Now:        c.now,
		Sleep:      func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{
		store: store, keys: keys, notify: notify, clock: c,
		grants: map[uuid.UUID]grants{}, tenant: uuid.New(),
		sessions: sessions, identities: identities,
	}

	f.handler, err = authhttp.New(authhttp.Config{
		Accounts:   accounts,
		Sessions:   sessions,
		Identities: identities,
		APIKeys:    keys,
		Tenant:     func(*http.Request) (uuid.UUID, error) { return f.tenant, nil },
		Grants: func(_ context.Context, _, accountID uuid.UUID) (roles, permissions []string, err error) {
			g := f.grants[accountID]
			return g.roles, g.permissions, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	f.handler.Mount(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	f.identity = &account.Identity{
		ID: uuid.New(), EmailAddress: "sam@example.com",
		DisplayName: "Sam", IsActive: true,
	}
	f.account = &account.Account{
		ID: uuid.New(), TenantID: f.tenant, DisplayName: "Sam", IsActive: true,
	}
	store.PutPerson(f.identity, f.account)
	if err := accounts.SetPassword(context.Background(), f.identity.ID, goodPassword); err != nil {
		t.Fatal(err)
	}
	return f
}

type response struct {
	status int
	body   []byte
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.body, into); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (f *fixture) do(t *testing.T, method, path, token, body string) response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, body: raw}
}

type pair struct {
	AccessToken      string    `json:"accessToken"`
	RefreshToken     string    `json:"refreshToken"`
	ExpiresAt        time.Time `json:"expiresAt"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
	SessionID        uuid.UUID `json:"sessionId"`
}

func (f *fixture) login(t *testing.T) pair {
	t.Helper()

	res := f.do(t, "POST", "/auth/login", "",
		`{"emailAddress":"sam@example.com","password":"`+goodPassword+`"}`)
	if res.status != http.StatusOK {
		t.Fatalf("login: status %d\n%s", res.status, res.body)
	}

	var p pair
	res.decode(t, &p)
	return p
}

func TestLogin(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	if p.AccessToken == "" || p.RefreshToken == "" {
		t.Fatal("both tokens should be returned")
	}
	// A client needs both deadlines: one says when to refresh, the other says
	// when to stop trying.
	if !p.ExpiresAt.Before(p.RefreshExpiresAt) {
		t.Error("the access token should expire before the session does")
	}
}

func TestLoginRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"wrong password", `{"emailAddress":"sam@example.com","password":"nope"}`, http.StatusUnauthorized},
		{"unknown address", `{"emailAddress":"nobody@example.com","password":"` + goodPassword + `"}`, http.StatusUnauthorized},
		{"empty body", ``, http.StatusBadRequest},
		{"unknown field", `{"email":"sam@example.com"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := f.do(t, "POST", "/auth/login", "", tc.body); res.status != tc.want {
				t.Errorf("status = %d, want %d\n%s", res.status, tc.want, res.body)
			}
		})
	}
}

// The whole reason these are not JWTs.
func TestClaimsResolveASession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)
	f.grants[f.account.ID] = grants{roles: []string{"editor"}, permissions: []string{"lesson.publish"}}

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+p.AccessToken)

	claims, err := f.handler.Claims(req)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AccountID != f.account.ID || claims.Subject != tenancy.SubjectAccount {
		t.Errorf("claims = %+v", claims)
	}
	// Resolved per request, so revoking a role takes effect now rather than
	// whenever the session next refreshes.
	if !claims.Can("lesson.publish") {
		t.Error("permissions should be resolved for the request")
	}

	f.grants[f.account.ID] = grants{}
	claims, _ = f.handler.Claims(req)
	if claims.Can("lesson.publish") {
		t.Error("a revoked role should stop working on the next request")
	}
}

func TestClaimsRefuseTheWrongCredential(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	for _, tc := range []struct{ name, header string }{
		{"nothing", ""},
		{"not bearer", "Basic c2FtOmh1bnRlcjI="},
		{"a refresh token", "Bearer " + p.RefreshToken},
		{"nonsense", "Bearer hunter2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/anything", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if _, err := f.handler.Claims(req); err == nil {
				t.Error("this should not have authenticated")
			}
		})
	}
}

func TestRefresh(t *testing.T) {
	t.Parallel()

	f := setup(t)
	first := f.login(t)

	f.clock.advance(time.Minute)
	res := f.do(t, "POST", "/auth/refresh", "", `{"refreshToken":"`+first.RefreshToken+`"}`)
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var second pair
	res.decode(t, &second)
	if second.RefreshToken == first.RefreshToken {
		t.Error("rotation should mint a new refresh token")
	}
	if second.SessionID != first.SessionID {
		t.Error("a rotation stays inside the same session")
	}

	// Replaying the consumed token an hour later revokes the family.
	f.clock.advance(time.Hour)
	if res := f.do(t, "POST", "/auth/refresh", "", `{"refreshToken":"`+first.RefreshToken+`"}`); res.status != http.StatusUnauthorized {
		t.Errorf("a replay should be refused, got %d", res.status)
	}
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+second.AccessToken)
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("the whole family should have been revoked")
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	if res := f.do(t, "POST", "/auth/logout", p.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+p.AccessToken)
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("the session should be gone immediately")
	}
}

// Whether an address is registered is not the caller's business, and any
// difference in status is the enumeration this endpoint is used for.
func TestPasswordResetAnswersTheSameEitherWay(t *testing.T) {
	t.Parallel()

	f := setup(t)

	known := f.do(t, "POST", "/auth/password/reset", "", `{"emailAddress":"sam@example.com"}`)
	unknown := f.do(t, "POST", "/auth/password/reset", "", `{"emailAddress":"nobody@example.com"}`)

	if known.status != http.StatusAccepted || unknown.status != http.StatusAccepted {
		t.Errorf("statuses differ: %d and %d", known.status, unknown.status)
	}
	if !bytes.Equal(known.body, unknown.body) {
		t.Errorf("bodies differ:\n  %s\n  %s", known.body, unknown.body)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	t.Parallel()

	f := setup(t)
	old := f.login(t)

	if res := f.do(t, "POST", "/auth/password/reset", "", `{"emailAddress":"sam@example.com"}`); res.status != http.StatusAccepted {
		t.Fatalf("status %d", res.status)
	}
	if f.notify.reset == "" {
		t.Fatal("a link should have been sent")
	}

	const newPassword = "an entirely different passphrase"
	res := f.do(t, "POST", "/auth/password/reset/confirm", "",
		`{"token":"`+f.notify.reset+`","newPassword":"`+newPassword+`"}`)
	if res.status != http.StatusNoContent {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	// Everything from before the reset is dead.
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+old.AccessToken)
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("the sessions from before the reset should be gone")
	}

	if res := f.do(t, "POST", "/auth/login", "",
		`{"emailAddress":"sam@example.com","password":"`+newPassword+`"}`); res.status != http.StatusOK {
		t.Errorf("the new password should work, got %d", res.status)
	}
}

func TestChangePasswordReturnsAFreshSession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	old := f.login(t)

	res := f.do(t, "POST", "/auth/password/change", old.AccessToken,
		`{"currentPassword":"`+goodPassword+`","newPassword":"an entirely different passphrase"}`)
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var fresh pair
	res.decode(t, &fresh)

	// The caller keeps working; everything else does not.
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+fresh.AccessToken)
	if _, err := f.handler.Claims(req); err != nil {
		t.Errorf("the caller should be handed a working session: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+old.AccessToken)
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("the old session should be gone")
	}
}

func TestSessions(t *testing.T) {
	t.Parallel()

	f := setup(t)
	phone := f.login(t)
	f.clock.advance(time.Second)
	laptop := f.login(t)

	res := f.do(t, "GET", "/auth/sessions", laptop.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var listed struct {
		Data []struct {
			ID      uuid.UUID `json:"id"`
			Current bool      `json:"current"`
		} `json:"data"`
	}
	res.decode(t, &listed)

	if len(listed.Data) != 2 {
		t.Fatalf("got %d sessions, want 2", len(listed.Data))
	}
	// An interface should be able to label the tab you are looking at rather
	// than inviting you to revoke it by accident.
	current := 0
	for _, s := range listed.Data {
		if s.Current {
			current++
			if s.ID != laptop.SessionID {
				t.Error("the wrong session is marked current")
			}
		}
	}
	if current != 1 {
		t.Errorf("%d sessions marked current, want 1", current)
	}

	if res := f.do(t, "DELETE", "/auth/sessions/"+phone.SessionID.String(), laptop.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("revoke: status %d\n%s", res.status, res.body)
	}

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+phone.AccessToken)
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("the revoked session should be dead")
	}
}

// A session identifier that answers 403 for somebody else's session is an
// identifier worth enumerating.
func TestRevokingSomebodyElsesSessionIs404(t *testing.T) {
	t.Parallel()

	f := setup(t)
	mine := f.login(t)

	other := &account.Account{
		ID: uuid.New(), TenantID: f.tenant, DisplayName: "Robin", IsActive: true,
	}
	f.store.PutPerson(&account.Identity{
		ID: uuid.New(), EmailAddress: "robin@example.com",
		DisplayName: "Robin", IsActive: true,
	}, other)
	theirs, err := f.sessions.Issue(context.Background(), session.IssueInput{
		TenantID: f.tenant, AccountID: other.ID, Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}

	res := f.do(t, "DELETE", "/auth/sessions/"+theirs.RootTokenID.String(), mine.AccessToken, "")
	if res.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.status)
	}
}

// grants is what the fixture's Grants function answers with — the shape an
// application supplies now that rig ships no role model of its own.
type grants struct {
	roles       []string
	permissions []string
}

func TestAPIKeys(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.grants[f.account.ID] = grants{
		permissions: []string{authhttp.PermissionManageAPIKeys, "export.run"},
	}
	p := f.login(t)

	res := f.do(t, "POST", "/auth/api-keys", p.AccessToken,
		`{"name":"nightly export","scopes":["export.run"]}`)
	if res.status != http.StatusCreated {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var created struct {
		Key struct {
			ID    uuid.UUID `json:"id"`
			KeyID string    `json:"keyId"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	res.decode(t, &created)
	if created.Secret == "" {
		t.Fatal("the secret should be returned exactly once")
	}

	// The key authenticates, with its scopes as its permissions.
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+created.Secret)
	claims, err := f.handler.Claims(req)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != tenancy.SubjectAPIKey {
		t.Errorf("subject = %q", claims.Subject)
	}
	if !claims.Can("export.run") {
		t.Error("the key should hold its scopes")
	}

	// Listing never shows the secret again, because nothing stored could
	// produce it.
	res = f.do(t, "GET", "/auth/api-keys", p.AccessToken, "")
	if bytes.Contains(res.body, []byte(created.Secret)) {
		t.Error("the secret appeared in a listing")
	}
	if !bytes.Contains(res.body, []byte(created.Key.KeyID)) {
		t.Error("the listing should show the public identifier")
	}

	if res := f.do(t, "DELETE", "/auth/api-keys/"+created.Key.ID.String(), p.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("revoke: status %d\n%s", res.status, res.body)
	}
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("a revoked key should stop working immediately")
	}
}

// Without this, "manage API keys" would quietly mean "grant yourself anything".
func TestAKeyCannotExceedItsCreator(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.grants[f.account.ID] = grants{permissions: []string{authhttp.PermissionManageAPIKeys}}
	p := f.login(t)

	res := f.do(t, "POST", "/auth/api-keys", p.AccessToken,
		`{"name":"escalation","scopes":["tenant.destroy"]}`)
	if res.status != http.StatusForbidden {
		t.Errorf("status = %d, want 403\n%s", res.status, res.body)
	}
}

func TestKeyEndpointsNeedThePermission(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/auth/api-keys", ""},
		{"POST", "/auth/api-keys", `{"name":"x"}`},
		{"DELETE", "/auth/api-keys/" + uuid.New().String(), ""},
	} {
		if res := f.do(t, tc.method, tc.path, p.AccessToken, tc.body); res.status != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", tc.method, tc.path, res.status)
		}
	}
}

func TestImpersonation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.grants[f.account.ID] = grants{permissions: []string{authhttp.PermissionImpersonate}}
	admin := f.login(t)

	target := &account.Account{
		ID: uuid.New(), TenantID: f.tenant, DisplayName: "Robin", IsActive: true,
	}
	f.store.PutPerson(&account.Identity{
		ID: uuid.New(), EmailAddress: "robin@example.com",
		DisplayName: "Robin", IsActive: true,
	}, target)

	res := f.do(t, "POST", "/auth/impersonate", admin.AccessToken,
		`{"accountId":"`+target.ID.String()+`"}`)
	if res.status != http.StatusCreated {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var as pair
	res.decode(t, &as)

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+as.AccessToken)
	claims, err := f.handler.Claims(req)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AccountID != target.ID {
		t.Error("the session should act as the target")
	}
	if claims.ImpersonatedByAccountID == nil || *claims.ImpersonatedByAccountID != f.account.ID {
		t.Error("the claims should say who is really behind it")
	}

	// Nesting would make the audit trail claim one person was two.
	if res := f.do(t, "POST", "/auth/impersonate", as.AccessToken,
		`{"accountId":"`+f.account.ID.String()+`"}`); res.status != http.StatusConflict {
		t.Errorf("nesting: status = %d, want 409", res.status)
	}

	if res := f.do(t, "DELETE", "/auth/impersonate", as.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("ending: status %d\n%s", res.status, res.body)
	}
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("ending should revoke the session")
	}
}

func TestImpersonationNeedsThePermission(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	res := f.do(t, "POST", "/auth/impersonate", p.AccessToken,
		`{"accountId":"`+uuid.New().String()+`"}`)
	if res.status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", res.status)
	}
}

// A limit keyed on an address a client can choose is a limit a client walks
// around.
func TestForwardedHeadersAreNotBelievedByDefault(t *testing.T) {
	t.Parallel()

	f := setup(t)

	for range 5 {
		req, _ := http.NewRequest("POST", f.srv.URL+"/auth/login",
			bytes.NewBufferString(`{"emailAddress":"sam@example.com","password":"nope"}`))
		req.Header.Set("Content-Type", "application/json")
		// A different claimed address every time. It must not buy new budget.
		req.Header.Set("X-Forwarded-For", uuid.New().String())
		res, err := f.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	res := f.do(t, "POST", "/auth/login", "",
		`{"emailAddress":"sam@example.com","password":"nope"}`)
	if res.status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: the header should have bought nothing", res.status)
	}
}

func TestLoginLockoutOverHTTP(t *testing.T) {
	t.Parallel()

	f := setup(t)

	for i := range 5 {
		res := f.do(t, "POST", "/auth/login", "",
			`{"emailAddress":"sam@example.com","password":"nope"}`)
		if res.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i+1, res.status)
		}
	}

	res := f.do(t, "POST", "/auth/login", "",
		`{"emailAddress":"sam@example.com","password":"`+goodPassword+`"}`)
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429\n%s", res.status, res.body)
	}
}

func TestATenantResolverIsRequired(t *testing.T) {
	t.Parallel()

	tokens := session.NewMemoryStore()
	sessions, err := session.New(session.Config{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := session.NewIdentity(session.IdentityConfig{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := account.New(account.Config{
		Store: account.NewMemoryStore(), Sessions: sessions, Identities: identities,
		Limiter: throttle.New(throttle.NewMemory()),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Only the application knows whether its customers arrive by subdomain, by
	// header, or by path. Guessing would mean guessing wrong for somebody.
	if _, err := authhttp.New(authhttp.Config{Accounts: accounts, Sessions: sessions}); err == nil {
		t.Error("a handler with no way to resolve a tenant should refuse to exist")
	}
}

// Without a key manager the endpoints are not mounted and a key is not a
// credential, which is right for a project that skipped that part.
func TestKeysAreOptional(t *testing.T) {
	t.Parallel()

	f := setup(t)
	handler, err := authhttp.New(authhttp.Config{
		Accounts:   mustAccounts(t, f),
		Sessions:   f.sessions,
		Identities: f.identities,
		Tenant:     func(*http.Request) (uuid.UUID, error) { return f.tenant, nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	handler.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := srv.Client().Get(srv.URL + "/auth/api-keys")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404: the endpoint should not exist", res.StatusCode)
	}

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+apikey.Prefix+"AAAAAAAAAAAAAAAA_"+
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if _, err := handler.Claims(req); err == nil {
		t.Error("a key should not authenticate when keys are not enabled")
	}
}

func mustAccounts(t *testing.T, f *fixture) *account.Service {
	t.Helper()

	svc, err := account.New(account.Config{
		Store: f.store, Sessions: f.sessions, Identities: f.identities,
		Limiter: throttle.New(throttle.NewMemory()).WithClock(f.clock.now),
		Now:     f.clock.now,
		Sleep:   func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// request is `do` against a server other than the fixture's, for the tests
// that need a handler configured differently.
func request(t *testing.T, srv *httptest.Server, method, path, token, body string) response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, body: raw}
}

// serve mounts a handler of the caller's shape on its own server, sharing the
// fixture's account and session state so a login still works.
func (f *fixture) serve(t *testing.T, cfg authhttp.Config) *httptest.Server {
	t.Helper()

	if cfg.Accounts == nil {
		cfg.Accounts = mustAccounts(t, f)
	}
	if cfg.Sessions == nil {
		cfg.Sessions = f.sessions
	}
	if cfg.Tenant == nil {
		cfg.Tenant = func(*http.Request) (uuid.UUID, error) { return f.tenant, nil }
	}

	handler, err := authhttp.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	handler.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Verification is the one flow where the token arrives out of band, so the two
// halves have to fit: what the notifier was handed is what the endpoint takes.
func TestTheEmailVerificationFlow(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	if res := f.do(t, "POST", "/auth/email/verify/resend", p.AccessToken, ""); res.status != http.StatusAccepted {
		t.Fatalf("resend: status %d\n%s", res.status, res.body)
	}
	if f.notify.verify == "" {
		t.Fatal("nothing was sent to the address being verified")
	}

	res := f.do(t, "POST", "/auth/email/verify", "", `{"token":"`+f.notify.verify+`"}`)
	if res.status != http.StatusNoContent {
		t.Fatalf("verify: status %d\n%s", res.status, res.body)
	}
	stored, err := f.store.FindIdentityByID(context.Background(), f.identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Verified() {
		t.Error("the address should be verified once its token came back")
	}

	// And the token is spent: a link forwarded to somebody else is not a second
	// verification.
	if res := f.do(t, "POST", "/auth/email/verify", "", `{"token":"`+f.notify.verify+`"}`); res.status == http.StatusNoContent {
		t.Error("a consumed token should not verify again")
	}
}

// Verification is unauthenticated by necessity — the link is clicked from an
// email client — so a guessed token has to be refused rather than trusted.
func TestVerifyingWithATokenNobodyIssuedFails(t *testing.T) {
	t.Parallel()

	f := setup(t)

	res := f.do(t, "POST", "/auth/email/verify", "", `{"token":"not-a-token"}`)
	if res.status != http.StatusUnauthorized && res.status != http.StatusBadRequest {
		t.Errorf("status = %d, want a refusal\n%s", res.status, res.body)
	}

	// Resending needs a session: an unauthenticated resend endpoint is a mail
	// cannon pointed at any address somebody can name.
	if res := f.do(t, "POST", "/auth/email/verify/resend", "", ""); res.status != http.StatusUnauthorized {
		t.Errorf("resend without a credential = %d, want 401", res.status)
	}
}

// Behind a proxy the peer is always the proxy, so without this every session
// and every limit is keyed on one address for the whole fleet.
func TestAForwardedAddressIsBelievedOnlyFromAProxyTheApplicationNamed(t *testing.T) {
	t.Parallel()

	f := setup(t)
	srv := f.serve(t, authhttp.Config{TrustedProxies: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}})

	login := func(forwarded string) string {
		t.Helper()

		req, err := http.NewRequest("POST", srv.URL+"/auth/login",
			bytes.NewBufferString(`{"emailAddress":"sam@example.com","password":"`+goodPassword+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("login: status %d", res.StatusCode)
		}

		var p pair
		if err := json.NewDecoder(res.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		return p.AccessToken
	}

	addressOn := func(token string) string {
		t.Helper()

		res := request(t, srv, "GET", "/auth/sessions", token, "")
		var listed struct {
			Data []struct {
				IPAddress string `json:"ipAddress"`
				Current   bool   `json:"current"`
			} `json:"data"`
		}
		res.decode(t, &listed)
		for _, s := range listed.Data {
			if s.Current {
				return s.IPAddress
			}
		}
		t.Fatal("no current session")
		return ""
	}

	// The left-most entry is the original client; the rest were added by hops.
	if got := addressOn(login("203.0.113.7, 198.51.100.1")); got != "203.0.113.7" {
		t.Errorf("address = %q, want the client the proxy reported", got)
	}
	f.clock.advance(time.Second)

	// No header at all, and a header that is not an address, both fall back to
	// the peer rather than to nothing.
	if got := addressOn(login("")); got == "" {
		t.Error("with no forwarded header the peer is still the address")
	}
	f.clock.advance(time.Second)

	if got := addressOn(login("not-an-address")); got == "" || got == "not-an-address" {
		t.Errorf("address = %q, want the peer rather than the claim", got)
	}
}

// A session list that says "Web" for a phone is a list nobody can act on when
// deciding which session to revoke.
func TestTheClientKindIsWhateverTheCallerDeclared(t *testing.T) {
	t.Parallel()

	for body, want := range map[string]string{
		`{"emailAddress":"sam@example.com","password":"` + goodPassword + `","client":"mobile"}`:  "Mobile",
		`{"emailAddress":"sam@example.com","password":"` + goodPassword + `","client":"MACHINE"}`: "Machine",
		`{"emailAddress":"sam@example.com","password":"` + goodPassword + `"}`:                    "Web",
		// Anything unrecognised is a browser, not an error: the field is a hint
		// for a human reading a session list, not a security boundary.
		`{"emailAddress":"sam@example.com","password":"` + goodPassword + `","client":"toaster"}`: "Web",
	} {
		f := setup(t)

		res := f.do(t, "POST", "/auth/login", "", body)
		if res.status != http.StatusOK {
			t.Fatalf("login: status %d\n%s", res.status, res.body)
		}
		var p pair
		res.decode(t, &p)

		var listed struct {
			Data []struct {
				Client string `json:"client"`
			} `json:"data"`
		}
		f.do(t, "GET", "/auth/sessions", p.AccessToken, "").decode(t, &listed)

		if len(listed.Data) != 1 || listed.Data[0].Client != want {
			t.Errorf("%s → client %+v, want %q", body, listed.Data, want)
		}
	}
}

// A misspelled field silently ignored is a password change that quietly did
// not change the password.
func TestABodyTheEndpointDoesNotUnderstandIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)

	for name, body := range map[string]string{
		"empty":         "",
		"not json":      "{",
		"unknown field": `{"emailAddress":"sam@example.com","passwrd":"x"}`,
	} {
		res := f.do(t, "POST", "/auth/login", "", body)
		if res.status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400\n%s", name, res.status, res.body)
		}

		var problem struct{ Code, Message string }
		res.decode(t, &problem)
		if problem.Code == "" || problem.Message == "" {
			t.Errorf("%s: the refusal should say what is wrong: %s", name, res.body)
		}
	}
}

// Every endpoint behind a credential reads it the same way, so the parsing is
// worth pinning once.
func TestTheAuthorizationHeaderHasToBeABearerToken(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	for name, header := range map[string]string{
		"absent":       "",
		"no scheme":    "sometoken",
		"wrong scheme": "Basic c2FtOmh1bnRlcjI=",
		"no token":     "Bearer ",
	} {
		req := httptest.NewRequest(http.MethodGet, "/anything", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if _, err := f.handler.Claims(req); err == nil {
			t.Errorf("%s: should not authenticate", name)
		}
	}

	// The scheme itself is case-insensitive, because clients disagree about it.
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "bearer "+p.AccessToken)
	if _, err := f.handler.Claims(req); err != nil {
		t.Errorf("`bearer` is the same scheme as `Bearer`: %v", err)
	}

	// A refresh token is a credential for exactly one endpoint. Accepting it as
	// a session would make a twelve-hour token do a ten-minute token's job.
	req = httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+p.RefreshToken)
	if _, err := f.handler.Claims(req); err == nil {
		t.Error("a refresh token should not authenticate a request")
	}
	if _, err := f.handler.Session(req); err == nil {
		t.Error("nor stand in for a session")
	}
}

// Ending an impersonation returns the administrator to their own session. On a
// session that was never one, there is nothing to return to.
func TestEndingAnImpersonationThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	res := f.do(t, "DELETE", "/auth/impersonate", p.AccessToken, "")
	if res.status == http.StatusNoContent {
		t.Errorf("an ordinary session has no impersonation to end\n%s", res.body)
	}
	if res := f.do(t, "DELETE", "/auth/impersonate", "", ""); res.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.status)
	}
}

// An application that already has an error format wants one format, not two,
// and the auth endpoints are the ones most likely to be behind somebody else's
// gateway.
func TestOnErrorTakesOverTheResponse(t *testing.T) {
	t.Parallel()

	f := setup(t)
	srv := f.serve(t, authhttp.Config{
		OnError: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte(err.Error()))
		},
	})

	res := request(t, srv, "POST", "/auth/login", "",
		`{"emailAddress":"sam@example.com","password":"nope"}`)
	if res.status != http.StatusTeapot {
		t.Errorf("status = %d, want the mapper's own\n%s", res.status, res.body)
	}
}

// The routes hang off a configurable prefix so they can live under an
// application's own namespace without a reverse proxy rewriting paths.
func TestTheBasePathIsConfigurable(t *testing.T) {
	t.Parallel()

	f := setup(t)
	// The trailing slash is trimmed, or every route would carry a double one.
	srv := f.serve(t, authhttp.Config{BasePath: "/api/v1/identity/"})

	if res := request(t, srv, "POST", "/api/v1/identity/login", "",
		`{"emailAddress":"sam@example.com","password":"`+goodPassword+`"}`); res.status != http.StatusOK {
		t.Errorf("status = %d, want the routes under the configured prefix\n%s", res.status, res.body)
	}
	if res := request(t, srv, "POST", "/auth/login", "", `{}`); res.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404: the default prefix should be gone", res.status)
	}
}

// A path parameter and a CIDR list are the two places a caller types something
// structured, and both have to be refused at the edge rather than reaching a
// store as a zero value.
func TestMalformedParametersAreRefusedAtTheEdge(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)
	f.grants[f.account.ID] = grants{permissions: []string{authhttp.PermissionManageAPIKeys}}

	if res := f.do(t, "DELETE", "/auth/sessions/not-a-uuid", p.AccessToken, ""); res.status != http.StatusBadRequest {
		t.Errorf("session id: status = %d, want 400\n%s", res.status, res.body)
	}
	if res := f.do(t, "DELETE", "/auth/api-keys/not-a-uuid", p.AccessToken, ""); res.status != http.StatusBadRequest {
		t.Errorf("key id: status = %d, want 400\n%s", res.status, res.body)
	}

	res := f.do(t, "POST", "/auth/api-keys", p.AccessToken,
		`{"name":"ci","cidrAllowList":["203.0.113.0"]}`)
	if res.status != http.StatusUnprocessableEntity && res.status != http.StatusBadRequest {
		t.Errorf("a bare address is not a network: status = %d\n%s", res.status, res.body)
	}

	// And a good one round-trips, so the list is reported back the way it was
	// given rather than as Go's own formatting of a prefix.
	res = f.do(t, "POST", "/auth/api-keys", p.AccessToken,
		`{"name":"ci","cidrAllowList":["203.0.113.0/24"]}`)
	if res.status != http.StatusCreated {
		t.Fatalf("status = %d\n%s", res.status, res.body)
	}
	var created struct {
		Key struct {
			CIDRAllowList []string `json:"cidrAllowList"`
		} `json:"key"`
	}
	res.decode(t, &created)
	if len(created.Key.CIDRAllowList) != 1 || created.Key.CIDRAllowList[0] != "203.0.113.0/24" {
		t.Errorf("allow list = %v", created.Key.CIDRAllowList)
	}
}
