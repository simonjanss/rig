//go:build docker

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/services/authz"
	"github.com/simonjanss/rig/examples/auth/services/note"
	"github.com/simonjanss/rig/examples/auth/services/outbox"
)

// This is the walk-through the README describes, run against a real database:
// sign in, use what came back, refresh it, sign out, and find that it no longer
// works. What it is really checking is that the two halves agree — the auth
// endpoints issue the token, and the generated handlers believe it.
func TestTheSessionFlow(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	var pair struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		SessionID    string `json:"sessionId"`
	}

	t.Run("signing in returns a pair", func(t *testing.T) {
		res := api.verify(t, tenant, SeedEmail, api.askForCode(t, tenant, SeedEmail))
		if res.status != http.StatusOK {
			t.Fatalf("sign in: %d %s", res.status, res.body)
		}
		res.decode(t, &pair)

		// Opaque, not a JWT: the prefix says which kind it is and the rest is
		// half identifier and half secret, so revoking one is a row update
		// rather than a key rotation.
		if !strings.HasPrefix(pair.AccessToken, "rig_at_") {
			t.Errorf("unexpected access token: %s", pair.AccessToken)
		}
		if !strings.HasPrefix(pair.RefreshToken, "rig_rt_") {
			t.Errorf("unexpected refresh token: %s", pair.RefreshToken)
		}
	})

	t.Run("the generated endpoints accept it", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/api/v1/notes", token: pair.AccessToken,
			body: map[string]any{"title": "Written while signed in"},
		})
		if res.status != http.StatusCreated {
			t.Fatalf("create a note: %d %s", res.status, res.body)
		}

		// The tenant and the author are stamped from the claims, not from the
		// request: a caller cannot write a row into somebody else's tenant or
		// attribute it to somebody else.
		var note struct {
			TenantID           string `json:"tenantId"`
			CreatedByAccountID string `json:"createdByAccountId"`
		}
		res.decode(t, &note)
		if note.TenantID != tenant.String() {
			t.Errorf("tenant = %s, want %s", note.TenantID, tenant)
		}
		if note.CreatedByAccountID == "" {
			t.Error("the note should be attributed to the account that wrote it")
		}
	})

	t.Run("without a credential they refuse", func(t *testing.T) {
		if res := api.do(t, request{method: http.MethodGet, path: "/api/v1/notes"}); res.status != http.StatusUnauthorized {
			t.Errorf("no token: %d, want 401", res.status)
		}
		if res := api.do(t, request{
			method: http.MethodGet, path: "/api/v1/notes", token: "rig_at_nonsense.nonsense",
		}); res.status != http.StatusUnauthorized {
			t.Errorf("a made-up token: %d, want 401", res.status)
		}
	})

	t.Run("refreshing rotates the pair", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/refresh", tenant: tenant,
			body: map[string]any{"refreshToken": pair.RefreshToken},
		})
		if res.status != http.StatusOK {
			t.Fatalf("refresh: %d %s", res.status, res.body)
		}

		var next struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		}
		res.decode(t, &next)
		if next.RefreshToken == pair.RefreshToken {
			t.Error("a refresh must not hand back the token it consumed")
		}

		// The new one works and the old one is spent. Rotation is what makes a
		// stolen refresh token a detectable event rather than a permanent one.
		if res := api.do(t, request{
			method: http.MethodGet, path: "/api/v1/notes", token: next.AccessToken,
		}); res.status != http.StatusOK {
			t.Errorf("the rotated access token should work: %d", res.status)
		}
		pair.AccessToken, pair.RefreshToken = next.AccessToken, next.RefreshToken
	})

	t.Run("signing out ends the session", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/logout", token: pair.AccessToken,
		})
		if res.status != http.StatusOK && res.status != http.StatusNoContent {
			t.Fatalf("logout: %d %s", res.status, res.body)
		}

		if res := api.do(t, request{
			method: http.MethodGet, path: "/api/v1/notes", token: pair.AccessToken,
		}); res.status != http.StatusUnauthorized {
			t.Errorf("the token should be dead: %d, want 401", res.status)
		}
	})
}

// Permissions are derived from the schema and checked in the generated handler,
// so an authenticated caller with no grants reaches nothing.
//
// That is a deliberate posture and worth pinning: being signed in is not being
// allowed. The answer names the key it wanted, so somebody who legitimately lacks
// it knows what to ask for.
func TestPermissionsAreRequiredAndDerived(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	// A second person in the same tenant, with no role.
	reader := "bob-" + uuid.New().String()[:8] + "@example.com"
	api.addPerson(t, tenant, reader, "Bob")

	token := api.login(t, tenant, reader)

	for _, tc := range []struct {
		name   string
		req    request
		wanted string
	}{
		{"reading", request{method: http.MethodGet, path: "/api/v1/notes"}, "note.read"},
		{"writing", request{
			method: http.MethodPost, path: "/api/v1/notes",
			body: map[string]any{"title": "Not allowed"},
		}, "note.write"},
	} {
		tc.req.token = token
		res := api.do(t, tc.req)

		// 403 and not 401: the caller is known and simply not allowed, which is a
		// different thing to fix.
		if res.status != http.StatusForbidden {
			t.Errorf("%s without a grant: %d %s, want 403", tc.name, res.status, res.body)
		}
		if !strings.Contains(res.body, tc.wanted) {
			t.Errorf("%s: the answer should name %q: %s", tc.name, tc.wanted, res.body)
		}
	}

	// The seeded Owner holds every derived key, so the same requests work.
	owner := api.login(t, tenant, SeedEmail)
	if res := api.do(t, request{
		method: http.MethodGet, path: "/api/v1/notes", token: owner,
	}); res.status != http.StatusOK {
		t.Errorf("the owner should read: %d %s", res.status, res.body)
	}
}

