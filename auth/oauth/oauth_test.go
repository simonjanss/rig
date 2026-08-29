package oauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	xoauth2 "golang.org/x/oauth2"

	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// fakeProvider is a whole OAuth provider in thirty lines, so the flow can be
// driven end to end without the internet.
type fakeProvider struct {
	srv     *httptest.Server
	profile oauth.Profile
	// challenge is what the authorization request carried, so a test can check
	// that PKCE actually happened.
	challenge string
	// verifier is what the token request sent back.
	verifier string
}

func newFakeProvider(t *testing.T, profile oauth.Profile) *fakeProvider {
	t.Helper()

	f := &fakeProvider{profile: profile}
	mux := http.NewServeMux()

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.challenge = q.Get("code_challenge")
		// Straight back, the way a provider does once somebody approves.
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=the-code&state="+url.QueryEscape(q.Get("state")),
			http.StatusFound)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.verifier = r.Form.Get("code_verifier")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "the-access-token", "token_type": "Bearer", "expires_in": 3600,
		})
	})

	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer the-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": f.profile.Subject, "email": f.profile.EmailAddress,
			"email_verified": f.profile.EmailVerified, "name": f.profile.DisplayName,
		})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProvider) provider() oauth.Provider {
	p := oauth.Google("client", "secret")
	p.Endpoint = xoauth2.Endpoint{
		AuthURL:   f.srv.URL + "/authorize",
		TokenURL:  f.srv.URL + "/token",
		AuthStyle: xoauth2.AuthStyleInParams,
	}
	p.UserInfoURL = f.srv.URL + "/userinfo"
	return p
}

// membership is one person's account in one tenant.
type membership struct{ tenantID, identityID, accountID uuid.UUID }

// store is an in-memory oauth.Store.
type store struct {
	mu    sync.Mutex
	links []*oauth.Link
	// identities maps a lowercased address to the person who has it. It is not
	// per tenant, which is the point: one address is one person.
	identities  map[string]uuid.UUID
	memberships []membership

	provisions int
	joins      int
}

func newStore() *store {
	return &store{identities: map[string]uuid.UUID{}}
}

func (s *store) FindLink(_ context.Context, provider, subject string) (*oauth.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, l := range s.links {
		if l.Provider == provider && l.Subject == subject {
			return l, nil
		}
	}
	return nil, nil
}

func (s *store) FindIdentityByEmail(_ context.Context, lowercased string) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identities[lowercased], nil
}

func (s *store) LinkIdentity(_ context.Context, in oauth.LinkInput) (*oauth.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := &oauth.Link{
		ID: uuid.New(), IdentityID: in.IdentityID,
		Provider: in.Provider, Subject: in.Profile.Subject,
		EmailAddress: in.Profile.EmailAddress,
	}
	s.links = append(s.links, l)
	return l, nil
}

func (s *store) ProvisionIdentity(ctx context.Context, in oauth.ProvisionInput) (*oauth.Link, error) {
	s.mu.Lock()
	s.provisions++
	identityID := uuid.New()
	s.identities[strings.ToLower(in.Profile.EmailAddress)] = identityID
	s.mu.Unlock()

	return s.LinkIdentity(ctx, oauth.LinkInput{
		IdentityID: identityID, Provider: in.Provider, Profile: in.Profile,
	})
}

func (s *store) FindAccount(_ context.Context, tenantID, identityID uuid.UUID) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, m := range s.memberships {
		if m.tenantID == tenantID && m.identityID == identityID {
			return m.accountID, nil
		}
	}
	return uuid.Nil, nil
}

func (s *store) JoinTenant(_ context.Context, in oauth.JoinInput) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.joins++
	accountID := uuid.New()
	s.memberships = append(s.memberships, membership{
		tenantID: in.TenantID, identityID: in.IdentityID, accountID: accountID,
	})
	return accountID, nil
}

// put registers somebody who already exists: an address, and an account in one
// tenant.
func (s *store) put(tenantID uuid.UUID, lowercased string) (identityID, accountID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	identityID, ok := s.identities[lowercased]
	if !ok {
		identityID = uuid.New()
		s.identities[lowercased] = identityID
	}
	accountID = uuid.New()
	s.memberships = append(s.memberships, membership{
		tenantID: tenantID, identityID: identityID, accountID: accountID,
	})
	return identityID, accountID
}

