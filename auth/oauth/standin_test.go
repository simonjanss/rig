package oauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/identity"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/auth/oauthtest"
)

// TestEveryProviderReadsWhatTheStandInWrites is the test that keeps
// auth/oauthtest honest, and it is the reason that package exists rather than
// each project writing its own.
//
// Every provider spells the same three facts differently. The stand-in has to
// write that spelling, and getting it wrong does not fail where the mistake is:
// a wrong key parses to an empty profile and arrives as ReasonInternal two
// layers away. So the two directions are checked against each other, through the
// real flow — Wear, the real Endpoint, the real Parse and the real Extra — rather
// than by comparing two literals.
//
// Extra is why this drives the whole sign-in instead of calling Parse. GitHub's
// overwrites EmailAddress and EmailVerified after Parse has run, so a Parse-only
// round trip would go on passing with that provider entirely broken.
func TestEveryProviderReadsWhatTheStandInWrites(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// build is the real constructor, untouched.
		build oauth.Provider
		// says is what the stand-in is told to say.
		says oauth.Profile
		// reads is what rig should end up with. It differs from says wherever a
		// provider genuinely tells you less than it was asked to.
		reads oauth.Profile
		// refuses is set instead of reads where telling you less is enough to
		// end the sign-in.
		refuses oauth.Reason
	}{
		{
			name:  "Google",
			build: oauth.Google("client", "secret"),
			says: oauth.Profile{
				Subject: "google-1", EmailAddress: "ada@example.com",
				EmailVerified: true, DisplayName: "Ada",
			},
			reads: oauth.Profile{
				Subject: "google-1", EmailAddress: "ada@example.com",
				EmailVerified: true, DisplayName: "Ada",
			},
		},
		{
			name:  "Google, unverified",
			build: oauth.Google("client", "secret"),
			says: oauth.Profile{
				Subject: "google-2", EmailAddress: "ada@example.com", DisplayName: "Ada",
			},
			reads: oauth.Profile{
				Subject: "google-2", EmailAddress: "ada@example.com", DisplayName: "Ada",
			},
		},
		{
			name:  "Microsoft",
			build: oauth.Microsoft("client", "secret", ""),
			says: oauth.Profile{
				Subject: "entra-1", EmailAddress: "ada@example.com", DisplayName: "Ada",
			},
			// Microsoft's userinfo returns no email_verified, and rig reads an
			// address in a directory it controls as established. So a profile
			// the stand-in called unverified comes back verified, and that is
			// the provider's rule rather than the double's mistake.
			reads: oauth.Profile{
				Subject: "entra-1", EmailAddress: "ada@example.com",
				EmailVerified: true, DisplayName: "Ada",
			},
		},
		{
			name:  "GitHub",
			build: oauth.GitHub("client", "secret"),
			says: oauth.Profile{
				Subject: "4242", EmailAddress: "ada@example.com",
				EmailVerified: true, DisplayName: "Ada",
			},
			// The address arrives from Extra's second call, not from the user
			// endpoint, which is the whole reason that hook exists.
			reads: oauth.Profile{
				Subject: "4242", EmailAddress: "ada@example.com",
				EmailVerified: true, DisplayName: "Ada",
			},
		},
		{
			name:  "GitHub, unverified",
			build: oauth.GitHub("client", "secret"),
			says: oauth.Profile{
				Subject: "4343", EmailAddress: "ada@example.com", DisplayName: "Ada",
			},
			// GitHub shares no unverified address at all, so rig gets none and
			// refuses — ReasonNoAddress rather than ReasonUnverifiedAddress,
			// which is a difference worth having a test say out loud. The
			// profile still arrived intact; there was just nothing in it to
			// sign anybody in with.
			refuses: oauth.ReasonNoAddress,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, fail := standIn(t, tc.build, tc.says)
			switch {
			case tc.refuses != "":
				if fail == nil {
					t.Fatalf("rig read %+v and signed in, want a %s refusal", got, tc.refuses)
				}
				if fail.Reason != tc.refuses {
					t.Errorf("refused with %s, want %s", fail.Reason, tc.refuses)
				}
			case fail != nil:
				t.Fatalf("rig refused with %s: %v", fail.Reason, fail.Err)
			case got != tc.reads:
				t.Errorf("rig read %+v\n      want %+v", got, tc.reads)
			}
		})
	}
}

// TestACustomProviderIsReadAsOpenIDConnect covers the in-house case, where the
// application owns both ends and the enum label is its own.
func TestACustomProviderIsReadAsOpenIDConnect(t *testing.T) {
	t.Parallel()

	srv := oauthtest.Start(t)
	want := oauth.Profile{
		Subject: "in-house-1", EmailAddress: "ada@example.com",
		EmailVerified: true, DisplayName: "Ada",
	}
	got, fail := standInWith(t, srv, srv.Custom("Acme"), want)
	if fail != nil {
		t.Fatalf("rig refused with %s: %v", fail.Reason, fail.Err)
	}
	if got != want {
		t.Errorf("rig read %+v, want %+v", got, want)
	}
}

