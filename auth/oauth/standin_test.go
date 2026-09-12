package oauth_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

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