// One person, two tenants, one password — and a token that only reaches the
// tenant it was issued for. This is the whole point of the identity split, and
// the isolation it must not cost.
func TestATokenIsScopedToItsTenant(t *testing.T) {
	api := newServer(t)
	first := api.seed(t)

	second := uuid.New()
	api.exec(t, `INSERT INTO rig_tenant (id, created_at, name, slug, is_active)
		VALUES ($1, now(), 'Other', $2, true)`, second, "other-"+second.String()[:8])

	// The same person, joining the second tenant. No second identity and no
	// second password: addPerson finds the address it already knows.
	identityID, newAccount := api.addPerson(t, second, SeedEmail, "Ada elsewhere")

	var seeded uuid.UUID
	if err := api.pool.QueryRow(context.Background(),
		`SELECT identity_id FROM rig_account WHERE id = $1`,
		api.accountID(t, first, SeedEmail)).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	if identityID != seeded {
		t.Fatalf("the second tenant's account belongs to %s, want the person the seed created (%s)",
			identityID, seeded)
	}
	if newAccount == api.accountID(t, first, SeedEmail) {
		t.Fatal("joining a second tenant should be a second account, not the same one")
	}

	// The account in the second tenant has no grants, so it can sign in and read
	// nothing — which is the point of the posture, and not what this test is
	// about. Give it the same permissions the seed gave the first one.
	api.grantEverything(t, second, newAccount)

	// The password she already had, unchanged, signing in to both.
	elsewhere := api.login(t, second, SeedEmail)
	here := api.login(t, first, SeedEmail)

	api.do(t, request{
		method: http.MethodPost, path: "/api/v1/notes", token: here,
		body: map[string]any{"title": "Only in the first tenant"},
	})

	res := api.do(t, request{method: http.MethodGet, path: "/api/v1/notes", token: elsewhere})
	if res.status != http.StatusOK {
		t.Fatalf("reading as the other tenant: %d %s", res.status, res.body)
	}
	if strings.Contains(res.body, "Only in the first tenant") {
		t.Errorf("a token must not read across tenants: %s", res.body)
	}
}

// Wrong codes are counted, and enough of them stop the address being a guessing
// oracle. The window is per address, so this uses one of its own.
//
// Two limits are in play and the interaction is the interesting part: a code
// dies after three wrong guesses, and asking for another is itself limited to
// five an hour. So a sprayer runs out of codes before they run out of guesses,
// and what they hit is whichever limit comes first.
func TestRepeatedFailuresLockTheAddress(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	victim := "locked-" + uuid.New().String()[:8] + "@example.com"
	api.addPerson(t, tenant, victim, "Locked")
	code := api.askForCode(t, tenant, victim)

	var last response
	for range 8 {
		last = api.verify(t, tenant, victim, "000000")
		if last.status == http.StatusTooManyRequests {
			break
		}
		if last.status != http.StatusUnauthorized {
			t.Fatalf("a wrong code should be 401, got %d %s", last.status, last.body)
		}
	}

	if last.status != http.StatusTooManyRequests {
		t.Fatalf("repeated failures should lock the address, got %d", last.status)
	}
	if last.headers.Get("Retry-After") == "" {
		t.Error("a 429 should say when to come back")
	}

	// And the lockout is about the address, not about the code: the right one is
	// refused too while the window is open.
	if res := api.verify(t, tenant, victim, code); res.status != http.StatusTooManyRequests {
		t.Errorf("the correct code during a lockout: %d, want 429", res.status)
	}
}

// A code that has been guessed at enough times is dead, and asking for another
// is the way out — which is why the ceiling is well below the address lockout.
func TestACodeDiesOfGuessing(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	person := "guessed-" + uuid.New().String()[:8] + "@example.com"
	api.addPerson(t, tenant, person, "Guessed")
	code := api.askForCode(t, tenant, person)

	for i := range 3 {
		if res := api.verify(t, tenant, person, "000000"); res.status != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d %s", i+1, res.status, res.body)
		}
	}
	if res := api.verify(t, tenant, person, code); res.status != http.StatusUnauthorized {
		t.Errorf("the burned code should stay dead: %d %s", res.status, res.body)
	}

	// The way out, and it works while the address is still well inside its own
	// window: a fresh code signs them in.
	if token := api.login(t, tenant, person); token == "" {
		t.Error("a fresh code should sign them in")
	}
}