type fixture struct {
	srv      *httptest.Server
	provider *fakeProvider
	store    *store
	tenant   uuid.UUID
	signedIn *oauth.SignIn
}

func setup(t *testing.T, profile oauth.Profile, tweak func(*oauth.Config)) *fixture {
	t.Helper()

	f := &fixture{provider: newFakeProvider(t, profile), store: newStore(), tenant: uuid.New()}

	cfg := oauth.Config{
		Store:      f.store,
		Providers:  []oauth.Provider{f.provider.provider()},
		SigningKey: []byte("a signing key of at least thirty-two bytes"),
		Tenant:     func(*http.Request) (uuid.UUID, error) { return f.tenant, nil },
		// Plain HTTP, because httptest is.
		Insecure: true,
		OnSignIn: func(w http.ResponseWriter, _ *http.Request, in oauth.SignIn) error {
			f.signedIn = &in
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
	}
	if tweak != nil {
		tweak(&cfg)
	}

	mux := http.NewServeMux()
	// BaseURL has to be the server's own address, and the server does not
	// exist until it is started — so the handler is built after.
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	cfg.BaseURL = f.srv.URL

	h, err := oauth.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.Mount(mux)
	return f
}

// signIn drives the whole round trip the way a browser would.
func (f *fixture) signIn(t *testing.T, query string) *http.Response {
	t.Helper()

	jar := &recordingJar{}
	client := &http.Client{Jar: jar}

	res, err := client.Get(f.srv.URL + "/auth/oauth/google/start" + query)
	if err != nil {
		t.Fatal(err)
	}
	// Read and replace it, so a caller can still look at what was said.
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res
}

// recordingJar is a cookie jar that ignores every rule about hosts and
// security, because httptest speaks plain HTTP to 127.0.0.1 and a real jar
// would drop the cookie.
type recordingJar struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

func (j *recordingJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()

	for _, c := range cookies {
		if c.MaxAge < 0 {
			j.cookies = nil
			continue
		}
		j.cookies = append(j.cookies, c)
	}
}

func (j *recordingJar) Cookies(*url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cookies
}

func TestSignInProvisionsAnAccount(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "provider-subject-1", EmailAddress: "sam@example.com",
		EmailVerified: true, DisplayName: "Sam",
	}, func(c *oauth.Config) { c.AllowProvisioning = true })

	res := f.signIn(t, "")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", res.StatusCode)
	}

	if f.signedIn == nil {
		t.Fatal("OnSignIn should have run")
	}
	if !f.signedIn.New {
		t.Error("the account was created by this sign-in and should say so")
	}
	if f.store.provisions != 1 {
		t.Errorf("%d accounts provisioned, want 1", f.store.provisions)
	}

	// A stolen authorization code is useless without the verifier, which never
	// left the server.
	if f.provider.challenge == "" {
		t.Error("the authorization request should carry a PKCE challenge")
	}
	if f.provider.verifier == "" {
		t.Error("the token request should carry the verifier")
	}
}

// The second sign-in is the same person, and matching on the subject is what
// makes that true even if their address changed in between.
func TestASecondSignInFindsTheSameIdentity(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "provider-subject-1", EmailAddress: "sam@example.com", EmailVerified: true,
	}, func(c *oauth.Config) { c.AllowProvisioning = true })

	f.signIn(t, "")
	first := f.signedIn.AccountID

	// Same subject, different address: people change theirs.
	f.provider.profile.EmailAddress = "sam.new@example.com"
	f.signIn(t, "")

	if f.signedIn.AccountID != first {
		t.Error("a changed address should not produce a second account")
	}
	if f.signedIn.New {
		t.Error("this account already existed")
	}
	if f.store.provisions != 1 {
		t.Errorf("%d accounts provisioned, want 1", f.store.provisions)
	}
}

func TestAVerifiedAddressLinksAnExistingAccount(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "provider-subject-1", EmailAddress: "sam@example.com", EmailVerified: true,
	}, nil)

	_, existing := f.store.put(f.tenant, "sam@example.com")

	f.signIn(t, "")
	if f.signedIn == nil {
		t.Fatal("the sign-in should have completed")
	}
	if f.signedIn.AccountID != existing {
		t.Error("the provider identity should have been linked to the account that already existed")
	}
	if f.signedIn.New {
		t.Error("nothing was created")
	}
}

