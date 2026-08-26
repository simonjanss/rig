package rigclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/authwire"
)

// profile is what a generated client would declare for a project with the
// default lifetimes.
var profile = rigclient.AuthProfile{
	BasePath:            "/auth",
	AccessTTL:           10 * time.Minute,
	RefreshTTL:          12 * time.Hour,
	RotationLeeway:      45 * time.Second,
	IdentityTTL:         10 * time.Minute,
	TenantHeader:        "X-Tenant-Id",
	HasRegistration:     true,
	HasIdentitySessions: true,
	HasAPIKeys:          true,
}

// authServer answers the two endpoints a session needs and counts what it was
// asked for. now is shared with the client, so a test can move both together.
type authServer struct {
	mu        sync.Mutex
	now       time.Time
	refreshes int32
	issued    int32
	// rejectAccess makes the data endpoint answer 401 once, as a revoked token
	// would.
	rejectAccess bool
}

func (s *authServer) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.now
}

func (s *authServer) advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.now = s.now.Add(d)
}

// pair mints a token that expires an access lifetime from the current fake now.
func (s *authServer) pair() authwire.TokenPair {
	n := atomic.AddInt32(&s.issued, 1)
	return authwire.TokenPair{
		AccessToken:      "rig_at_" + time.Duration(n).String(),
		RefreshToken:     "rig_rt_" + time.Duration(n).String(),
		ExpiresAt:        s.clock().Add(profile.AccessTTL),
		RefreshExpiresAt: s.clock().Add(profile.RefreshTTL),
	}
}

func (s *authServer) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(authwire.SignInResponse{
			TokenPair: s.pair(), IdentityToken: "rig_it_1",
			Tenants: []authwire.TenantView{},
		})
	})
	mux.HandleFunc("POST /auth/refresh", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&s.refreshes, 1)
		json.NewEncoder(w).Encode(s.pair())
	})
	mux.HandleFunc("GET /api/v1/todos", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		reject := s.rejectAccess
		s.rejectAccess = false
		s.mu.Unlock()

		if reject {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":"Unauthorized","message":"that token is not valid"}`))
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	})

	return mux
}

func newSignedIn(t *testing.T) (*rigclient.Runtime, *authServer) {
	t.Helper()

	server := &authServer{now: time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)}
	srv := httptest.NewServer(server.handler())
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{
		BaseURL: srv.URL,
		Now:     server.clock,
	}, rigclient.API{BasePath: "/api/v1", Auth: &profile})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.Auth().SignIn(t.Context(), authwire.LoginRequest{
		EmailAddress: "someone@example.com", Password: "correct horse",
	}); err != nil {
		t.Fatal(err)
	}
	return rt, server
}

func TestSigningInInstallsASession(t *testing.T) {
	rt, _ := newSignedIn(t)

	s, ok := rt.Session()
	if !ok {
		t.Fatal("no session was installed")
	}
	if s.Tokens().AccessToken == "" {
		t.Error("the session holds no access token")
	}
}

// The point of reading the lifetimes out of the document: the refresh happens
// before the token expires, not after a request has been refused for it.
func TestASessionRefreshesAheadOfExpiry(t *testing.T) {
	rt, server := newSignedIn(t)

	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&server.refreshes); got != 0 {
		t.Fatalf("refreshes = %d, want none while the token is fresh", got)
	}

	// Inside the leeway, and the token has not expired yet.
	server.advance(profile.AccessTTL - profile.RotationLeeway/2)

	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&server.refreshes); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}
}

// The server bounds how often a session may rotate, so a client that raced with
// itself would spend that budget on nothing.
func TestConcurrentCallersRefreshOnce(t *testing.T) {
	rt, server := newSignedIn(t)
	server.advance(profile.AccessTTL)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
				Method: http.MethodGet, Path: "/todos",
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&server.refreshes); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}
}

// A token that looked current and was refused anyway is a revoked one. The
// refresh token is the only thing left to try, and it is tried once.
func TestARefusedTokenIsRefreshedAndTheCallRetried(t *testing.T) {
	rt, server := newSignedIn(t)

	server.mu.Lock()
	server.rejectAccess = true
	server.mu.Unlock()

	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}); err != nil {
		t.Fatalf("the call was not retried after the refresh: %v", err)
	}
	if got := atomic.LoadInt32(&server.refreshes); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}
}