// server is the example's own wiring, over a real database.
type server struct {
	pool *pgxpool.Pool
	http *httptest.Server
	// mail is the example's own mailbox, which is the only place a sign-in code
	// exists in plaintext: the database keeps a hash. A test reads it the way
	// somebody reads their inbox.
	mail *outbox.Box
}

func newServer(t *testing.T) *server {
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

	handler, front, _, mail, err := newAPI(context.Background(), pool, baseURL(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	closeAuth(t, front)
	srv.Config.Handler = handler
	srv.Start()
	t.Cleanup(srv.Close)
	return &server{pool: pool, http: srv, mail: mail}
}

// closeAuth registers the one shutdown `cache:` in rig.yaml adds, which is the
// same line main.go hands to app.CloseWithin.
//
// Every newAPI in this suite starts the invalidation listener, and the listener
// opens a connection of its own from the pool's configuration rather than
// taking one out of the pool — so t.Cleanup(pool.Close) does not reach it, and
// without this each test would leave a connection and the goroutine holding it
// open for the life of the test binary.
func closeAuth(t *testing.T, front *auth.Auth) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := front.Close(ctx); err != nil {
			t.Errorf("closing the auth foundation: %v", err)
		}
	})
}

// seed makes the tenant, account and role the example ships with.
func (s *server) seed(t *testing.T) uuid.UUID {
	t.Helper()
	if err := seed(context.Background(), s.pool); err != nil {
		t.Fatal(err)
	}
	return uuid.MustParse(SeedTenantID)
}

// keys and roles are the assembled pieces, for the things no endpoint does.
func (s *server) keys(t *testing.T) *apikey.Manager {
	t.Helper()
	front, err := auth.New(auth.Config{Pool: s.pool})
	if err != nil {
		t.Fatal(err)
	}
	return front.Parts().APIKeys
}

// grants is the example's own resolver, which is what auth.Config is handed.
func (s *server) grants(t *testing.T) func(context.Context, uuid.UUID, uuid.UUID) ([]string, []string, error) {
	t.Helper()
	return authz.Grants(s.pool)
}

func (s *server) accountID(t *testing.T, tenant uuid.UUID, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM rig_account WHERE tenant_id = $1 AND lower(email_address) = lower($2)`,
		tenant, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// mintLike copies an existing key's scopes into a fresh one, so a test can act
// as an integration without inventing what it is allowed to do.
func (s *server) mintLike(t *testing.T, tenant, like uuid.UUID, name string) string {
	t.Helper()

	var (
		actsAs uuid.UUID
		scopes []string
		by     *uuid.UUID
	)
	if err := s.pool.QueryRow(context.Background(),
		`SELECT account_id, scopes, created_by_account_id FROM rig_api_key WHERE id = $1`,
		like).Scan(&actsAs, &scopes, &by); err != nil {
		t.Fatal(err)
	}

	minted, err := s.keys(t).Mint(context.Background(), apikey.MintInput{
		TenantID: tenant, AccountID: actsAs, Kind: apikey.KindIntegration,
		Name: name, Scopes: scopes, CreatedByAccountID: by,
	})
	if err != nil {
		t.Fatal(err)
	}
	return minted.Secret
}

func (s *server) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
	}
}

// addPerson creates somebody: the identity, which is global, and their account
// in one tenant.
//
// Both halves, always, because either on its own is a state the flows are
// entitled to refuse — an account with no identity is a service account, and an
// identity with no account belongs to no tenant.
func (s *server) addPerson(t *testing.T, tenant uuid.UUID, email, name string) (identityID, accountID uuid.UUID) {
	t.Helper()

	identityID, accountID = uuid.New(), uuid.New()
	ctx := context.Background()

	// The address may already be somebody: a test that puts the same person in
	// two tenants is exactly what this model is for.
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM rig_identity WHERE lower(email_address) = lower($1)`, email).Scan(&identityID)
	if err != nil {
		s.exec(t, `INSERT INTO rig_identity (id, created_at, email_address, display_name, is_active)
			VALUES ($1, now(), $2, $3, true)`, identityID, email, name)
	}

	s.exec(t, `
		INSERT INTO rig_account (id, tenant_id, identity_id, created_at, email_address,
		                     display_name, is_active)
		VALUES ($1, $2, $3, now(), $4, $5, true)`,
		accountID, tenant, identityID, email, name)
	return identityID, accountID
}