// Somebody who already works at one of your customers signing in to another.
// The person is found — one Google account, one link, no second identity — and
// then the tenant decides.
func TestAKnownPersonJoiningAnotherTenant(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "provider-subject-1", EmailAddress: "sam@example.com", EmailVerified: true,
	}, func(c *oauth.Config) { c.AllowProvisioning = true })

	// They exist, with an account somewhere else entirely.
	identityID, elsewhere := f.store.put(uuid.New(), "sam@example.com")

	f.signIn(t, "")
	if f.signedIn == nil {
		t.Fatal("the sign-in should have completed")
	}
	if f.signedIn.Link.IdentityID != identityID {
		t.Error("the same person should have been recognised, not created again")
	}
	if f.store.provisions != 0 {
		t.Errorf("%d people created, want 0 — this one already existed", f.store.provisions)
	}
	if f.signedIn.AccountID == elsewhere {
		t.Error("the session should be for an account in this tenant, not the other one")
	}
	if f.signedIn.TenantID != f.tenant {
		t.Error("the session should be for the tenant that was signed in to")
	}
	if !f.signedIn.New {
		t.Error("the account in this tenant is new, which is what onboarding hangs off")
	}
	if f.store.joins != 1 {
		t.Errorf("%d tenants joined, want 1", f.store.joins)
	}
}

// Without provisioning, being somebody is not the same as belonging here. This
// is the case that keeps one customer's staff out of another customer's data.
func TestAKnownPersonCannotJoinWithoutProvisioning(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "provider-subject-1", EmailAddress: "sam@example.com", EmailVerified: true,
	}, nil)

	f.store.put(uuid.New(), "sam@example.com")

	res := f.signIn(t, "")
	if res.StatusCode != rigerr.CodeForbidden.HTTPStatus() {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
	if f.signedIn != nil {
		t.Error("the sign-in should not have completed")
	}
	if f.store.joins != 0 {
		t.Error("nothing should have joined this tenant")
	}
}

// The check the whole package turns on. Anybody can register any address at
// some provider; only a verified one is evidence.
func TestAnUnverifiedAddressCannotTakeOverAnAccount(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "an-attacker", EmailAddress: "sam@example.com", EmailVerified: false,
	}, nil)

	f.store.put(f.tenant, "sam@example.com")

	res := f.signIn(t, "")
	if res.StatusCode != rigerr.CodeForbidden.HTTPStatus() {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
	if f.signedIn != nil {
		t.Fatal("the sign-in should not have completed")
	}
	if len(f.store.links) != 0 {
		t.Error("nothing should have been linked")
	}
}

// An open sign-in endpoint lets anybody with a Google account appear inside a
// customer's tenant.
func TestProvisioningIsOffByDefault(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "somebody-new", EmailAddress: "nobody@example.com", EmailVerified: true,
	}, nil)

	res := f.signIn(t, "")
	if res.StatusCode != rigerr.CodeForbidden.HTTPStatus() {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
	if f.store.provisions != 0 {
		t.Error("no account should have been created")
	}
}

// A callback with no matching cookie is somebody else's sign-in being finished
// in this browser.
func TestACallbackWithoutTheCookieIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s", EmailAddress: "sam@example.com", EmailVerified: true},
		func(c *oauth.Config) { c.AllowProvisioning = true })

	res, err := http.Get(f.srv.URL + "/auth/oauth/google/callback?code=the-code&state=invented")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if f.signedIn != nil {
		t.Error("the sign-in should not have completed")
	}
}

// The state in the query must match the one in the cookie, or an attacker can
// have their own authorization code redeemed in your browser.
func TestAMismatchedStateIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s", EmailAddress: "sam@example.com", EmailVerified: true},
		func(c *oauth.Config) { c.AllowProvisioning = true })

	// Start, to get a real cookie.
	client := &http.Client{
		Jar: &recordingJar{},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Get(f.srv.URL + "/auth/oauth/google/start")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// Then come back with somebody else's state.
	res, err = client.Get(f.srv.URL + "/auth/oauth/google/callback?code=the-code&state=not-the-one")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if f.signedIn != nil {
		t.Error("the sign-in should not have completed")
	}
}

