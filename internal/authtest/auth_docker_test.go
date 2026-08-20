//go:build docker

// The authentication foundation, working.
//
//	go test -tags docker ./internal/authtest/
//
// Every other auth test runs against an in-memory store, which proves the rules
// and nothing about the SQL. This one applies the migrations `rig setup-project`
// writes, wires the real stores over them, and drives the whole thing over
// HTTP — so a column that does not exist, a query that does not run, and a lock
// that is not taken all fail here.
package authtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/authpg"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
	"github.com/simonjanss/rig/runtime/throttle"
)

const (
	containerName = "rigAuth-db"
	containerPort = dockerdb.PortAuth
	goodPassword  = "correct horse battery staple"
)

// harness is one test's world: a schema, a server, and a tenant of its own.
type harness struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	h      *authhttp.Handler
	stores *authpg.Stores
	notify *notifier
	// accounts and sessions are the real services over the real tables, for the
	// checks that are about SQL rather than about HTTP.
	accounts *account.Service
	sessions *session.Manager

	// held is what the caller may do. rig ships no authorization model, so a
	// suite over the real SQL supplies one the same way an application does: a
	// function handed to authhttp.Config. A map is the whole of it, because what
	// is under test here is that the check reads the claims — not where an
	// application chose to keep its roles.
	held map[string]bool

	// tenants are the hooks the auth package is configured with, and build makes
	// a service using them. A test that changes the policy calls rebuild.
	tenants account.TenantOptions
	build   func(account.TenantOptions) *account.Service
	mount   func(*account.Service)
	// routes is the mux the server delegates to, replaced on every rebuild.
	routes atomic.Value

	tenant uuid.UUID
	// identity is the person and account is who they are in this tenant. Both,
	// because the flows below need both: a password belongs to the first and a
	// session to the second.
	identity uuid.UUID
	account  uuid.UUID
	email    string
}

// pool is shared: one container and one migration run for the whole package,
// with each test isolated by its own tenant.
var (
	once   sync.Once
	shared *pgxpool.Pool
	setErr error
)

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()

	once.Do(func() { shared, setErr = startDatabase() })
	if setErr != nil {
		t.Fatal(setErr)
	}
	return shared
}

func startDatabase() (*pgxpool.Pool, error) {
	ctx := context.Background()

	// A schema left behind by an earlier run would make every one of these
	// tests pass for the wrong reason.
	_ = exec.Command("docker", "rm", "-f", "-v", dockerdb.Qualify(containerName)).Run()

	cfg := dockerdb.Config{
		Image: "postgres:17-alpine",
		Name:  dockerdb.Qualify(containerName), Port: dockerdb.HostPort(containerPort),
		Database: "rig", User: "rig", Password: "rig",
	}
	db, err := dockerdb.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "rigauth")
	if err != nil {
		return nil, err
	}
	migrations := filepath.Join(dir, "migrations")
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		return nil, err
	}

	// The same files the command writes. Testing a copy would test the copy.
	for _, f := range scaffold.Foundation(scaffold.FoundationOptions{
		FirstNumber:   1,
		MigrationsDir: migrations,
		ConfigPath:    func(table string) string { return filepath.Join(dir, table+".yaml") },
	}) {
		if !strings.HasSuffix(f.Path, ".sql") {
			continue
		}
		if err := os.WriteFile(f.Path, []byte(f.Content), 0o644); err != nil {
			return nil, err
		}
	}

	if _, err := dockerdb.Migrate(ctx, dockerdb.MigrateOptions{
		Dir: migrations, Table: "rig_auth_migrations", URL: db.URL(),
	}); err != nil {
		return nil, err
	}

	return pgxpool.New(ctx, db.URL())
}

type notifier struct {
	mu                     sync.Mutex
	reset, confirm, invite string
}

func (n *notifier) SendPasswordReset(_ context.Context, _ *account.Identity, token string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reset = token
	return nil
}

func (n *notifier) SendEmailVerification(_ context.Context, _ *account.Identity, token string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.confirm = token
	return nil
}

func (n *notifier) SendInvitation(_ context.Context, _ *account.Identity, _ *account.Account, token string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.invite = token
	return nil
}

func (n *notifier) resetToken() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reset
}

func setup(t *testing.T) *harness {
	t.Helper()

	pool := database(t)
	ctx := context.Background()

	h := &harness{
		pool:   pool,
		notify: &notifier{},
		tenant: uuid.New(),
		email:  "sam-" + uuid.NewString()[:8] + "@example.com",
	}
	h.stores = authpg.New(pool)

	if _, err := pool.Exec(ctx,
		`INSERT INTO rig_tenant (id, name, slug) VALUES ($1, $2, $3)`,
		h.tenant, "Test", h.tenant.String()); err != nil {
		t.Fatal(err)
	}

	// The person, then their account here. Two rows because they are two
	// things: the identity owns the address and the password, and the account is
	// who they are in this tenant.
	h.identity, h.account = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO rig_identity (id, email_address, display_name)
		VALUES ($1, $2, $3)`, h.identity, h.email, "Sam"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name)
		VALUES ($1, $2, $3, $4, $5)`,
		h.account, h.tenant, h.identity, h.email, "Sam"); err != nil {
		t.Fatal(err)
	}

	sessions, err := session.New(session.Config{
		Store: h.stores.Sessions, Log: h.stores.Log,
		// Short enough that a test can watch one expire without sleeping for
		// ten minutes.
		AccessTTL: 2 * time.Second, RefreshTTL: time.Hour,
		RotationLeeway: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	keys, err := apikey.New(apikey.Config{Store: h.stores.APIKeys, Log: h.stores.Log})
	if err != nil {
		t.Fatal(err)
	}

	// The real counter, reading the real rig_auth_log. This is the part no
	// in-memory test can check: the SQL that turns a trail into a limit.
	limiter := throttle.New(throttle.NewPostgres(pool, throttle.DefaultPostgresConfig()))

	// The tenant-less credential, over the same store the tokens are in.
	identities, err := session.NewIdentity(session.IdentityConfig{Store: h.stores.Sessions})
	if err != nil {
		t.Fatal(err)
	}

	// The account service is rebuilt whenever a test changes the tenant hooks,
	// because they are configuration rather than state — so the fixture keeps what
	// it takes to make another one.
	h.build = func(opts account.TenantOptions) *account.Service {
		svc, err := account.New(account.Config{
			Store:      h.stores.Accounts,
			Sessions:   sessions,
			Identities: identities,
			Hasher:     password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}),
			Log:        h.stores.Log,
			Notifier:   h.notify,
			Limiter:    limiter,
			Tenants:    opts,
			Sleep:      func(context.Context, time.Duration) {},
		})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}

	// The handler is rebuilt with it, because it captures the service.
	h.mount = func(accounts *account.Service) {
		handler, err := authhttp.New(authhttp.Config{
			Accounts: accounts, Sessions: sessions, APIKeys: keys,
			// The store that has been writing since M4, handed over as a reader.
			AuditLog: h.stores.Log,
			// Header first, falling back to the harness's own tenant. Most tests do
			// not care and want the fallback; the ones about signing in with no
			// tenant send the nil identifier, which is what "unspecified" is.
			Tenant: func(r *http.Request) (uuid.UUID, error) {
				if raw := r.Header.Get("X-Tenant-Id"); raw != "" {
					return uuid.Parse(raw)
				}
				return h.tenant, nil
			},
			Identities: identities,
			// The suite drives registration directly, so the route has to exist.
			AllowRegistration: true,
			// And the other picker exit. The auth package writes the tenant and the
			// first account itself; a suite that wants to see the route work only has
			// to say the route exists.
			AllowTenantCreation: true,
			Grants: func(_ context.Context, _, accountID uuid.UUID) (roles, permissions []string, err error) {
				if accountID != h.account {
					return nil, nil, nil
				}
				for key := range h.held {
					permissions = append(permissions, key)
				}
				slices.Sort(permissions)
				return nil, permissions, nil
			},
			// The default mapper hides an internal failure's detail, which is right
			// in production and useless in a test: a 500 with no cause is a
			// debugging session that starts from nothing.
			OnError: func(w http.ResponseWriter, _ *http.Request, err error) {
				if refusal, ok := throttle.RefusalOf(err); ok {
					refusal.Decision().SetHeaders(w.Header())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(rigerr.StatusOf(err))
				_ = json.NewEncoder(w).Encode(map[string]string{
					"code": string(rigerr.CodeOf(err)), "message": err.Error(),
				})
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.h, h.accounts = handler, accounts

		// A fresh mux, swapped in behind the server. Go's ServeMux refuses a
		// duplicate pattern, so remounting onto the old one would panic — and the
		// server has to keep its address, because tests hold URLs built from it.
		mux := http.NewServeMux()
		handler.Mount(mux)
		h.routes.Store(mux)
	}

	accounts := h.build(h.tenants)
	if err := accounts.SetPassword(ctx, h.identity, goodPassword); err != nil {
		t.Fatal(err)
	}
	h.mount(accounts)
	h.sessions = sessions

	// One indirection, so reconfiguring the auth package does not need a new
	// server: the handler behind it is replaced, the address is not.
	h.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			h.routes.Load().(*http.ServeMux).ServeHTTP(w, r)
		}))
	t.Cleanup(h.srv.Close)

	return h
}