// grantEverything gives an account every derived permission, through a role.
//
// The coarse level on an account is a label; what a check reads is
// account_role → role_permission → permission. Nothing connects the two unless
// an application says how, which is what services/tenant does in the
// interface and what this does here.
func (s *server) grantEverything(t *testing.T, tenantID, accountID uuid.UUID) {
	t.Helper()

	roleID := uuid.New()
	s.exec(t, `
		INSERT INTO role (id, tenant_id, created_at, key, name, is_system)
		VALUES ($1, $2, now(), 'everything', 'Everything', true)
		ON CONFLICT (tenant_id, key) DO NOTHING`, roleID, tenantID)
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM role WHERE tenant_id = $1 AND key = 'everything'`,
		tenantID).Scan(&roleID); err != nil {
		t.Fatal(err)
	}

	for _, key := range api.PermissionKeys() {
		var permissionID uuid.UUID
		if err := s.pool.QueryRow(context.Background(), `
			INSERT INTO permission (id, created_at, key, name)
			VALUES ($1, now(), $2, $2)
			ON CONFLICT (key) DO UPDATE SET key = excluded.key
			RETURNING id`, uuid.New(), key).Scan(&permissionID); err != nil {
			t.Fatal(err)
		}
		s.exec(t, `INSERT INTO role_permission (role_id, permission_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, roleID, permissionID)
	}
	s.exec(t, `INSERT INTO account_role (id, account_id, role_id) VALUES ($1, $2, $3)
		ON CONFLICT (account_id, role_id) DO NOTHING`, uuid.New(), accountID, roleID)
}

// askForCode asks for a sign-in code and reads it out of the mailbox.
//
// The whole of what a sign-in needs to be set up: no credential to create, no
// hash for a test to invent. An address that has never been seen gets an
// identity here too, because this example sets allow_provisioning.
func (s *server) askForCode(t *testing.T, tenant uuid.UUID, email string) string {
	t.Helper()

	res := s.do(t, request{
		method: http.MethodPost, path: "/auth/email-code", tenant: tenant,
		body: map[string]any{"emailAddress": email},
	})
	if res.status != http.StatusNoContent {
		t.Fatalf("asking for a code for %s: %d %s", email, res.status, res.body)
	}
	return s.codeFor(t, email)
}

// codeFor reads the newest code mailed to an address out of the mailbox.
//
// The only place a code exists in plaintext: the database keeps a hash, which is
// what makes a dump of it not a dump of everybody's account.
func (s *server) codeFor(t *testing.T, email string) string {
	t.Helper()

	for _, m := range s.mail.Messages() {
		if m.Kind == outbox.KindEmailCode && strings.EqualFold(m.To, email) {
			return m.Token
		}
	}
	t.Fatalf("no code was mailed to %s", email)
	return ""
}

// login is the whole flow: ask for a code, type it back, keep the session.
func (s *server) login(t *testing.T, tenant uuid.UUID, email string) string {
	t.Helper()

	res := s.verify(t, tenant, email, s.askForCode(t, tenant, email))
	if res.status != http.StatusOK {
		t.Fatalf("sign in as %s: %d %s", email, res.status, res.body)
	}

	var pair struct {
		AccessToken string `json:"accessToken"`
	}
	res.decode(t, &pair)
	return pair.AccessToken
}

// verify types a code back without insisting it worked, for the tests whose
// subject is the refusal.
func (s *server) verify(t *testing.T, tenant uuid.UUID, email, code string) response {
	t.Helper()

	return s.do(t, request{
		method: http.MethodPost, path: "/auth/email-code/verify", tenant: tenant,
		body: map[string]any{"emailAddress": email, "code": code},
	})
}

type request struct {
	method, path string
	tenant       uuid.UUID
	token        string
	body         any
}