// A provider saying "the person pressed cancel" is not a server failure.
func TestAProviderErrorIsNotAServerError(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s"}, nil)

	res, err := http.Get(f.srv.URL + "/auth/oauth/google/callback?error=access_denied")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

// An unchecked returnTo is an open redirect, and an open redirect on a sign-in
// endpoint wears your domain in a phishing link.
func TestReturnTo(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{
		Subject: "s", EmailAddress: "sam@example.com", EmailVerified: true,
	}, func(c *oauth.Config) {
		c.AllowProvisioning = true
		c.AllowedReturnTo = []string{"https://app.example.com"}
	})

	t.Run("a path on this origin", func(t *testing.T) {
		f.signIn(t, "?returnTo=%2Fdashboard")
		if f.signedIn == nil || f.signedIn.ReturnTo != "/dashboard" {
			t.Errorf("returnTo = %+v", f.signedIn)
		}
	})

	t.Run("a listed origin", func(t *testing.T) {
		f.signIn(t, "?returnTo=https%3A%2F%2Fapp.example.com%2Fwelcome")
		if f.signedIn == nil || f.signedIn.ReturnTo != "https://app.example.com/welcome" {
			t.Errorf("returnTo = %+v", f.signedIn)
		}
	})

	for _, hostile := range []string{
		"https%3A%2F%2Fevil.example.com",
		"%2F%2Fevil.example.com",
		"javascript%3Aalert(1)",
	} {
		t.Run("refused: "+hostile, func(t *testing.T) {
			res, err := http.Get(f.srv.URL + "/auth/oauth/google/start?returnTo=" + hostile)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", res.StatusCode)
			}
		})
	}
}

func TestUnknownProvider(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s"}, nil)

	res, err := http.Get(f.srv.URL + "/auth/oauth/facebook/start")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestConfigurationIsChecked(t *testing.T) {
	t.Parallel()

	valid := oauth.Config{
		Store:      newStore(),
		Providers:  []oauth.Provider{oauth.Google("id", "secret")},
		BaseURL:    "https://app.example.com",
		SigningKey: []byte("a signing key of at least thirty-two bytes"),
		Tenant:     func(*http.Request) (uuid.UUID, error) { return uuid.New(), nil },
		OnSignIn:   func(http.ResponseWriter, *http.Request, oauth.SignIn) error { return nil },
	}

	for name, break_ := range map[string]func(*oauth.Config){
		"no store":       func(c *oauth.Config) { c.Store = nil },
		"no providers":   func(c *oauth.Config) { c.Providers = nil },
		"no base url":    func(c *oauth.Config) { c.BaseURL = "" },
		"a short key":    func(c *oauth.Config) { c.SigningKey = []byte("too short") },
		"no tenant":      func(c *oauth.Config) { c.Tenant = nil },
		"no sign-in":     func(c *oauth.Config) { c.OnSignIn = nil },
		"a duplicate":    func(c *oauth.Config) { c.Providers = append(c.Providers, oauth.Google("a", "b")) },
		"an unnamed one": func(c *oauth.Config) { c.Providers = []oauth.Provider{{}} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			break_(&cfg)
			if _, err := oauth.New(cfg); err == nil {
				t.Error("this configuration should have been refused")
			}
		})
	}

	if _, err := oauth.New(valid); err != nil {
		t.Errorf("the valid configuration was refused: %v", err)
	}
}

// begin runs the start leg and returns the state cookie and the state the
// provider was told, so a test can come back with either of them changed.
func (f *fixture) begin(t *testing.T, query string) (*http.Cookie, string) {
	t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Get(f.srv.URL + "/auth/oauth/google/start" + query)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if res.StatusCode != http.StatusFound {
		t.Fatalf("start: status %d, want a redirect", res.StatusCode)
	}
	if len(res.Cookies()) != 1 {
		t.Fatalf("start set %d cookies, want the state", len(res.Cookies()))
	}

	to, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return res.Cookies()[0], to.Query().Get("state")
}