type response struct {
	status     int
	body       []byte
	retryAfter string
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.body, into); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// doUnscoped is `do` with no tenant named, which is what a sign-in page sends:
// a visitor cannot say which tenants an address belongs to before the password
// has been checked.
func (h *harness) doUnscoped(t *testing.T, method, path, token, body string) response {
	t.Helper()
	return h.doWith(t, method, path, token, body, map[string]string{
		"X-Tenant-Id": uuid.Nil.String(),
	})
}

func (h *harness) do(t *testing.T, method, path, token, body string) response {
	t.Helper()
	return h.doWith(t, method, path, token, body, nil)
}

func (h *harness) doWith(t *testing.T, method, path, token, body string, headers map[string]string) response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	return response{
		status:     res.StatusCode,
		body:       raw,
		retryAfter: res.Header.Get("Retry-After"),
	}
}

type pair struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	SessionID    uuid.UUID `json:"sessionId"`
}

func (h *harness) login(t *testing.T) pair {
	t.Helper()

	res := h.do(t, "POST", "/auth/login", "",
		fmt.Sprintf(`{"emailAddress":%q,"password":%q}`, h.email, goodPassword))
	if res.status != http.StatusOK {
		t.Fatalf("login: status %d\n%s", res.status, res.body)
	}

	var p pair
	res.decode(t, &p)
	return p
}

// authenticated reports whether a token still resolves.
func (h *harness) authenticated(t *testing.T, token string) bool {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	// httptest invents an address outside every private range, which an API key
	// restricted to one would rightly refuse.
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer "+token)
	_, err := h.h.Claims(req)
	return err == nil
}

func (h *harness) events(t *testing.T, event string) int {
	t.Helper()

	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_auth_log WHERE tenant_id = $1 AND event = $2`,
		h.tenant, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoginAndSession(t *testing.T) {
	h := setup(t)
	p := h.login(t)

	if !h.authenticated(t, p.AccessToken) {
		t.Fatal("the access token should work")
	}

	// The session is rows, not a signed blob, so it can be counted.
	var tokens int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_account_token WHERE root_token_id = $1`, p.SessionID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 2 {
		t.Errorf("%d tokens in the family, want a refresh and an access", tokens)
	}
	if h.events(t, authlog.EventLoginSucceeded) != 1 {
		t.Error("the login should be in the trail")
	}
}

func TestAccessTokenExpires(t *testing.T) {
	h := setup(t)
	p := h.login(t)

	// Two seconds, from the configuration above. Waiting is the only way to
	// test an expiry the database evaluates.
	time.Sleep(2500 * time.Millisecond)

	if h.authenticated(t, p.AccessToken) {
		t.Error("an expired access token should stop working")
	}
	res := h.do(t, "POST", "/auth/refresh", "",
		fmt.Sprintf(`{"refreshToken":%q}`, p.RefreshToken))
	if res.status != http.StatusOK {
		t.Errorf("the refresh token should outlive it: status %d\n%s", res.status, res.body)
	}
}

// The signal that matters, and the one that needs a real lock to be believable.
func TestReplayRevokesTheFamily(t *testing.T) {
	h := setup(t)
	first := h.login(t)

	res := h.do(t, "POST", "/auth/refresh", "",
		fmt.Sprintf(`{"refreshToken":%q}`, first.RefreshToken))
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}
	var second pair
	res.decode(t, &second)

	// Past the leeway.
	time.Sleep(2500 * time.Millisecond)

	if res := h.do(t, "POST", "/auth/refresh", "",
		fmt.Sprintf(`{"refreshToken":%q}`, first.RefreshToken)); res.status != http.StatusUnauthorized {
		t.Fatalf("a replay should be refused, got %d", res.status)
	}

	if h.authenticated(t, second.AccessToken) {
		t.Error("the whole family should be revoked, including the live pair")
	}
	if h.events(t, authlog.EventTokenReuseDetected) != 1 {
		t.Error("the reuse should be recorded")
	}

	// And it is recorded in the rows, not only in the log.
	var live int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_account_token WHERE root_token_id = $1 AND revoked_at IS NULL`,
		first.SessionID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("%d tokens are still live", live)
	}
}

// Two tabs refreshing at the same instant. The lock is what makes this survive;
// without FOR UPDATE both would see an unconsumed token and the second would
// look like a replay.
func TestConcurrentRefreshesConverge(t *testing.T) {
	h := setup(t)
	first := h.login(t)

	const attempts = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []response
	)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := h.do(t, "POST", "/auth/refresh", "",
				fmt.Sprintf(`{"refreshToken":%q}`, first.RefreshToken))
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()

	for i, res := range results {
		if res.status != http.StatusOK {
			t.Errorf("attempt %d: status %d\n%s", i, res.status, res.body)
		}
	}

	// Nobody was signed out. Inside the leeway, a race is a race, not an attack.
	if h.events(t, authlog.EventTokenReuseDetected) != 0 {
		t.Error("concurrent refreshes should not look like reuse")
	}
	for i, res := range results {
		var p pair
		res.decode(t, &p)
		if !h.authenticated(t, p.AccessToken) {
			t.Errorf("attempt %d ended up with a dead token", i)
		}
	}
}

// The rate limiter reading the real rig_auth_log. This is the SQL no in-memory test
// exercises: the window, the join, and the clearing event.
func TestLockoutOverRealSQL(t *testing.T) {
	h := setup(t)

	for i := range 5 {
		res := h.do(t, "POST", "/auth/login", "",
			fmt.Sprintf(`{"emailAddress":%q,"password":"nope"}`, h.email))
		if res.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d\n%s", i+1, res.status, res.body)
		}
	}

	res := h.do(t, "POST", "/auth/login", "",
		fmt.Sprintf(`{"emailAddress":%q,"password":%q}`, h.email, goodPassword))
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429\n%s", res.status, res.body)
	}
	// A refusal a client cannot act on is just a wall.
	if res.retryAfter == "" {
		t.Error("the refusal should carry a Retry-After")
	}
	if h.events(t, authlog.EventAccountLocked) == 0 {
		t.Error("the lockout should be recorded")
	}
}

// A success clears the window, through the same SQL.
func TestASuccessClearsTheWindowOverRealSQL(t *testing.T) {
	h := setup(t)

	for range 4 {
		h.do(t, "POST", "/auth/login", "",
			fmt.Sprintf(`{"emailAddress":%q,"password":"nope"}`, h.email))
	}
	h.login(t)

	for i := range 4 {
		res := h.do(t, "POST", "/auth/login", "",
			fmt.Sprintf(`{"emailAddress":%q,"password":"nope"}`, h.email))
		if res.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 — the success cleared the earlier failures",
				i+1, res.status)
		}
	}
}

func TestPasswordResetOverRealSQL(t *testing.T) {
	h := setup(t)
	old := h.login(t)

	if res := h.do(t, "POST", "/auth/password/reset", "",
		fmt.Sprintf(`{"emailAddress":%q}`, h.email)); res.status != http.StatusAccepted {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}
	token := h.notify.resetToken()
	if token == "" {
		t.Fatal("a link should have been sent")
	}

	const newPassword = "an entirely different passphrase"
	res := h.do(t, "POST", "/auth/password/reset/confirm", "",
		fmt.Sprintf(`{"token":%q,"newPassword":%q}`, token, newPassword))
	if res.status != http.StatusNoContent {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	if h.authenticated(t, old.AccessToken) {
		t.Error("the sessions from before the reset should be gone")
	}
	if res := h.do(t, "POST", "/auth/login", "",
		fmt.Sprintf(`{"emailAddress":%q,"password":%q}`, h.email, newPassword)); res.status != http.StatusOK {
		t.Errorf("the new password should work: %d\n%s", res.status, res.body)
	}

	// The link is spent, and the row says so.
	if res := h.do(t, "POST", "/auth/password/reset/confirm", "",
		fmt.Sprintf(`{"token":%q,"newPassword":"yet another passphrase"}`, token)); res.status == http.StatusNoContent {
		t.Error("a consumed link should not work twice")
	}
	var consumed int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_identity_verification WHERE identity_id = $1 AND consumed_at IS NOT NULL`,
		h.identity).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 1 {
		t.Errorf("%d links consumed, want 1", consumed)
	}
}