type response struct {
	status  int
	body    string
	headers http.Header
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), into); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (s *server) do(t *testing.T, in request) response {
	t.Helper()

	var body *bytes.Reader
	if in.body != nil {
		raw, err := json.Marshal(in.body)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(in.method, s.http.URL+in.path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if in.tenant != uuid.Nil {
		req.Header.Set("X-Tenant-Id", in.tenant.String())
	}
	if in.token != "" {
		req.Header.Set("Authorization", "Bearer "+in.token)
	}

	res, err := s.http.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", in.method, in.path, err)
	}
	defer res.Body.Close()

	var got bytes.Buffer
	if _, err := got.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	return response{status: res.StatusCode, body: got.String(), headers: res.Header}
}

// A key belongs to a tenant and always acts as an account, which is what makes
// an integration's writes attributable. The two kinds differ in whose account
// that is.
func TestTheTwoKindsOfAPIKey(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	var ada uuid.UUID
	if err := api.pool.QueryRow(context.Background(),
		`SELECT id FROM rig_account WHERE tenant_id = $1 AND lower(email_address) = $2`,
		tenant, SeedEmail).Scan(&ada); err != nil {
		t.Fatal(err)
	}

	keys := api.keys(t)

	t.Run("an integration acts as its own service account", func(t *testing.T) {
		// The seed made one, so this reads what it left rather than making a
		// second live credential.
		var (
			kind, accountKind string
			actsAs, mintedBy  uuid.UUID
		)
		// By name: other tests in this package mint keys of their own, and "the
		// integration key" stops being a single row the moment they do.
		if err := api.pool.QueryRow(context.Background(), `
			SELECT rig_api_key.kind::text, rig_account.kind::text, rig_api_key.account_id,
			       rig_api_key.created_by_account_id
			  FROM rig_api_key JOIN rig_account ON rig_account.id = rig_api_key.account_id
			 WHERE rig_api_key.tenant_id = $1 AND rig_api_key.name = 'Nightly import'`,
			tenant).Scan(&kind, &accountKind, &actsAs, &mintedBy); err != nil {
			t.Fatal(err)
		}

		if accountKind != "Service" {
			t.Errorf("an integration key should act as a service account, got %s", accountKind)
		}
		if actsAs == ada {
			t.Error("it should not act as the person who set it up")
		}
		// Which is the auditing half: the integration did the writing, and this
		// says who connected it.
		if mintedBy != ada {
			t.Errorf("minted by %s, want Ada %s", mintedBy, ada)
		}
	})

	t.Run("a personal key acts as its owner", func(t *testing.T) {
		minted, err := keys.Mint(context.Background(), apikey.MintInput{
			TenantID:           tenant,
			AccountID:          ada,
			Kind:               apikey.KindPersonal,
			Name:               "Ada's own scripts",
			CreatedByAccountID: &ada,
		})
		if err != nil {
			t.Fatalf("minting a personal key: %v", err)
		}
		if minted.Key.AccountID != ada {
			t.Error("a personal key should act as its owner")
		}

		// And it works on the API, as that person: what it can reach is exactly
		// what they can reach, which is the reason to want one.
		res := api.do(t, request{
			method: http.MethodPost, path: "/api/v1/notes", token: minted.Secret,
			body: map[string]any{"title": "Written by my own script"},
		})
		if res.status != http.StatusCreated {
			t.Fatalf("using a personal key: %d %s", res.status, res.body)
		}

		var note struct {
			CreatedByAccountID string `json:"createdByAccountId"`
		}
		res.decode(t, &note)
		if note.CreatedByAccountID != ada.String() {
			t.Errorf("the write should be attributed to Ada, got %s", note.CreatedByAccountID)
		}
	})

	t.Run("a personal key cannot act as somebody else", func(t *testing.T) {
		_, other := api.addPerson(t, tenant,
			"bob-"+uuid.New().String()[:8]+"@example.com", "Bob")

		// Refused before it reaches the database, and the database would refuse
		// it too — the foundation carries a CHECK for the same rule.
		_, err := keys.Mint(context.Background(), apikey.MintInput{
			TenantID:           tenant,
			AccountID:          other,
			Kind:               apikey.KindPersonal,
			Name:               "Not mine to make",
			CreatedByAccountID: &ada,
		})
		if err == nil {
			t.Fatal("a personal key acting as another account should be refused")
		}
		if !strings.Contains(err.Error(), "acts as the account that created it") {
			t.Errorf("the error should say what the rule is: %v", err)
		}

		// The same insert straight past the manager still fails, which is the
		// point of having it in both places.
		_, direct := api.pool.Exec(context.Background(), `
			INSERT INTO rig_api_key (id, tenant_id, account_id, created_by_account_id, kind,
			                     name, key_id, secret_hash)
			VALUES ($1, $2, $3, $4, 'Personal', 'Sneaky', $5, '\x00')`,
			uuid.New(), tenant, other, ada, "sneaky-"+other.String()[:8])
		if direct == nil {
			t.Error("the database should refuse it as well")
		}
	})
}

// A service account is what a key acts as, not somebody who signs in. It has no
// credential, and a password written for one anyway is still not a way in.
func TestAServiceAccountCannotSignIn(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	address := "service-" + uuid.New().String()[:8] + "@example.com"
	id := uuid.New()
	api.exec(t, `
		INSERT INTO rig_account (id, tenant_id, created_at, kind, email_address, display_name, is_active)
		VALUES ($1, $2, now(), 'Service', $3, 'A machine', true)`, id, tenant, address)

	// There is nothing to set up: a service account has no identity, so a code
	// has nothing to hang off. The address resolves to nobody.
	var exists bool
	if err := api.pool.QueryRow(context.Background(),
		`SELECT exists (SELECT 1 FROM rig_identity WHERE lower(email_address) = lower($1))`,
		address).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("a service account should not put a person in the identity table")
	}

	// 401, and deliberately the same 401 an address nobody has ever registered
	// gets. Before identities were separate this was a 403 saying "use its API
	// key", which was friendlier and also confirmed that the integration exists;
	// now the address resolves to nobody and the answer gives nothing away.
	res := api.verify(t, tenant, address, "000000")
	if res.status != http.StatusUnauthorized {
		t.Fatalf("a service account signing in: %d %s, want 401", res.status, res.body)
	}

	stranger := api.verify(t, tenant, "nobody-at-all@example.com", "000000")
	// Everything except the requestId, which is per request by design: every
	// request is named, so two of them are never byte-identical and never should
	// be. What must not differ is what the answer says.
	if told(t, stranger.body) != told(t, res.body) {
		t.Errorf("a service account and an unknown address must answer alike:\n  service: %s\n  unknown: %s",
			res.body, stranger.body)
	}
}