// callback issues the return leg by hand, so a test can present a cookie and a
// state that do not agree.
func (f *fixture) callback(t *testing.T, cookie *http.Cookie, query string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/auth/oauth/google/callback"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}

	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// The cookie is the only thing standing between a signed state and one
// somebody wrote themselves, so every way of breaking it has to land in the
// same place: start again.
func TestAStateCookieThatDoesNotVerifyIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s", EmailAddress: "sam@example.com", EmailVerified: true},
		func(c *oauth.Config) { c.AllowProvisioning = true })

	good, state := f.begin(t, "")

	for name, value := range map[string]string{
		"empty":                     "",
		"no signature":              strings.Split(good.Value, ".")[0],
		"a wrong signature":         strings.Split(good.Value, ".")[0] + ".AAAA",
		"a body that is not base64": "!!!." + strings.Split(good.Value, ".")[1],
		// One byte off the payload: the signature is what makes the rest of it
		// worth reading at all.
		"a body that was edited": good.Value[:len(good.Value)-40] + good.Value[len(good.Value)-39:],
	} {
		tampered := *good
		tampered.Value = value

		res := f.callback(t, &tampered, "?code=the-code&state="+url.QueryEscape(state))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, res.StatusCode)
		}
	}

	if f.signedIn != nil {
		t.Error("none of those should have signed anybody in")
	}
}

// A state that outlived its window is one that was captured and kept, and the
// window is the only thing that makes a signed cookie safe to hand out.
func TestAnExpiredStateIsRefused(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	now := at

	f := setup(t, oauth.Profile{Subject: "s", EmailAddress: "sam@example.com", EmailVerified: true},
		func(c *oauth.Config) {
			c.AllowProvisioning = true
			c.StateTTL = time.Minute
			c.Now = func() time.Time { return now }
		})

	cookie, state := f.begin(t, "")
	now = at.Add(time.Minute + time.Second)

	if res := f.callback(t, cookie, "?code=the-code&state="+url.QueryEscape(state)); res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if f.signedIn != nil {
		t.Error("an expired sign-in should not complete")
	}
}

// A provider that redirects back with neither a code nor an error has failed
// in a way nothing downstream can recover from, and exchanging an empty code
// would just turn it into a confusing 400 from somewhere else.
func TestACallbackWithNoCodeIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s"}, nil)

	cookie, state := f.begin(t, "")

	if res := f.callback(t, cookie, "?state="+url.QueryEscape(state)); res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

// The cookie carries which provider the sign-in started with, because finishing
// a Google sign-in at the GitHub callback would resolve one provider's subject
// against the other's namespace.
func TestAStateFromAnotherProviderIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s", EmailAddress: "sam@example.com", EmailVerified: true},
		func(c *oauth.Config) {
			c.AllowProvisioning = true
			c.Providers = append(c.Providers, oauth.GitHub("id", "secret"))
		})

	cookie, state := f.begin(t, "")

	req, err := http.NewRequest(http.MethodGet,
		f.srv.URL+"/auth/oauth/github/callback?code=the-code&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if f.signedIn != nil {
		t.Error("the sign-in should not have completed")
	}
}

// Over HTTPS the cookie carries the __Host- prefix, which is a promise the
// browser enforces: secure, path /, and not settable by a subdomain. Insecure
// drops it because a browser would refuse the cookie over plain HTTP and local
// development would stop working entirely.
func TestTheStateCookieIsHostPrefixedUnlessDevelopmentSaysOtherwise(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s"}, nil)
	insecure, _ := f.begin(t, "")

	if insecure.Name != "rig_oauth" || insecure.Secure {
		t.Errorf("insecure cookie = %+v", insecure)
	}

	secure := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) { c.Insecure = false })
	cookie, _ := secure.begin(t, "")

	if cookie.Name != "__Host-rig_oauth" {
		t.Errorf("name = %q, want the __Host- prefix", cookie.Name)
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" {
		t.Errorf("cookie = %+v, want the prefix's own guarantees", cookie)
	}
	// Lax, not Strict: the provider redirects back with a top-level GET, and
	// Strict would drop the cookie and turn every sign-in into a dead end.
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
}

// A provider that answers with no subject has given nothing to key an identity
// on, and guessing one from the address is how two people end up sharing an
// account.
func TestAProfileWithNoSubjectIsAServerError(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{EmailAddress: "sam@example.com", EmailVerified: true},
		func(c *oauth.Config) { c.AllowProvisioning = true })

	res := f.signIn(t, "")
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}

	// And it says nothing else: an internal failure's detail is what leaks the
	// shape of the thing that failed.
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "subject") {
		t.Errorf("the response should not describe the failure: %s", body)
	}
	if f.signedIn != nil {
		t.Error("nobody should have been signed in")
	}
}