// standIn drives one sign-in end to end and answers with the profile rig read,
// or with the refusal if it never got that far.
func standIn(t *testing.T, build oauth.Provider, says oauth.Profile) (oauth.Profile, *oauth.Failure) {
	t.Helper()
	srv := oauthtest.Start(t)
	return standInWith(t, srv, srv.Wear(build), says)
}

func standInWith(
	t *testing.T, srv *oauthtest.Server, p oauth.Provider, says oauth.Profile,
) (oauth.Profile, *oauth.Failure) {
	t.Helper()

	var (
		signedIn *oauth.SignIn
		failed   *oauth.Failure
	)
	mux := http.NewServeMux()

	// The stand-in's transport has to be on the request context, because the
	// exchange and the profile read are made by this server rather than by the
	// client below — and GitHub's second call has no URL to repoint.
	app := httptest.NewServer(srv.Middleware(mux))
	t.Cleanup(app.Close)

	h, err := oauth.New(oauth.Config{
		Store:             newStore(),
		Providers:         []oauth.Provider{p},
		BaseURL:           app.URL,
		SigningKey:        []byte("a signing key of at least thirty-two bytes"),
		Tenant:            func(*http.Request) (uuid.UUID, error) { return uuid.Nil, nil },
		AllowProvisioning: true,
		Insecure:          true, // plain HTTP, because httptest is
		OnSignIn: func(w http.ResponseWriter, _ *http.Request, in oauth.SignIn) error {
			signedIn = &in
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
		OnError: func(w http.ResponseWriter, _ *http.Request, f *oauth.Failure) {
			failed = f
			w.WriteHeader(http.StatusBadRequest)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Mount(mux)

	client := oauthtest.Client()
	route := app.URL + "/auth/oauth/" + strings.ToLower(p.Name)

	start, err := client.Get(route + "/start")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	if start.StatusCode != http.StatusFound {
		t.Fatalf("start answered %d, want a redirect to the provider", start.StatusCode)
	}
	to, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	// A zero profile is how this helper says "refuse the exchange": there is
	// nothing to sign in as, so the only thing left to exercise is the refusal.
	code := srv.Expect(says)
	if says == (oauth.Profile{}) {
		code = srv.ExpectRefusal()
	}

	back, err := client.Get(route + "/callback?code=" + url.QueryEscape(code) +
		"&state=" + url.QueryEscape(to.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	back.Body.Close()

	if signedIn == nil && failed == nil {
		t.Fatalf("the callback answered %d, and neither ending was reached", back.StatusCode)
	}
	if signedIn == nil {
		return oauth.Profile{}, failed
	}
	return signedIn.Profile, failed
}

// TestAProviderThatRefusesTheExchange covers the branch a project cannot reach
// without a stand-in: the provider authenticated somebody and then would not
// trade the code, which is what a wrong client secret looks like from here.
func TestAProviderThatRefusesTheExchange(t *testing.T) {
	t.Parallel()

	srv := oauthtest.Start(t)
	_, fail := standInWith(t, srv, srv.Wear(oauth.Google("client", "secret")), oauth.Profile{})
	if fail == nil {
		t.Fatal("a refused exchange signed somebody in")
	}
	if fail.Reason != oauth.ReasonExchange {
		t.Errorf("refused with %s, want %s", fail.Reason, oauth.ReasonExchange)
	}
}

// TestTheIdentityGateIsAskedAboutStrangersAndNobodyElse is the contract
// auth.Config.AllowIdentity states, and the reason an application's gate can be
// the domain rule and nothing else.
//
// If it were asked about everybody, the body every deployment wrote would have
// to be "allowed if the domain matches, or if they already have an account, or
// if somebody invited them" — and the last two branches would be guarding rows
// rig had already decided about.
func TestTheIdentityGateIsAskedAboutStrangersAndNobodyElse(t *testing.T) {
	t.Parallel()

	srv := oauthtest.Start(t)
	p := srv.Wear(oauth.Google("client", "secret"))

	var asked []identity.Candidate
	f := gated(t, srv, p, func(_ context.Context, in identity.Candidate) error {
		asked = append(asked, in)
		return nil
	})

	// A stranger. Asked about.
	ada := oauth.Profile{
		Subject: "google-ada", EmailAddress: "ada@example.com",
		EmailVerified: true, DisplayName: "Ada",
	}
	if _, fail := f.signIn(t, ada); fail != nil {
		t.Fatalf("the first sign-in was refused: %v", fail.Err)
	}
	if len(asked) != 1 {
		t.Fatalf("a stranger was asked about %d times, want 1", len(asked))
	}
	if asked[0].Via != identity.SourceProvider || asked[0].Provider != oauth.ProviderGoogle {
		t.Errorf("asked about %+v", asked[0])
	}
	if !asked[0].EmailVerified {
		t.Error("a provider sign-in reaches the gate with a verified address, and this one did not")
	}

	// The same person again, on the link. Not asked about.
	if _, fail := f.signIn(t, ada); fail != nil {
		t.Fatalf("the second sign-in was refused: %v", fail.Err)
	}
	if len(asked) != 1 {
		t.Fatalf("somebody who already signs in here was asked about again: %+v", asked[1:])
	}

	// The same address at a different provider subject, which is the
	// second-provider case: matched by address, linked, and not asked about.
	if _, fail := f.signIn(t, oauth.Profile{
		Subject: "google-ada-2", EmailAddress: "ada@example.com",
		EmailVerified: true, DisplayName: "Ada",
	}); fail != nil {
		t.Fatalf("linking a second provider was refused: %v", fail.Err)
	}
	if len(asked) != 1 {
		t.Fatalf("somebody adding a second provider was asked about: %+v", asked[1:])
	}
}

// TestARefusedStrangerIsNoAccount pins what a refusal looks like from outside,
// including that the gate's own sentence is what reaches the ending — which is
// the part somebody can act on, given that Reason cannot say "wrong domain".
func TestARefusedStrangerIsNoAccount(t *testing.T) {
	t.Parallel()

	srv := oauthtest.Start(t)
	p := srv.Wear(oauth.Google("client", "secret"))
	f := gated(t, srv, p, identity.AllowDomains("example.com"))

	_, fail := f.signIn(t, oauth.Profile{
		Subject: "google-mallory", EmailAddress: "mallory@other.test",
		EmailVerified: true, DisplayName: "Mallory",
	})
	if fail == nil {
		t.Fatal("an address outside the list signed in")
	}
	if fail.Reason != oauth.ReasonNoAccount {
		t.Errorf("refused with %s, want %s", fail.Reason, oauth.ReasonNoAccount)
	}
	if !strings.Contains(fail.Err.Error(), "example.com") {
		t.Errorf("the refusal said %q, and the gate's own sentence is what a front end has to show",
			fail.Err)
	}
}

// TestAGateCannotOpenADoorAllowProvisioningShut is the other half: the bool and
// the gate are both asked, so adding a gate to a deployment that had
// provisioning off does not quietly turn it on.
func TestAGateCannotOpenADoorAllowProvisioningShut(t *testing.T) {
	t.Parallel()

	srv := oauthtest.Start(t)
	p := srv.Wear(oauth.Google("client", "secret"))

	asked := false
	f := gated(t, srv, p, func(context.Context, identity.Candidate) error {
		asked = true
		return nil
	}, func(c *oauth.Config) { c.AllowProvisioning = false })

	_, fail := f.signIn(t, oauth.Profile{
		Subject: "google-nobody", EmailAddress: "nobody@example.com",
		EmailVerified: true, DisplayName: "Nobody",
	})
	if fail == nil {
		t.Fatal("provisioning was off and somebody was still created")
	}
	if fail.Reason != oauth.ReasonNoAccount {
		t.Errorf("refused with %s, want %s", fail.Reason, oauth.ReasonNoAccount)
	}
	if asked {
		t.Error("the gate was asked about a door that was already shut, which spends a hook on nothing")
	}
}

// gatedFixture is one handler, reused across several sign-ins, so that a test
// about who is asked about can sign the same person in twice.
type gatedFixture struct {
	srv   *oauthtest.Server
	app   *httptest.Server
	route string

	signedIn *oauth.SignIn
	failed   *oauth.Failure
}

func gated(
	t *testing.T, srv *oauthtest.Server, p oauth.Provider,
	gate identity.Gate, tweak ...func(*oauth.Config),
) *gatedFixture {
	t.Helper()

	f := &gatedFixture{srv: srv}
	mux := http.NewServeMux()
	f.app = httptest.NewServer(srv.Middleware(mux))
	t.Cleanup(f.app.Close)

	cfg := oauth.Config{
		Store:             newStore(),
		Providers:         []oauth.Provider{p},
		BaseURL:           f.app.URL,
		SigningKey:        []byte("a signing key of at least thirty-two bytes"),
		Tenant:            func(*http.Request) (uuid.UUID, error) { return uuid.Nil, nil },
		AllowProvisioning: true,
		AllowIdentity:     gate,
		Insecure:          true,
		OnSignIn: func(w http.ResponseWriter, _ *http.Request, in oauth.SignIn) error {
			f.signedIn, f.failed = &in, nil
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
		OnError: func(w http.ResponseWriter, _ *http.Request, fail *oauth.Failure) {
			f.signedIn, f.failed = nil, fail
			w.WriteHeader(http.StatusForbidden)
		},
	}
	for _, tw := range tweak {
		tw(&cfg)
	}

	h, err := oauth.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.Mount(mux)
	f.route = f.app.URL + "/auth/oauth/" + strings.ToLower(p.Name)
	return f
}

func (f *gatedFixture) signIn(t *testing.T, says oauth.Profile) (*oauth.SignIn, *oauth.Failure) {
	t.Helper()

	client := oauthtest.Client()
	start, err := client.Get(f.route + "/start")
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	to, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	back, err := client.Get(f.route + "/callback?code=" + url.QueryEscape(f.srv.Expect(says)) +
		"&state=" + url.QueryEscape(to.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	back.Body.Close()
	return f.signedIn, f.failed
}