// told is what an error body tells the caller, with the request's own name
// dropped. Two refusals that must give nothing away are compared on this.
func told(t *testing.T, body string) string {
	t.Helper()

	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("the error body is not JSON: %v\n%s", err, body)
	}
	return e.Code + ": " + e.Message
}

// The level reaches a caller's claims as a role name, so a check sees it without
// needing to know it came from a column rather than from account_role.
func TestTheAccountLevelReachesTheClaims(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	roles, permissions, err := api.grants(t)(
		context.Background(), tenant, api.accountID(t, tenant, SeedEmail))
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, role := range roles {
		if role == string(account.RoleOwner) {
			found = true
		}
	}
	if !found {
		t.Errorf("Ada is an Owner, so her roles should say so: %v", roles)
	}
	// And the role she was granted the ordinary way is still there.
	if len(permissions) == 0 {
		t.Error("she should still hold the permissions her role grants")
	}
}

// The account columns say whose change it was; the key columns say which
// credential it came through. An integration's service account can be shared
// between several keys, so the second question is the one with the useful answer
// when something has gone wrong.
func TestAWriteRecordsTheKeyItCameThrough(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	// The seed's integration key, and the service account it acts as.
	var (
		keyID   uuid.UUID
		service uuid.UUID
	)
	if err := api.pool.QueryRow(context.Background(), `
		SELECT id, account_id FROM rig_api_key
		 WHERE tenant_id = $1 AND name = 'Nightly import'`, tenant).Scan(&keyID, &service); err != nil {
		t.Fatal(err)
	}

	// Mint a second key for the same service account, so that "which key" is a
	// question the account cannot answer.
	var ada uuid.UUID
	if err := api.pool.QueryRow(context.Background(),
		`SELECT id FROM rig_account WHERE tenant_id = $1 AND lower(email_address) = $2`,
		tenant, SeedEmail).Scan(&ada); err != nil {
		t.Fatal(err)
	}

	second, err := api.keys(t).Mint(context.Background(), apikey.MintInput{
		TenantID: tenant, AccountID: service, Kind: apikey.KindIntegration,
		Name: "Second credential", Scopes: []string{note.PermissionWrite},
		CreatedByAccountID: &ada,
	})
	if err != nil {
		t.Fatal(err)
	}

	res := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/notes", token: second.Secret,
		body: map[string]any{"title": "Imported overnight"},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("writing with an integration key: %d %s", res.status, res.body)
	}

	var wrote struct {
		ID                string `json:"id"`
		CreatedBy         string `json:"createdByAccountId"`
		CreatedByAPIKeyID string `json:"createdByApiKeyId"`
	}
	res.decode(t, &wrote)

	if wrote.CreatedBy != service.String() {
		t.Errorf("the account should be the service account: %s", wrote.CreatedBy)
	}
	if wrote.CreatedByAPIKeyID != second.Key.ID.String() {
		t.Errorf("the key should be the one used, %s, got %s",
			second.Key.ID, wrote.CreatedByAPIKeyID)
	}
	if wrote.CreatedByAPIKeyID == keyID.String() {
		t.Error("it recorded the wrong key of the two")
	}

	// A person's own session leaves the key null, so the column answers "did a
	// machine do this" as well as "which one".
	human := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/notes",
		token: api.login(t, tenant, SeedEmail),
		body:  map[string]any{"title": "Typed by hand"},
	})
	if human.status != http.StatusCreated {
		t.Fatalf("writing as a person: %d %s", human.status, human.body)
	}
	if strings.Contains(human.body, "createdByApiKeyId") {
		t.Errorf("a person's write should record no key: %s", human.body)
	}
}