// A provider whose profile endpoint fails has not signed anybody in, and the
// only wrong answer is to continue with an empty profile.
func TestAProfileEndpointThatFailsStopsTheSignIn(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s", EmailAddress: "sam@example.com"},
		func(c *oauth.Config) {
			c.AllowProvisioning = true
			// Somewhere the fake provider does not serve, so the fetch fails
			// the way a provider having a bad afternoon does.
			c.Providers[0].UserInfoURL += "-gone"
		})

	if res := f.signIn(t, ""); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
	if f.signedIn != nil {
		t.Error("nobody should have been signed in")
	}
}

// A sign-in page has to list what it can offer, and reading the configuration
// back beats writing the same list twice.
func TestProvidersListsWhatASignInPageShouldOffer(t *testing.T) {
	t.Parallel()

	h, err := oauth.New(oauth.Config{
		Store: newStore(),
		Providers: []oauth.Provider{
			oauth.Google("id", "secret"),
			oauth.Microsoft("id", "secret", ""),
			oauth.GitHub("id", "secret"),
		},
		BaseURL:    "https://app.example.com",
		SigningKey: []byte("a signing key of at least thirty-two bytes"),
		Tenant:     func(*http.Request) (uuid.UUID, error) { return uuid.New(), nil },
		OnSignIn:   func(http.ResponseWriter, *http.Request, oauth.SignIn) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(h.Providers(), ",")
	if got != "Google,Microsoft,GitHub" {
		t.Errorf("Providers = %q, want them in the order they were configured", got)
	}
}

// Every provider spells the same three facts differently, and Parse is where
// that ends. A subject read from the wrong field is an identity that changes
// under somebody.
func TestTheBuiltInProvidersReadTheirOwnProfileShape(t *testing.T) {
	t.Parallel()

	t.Run("Google", func(t *testing.T) {
		p := oauth.Google("id", "secret")
		got, err := p.Parse([]byte(`{"sub":"1234","email":"sam@example.com",
			"email_verified":true,"name":"Sam"}`))
		if err != nil {
			t.Fatal(err)
		}
		want := oauth.Profile{Subject: "1234", EmailAddress: "sam@example.com",
			EmailVerified: true, DisplayName: "Sam"}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
		if _, err := p.Parse([]byte("{")); err == nil {
			t.Error("a body that is not JSON should be an error")
		}
	})

	t.Run("Microsoft", func(t *testing.T) {
		p := oauth.Microsoft("id", "secret", "")
		// The userinfo endpoint does not return email_verified, and an address
		// in a directory Microsoft controls is one it has already established.
		// Treating it as unverified would make linking impossible for every
		// Entra customer.
		got, err := p.Parse([]byte(`{"sub":"abcd","email":"sam@example.com","name":"Sam"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !got.EmailVerified {
			t.Errorf("got %+v, want a verified address", got)
		}
		blank, err := p.Parse([]byte(`{"sub":"abcd"}`))
		if err != nil {
			t.Fatal(err)
		}
		if blank.EmailVerified {
			t.Error("no address is not a verified address")
		}

		// The tenant in the URL is Microsoft's, not rig's: "common" accepts any
		// account, and a directory identifier restricts sign-in to one
		// organization.
		if !strings.Contains(p.Endpoint.AuthURL, "/common/") {
			t.Errorf("AuthURL = %q, want the common tenant by default", p.Endpoint.AuthURL)
		}
		one := oauth.Microsoft("id", "secret", "contoso.onmicrosoft.com")
		if !strings.Contains(one.Endpoint.AuthURL, "/contoso.onmicrosoft.com/") ||
			!strings.Contains(one.Endpoint.TokenURL, "/contoso.onmicrosoft.com/") {
			t.Errorf("both URLs should carry the tenant: %+v", one.Endpoint)
		}
	})

	t.Run("GitHub", func(t *testing.T) {
		p := oauth.GitHub("id", "secret")
		// The numeric id, not the login: a login can be given up and taken by
		// somebody else, which is exactly what this package exists to prevent.
		got, err := p.Parse([]byte(`{"id":42,"email":"sam@example.com","name":"Sam","login":"sam"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Subject != "42" {
			t.Errorf("Subject = %q, want the numeric id", got.Subject)
		}

		// The name is optional on GitHub, and a sign-in list of blanks is
		// unusable, so the login stands in.
		anonymous, err := p.Parse([]byte(`{"id":42,"login":"sam"}`))
		if err != nil {
			t.Fatal(err)
		}
		if anonymous.DisplayName != "sam" {
			t.Errorf("DisplayName = %q, want the login", anonymous.DisplayName)
		}
	})
}

// GitHub's user endpoint returns null for anybody who kept their address
// private and never says whether one is verified, so the addresses endpoint is
// the only one worth believing.
func TestGitHubAsksTheAddressesEndpointForAVerifiedAddress(t *testing.T) {
	t.Parallel()

	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/emails" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	// api.github.com is not configurable, so the client is what gets pointed
	// somewhere else.
	client := &http.Client{Transport: rewriteTo(srv.URL)}
	extra := oauth.GitHub("id", "secret").Extra

	for name, tc := range map[string]struct {
		addresses string
		want      string
		verified  bool
	}{
		"the primary verified one": {
			`[{"email":"other@example.com","verified":true},
			  {"email":"sam@example.com","primary":true,"verified":true}]`,
			"sam@example.com", true,
		},
		// Somebody with no primary set still has an account worth signing in to.
		"any verified one": {
			`[{"email":"sam@example.com","primary":true,"verified":false},
			  {"email":"other@example.com","verified":true}]`,
			"other@example.com", true,
		},
		// An unverified address is not evidence of anything, and linking on one
		// is the takeover this package is built to refuse.
		"none verified": {
			`[{"email":"sam@example.com","primary":true,"verified":false}]`,
			"", false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			body = tc.addresses

			profile := oauth.Profile{Subject: "42"}
			if err := extra(t.Context(), client, &profile); err != nil {
				t.Fatal(err)
			}
			if profile.EmailAddress != tc.want || profile.EmailVerified != tc.verified {
				t.Errorf("got %+v, want %q verified=%v", profile, tc.want, tc.verified)
			}
		})
	}

	// A malformed answer is a failure, not an empty address that would send the
	// sign-in on to a confusing error somewhere else.
	body = "{"
	profile := oauth.Profile{Subject: "42"}
	if err := extra(t.Context(), client, &profile); err == nil {
		t.Error("an unreadable answer should be an error")
	}
}

// rewriteTo sends every request to one host, whatever it was addressed to.
type rewriteTo string

func (to rewriteTo) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := url.Parse(string(to))
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL.Scheme, clone.URL.Host, clone.Host = u.Scheme, u.Host, u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// The tenant is decided at the start and carried, not resolved twice.
//
// The callback URL is registered with the provider and fixed, so it carries nothing
// an application's resolver could read: a header or a query parameter that was there
// at the start is gone by the time the provider sends the browser back. Only a host
// survives, which is why a subdomain deployment never noticed this. A resolver that
// answers once and then cannot is the case that used to sign somebody into the
// zero tenant.
func TestTheTenantSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	wanted := uuid.New()
	var calls int

	f := setup(t, oauth.Profile{
		Subject: "subject-1", EmailAddress: "ada@example.com",
		EmailVerified: true, DisplayName: "Ada",
	}, func(cfg *oauth.Config) {
		cfg.AllowProvisioning = true
		// Answers on the way out and refuses on the way back, which is what a
		// query parameter or a header does.
		cfg.Tenant = func(r *http.Request) (uuid.UUID, error) {
			calls++
			if raw := r.URL.Query().Get("tenant"); raw != "" {
				return uuid.Parse(raw)
			}
			return uuid.Nil, rigerr.BadRequest("no tenant on this request")
		}
	})

	res := f.signIn(t, "?tenant="+wanted.String())
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	if f.signedIn == nil {
		t.Fatal("no sign-in reached the application")
	}
	if f.signedIn.TenantID != wanted {
		t.Errorf("signed in to %s, want %s", f.signedIn.TenantID, wanted)
	}
	// Once. The second read is what used to lose it.
	if calls != 1 {
		t.Errorf("the resolver was called %d times, want 1", calls)
	}
}