func TestAPIKeysOverRealSQL(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	// Grant the caller the permission through the real tables, so the
	// authorization path is the one an application would use.
	grant(t, h, authhttp.PermissionManageAPIKeys)
	grant(t, h, "export.run")

	p := h.login(t)
	res := h.do(t, "POST", "/auth/api-keys", p.AccessToken,
		`{"name":"nightly export","scopes":["export.run"],"cidrAllowList":["127.0.0.0/8"]}`)
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

	if !h.authenticated(t, created.Secret) {
		t.Fatal("the key should authenticate")
	}

	// The array columns round-tripped.
	var (
		scopes []string
		allow  []string
	)
	if err := h.pool.QueryRow(ctx,
		`SELECT scopes, cidr_allow_list::text[] FROM rig_api_key WHERE id = $1`,
		created.Key.ID).Scan(&scopes, &allow); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0] != "export.run" {
		t.Errorf("scopes = %v", scopes)
	}
	if len(allow) != 1 {
		t.Errorf("cidr allow list = %v", allow)
	}

	// The secret is not recoverable from anything stored.
	var stored string
	if err := h.pool.QueryRow(ctx,
		`SELECT encode(secret_hash, 'hex') FROM rig_api_key WHERE id = $1`, created.Key.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(created.Secret, stored) || strings.Contains(stored, created.Secret) {
		t.Error("the stored value is related to the secret")
	}

	if res := h.do(t, "DELETE", "/auth/api-keys/"+created.Key.ID.String(), p.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("revoke: status %d\n%s", res.status, res.body)
	}
	if h.authenticated(t, created.Secret) {
		t.Error("a revoked key should stop working immediately")
	}
	if h.events(t, authlog.EventAPIKeyAuthSucceeded) == 0 {
		t.Error("key authentication should be recorded")
	}
}

// A key restricted to a network must be refused from outside it, and the
// address has to survive the round trip through an inet column to know.
func TestAPIKeyCIDREnforcementOverRealSQL(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	keys, err := apikey.New(apikey.Config{Store: h.stores.APIKeys, Log: h.stores.Log})
	if err != nil {
		t.Fatal(err)
	}

	minted, err := keys.Mint(ctx, apikey.MintInput{
		TenantID: h.tenant, AccountID: h.account, Name: "restricted",
		CIDRAllowList: mustPrefixes(t, "10.0.0.0/8"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := keys.Verify(ctx, minted.Secret, mustAddr(t, "10.1.2.3")); err != nil {
		t.Errorf("an address inside the range should be allowed: %v", err)
	}
	if _, _, err := keys.Verify(ctx, minted.Secret, mustAddr(t, "203.0.113.10")); err == nil {
		t.Error("an address outside the range should be refused")
	}
}

func TestSessionsOverRealSQL(t *testing.T) {
	h := setup(t)

	phone := h.login(t)
	laptop := h.login(t)

	res := h.do(t, "GET", "/auth/sessions", laptop.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var listed struct {
		Data []struct {
			ID         uuid.UUID `json:"id"`
			Current    bool      `json:"current"`
			LastUsedAt time.Time `json:"lastUsedAt"`
		} `json:"data"`
	}
	res.decode(t, &listed)

	if len(listed.Data) != 2 {
		t.Fatalf("got %d sessions, want 2\n%s", len(listed.Data), res.body)
	}
	for _, s := range listed.Data {
		if s.LastUsedAt.IsZero() {
			t.Error("last used should be derived from the newest token in the family")
		}
	}

	if res := h.do(t, "DELETE", "/auth/sessions/"+phone.SessionID.String(),
		laptop.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("revoke: status %d\n%s", res.status, res.body)
	}
	if h.authenticated(t, phone.AccessToken) {
		t.Error("the revoked session should be dead")
	}
	if !h.authenticated(t, laptop.AccessToken) {
		t.Error("the other session should be untouched")
	}

	// A revoked session drops out of the listing.
	res = h.do(t, "GET", "/auth/sessions", laptop.AccessToken, "")
	res.decode(t, &listed)
	if len(listed.Data) != 1 {
		t.Errorf("got %d sessions after revoking one, want 1", len(listed.Data))
	}
}

func TestImpersonationOverRealSQL(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	grant(t, h, authhttp.PermissionImpersonate)
	admin := h.login(t)

	target, targetIdentity := uuid.New(), uuid.New()
	address := "robin-" + target.String()[:8] + "@example.com"
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO rig_identity (id, email_address, display_name)
		VALUES ($1, $2, $3)`, targetIdentity, address, "Robin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name)
		VALUES ($1, $2, $3, $4, $5)`,
		target, h.tenant, targetIdentity, address, "Robin"); err != nil {
		t.Fatal(err)
	}

	res := h.do(t, "POST", "/auth/impersonate", admin.AccessToken,
		fmt.Sprintf(`{"accountId":%q}`, target))
	if res.status != http.StatusCreated {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}
	var as pair
	res.decode(t, &as)

	// The marker is a column, and it has to survive a rotation.
	rotated := h.do(t, "POST", "/auth/refresh", "", fmt.Sprintf(`{"refreshToken":%q}`, as.RefreshToken))
	if rotated.status != http.StatusOK {
		t.Fatalf("refresh: status %d\n%s", rotated.status, rotated.body)
	}
	var after pair
	rotated.decode(t, &after)

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+after.AccessToken)
	claims, err := h.h.Claims(req)
	if err != nil {
		t.Fatal(err)
	}
	if claims.ImpersonatedByAccountID == nil || *claims.ImpersonatedByAccountID != h.account {
		t.Error("the impersonation marker should survive a rotation")
	}

	if res := h.do(t, "DELETE", "/auth/impersonate", after.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("ending: status %d\n%s", res.status, res.body)
	}
	if h.events(t, authlog.EventImpersonationStarted) != 1 ||
		h.events(t, authlog.EventImpersonationEnded) != 1 {
		t.Error("both ends should be recorded")
	}
}

// Two tenants in one database, which is the arrangement everything else assumes.
func TestTenantsAreIsolated(t *testing.T) {
	a := setup(t)
	b := setup(t)

	pa := a.login(t)

	// b's handler resolves b's tenant, so a's token is not a credential there.
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+pa.AccessToken)
	claims, err := b.h.Claims(req)
	if err != nil {
		t.Fatal(err)
	}
	if claims.TenantID == b.tenant {
		t.Error("a token from one tenant resolved into another")
	}

	// And b cannot see a's sessions.
	pb := b.login(t)
	res := b.do(t, "DELETE", "/auth/sessions/"+pa.SessionID.String(), pb.AccessToken, "")
	if res.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.status)
	}
	if !a.authenticated(t, pa.AccessToken) {
		t.Error("the other tenant's session should be untouched")
	}
}