// Provisioning is the door an integration uses to create an account, and the
// reason it exists rather than a POST on the table: the address is checked
// against the tenant's domains, a second account for one address is refused, no
// credential comes into existence, and the row records who asked.
func TestProvisioningAnAccount(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	// The seeded integration key holds account.provision, so this is an
	// integration creating people — an HR sync, an SSO provisioner.
	var keyID uuid.UUID
	if err := api.pool.QueryRow(context.Background(),
		`SELECT id FROM rig_api_key WHERE tenant_id = $1 AND name = 'Nightly import'`,
		tenant).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	key := api.mintLike(t, tenant, keyID, "Provisioner")

	address := "grace-" + uuid.New().String()[:8] + "@example.com"

	t.Run("an integration can create one", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/accounts", token: key,
			body: map[string]any{"emailAddress": address, "displayName": "Grace", "role": "Admin"},
		})
		if res.status != http.StatusCreated {
			t.Fatalf("provisioning: %d %s", res.status, res.body)
		}

		var got struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Role string `json:"role"`
		}
		res.decode(t, &got)
		if got.Kind != string(account.KindPerson) || got.Role != string(account.RoleAdmin) {
			t.Errorf("kind/role = %s/%s, want Person/Admin", got.Kind, got.Role)
		}

		// The row says both who and through what — the whole point of the key
		// audit columns.
		var byAccount, byKey *uuid.UUID
		if err := api.pool.QueryRow(context.Background(), `
			SELECT created_by_account_id, created_by_api_key_id
			  FROM rig_account WHERE id = $1`, got.ID).Scan(&byAccount, &byKey); err != nil {
			t.Fatal(err)
		}
		if byKey == nil || *byKey == uuid.Nil {
			t.Error("the provisioned account should name the key that made it")
		}
	})

	t.Run("the same address twice is a conflict", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/accounts", token: key,
			body: map[string]any{"emailAddress": address, "displayName": "Grace again"},
		})
		if res.status != http.StatusConflict {
			t.Errorf("a duplicate address: %d %s, want 409", res.status, res.body)
		}
	})

	t.Run("an address outside the tenant's domains is refused", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/accounts", token: key,
			body: map[string]any{"emailAddress": "someone@elsewhere.test", "displayName": "Nope"},
		})
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("a foreign domain: %d %s, want 422", res.status, res.body)
		}
		if !strings.Contains(res.body, "not in a domain this tenant allows") {
			t.Errorf("the answer should say why: %s", res.body)
		}

		// A subdomain of an allowed domain is the same company. The address is
		// unique per run, because this suite runs against a database that keeps
		// what the last run created.
		subdomain := "ci-" + uuid.New().String()[:8] + "@build.example.com"
		if res := api.do(t, request{
			method: http.MethodPost, path: "/auth/accounts", token: key,
			body: map[string]any{"emailAddress": subdomain, "displayName": "CI"},
		}); res.status != http.StatusCreated {
			t.Errorf("a subdomain should be allowed: %d %s", res.status, res.body)
		}
	})

	t.Run("without the permission it is refused", func(t *testing.T) {
		// Bob has an account and no role, so he is authenticated and not
		// allowed.
		bob := "bob-" + uuid.New().String()[:8] + "@example.com"
		api.addPerson(t, tenant, bob, "Bob")

		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/accounts",
			token: api.login(t, tenant, bob),
			body:  map[string]any{"emailAddress": "another@example.com", "displayName": "Another"},
		})
		if res.status != http.StatusForbidden {
			t.Errorf("provisioning without the permission: %d %s, want 403", res.status, res.body)
		}
	})

	// The table itself still has no Create, so provisioning is the only door and
	// it is the one that does the extra work.
	t.Run("the table has no create route", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/api/v1/accounts", token: key,
			body: map[string]any{"emailAddress": "crud@example.com"},
		})
		if res.status != http.StatusMethodNotAllowed && res.status != http.StatusNotFound {
			t.Errorf("POST /api/v1/accounts: %d %s, want it not to exist", res.status, res.body)
		}
	})
}