func TestSignInDoesNotPresentTheOldToken(t *testing.T) {
	var presented string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(authwire.SignInResponse{IdentityToken: "rig_it_1"})
	}))
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{
		BaseURL:    srv.URL,
		Credential: rigclient.StaticToken("rig_at_expired"),
	}, rigclient.API{BasePath: "/api/v1", Auth: &profile})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.Auth().Login(t.Context(), authwire.LoginRequest{}); err != nil {
		t.Fatal(err)
	}
	if presented != "" {
		t.Errorf("Authorization = %q, want none on a sign-in", presented)
	}
}

// A route this project does not mount is refused here, with a sentence naming
// the setting that would mount it. A bare 404 says only that the URL is wrong.
func TestAnUnmountedRouteIsRefusedLocally(t *testing.T) {
	narrow := profile
	narrow.HasTenantCreation = false

	rt, err := rigclient.New(rigclient.Config{BaseURL: "http://example.invalid"},
		rigclient.API{BasePath: "/api/v1", Auth: &narrow})
	if err != nil {
		t.Fatal(err)
	}

	_, err = rt.Auth().CreateTenant(t.Context(), "rig_it_1", authwire.CreateTenantRequest{Name: "acme"})
	if err == nil {
		t.Fatal("creating a tenant was allowed on a project that does not mount it")
	}
	if got := err.Error(); !strings.Contains(got, "allow_tenant_creation") {
		t.Errorf("err = %q, want it to name the setting", got)
	}
}

func TestTheTenantIsNamedOnASignIn(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Tenant-Id")
		json.NewEncoder(w).Encode(authwire.SignInResponse{})
	}))
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{BaseURL: srv.URL},
		rigclient.API{BasePath: "/api/v1", Auth: &profile})
	if err != nil {
		t.Fatal(err)
	}

	tenant := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	auth := rt.Auth()
	if _, err := auth.Login(t.Context(), authwire.LoginRequest{}, auth.WithTenant(tenant)); err != nil {
		t.Fatal(err)
	}
	if got != tenant.String() {
		t.Errorf("X-Tenant-Id = %q, want %q", got, tenant)
	}
}

// Signing in again is somebody else arriving at the same client, so it installs
// a fresh session rather than handing new tokens to the old one. An answer that
// carries an access token alone must not inherit the refresh token the person
// before it left behind — a client that then refreshed would come back as them.
//
// This is the whole of the difference between installing and adopting, and the
// reason the two are separate.
func TestASecondSignInDoesNotInheritTheFirstsRefreshToken(t *testing.T) {
	var signins int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		signins++
		pair := authwire.TokenPair{AccessToken: "rig_at_1", RefreshToken: "rig_rt_1"}
		if signins > 1 {
			// The second person's session, issued without a refresh token.
			pair = authwire.TokenPair{AccessToken: "rig_at_2"}
		}
		json.NewEncoder(w).Encode(authwire.SignInResponse{TokenPair: pair})
	}))
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{BaseURL: srv.URL},
		rigclient.API{BasePath: "/api/v1", Auth: &profile})
	if err != nil {
		t.Fatal(err)
	}

	auth := rt.Auth()
	for range 2 {
		if _, err := auth.SignIn(t.Context(), authwire.LoginRequest{}); err != nil {
			t.Fatal(err)
		}
	}

	s, ok := rt.Session()
	if !ok {
		t.Fatal("no session was installed")
	}
	if got := s.Tokens().AccessToken; got != "rig_at_2" {
		t.Errorf("AccessToken = %q, want the second sign-in's", got)
	}
	if got := s.Tokens().RefreshToken; got != "" {
		t.Errorf("RefreshToken = %q, want none: the second sign-in inherited the first's", got)
	}
}