// grant gives the caller a permission, through the harness's own model.
func grant(t *testing.T, h *harness, permission string) {
	t.Helper()
	if h.held == nil {
		h.held = map[string]bool{}
	}
	h.held[permission] = true
}

func mustPrefixes(t *testing.T, raw ...string) []netip.Prefix {
	t.Helper()

	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func mustAddr(t *testing.T, raw string) netip.Addr {
	t.Helper()

	a, err := netip.ParseAddr(raw)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The OAuth store, against the real tables. Provisioning is two writes in one
// transaction, and the link is an upsert on a unique index — neither of which
// an in-memory store can be wrong about.
func TestOAuthIdentitiesOverRealSQL(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	store := h.stores.OAuth()

	profile := oauth.Profile{
		Subject: "google-" + uuid.NewString(), EmailAddress: h.email,
		EmailVerified: true, DisplayName: "Sam",
	}

	// Nothing yet.
	found, err := store.FindLink(ctx, oauth.ProviderGoogle, profile.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Fatal("there should be no link yet")
	}

	// The person already exists with a password, and a verified address is what
	// lets the provider be linked to them rather than to a second identity.
	identityID, err := store.FindIdentityByEmail(ctx, h.email)
	if err != nil {
		t.Fatal(err)
	}
	if identityID != h.identity {
		t.Fatalf("found %s, want the existing person", identityID)
	}

	linked, err := store.LinkIdentity(ctx, oauth.LinkInput{
		IdentityID: identityID, Provider: oauth.ProviderGoogle, Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if linked.IdentityID != h.identity {
		t.Error("the link should point at the existing person")
	}

	// Linking again is the same row, with the address refreshed. Two sign-ins
	// racing on a first link must not produce two rows or a failure.
	profile.EmailAddress = "renamed-" + h.email
	again, err := store.LinkIdentity(ctx, oauth.LinkInput{
		IdentityID: identityID, Provider: oauth.ProviderGoogle, Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != linked.ID {
		t.Error("linking twice should update the row, not add one")
	}
	if again.EmailAddress == linked.EmailAddress {
		t.Error("the address should have been refreshed")
	}

	// The link reaches the person from any tenant, because it has no tenant of
	// its own. This is what makes one Google account work in every tenant
	// somebody belongs to.
	elsewhere, err := store.FindLink(ctx, oauth.ProviderGoogle, profile.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere == nil || elsewhere.IdentityID != h.identity {
		t.Error("a provider link is global; it should be found without naming a tenant")
	}

	// The account in this tenant is what a session is issued for, and it is a
	// separate question from who they are.
	accountID, err := store.FindAccount(ctx, h.tenant, identityID)
	if err != nil {
		t.Fatal(err)
	}
	if accountID != h.account {
		t.Errorf("found account %s, want %s", accountID, h.account)
	}

	// And provisioning writes the person and their link together.
	fresh := oauth.Profile{
		Subject: "google-" + uuid.NewString(),
		// A distinct address, so this is genuinely a new person.
		EmailAddress:  "new-" + uuid.NewString()[:8] + "@example.com",
		EmailVerified: true, DisplayName: "Robin",
	}
	provisioned, err := store.ProvisionIdentity(ctx, oauth.ProvisionInput{
		Provider: oauth.ProviderGoogle, Profile: fresh,
	})
	if err != nil {
		t.Fatal(err)
	}

	var verified *time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT email_verified_at FROM rig_identity WHERE id = $1`,
		provisioned.IdentityID).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	// The provider already confirmed the address; asking again would be asking
	// somebody to prove what they just proved.
	if verified == nil {
		t.Error("a provisioned person with a verified address should arrive verified")
	}

	// They are somebody, and nobody here yet: provisioning an identity grants
	// nothing, and joining a tenant is the separate step that does.
	none, err := store.FindAccount(ctx, h.tenant, provisioned.IdentityID)
	if err != nil {
		t.Fatal(err)
	}
	if none != uuid.Nil {
		t.Error("a new person should not have an account in this tenant until they join one")
	}

	joined, err := store.JoinTenant(ctx, oauth.JoinInput{
		TenantID: h.tenant, IdentityID: provisioned.IdentityID, Profile: fresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	if joined == uuid.Nil {
		t.Fatal("joining should have produced an account")
	}

	var identity uuid.UUID
	if err := h.pool.QueryRow(ctx,
		`SELECT identity_id FROM rig_account WHERE id = $1`, joined).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if identity != provisioned.IdentityID {
		t.Error("the new account should belong to the person who was provisioned")
	}
}

// One person in two tenants, against the real schema.
//
// The in-memory suite proves the rules; what can only be wrong here is the SQL —
// the global unique index on the address, the per-tenant unique on the person,
// and the one query that deliberately has no tenant predicate because its whole
// job is to reach the tenants the caller is not in.
func TestOnePersonInTwoTenantsOverRealSQL(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	// A second tenant, and the same person joining it. No second identity: the
	// address is globally unique, and inserting one would be refused.
	second := uuid.New()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO rig_tenant (id, name, slug) VALUES ($1, $2, $3)`,
		second, "Other", second.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO rig_identity (id, email_address, display_name) VALUES ($1, $2, $3)`,
		uuid.New(), h.email, "An impostor"); err == nil {
		t.Error("a second person with the same address should be refused by the database")
	}

	elsewhereAccount := uuid.New()
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name, role)
		VALUES ($1, $2, $3, $4, $5, 'Basic')`,
		elsewhereAccount, second, h.identity, h.email, "Sam elsewhere"); err != nil {
		t.Fatal(err)
	}

	// The role is per account, so the same person is an Owner here and Basic
	// there. This is the thing the old model could not express at all.
	if _, err := h.pool.Exec(ctx,
		`UPDATE rig_account SET role = 'Owner' WHERE id = $1`, h.account); err != nil {
		t.Fatal(err)
	}

	here, err := h.accounts.Login(ctx, account.LoginInput{
		TenantID: h.tenant, EmailAddress: h.email, Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	there, err := h.accounts.Login(ctx, account.LoginInput{
		TenantID: second, EmailAddress: h.email, Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatalf("the same password should sign in to the second tenant: %v", err)
	}

	// Each session belongs to the account in its own tenant.
	first, err := h.sessions.Verify(ctx, here.Session.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.sessions.Verify(ctx, there.Session.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != h.account || other.AccountID != elsewhereAccount {
		t.Error("the sessions should belong to the accounts of their own tenants")
	}

	// A second account for the same person in the same tenant is refused by the
	// partial unique index, not by anything in Go.
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name)
		VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), second, h.identity, h.email, "Twice"); err == nil {
		t.Error("joining the same tenant twice should be refused by the database")
	}

	// And changing the password ends both sessions, which is the query with no
	// tenant predicate doing its job.
	if _, err := h.accounts.ChangePassword(ctx, account.ChangePasswordInput{
		TenantID: h.tenant, AccountID: h.account,
		CurrentPassword: goodPassword, NewPassword: "a completely different passphrase",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sessions.Verify(ctx, there.Session.Access.Token); err == nil {
		t.Error("the session in the other tenant should have been revoked too")
	}
}

// A response built in memory is UTC too, which is the half a scan cannot settle.
//
// Minting a key and issuing a session both answer with a row this process just
// built, without reading it back — so the normalization on the scan path never
// touches them, and both carried the host's offset until the clock itself was
// wrapped. Worth its own test because the obvious one passes without it.
func TestAConstructedInstantIsUTC(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	// A session, straight from the manager.
	pair, err := h.accounts.Login(ctx, account.LoginInput{
		TenantID: h.tenant, EmailAddress: h.email, Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		got  time.Time
	}{
		{"access expiry", pair.Session.Access.ExpiresAt},
		{"refresh expiry", pair.Session.Refresh.ExpiresAt},
	} {
		if tc.got.Location() != time.UTC {
			t.Errorf("%s is in %s, want UTC", tc.name, tc.got.Location())
		}
	}

	// And a key, which never round-trips at all.
	keys, err := apikey.New(apikey.Config{Store: h.stores.APIKeys, Log: h.stores.Log})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := keys.Mint(ctx, apikey.MintInput{
		TenantID: h.tenant, AccountID: h.account, Name: "Instants",
	})
	if err != nil {
		t.Fatal(err)
	}
	if minted.Key.CreatedAt.Location() != time.UTC {
		t.Errorf("a minted key's createdAt is in %s, want UTC", minted.Key.CreatedAt.Location())
	}
}

// The auth package writes its own SQL, so the rule that every instant leaves the
// database in UTC has to hold there too — the JSON these endpoints return carries
// a timestamp, and a response whose offset depends on the host's TZ is a response
// two replicas disagree about.
func TestTheAuthEndpointsReturnUTC(t *testing.T) {
	h := setup(t)
	pair := h.login(t)

	res := h.do(t, "GET", "/auth/sessions", pair.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var body struct {
		Data []struct {
			CreatedAt  string `json:"createdAt"`
			ExpiresAt  string `json:"expiresAt"`
			LastUsedAt string `json:"lastUsedAt"`
		} `json:"data"`
	}
	res.decode(t, &body)
	if len(body.Data) == 0 {
		t.Fatal("the session that just signed in should be listed")
	}

	for _, tc := range []struct{ name, got string }{
		{"createdAt", body.Data[0].CreatedAt},
		{"expiresAt", body.Data[0].ExpiresAt},
		{"lastUsedAt", body.Data[0].LastUsedAt},
	} {
		// Z rather than an offset. RFC 3339 with any offset names the same
		// instant, so this is about the answer being the same everywhere rather
		// than about it being correct.
		if !strings.HasSuffix(tc.got, "Z") {
			t.Errorf("%s = %q, want a UTC instant ending in Z", tc.name, tc.got)
		}
	}
}

// A session payload is the application's own context, and this is the half the
// in-memory tests cannot check: that it round-trips through a jsonb column, that
// NULL comes back as nothing rather than as invalid JSON, and that a rotation
// carries it in the database rather than only in a struct copy.
func TestASessionPayloadRoundTripsThroughPostgres(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	type deviceContext struct {
		Device    string `json:"device"`
		SteppedUp bool   `json:"steppedUp"`
	}
	want := deviceContext{Device: "laptop", SteppedUp: true}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	pair, err := h.sessions.Issue(ctx, session.IssueInput{
		TenantID: h.tenant, AccountID: h.account, Payload: raw,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Out of the column, into the application's own type, the way a handler
	// reaches it.
	tok, err := h.sessions.Verify(ctx, pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tenancy.Extra[deviceContext](tenancy.Claims{Extra: tok.Payload})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("payload = %+v, want %+v", got, want)
	}

	// Postgres stores jsonb normalized, so comparing bytes would be comparing
	// whitespace. What has to survive is the value.
	rotated, err := h.sessions.Rotate(ctx, pair.Refresh.Token)
	if err != nil {
		t.Fatal(err)
	}
	after, err := h.sessions.Verify(ctx, rotated.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	got, err = tenancy.Extra[deviceContext](tenancy.Claims{Extra: after.Payload})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("after rotation the payload is %+v, want %+v", got, want)
	}

	// A session nobody set one on stores NULL, not an empty string — which is
	// what a naive insert of a nil json.RawMessage would write, and which is not
	// valid jsonb.
	plain, err := h.sessions.Issue(ctx, session.IssueInput{
		TenantID: h.tenant, AccountID: h.account,
	})
	if err != nil {
		t.Fatal(err)
	}
	bare, err := h.sessions.Verify(ctx, plain.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Payload != nil {
		t.Errorf("a session with no payload came back with %q", bare.Payload)
	}

	var isNull bool
	if err := h.pool.QueryRow(ctx,
		`SELECT payload IS NULL FROM rig_account_token WHERE id = $1`,
		plain.Access.TokenID).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("an absent payload should be NULL in the column")
	}
}

// The state that could not exist before this: signed in, belonging nowhere.
//
// It is the whole reason for a second credential. A person with an invitation
// waiting has an account and no tenant, and the old answer to their sign-in was
// 403 — which made accepting the invitation impossible, because accepting it needs
// somebody signed in. What they get now is a token that proves who they are and
// scopes nothing, and a list of where they have been asked to go.
func TestSigningInWithNoTenant(t *testing.T) {
	h := setup(t)

	// Somebody real, with a password and no account anywhere.
	stranger := "wanderer-" + uuid.New().String()[:8] + "@example.com"
	res := h.doUnscoped(t, "POST", "/auth/register", "",
		`{"emailAddress":"`+stranger+`","displayName":"Wanderer","password":"`+goodPassword+`"}`)
	if res.status != http.StatusCreated {
		t.Fatalf("register: %d %s", res.status, res.body)
	}

	var signedUp struct {
		AccessToken   string `json:"accessToken"`
		IdentityToken string `json:"identityToken"`
		Tenants       []any  `json:"tenants"`
	}
	res.decode(t, &signedUp)

	if signedUp.IdentityToken == "" {
		t.Fatal("registering should hand back the tenant-less credential")
	}
	if signedUp.AccessToken != "" {
		t.Error("there is no tenant, so there should be no tenant session")
	}
	if len(signedUp.Tenants) != 0 {
		t.Errorf("tenants = %v, want none", signedUp.Tenants)
	}
	if !strings.HasPrefix(signedUp.IdentityToken, session.PrefixIdentity) {
		t.Errorf("the identity token should be identifiable on sight: %q", signedUp.IdentityToken)
	}

	t.Run("signing in again answers the same way", func(t *testing.T) {
		res := h.doUnscoped(t, "POST", "/auth/login", "",
			`{"emailAddress":"`+stranger+`","password":"`+goodPassword+`"}`)
		if res.status != http.StatusOK {
			t.Fatalf("login with no tenant: %d %s, want 200", res.status, res.body)
		}
		var out struct {
			AccessToken   string `json:"accessToken"`
			IdentityToken string `json:"identityToken"`
		}
		res.decode(t, &out)
		if out.IdentityToken == "" || out.AccessToken != "" {
			t.Errorf("want an identity token and no session: %s", res.body)
		}
	})

	t.Run("it can see the picker and nothing else", func(t *testing.T) {
		for _, path := range []string{"/auth/me/tenants", "/auth/me/invitations"} {
			if res := h.do(t, "GET", path, signedUp.IdentityToken, ""); res.status != http.StatusOK {
				t.Errorf("GET %s: %d %s", path, res.status, res.body)
			}
		}

		// And it is not a session. The endpoints that need a tenant refuse it,
		// by shape rather than by a check somebody remembered to add.
		for _, path := range []string{"/auth/sessions", "/auth/tenants", "/auth/api-keys"} {
			if res := h.do(t, "GET", path, signedUp.IdentityToken, ""); res.status != http.StatusUnauthorized {
				t.Errorf("GET %s with an identity token: %d %s, want 401", path, res.status, res.body)
			}
		}
	})

	t.Run("a tenant token is refused at the picker", func(t *testing.T) {
		// The other direction, and the reason the two prefixes exist. A session
		// token here would usually name the right person; accepting it would make
		// the credentials interchangeable.
		p := h.login(t)
		for _, path := range []string{"/auth/me/tenants", "/auth/me/invitations"} {
			if res := h.do(t, "GET", path, p.AccessToken, ""); res.status != http.StatusUnauthorized {
				t.Errorf("GET %s with a session token: %d %s, want 401", path, res.status, res.body)
			}
		}
	})

	t.Run("an invitation shows up, named", func(t *testing.T) {
		// Invited by somebody who can, into the tenant they run.
		grant(t, h, account.PermissionProvision)
		p := h.login(t)
		// Inviting is provisioning with a link: the account is made and a
		// single-use token is sent, rather than a password being set for somebody.
		res := h.do(t, "POST", "/auth/accounts", p.AccessToken,
			`{"emailAddress":"`+stranger+`","displayName":"Wanderer","invite":true}`)
		if res.status != http.StatusCreated {
			t.Fatalf("invite: %d %s", res.status, res.body)
		}

		res = h.do(t, "GET", "/auth/me/invitations", signedUp.IdentityToken, "")
		if res.status != http.StatusOK {
			t.Fatalf("my invitations: %d %s", res.status, res.body)
		}
		var page struct {
			Data []struct {
				TenantID   uuid.UUID `json:"tenantId"`
				TenantName string    `json:"tenantName"`
				Token      string    `json:"token"`
			} `json:"data"`
		}
		res.decode(t, &page)

		if len(page.Data) != 1 {
			t.Fatalf("got %d invitations, want 1: %s", len(page.Data), res.body)
		}
		if page.Data[0].TenantID != h.tenant {
			t.Errorf("the invitation is into %s, want %s", page.Data[0].TenantID, h.tenant)
		}
		// The name is the part somebody who has never been there recognises.
		if page.Data[0].TenantName == "" {
			t.Error("an invitation should name the tenant")
		}
		// And not the token. Being able to see an invitation is not being able to
		// redeem one: the token went to an address, and that is the claim it makes.
		if page.Data[0].Token != "" {
			t.Error("listing invitations must not hand out their tokens")
		}
	})

	t.Run("signing out of the picker ends it", func(t *testing.T) {
		if res := h.do(t, "DELETE", "/auth/me/session", signedUp.IdentityToken, ""); res.status != http.StatusNoContent {
			t.Fatalf("end the identity session: %d %s", res.status, res.body)
		}
		if res := h.do(t, "GET", "/auth/me/tenants", signedUp.IdentityToken, ""); res.status != http.StatusUnauthorized {
			t.Errorf("a revoked identity session: %d, want 401", res.status)
		}
	})
}

// The two ways out of the picker: join a tenant you were invited to, or make
// one. Both end the same way — a tenant session, and a list with it in.
func TestLeavingThePicker(t *testing.T) {
	h := setup(t)

	// newcomer registers somebody and hands back the credential they hold: signed
	// in, belonging nowhere, looking at the picker.
	newcomer := func(t *testing.T) (token, address string) {
		t.Helper()
		address = "newcomer-" + uuid.New().String()[:8] + "@example.com"
		res := h.doUnscoped(t, "POST", "/auth/register", "",
			`{"emailAddress":"`+address+`","displayName":"Newcomer","password":"`+goodPassword+`"}`)
		if res.status != http.StatusCreated {
			t.Fatalf("register: %d %s", res.status, res.body)
		}
		var out struct {
			IdentityToken string `json:"identityToken"`
		}
		res.decode(t, &out)
		return out.IdentityToken, address
	}

	t.Run("joining one you were invited to", func(t *testing.T) {
		token, email := newcomer(t)

		// Nothing waiting yet, and nowhere to be.
		invited := h.do(t, "GET", "/auth/me/invitations", token, "")
		var before struct {
			Data []struct{ ID uuid.UUID } `json:"data"`
		}
		invited.decode(t, &before)
		if len(before.Data) != 0 {
			t.Fatalf("a brand new person has no invitations: %s", invited.body)
		}

		// Invited into the harness's tenant by somebody who may.
		grant(t, h, account.PermissionProvision)
		p := h.login(t)
		if res := h.do(t, "POST", "/auth/accounts", p.AccessToken,
			`{"emailAddress":"`+email+`","displayName":"Newcomer","invite":true}`); res.status != http.StatusCreated {
			t.Fatalf("invite: %d %s", res.status, res.body)
		}

		listed := h.do(t, "GET", "/auth/me/invitations", token, "")
		var page struct {
			Data []struct {
				ID uuid.UUID `json:"id"`
			} `json:"data"`
		}
		listed.decode(t, &page)
		if len(page.Data) != 1 {
			t.Fatalf("got %d invitations, want 1: %s", len(page.Data), listed.body)
		}

		// The identifier is enough. Being signed in as the person invited is a
		// stronger claim than holding the token that was emailed to them.
		joined := h.do(t, "POST", "/auth/me/invitations/accept", token,
			`{"invitationId":"`+page.Data[0].ID.String()+`"}`)
		if joined.status != http.StatusOK {
			t.Fatalf("accept: %d %s", joined.status, joined.body)
		}
		var out struct {
			AccessToken string `json:"accessToken"`
			Tenants     []struct {
				TenantID uuid.UUID `json:"tenantId"`
				Current  bool      `json:"current"`
			} `json:"tenants"`
		}
		joined.decode(t, &out)

		if out.AccessToken == "" {
			t.Fatal("joining should hand back a tenant session")
		}
		if len(out.Tenants) != 1 || out.Tenants[0].TenantID != h.tenant {
			t.Errorf("tenants = %+v, want the one they joined", out.Tenants)
		}
		if !out.Tenants[0].Current {
			t.Error("the tenant just joined is the one they are in")
		}

		// And the session works where the identity token did not.
		if res := h.do(t, "GET", "/auth/sessions", out.AccessToken, ""); res.status != http.StatusOK {
			t.Errorf("the new session should reach a tenant endpoint: %d %s", res.status, res.body)
		}

		// Twice is refused. The link is consumed, and the answer says nothing
		// about which of the several ways it could be invalid applies.
		again := h.do(t, "POST", "/auth/me/invitations/accept", token,
			`{"invitationId":"`+page.Data[0].ID.String()+`"}`)
		if again.status != http.StatusBadRequest {
			t.Errorf("accepting twice: %d %s, want 400", again.status, again.body)
		}
	})

	t.Run("making one instead", func(t *testing.T) {
		token, _ := newcomer(t)

		res := h.do(t, "POST", "/auth/tenants", token, `{"name":"A tenant of one"}`)
		if res.status != http.StatusOK {
			t.Fatalf("create a tenant: %d %s", res.status, res.body)
		}
		var out struct {
			AccessToken string `json:"accessToken"`
			Tenants     []struct {
				TenantName string `json:"tenantName"`
				Role       string `json:"role"`
				Current    bool   `json:"current"`
			} `json:"tenants"`
		}
		res.decode(t, &out)

		if out.AccessToken == "" {
			t.Fatal("making a tenant should sign them into it")
		}
		if len(out.Tenants) != 1 {
			t.Fatalf("tenants = %+v, want the one just made", out.Tenants)
		}
		if got := out.Tenants[0]; got.TenantName != "A tenant of one" ||
			got.Role != "Owner" || !got.Current {
			t.Errorf("tenant = %+v", got)
		}

		// A nameless one is refused before the hook is reached.
		if res := h.do(t, "POST", "/auth/tenants", token, `{"name":"  "}`); res.status != http.StatusUnprocessableEntity {
			t.Errorf("a nameless tenant: %d %s, want 422", res.status, res.body)
		}
	})

	t.Run("a tenant session cannot use the picker's exits", func(t *testing.T) {
		p := h.login(t)
		for _, path := range []string{"/auth/tenants", "/auth/me/invitations/accept"} {
			if res := h.do(t, "POST", path, p.AccessToken, `{"name":"x"}`); res.status != http.StatusUnauthorized {
				t.Errorf("POST %s with a session token: %d %s, want 401", path, res.status, res.body)
			}
		}
	})
}

// Two permissions, because minting the two kinds of key are different acts.
//
// A personal key is intersected with its owner's grants every time it is used, so
// making one grants nothing they did not already hold — it is a second way to
// present the same authority. A service key acts as an account nobody signs in to,
// with scopes of its own, which is authority creation. An ordinary member gets the
// first and not the second.
func TestTheTwoAPIKeyPermissions(t *testing.T) {
	h := setup(t)

	t.Run("holding neither, nothing works", func(t *testing.T) {
		p := h.login(t)
		for _, tc := range []struct{ method, path, body string }{
			{"GET", "/auth/api-keys", ""},
			{"POST", "/auth/api-keys", `{"name":"nope","kind":"Personal"}`},
		} {
			res := h.do(t, tc.method, tc.path, p.AccessToken, tc.body)
			if res.status != http.StatusForbidden {
				t.Errorf("%s %s: %d %s, want 403", tc.method, tc.path, res.status, res.body)
			}
		}
	})

	t.Run("apikey.own mints a personal key and not a service one", func(t *testing.T) {
		grant(t, h, authhttp.PermissionOwnAPIKey)
		p := h.login(t)

		mine := h.do(t, "POST", "/auth/api-keys", p.AccessToken,
			`{"name":"my laptop","kind":"Personal"}`)
		if mine.status != http.StatusCreated {
			t.Fatalf("a personal key: %d %s", mine.status, mine.body)
		}
		var minted struct {
			Key struct {
				ID uuid.UUID `json:"id"`
			} `json:"key"`
			Secret string `json:"secret"`
		}
		mine.decode(t, &minted)
		if minted.Secret == "" {
			t.Error("the secret is shown once, and this was the once")
		}

		// The service kind is the administrative one, and it is refused.
		service := h.do(t, "POST", "/auth/api-keys", p.AccessToken,
			`{"name":"an integration","kind":"Integration"}`)
		if service.status != http.StatusForbidden {
			t.Errorf("a service key: %d %s, want 403", service.status, service.body)
		}
		if !strings.Contains(string(service.body), authhttp.PermissionManageAPIKeys) {
			t.Errorf("the refusal should name what to ask for: %s", service.body)
		}

		// So is minting one that acts as somebody else, which is the same act
		// wearing the personal kind's clothes.
		asSomebodyElse := h.do(t, "POST", "/auth/api-keys", p.AccessToken,
			`{"name":"not mine","kind":"Personal","serviceAccountId":"`+uuid.New().String()+`"}`)
		if asSomebodyElse.status != http.StatusForbidden {
			t.Errorf("a key acting as another account: %d %s, want 403",
				asSomebodyElse.status, asSomebodyElse.body)
		}

		// It can see and revoke its own.
		listed := h.do(t, "GET", "/auth/api-keys", p.AccessToken, "")
		if listed.status != http.StatusOK {
			t.Fatalf("listing: %d %s", listed.status, listed.body)
		}
		var page struct {
			Data []struct {
				ID uuid.UUID `json:"id"`
			} `json:"data"`
		}
		listed.decode(t, &page)
		var found bool
		for _, k := range page.Data {
			if k.ID == minted.Key.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("somebody who may mint a key must be able to see it: %s", listed.body)
		}

		if res := h.do(t, "DELETE", "/auth/api-keys/"+minted.Key.ID.String(),
			p.AccessToken, ""); res.status != http.StatusNoContent {
			t.Errorf("revoking their own: %d %s", res.status, res.body)
		}
	})

	t.Run("somebody else's key is not there", func(t *testing.T) {
		// A service account of its own, so the key genuinely belongs to somebody
		// else. An Integration key with no account named acts as its creator, which
		// would make this test pass for the wrong reason.
		service := uuid.New()
		if _, err := h.pool.Exec(context.Background(), `
			INSERT INTO rig_account (id, tenant_id, created_at, kind, role, email_address,
			                     display_name, is_active)
			VALUES ($1, $2, now(), 'Service', 'Basic', $3, 'Nightly', true)`,
			service, h.tenant, "nightly-"+service.String()[:8]+"@example.com"); err != nil {
			t.Fatal(err)
		}

		// An integration's key, made by an administrator.
		grant(t, h, authhttp.PermissionManageAPIKeys)
		admin := h.login(t)
		res := h.do(t, "POST", "/auth/api-keys", admin.AccessToken,
			`{"name":"nightly","kind":"Integration","serviceAccountId":"`+service.String()+`"}`)
		if res.status != http.StatusCreated {
			t.Fatalf("a service key: %d %s", res.status, res.body)
		}
		var theirs struct {
			Key struct {
				ID uuid.UUID `json:"id"`
			} `json:"key"`
		}
		res.decode(t, &theirs)

		// Now the same caller with only the narrow permission. A 404 rather than a
		// 403: a refusal would confirm the key exists to somebody who may not see
		// it, which is the rule every cross-tenant read follows.
		h.held = map[string]bool{authhttp.PermissionOwnAPIKey: true}
		member := h.login(t)

		if got := h.do(t, "DELETE", "/auth/api-keys/"+theirs.Key.ID.String(), member.AccessToken, ""); got.status != http.StatusNotFound {
			t.Errorf("revoking somebody else's: %d %s, want 404", got.status, got.body)
		}

		listed := h.do(t, "GET", "/auth/api-keys", member.AccessToken, "")
		if strings.Contains(string(listed.body), theirs.Key.ID.String()) {
			t.Errorf("the narrow list should not include it: %s", listed.body)
		}
	})
}

// The hooks an application customises tenant creation with.
//
// rig writes the tenant, the first account and the slug. Who may, what a name is
// allowed to be, and what else a new tenant needs are three optional functions —
// which is the difference between configuring the auth package and writing a
// service beside it.
func TestTheTenantHooks(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	newcomer := func(t *testing.T, at string) string {
		t.Helper()
		res := h.doUnscoped(t, "POST", "/auth/register", "",
			`{"emailAddress":"`+at+`","displayName":"Somebody","password":"`+goodPassword+`"}`)
		if res.status != http.StatusCreated {
			t.Fatalf("register: %d %s", res.status, res.body)
		}
		var out struct {
			IdentityToken string `json:"identityToken"`
		}
		res.decode(t, &out)
		return out.IdentityToken
	}

	t.Run("Allow refuses on the address", func(t *testing.T) {
		// The rule the whole hook exists for: only one domain may create tenants.
		h.tenants = account.TenantOptions{
			Allow: func(_ context.Context, by account.Creator) error {
				if !strings.HasSuffix(by.EmailAddress, "@rig.app") {
					return rigerr.Forbidden("only rig.app may create tenants")
				}
				return nil
			},
		}
		h.rebuild(t)

		outsider := newcomer(t, "outsider-"+uuid.New().String()[:8]+"@example.com")
		res := h.do(t, "POST", "/auth/tenants", outsider, `{"name":"Nope"}`)
		if res.status != http.StatusForbidden {
			t.Fatalf("an outsider: %d %s, want 403", res.status, res.body)
		}
		if !strings.Contains(string(res.body), "only rig.app") {
			t.Errorf("the application's own message should reach the client: %s", res.body)
		}

		// And nothing was written. A refusal that left a tenant behind would be
		// worse than no rule at all.
		var made int
		if err := h.pool.QueryRow(ctx,
			`SELECT count(*) FROM rig_tenant WHERE name = 'Nope'`).Scan(&made); err != nil {
			t.Fatal(err)
		}
		if made != 0 {
			t.Errorf("%d tenants written by a refused request", made)
		}

		insider := newcomer(t, "insider-"+uuid.New().String()[:8]+"@rig.app")
		if res := h.do(t, "POST", "/auth/tenants", insider, `{"name":"Rig"}`); res.status != http.StatusOK {
			t.Errorf("somebody at rig.app: %d %s", res.status, res.body)
		}
	})

	t.Run("Validate refuses and may rewrite", func(t *testing.T) {
		h.tenants = account.TenantOptions{
			Validate: func(_ context.Context, d *account.TenantDraft) error {
				if r, _ := utf8.DecodeRuneInString(d.Name); !unicode.IsUpper(r) {
					return rigerr.Invalid("a tenant's name has to start with a capital letter")
				}
				// Normalizing rather than refusing, on a different field, to show
				// the pointer is not decoration.
				d.AllowedEmailDomains = []string{"rig.app"}
				return nil
			},
		}
		h.rebuild(t)

		who := newcomer(t, "namer-"+uuid.New().String()[:8]+"@example.com")

		if res := h.do(t, "POST", "/auth/tenants", who, `{"name":"lowercase"}`); res.status != http.StatusUnprocessableEntity {
			t.Errorf("a lowercase name: %d %s, want 422", res.status, res.body)
		}

		res := h.do(t, "POST", "/auth/tenants", who, `{"name":"Capitalised"}`)
		if res.status != http.StatusOK {
			t.Fatalf("a capitalised name: %d %s", res.status, res.body)
		}

		// What the hook wrote into the draft is what landed in the row.
		var domains []string
		if err := h.pool.QueryRow(ctx,
			`SELECT allowed_email_domains FROM rig_tenant WHERE name = 'Capitalised'`).Scan(&domains); err != nil {
			t.Fatal(err)
		}
		if len(domains) != 1 || domains[0] != "rig.app" {
			t.Errorf("allowed_email_domains = %v, want the hook's value", domains)
		}
	})

	t.Run("OnCreated runs in the transaction and can undo it", func(t *testing.T) {
		var sawTenant, sawAccount uuid.UUID
		h.tenants = account.TenantOptions{
			OnCreated: func(ctx context.Context, made account.NewTenant) error {
				sawTenant, sawAccount = made.TenantID, made.AccountID

				// The tenant and its first account are already there, in the
				// transaction this hook is inside — which is what makes seeding
				// roles here safe.
				tx, ok := dbx.Tx(ctx)
				if !ok {
					return errors.New("expected a transaction")
				}
				var n int
				if err := tx.QueryRow(ctx,
					`SELECT count(*) FROM rig_account WHERE tenant_id = $1`, made.TenantID).Scan(&n); err != nil {
					return err
				}
				if n != 1 {
					return fmt.Errorf("expected one account, found %d", n)
				}
				if made.TenantName == "Doomed" {
					return errors.New("the application said no")
				}
				return nil
			},
		}
		h.rebuild(t)

		who := newcomer(t, "seeder-"+uuid.New().String()[:8]+"@example.com")

		if res := h.do(t, "POST", "/auth/tenants", who, `{"name":"Seeded"}`); res.status != http.StatusOK {
			t.Fatalf("with a hook: %d %s", res.status, res.body)
		}
		if sawTenant == uuid.Nil || sawAccount == uuid.Nil {
			t.Error("the hook should be told what was made")
		}

		// A hook that fails takes the tenant with it. This is the whole reason it
		// runs inside the transaction rather than after: a tenant whose roles
		// failed to seed is a tenant whose Owner can do nothing.
		res := h.do(t, "POST", "/auth/tenants", who, `{"name":"Doomed"}`)
		if res.status == http.StatusOK {
			t.Fatal("a failing hook should fail the request")
		}
		var left int
		if err := h.pool.QueryRow(ctx,
			`SELECT count(*) FROM rig_tenant WHERE name = 'Doomed'`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Errorf("%d tenants survived a failing hook", left)
		}
	})

	t.Run("the slug is unique without a hook", func(t *testing.T) {
		h.tenants = account.TenantOptions{}
		h.rebuild(t)

		// Two customers with the same name is ordinary, and the column is unique.
		for range 2 {
			who := newcomer(t, "same-"+uuid.New().String()[:8]+"@example.com")
			if res := h.do(t, "POST", "/auth/tenants", who, `{"name":"Acme"}`); res.status != http.StatusOK {
				t.Fatalf("two tenants called Acme: %d %s", res.status, res.body)
			}
		}

		var slugs []string
		rows, err := h.pool.Query(ctx, `SELECT slug FROM rig_tenant WHERE name = 'Acme'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			slugs = append(slugs, s)
		}
		if len(slugs) != 2 || slugs[0] == slugs[1] {
			t.Errorf("slugs = %v, want two distinct", slugs)
		}
		for _, s := range slugs {
			if !strings.HasPrefix(s, "acme-") {
				t.Errorf("slug %q should be derived from the name", s)
			}
		}
	})
}

// rebuild reconfigures the auth package with the fixture's current tenant hooks.
//
// They are configuration, not state: changing them means a new account service and
// a new handler over it, which is what a deployment restarting with a different
// policy does.
func (h *harness) rebuild(t *testing.T) {
	t.Helper()
	h.mount(h.build(h.tenants))
}