// TestAScopedReadIsNarrowUntilItIsWidened is the whole feature end to end: two
// people in one tenant, notes each, and the three answers the parameter can
// produce.
//
// The note table declares `access: { scope: own }`, so rig derived note.read.all,
// the generated handler checks it, and the generated repository applies the
// predicate. Nothing in the application says any of that.
func TestAScopedReadIsNarrowUntilItIsWidened(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	// Ada is the seeded owner. Linus is an ordinary member, granted only the
	// derived read and write.
	ada := api.login(t, tenant, SeedEmail)
	linusEmail := "linus-" + uuid.New().String()[:8] + "@example.com"
	_, linusID := api.addPerson(t, tenant, linusEmail, "Linus")
	api.grant(t, tenant, linusID, "member", note.PermissionRead, note.PermissionWrite)
	linus := api.login(t, tenant, linusEmail)

	mine := api.writeNote(t, ada, "Ada wrote this")
	theirs := api.writeNote(t, linus, "Linus wrote this")

	t.Run("a read with no parameter returns only the caller's own", func(t *testing.T) {
		got := api.noteTitles(t, linus, "")
		if len(got) != 1 || got[0] != "Linus wrote this" {
			t.Errorf("Linus sees %v, want only his own", got)
		}
		if slices.Contains(got, "Ada wrote this") {
			t.Error("the narrow default leaked another account's row")
		}
	})

	t.Run("asking for all without the permission is refused, not narrowed", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodGet, path: "/api/v1/notes?scope=all", token: linus,
		})
		// The rule the design turns on: insufficient authority must never produce
		// a smaller result set, because a caller cannot tell that from there being
		// nothing else to see.
		if res.status != http.StatusForbidden {
			t.Fatalf("scope=all without the grant: %d %s, want 403", res.status, res.body)
		}
		if !strings.Contains(res.body, "note.read.all") {
			t.Errorf("the refusal should name what to ask for: %s", res.body)
		}
	})

	t.Run("a nonsense value is a bad request", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodGet, path: "/api/v1/notes?scope=everything", token: linus,
		})
		if res.status != http.StatusBadRequest {
			t.Errorf("scope=everything: %d %s, want 400", res.status, res.body)
		}
	})

	t.Run("the integration's key holds it and sees the tenant", func(t *testing.T) {
		var keyID uuid.UUID
		if err := api.pool.QueryRow(context.Background(),
			`SELECT id FROM rig_api_key WHERE tenant_id = $1 AND name = 'Nightly import'`,
			tenant).Scan(&keyID); err != nil {
			t.Fatal(err)
		}
		key := api.mintLike(t, tenant, keyID, "Scope reader")

		// Narrow first. A key acts as its service account, so the default answer
		// is that account's own rows and not the tenant's — the parameter is
		// what widens it, not the fact that the caller is a machine.
		narrow := api.noteTitles(t, key, "")
		for _, other := range []string{"Ada wrote this", "Linus wrote this"} {
			if slices.Contains(narrow, other) {
				t.Errorf("the key's narrow read returned %q: %v", other, narrow)
			}
		}

		got := api.noteTitles(t, key, "all")
		for _, want := range []string{"Ada wrote this", "Linus wrote this"} {
			if !slices.Contains(got, want) {
				t.Errorf("scope=all is missing %q: %v", want, got)
			}
		}
	})

	t.Run("a write to somebody else's row is a 404", func(t *testing.T) {
		// Not a 403. A 403 would confirm the identifier is real to somebody who
		// cannot see the row, which is the thing cross-tenant access already
		// avoids — and there is no scope parameter on a write to widen this.
		res := api.do(t, request{
			method: http.MethodPatch, path: "/api/v1/notes/" + theirs, token: ada,
			body: map[string]any{"title": "Ada edits Linus"},
		})
		if res.status != http.StatusNotFound {
			t.Errorf("editing another account's note: %d %s, want 404", res.status, res.body)
		}

		// The owner's own row still works, so the narrowing is a filter and not a
		// broken write path.
		own := api.do(t, request{
			method: http.MethodPatch, path: "/api/v1/notes/" + mine, token: ada,
			body: map[string]any{"title": "Ada edits her own"},
		})
		if own.status != http.StatusOK {
			t.Errorf("editing her own note: %d %s", own.status, own.body)
		}
	})
}

// grant gives one account exactly the named permissions, through a role. It is
// grantEverything's opposite: what a test needs when the interesting thing is what
// the caller does *not* hold.
func (s *server) grant(t *testing.T, tenantID, accountID uuid.UUID, roleKey string, keys ...string) {
	t.Helper()

	roleID := uuid.New()
	s.exec(t, `
		INSERT INTO role (id, tenant_id, created_at, key, name, is_system)
		VALUES ($1, $2, now(), $3, $3, false)
		ON CONFLICT (tenant_id, key) DO NOTHING`, roleID, tenantID, roleKey)
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM role WHERE tenant_id = $1 AND key = $2`,
		tenantID, roleKey).Scan(&roleID); err != nil {
		t.Fatal(err)
	}

	for _, key := range keys {
		var permissionID uuid.UUID
		if err := s.pool.QueryRow(context.Background(), `
			INSERT INTO permission (id, created_at, key, name)
			VALUES ($1, now(), $2, $2)
			ON CONFLICT (key) DO UPDATE SET key = excluded.key
			RETURNING id`, uuid.New(), key).Scan(&permissionID); err != nil {
			t.Fatal(err)
		}
		s.exec(t, `INSERT INTO role_permission (role_id, permission_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, roleID, permissionID)
	}
	s.exec(t, `INSERT INTO account_role (id, account_id, role_id) VALUES ($1, $2, $3)
		ON CONFLICT (account_id, role_id) DO NOTHING`, uuid.New(), accountID, roleID)
}

func (s *server) writeNote(t *testing.T, token, title string) string {
	t.Helper()

	res := s.do(t, request{
		method: http.MethodPost, path: "/api/v1/notes", token: token,
		body: map[string]any{"title": title},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("writing %q: %d %s", title, res.status, res.body)
	}
	var got struct {
		ID string `json:"id"`
	}
	res.decode(t, &got)
	return got.ID
}

// noteTitles lists notes at one scope. An empty scope sends no parameter at all,
// which is the case worth covering separately: the default has to come from the
// server, not from a client that remembered to ask.
func (s *server) noteTitles(t *testing.T, token, scope string) []string {
	t.Helper()

	path := "/api/v1/notes"
	if scope != "" {
		path += "?scope=" + scope
	}
	res := s.do(t, request{method: http.MethodGet, path: path, token: token})
	if res.status != http.StatusOK {
		t.Fatalf("listing at scope %q: %d %s", scope, res.status, res.body)
	}

	var page struct {
		Data []struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	res.decode(t, &page)

	out := make([]string, 0, len(page.Data))
	for _, row := range page.Data {
		out = append(out, row.Title)
	}
	return out
}