// The converse, and why adopting is not installing either. A tenant switch is
// the same person, re-tokened, and the endpoint answers with an access token
// alone — so dropping the refresh token in hand would end the session at the
// next expiry.
func TestATenantSwitchKeepsTheRefreshTokenItWasNotGivenAgain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(authwire.SignInResponse{TokenPair: authwire.TokenPair{
			AccessToken: "rig_at_1", RefreshToken: "rig_rt_1",
		}})
	})
	mux.HandleFunc("POST /auth/tenants/{id}/switch", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(authwire.TokenPair{AccessToken: "rig_at_2"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{BaseURL: srv.URL},
		rigclient.API{BasePath: "/api/v1", Auth: &profile})
	if err != nil {
		t.Fatal(err)
	}

	auth := rt.Auth()
	if _, err := auth.SignIn(t.Context(), authwire.LoginRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SwitchTenant(t.Context(), uuid.New()); err != nil {
		t.Fatal(err)
	}

	s, ok := rt.Session()
	if !ok {
		t.Fatal("no session was installed")
	}
	if got := s.Tokens().AccessToken; got != "rig_at_2" {
		t.Errorf("AccessToken = %q, want the switch's", got)
	}
	if got := s.Tokens().RefreshToken; got != "rig_rt_1" {
		t.Errorf("RefreshToken = %q, want the one already in hand", got)
	}
}

// The three key routes are refused on a project that mounts none, and each names
// its own route. One sentence explains where keys come from; the route is what
// tells the caller which of their calls this was.
func TestTheAPIKeyRoutesAreRefusedLocally(t *testing.T) {
	narrow := profile
	narrow.HasAPIKeys = false

	rt, err := rigclient.New(rigclient.Config{BaseURL: "http://example.invalid"},
		rigclient.API{BasePath: "/api/v1", Auth: &narrow})
	if err != nil {
		t.Fatal(err)
	}
	auth := rt.Auth()

	for _, tc := range []struct {
		name  string
		call  func() error
		route string
	}{
		{"APIKeys", func() error { _, err := auth.APIKeys(t.Context()); return err },
			"GET /auth/api-keys"},
		{"CreateAPIKey", func() error {
			_, err := auth.CreateAPIKey(t.Context(), authwire.CreateKeyRequest{Name: "ci"})
			return err
		}, "POST /auth/api-keys"},
		{"RevokeAPIKey", func() error { return auth.RevokeAPIKey(t.Context(), uuid.New()) },
			"DELETE /auth/api-keys/{id}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("the call was allowed on a project that mounts no keys")
			}
			if got := err.Error(); !strings.Contains(got, tc.route) {
				t.Errorf("err = %q, want it to name %q", got, tc.route)
			}
			if got := err.Error(); !strings.Contains(got, "apikey manager") {
				t.Errorf("err = %q, want it to say where keys come from", got)
			}
		})
	}
}

// The picker routes are refused on a project with no identity sessions, which is
// most of them: a project where everybody belongs to one tenant has no picker.
func TestThePickerRoutesAreRefusedLocally(t *testing.T) {
	narrow := profile
	narrow.HasIdentitySessions = false

	rt, err := rigclient.New(rigclient.Config{BaseURL: "http://example.invalid"},
		rigclient.API{BasePath: "/api/v1", Auth: &narrow})
	if err != nil {
		t.Fatal(err)
	}
	auth := rt.Auth()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"MyTenants", func() error { _, err := auth.MyTenants(t.Context(), "rig_it_1"); return err }},
		{"MyInvitations", func() error { _, err := auth.MyInvitations(t.Context(), "rig_it_1"); return err }},
		{"AcceptMyInvitation", func() error {
			_, err := auth.AcceptMyInvitation(t.Context(), "rig_it_1", uuid.New(), "web")
			return err
		}},
		{"EndIdentitySession", func() error { return auth.EndIdentitySession(t.Context(), "rig_it_1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("the call was allowed on a project with no identity sessions")
			}
			if got := err.Error(); !strings.Contains(got, "identity sessions") {
				t.Errorf("err = %q, want it to say what mounts these routes", got)
			}
		})
	}
}

// The identity token is prepended, not appended, so a caller who set the header
// themselves means it. Reverse the order and the library would overwrite what it
// was handed.
func TestACallersOwnHeaderBeatsTheIdentityToken(t *testing.T) {
	var presented string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(authwire.List[authwire.TenantView]{})
	}))
	t.Cleanup(srv.Close)

	rt, err := rigclient.New(rigclient.Config{BaseURL: srv.URL},
		rigclient.API{BasePath: "/api/v1", Auth: &profile})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.Auth().MyTenants(t.Context(), "rig_it_1"); err != nil {
		t.Fatal(err)
	}
	if presented != "Bearer rig_it_1" {
		t.Fatalf("Authorization = %q, want the identity token", presented)
	}

	if _, err := rt.Auth().MyTenants(t.Context(), "rig_it_1",
		rigclient.WithHeader("Authorization", "Bearer mine")); err != nil {
		t.Fatal(err)
	}
	if presented != "Bearer mine" {
		t.Errorf("Authorization = %q, want the caller's own header to win", presented)
	}
}
