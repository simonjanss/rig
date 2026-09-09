package oauth_test

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// caught is what an OnError hook kept, and a stand-in for the sign-in page an
// application would have redirected to.
type caught struct {
	failure *oauth.Failure
	calls   int
}

// hook is an OnError that records and redirects, which is what every
// browser-facing application does with one.
func (c *caught) hook(w http.ResponseWriter, r *http.Request, f *oauth.Failure) {
	c.calls++
	c.failure = f
	to := url.URL{Path: "/login", RawQuery: url.Values{"error": {string(f.Reason)}}.Encode()}
	http.Redirect(w, r, to.String(), http.StatusSeeOther)
}

// Pressing cancel is the failure that brought this hook into existence: rig's
// default leaves a browser on a plain-text page on the API's own origin, and
// there is no way back to the application from there.
func TestCancellingAtTheProviderReachesTheHook(t *testing.T) {
	t.Parallel()

	var got caught
	f := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) { c.OnError = got.hook })

	res := get(t, f.srv.URL+"/auth/oauth/google/callback?error=access_denied")
	defer res.Body.Close()

	if got.calls != 1 {
		t.Fatalf("%d calls to OnError, want 1", got.calls)
	}
	if got.failure.Reason != oauth.ReasonCancelled {
		t.Errorf("reason = %q, want %q", got.failure.Reason, oauth.ReasonCancelled)
	}
	if got.failure.Provider != "google" {
		t.Errorf("provider = %q, want google", got.failure.Provider)
	}
	if got.failure.ProviderError != "access_denied" {
		t.Errorf("provider error = %q, want access_denied", got.failure.ProviderError)
	}
	// The hook's response, and nothing of rig's around it.
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", res.StatusCode)
	}
	if to := res.Header.Get("Location"); to != "/login?error=cancelled" {
		t.Errorf("location = %q, want the sign-in page", to)
	}
	// None of what http.Error would have set is here, and that is the reason a
	// hook is not the same thing as a wrapper around the mux. Replacing the
	// response from outside this package means deleting these by hand, by name,
	// one at a time — clearing the header map wholesale would take rig's
	// state-cookie deletion with it. From in here they were never written.
	//
	// The text/html and Content-Length that *are* here are http.Redirect's own
	// little body, which is the hook's response and not rig's.
	if ct := res.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q: the default answered underneath the hook", ct)
	}
	if nosniff := res.Header.Get("X-Content-Type-Options"); nosniff != "" {
		t.Errorf("X-Content-Type-Options = %q, which only http.Error sets", nosniff)
	}
	// And the cookie rig deletes on its way into a failure is still deleted: it
	// is single-use, and one left behind is a state somebody could replay.
	var cleared bool
	for _, c := range res.Cookies() {
		if c.Name == "rig_oauth" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the state cookie was not cleared")
	}
}

// A cancel and a policy the account's administrator enforces are two different
// sentences to a person, and telling them apart is the whole reason the reason
// exists: both are a 400.
func TestAnyOtherProviderErrorIsNotACancel(t *testing.T) {
	t.Parallel()

	var got caught
	f := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) { c.OnError = got.hook })

	res := get(t, f.srv.URL+"/auth/oauth/google/callback?error=consent_required")
	defer res.Body.Close()

	if got.failure.Reason != oauth.ReasonProviderRefused {
		t.Errorf("reason = %q, want %q", got.failure.Reason, oauth.ReasonProviderRefused)
	}
	if got.failure.ProviderError != "consent_required" {
		t.Errorf("provider error = %q, want consent_required", got.failure.ProviderError)
	}
	if rigerr.CodeOf(got.failure) != rigerr.CodeBadRequest {
		t.Errorf("code = %q, want BadRequest: a refusal is not a server failure",
			rigerr.CodeOf(got.failure))
	}
}

// The provider's own error value is attacker-controlled text on a query string.
// A reason rig chose is what reaches the application, so a redirect to its own
// origin cannot carry anything a stranger wrote.
func TestTheProvidersErrorValueIsNotWhatTheApplicationRedirectsWith(t *testing.T) {
	t.Parallel()

	var got caught
	f := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) { c.OnError = got.hook })

	const injected = "access_denied\">click</a"
	res := get(t, f.srv.URL+"/auth/oauth/google/callback?error="+url.QueryEscape(injected))
	defer res.Body.Close()

	// On the Failure, for a log line.
	if got.failure.ProviderError != injected {
		t.Errorf("provider error = %q, want it kept verbatim", got.failure.ProviderError)
	}
	// And nowhere near the response.
	if to := res.Header.Get("Location"); to != "/login?error=provider_refused" {
		t.Errorf("location = %q, want only the reason", to)
	}
}

// An expired or missing state cookie is the commonest failure in a real
// deployment — a callback that landed on another host, a sign-in finished in a
// different browser — and rig cannot say where it was going, because where it
// was going was in the cookie.
func TestAStateFailureKnowsNothingAboutWhereItWasGoing(t *testing.T) {
	t.Parallel()

	var got caught
	f := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) { c.OnError = got.hook })

	res := get(t, f.srv.URL+"/auth/oauth/google/callback?code=a-code&state=invented")
	defer res.Body.Close()

	if got.failure.Reason != oauth.ReasonState {
		t.Errorf("reason = %q, want %q", got.failure.Reason, oauth.ReasonState)
	}
	if got.failure.ReturnTo != "" {
		t.Errorf("return to = %q, want empty: it was in the cookie", got.failure.ReturnTo)
	}
}

// The one thing a wrapper outside this package cannot recover. returnTo lives
// in the sealed state cookie, so only rig can hand it back — which is what lets
// a failed sign-in return to the page that started it rather than to a sign-in
// page's own default.
func TestAFailureAfterTheCookieOpensStillKnowsWhereItWasGoing(t *testing.T) {
	t.Parallel()

	var got caught
	f := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) {
		c.OnError = got.hook
		c.AllowProvisioning = true
	})

	// A real start, so there is a real cookie, and then a callback with no code
	// on it — a failure raised after open has already read the returnTo.
	jar := &recordingJar{}
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Get(f.srv.URL + "/auth/oauth/google/start?returnTo=%2Freports%2F7")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	state := stateFrom(t, res)

	res, err = client.Get(f.srv.URL + "/auth/oauth/google/callback?state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if got.failure.Reason != oauth.ReasonNoCode {
		t.Fatalf("reason = %q, want %q", got.failure.Reason, oauth.ReasonNoCode)
	}
	if got.failure.ReturnTo != "/reports/7" {
		t.Errorf("return to = %q, want /reports/7", got.failure.ReturnTo)
	}
	if got.failure.TenantID != f.tenant {
		t.Errorf("tenant = %v, want %v", got.failure.TenantID, f.tenant)
	}
}

// Two refusals that are one status and one sentence apart, and that a person
// needs told differently: one is worth trying again from another account, the
// other is a verification to go and do at the provider. From outside this
// package they are indistinguishable, which is why the platform that hit this
// had to answer both with the same message.
func TestARefusedSignInSaysWhichRefusalItWas(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		profile oauth.Profile
		known   bool
		want    oauth.Reason
		// wantEmail is what the hook should be handed, which is not always what
		// the provider said: an address is lowercased everywhere else in this
		// package, and a hook holding the provider's casing would be holding a
		// different string for the same person.
		wantEmail string
	}{
		{
			name:      "nobody has this address and provisioning is off",
			profile:   oauth.Profile{Subject: "new", EmailAddress: "nobody@example.com", EmailVerified: true},
			want:      oauth.ReasonNoAccount,
			wantEmail: "nobody@example.com",
		},
		{
			name:      "somebody has it and the provider has not verified it",
			profile:   oauth.Profile{Subject: "attacker", EmailAddress: "Sam@Example.com"},
			known:     true,
			want:      oauth.ReasonUnverifiedAddress,
			wantEmail: "sam@example.com",
		},
		{
			name:    "the provider shared no address at all",
			profile: oauth.Profile{Subject: "anonymous"},
			want:    oauth.ReasonNoAddress,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got caught
			f := setup(t, tc.profile, func(c *oauth.Config) { c.OnError = got.hook })
			if tc.known {
				f.store.put(f.tenant, strings.ToLower(tc.profile.EmailAddress))
			}

			res := f.signIn(t, "")
			res.Body.Close()

			if got.failure == nil {
				t.Fatal("the sign-in should have been refused")
			}
			if got.failure.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.failure.Reason, tc.want)
			}
			if got.failure.EmailAddress != tc.wantEmail {
				t.Errorf("email = %q, want %q", got.failure.EmailAddress, tc.wantEmail)
			}
			if f.signedIn != nil {
				t.Error("the sign-in should not have completed")
			}
		})
	}
}

// The last step refusing is its own reason: the provider did answer, and what
// failed is the session. Both records survive, and they are not in conflict.
func TestAnEndingThatRefusesIsRecordedBesideTheProvidersAnswer(t *testing.T) {
	t.Parallel()

	log := &recorder{}
	var got caught
	f := setup(t, oauth.Profile{
		Subject: "s", EmailAddress: "Sam@Example.com", EmailVerified: true,
	}, func(c *oauth.Config) {
		c.AllowProvisioning = true
		c.Log = log
		c.OnError = got.hook
		c.OnSignIn = func(http.ResponseWriter, *http.Request, oauth.SignIn) error {
			return rigerr.Forbidden("you do not belong to this tenant")
		}
	})

	res := f.signIn(t, "")
	res.Body.Close()

	if got.failure.Reason != oauth.ReasonEnding {
		t.Errorf("reason = %q, want %q", got.failure.Reason, oauth.ReasonEnding)
	}
	if rigerr.CodeOf(got.failure) != rigerr.CodeForbidden {
		t.Errorf("code = %q, want the ending's own", rigerr.CodeOf(got.failure))
	}
	if got.failure.EmailAddress != "sam@example.com" {
		t.Errorf("the hook was handed %q, want it lowercased", got.failure.EmailAddress)
	}

	entries := log.of(authlog.EventOAuthSignIn)
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2: the provider answered and the session was not issued", len(entries))
	}
	if entries[0].Outcome != authlog.Succeeded || entries[1].Outcome != authlog.Failed {
		t.Errorf("outcomes = %q then %q, want a success then a failure",
			entries[0].Outcome, entries[1].Outcome)
	}
	// And they name the same provider the same way. Failure.Provider is
	// lowercased for an application to switch on, and reading it straight into
	// the entry would put two spellings of Google in one column — one an
	// operator querying the trail has to remember to ask for twice.
	if got, want := entries[1].Detail["provider"], entries[0].Detail["provider"]; got != want {
		t.Errorf("the failure names the provider %q and the success %q", got, want)
	}
	if got := entries[1].Detail["provider"]; got != oauth.ProviderGoogle {
		t.Errorf("provider = %v, want the provider's own spelling %q", got, oauth.ProviderGoogle)
	}
	if got := entries[1].EmailAddress; got != "sam@example.com" {
		t.Errorf("email = %q, want it lowercased", got)
	}
}

// Every way a sign-in can fail leaves a record, which is what stops a branch
// added later being invisible: before this, fourteen of sixteen wrote nothing
// at all, so an expired cookie or a refused code exchange was evidenced only by
// a page nobody kept.
func TestEveryRefusalIsWrittenToTheAuthenticationLog(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		want oauth.Reason
		// wantProvider is the spelling the entry records, which is the
		// provider's own and not the route's — the whole trail is queried by
		// this column, and one provider under two spellings is one an operator
		// has to ask for twice. Empty for a provider rig does not have, since
		// there is nothing to spell.
		wantProvider string
		// drive returns the response, having done whatever it takes to fail.
		drive func(t *testing.T, f *fixture) *http.Response
	}{
		{
			name: "a provider this deployment does not have",
			want: oauth.ReasonUnknownProvider,
			drive: func(t *testing.T, f *fixture) *http.Response {
				return get(t, f.srv.URL+"/auth/oauth/gitlab/start")
			},
		},
		{
			name:         "a returnTo that is not allowed",
			want:         oauth.ReasonReturnTo,
			wantProvider: oauth.ProviderGoogle,
			drive: func(t *testing.T, f *fixture) *http.Response {
				return get(t, f.srv.URL+"/auth/oauth/google/start?returnTo=https%3A%2F%2Felsewhere.example")
			},
		},
		{
			name:         "a callback with no cookie",
			want:         oauth.ReasonState,
			wantProvider: oauth.ProviderGoogle,
			drive: func(t *testing.T, f *fixture) *http.Response {
				return get(t, f.srv.URL+"/auth/oauth/google/callback?code=c&state=invented")
			},
		},
		{
			name:         "a cancel at the consent screen",
			want:         oauth.ReasonCancelled,
			wantProvider: oauth.ProviderGoogle,
			drive: func(t *testing.T, f *fixture) *http.Response {
				return get(t, f.srv.URL+"/auth/oauth/google/callback?error=access_denied")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log := &recorder{}
			f := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) { c.Log = log })

			res := tc.drive(t, f)
			res.Body.Close()

			entries := log.of(authlog.EventOAuthSignIn)
			if len(entries) != 1 {
				t.Fatalf("%d entries, want 1", len(entries))
			}
			e := entries[0]
			if e.Outcome != authlog.Failed {
				t.Errorf("outcome = %q, want a failure", e.Outcome)
			}
			if e.Detail["reason"] != string(tc.want) {
				t.Errorf("reason = %v, want %q", e.Detail["reason"], tc.want)
			}
			if e.Detail["provider"] != tc.wantProvider {
				t.Errorf("provider = %v, want %q", e.Detail["provider"], tc.wantProvider)
			}
			if e.Detail["error"] == nil {
				t.Error("no error detail: the message is the only thing that says which branch")
			}
			if e.UserAgent == "" || e.IPAddress == "" {
				t.Errorf("address %q and agent %q, want both recorded", e.IPAddress, e.UserAgent)
			}
			if e.TenantID != nil {
				t.Errorf("tenant %v recorded, want none: no sign-in got far enough", *e.TenantID)
			}
		})
	}
}

// A wrong client secret refuses every sign-in identically, so the provider's own
// refusal is the only thing that distinguishes it from a broken flow. It used to
// be dropped on the floor.
func TestARefusedCodeExchangeKeepsWhatTheProviderSaid(t *testing.T) {
	t.Parallel()

	log := &recorder{}
	var got caught
	f := setup(t, oauth.Profile{Subject: "s"}, func(c *oauth.Config) {
		c.Log = log
		c.OnError = got.hook
	})
	f.provider.refuseExchange = true

	res := f.signIn(t, "")
	res.Body.Close()

	if got.failure.Reason != oauth.ReasonExchange {
		t.Fatalf("reason = %q, want %q", got.failure.Reason, oauth.ReasonExchange)
	}
	// What the person is told is unchanged.
	if !strings.Contains(got.failure.Error(), "refused the authorization code") {
		t.Errorf("message = %q, want the one rig has always answered with", got.failure.Error())
	}
	// And the cause is reachable underneath it.
	if errors.Unwrap(got.failure.Err) == nil {
		t.Error("the provider's refusal was dropped, so a wrong secret is undebuggable")
	}
	if detail := log.of(authlog.EventOAuthSignIn); len(detail) != 1 {
		t.Errorf("%d entries, want 1", len(detail))
	}
}

// A single-use cookie left in the browser is a state somebody could replay, and
// the callback promises to clear it whatever happens next — including a callback
// naming a provider that does not exist, which used to return first.
func TestAnUnknownProviderCallbackStillClearsTheState(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s"}, nil)

	res := get(t, f.srv.URL+"/auth/oauth/gitlab/callback?code=c&state=s")
	res.Body.Close()

	var cleared bool
	for _, c := range res.Cookies() {
		if c.Name == "rig_oauth" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the state cookie was left behind")
	}
}

// The default is what every project that sets no hook still gets, so it is
// pinned by body and not only by status — which is what makes the hook additive.
func TestWithNoHookTheFailureIsStillPlainText(t *testing.T) {
	t.Parallel()

	f := setup(t, oauth.Profile{Subject: "s"}, nil)

	res := get(t, f.srv.URL+"/auth/oauth/google/callback?error=access_denied")
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q, want text/plain", ct)
	}
	body, _ := io.ReadAll(res.Body)
	const want = "Google did not complete the sign-in: access_denied\n"
	if string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// A Failure is an error, so everything that reads one keeps working: a project
// that wants the JSON envelope on these routes passes its own error writer and
// gets the code and the message it always would have.
func TestAFailureIsTheErrorItCarries(t *testing.T) {
	t.Parallel()

	inner := rigerr.Forbidden("there is no account for this address")
	f := &oauth.Failure{Reason: oauth.ReasonNoAccount, Provider: "google", Err: inner}

	if rigerr.CodeOf(f) != rigerr.CodeForbidden {
		t.Errorf("code = %q, want Forbidden", rigerr.CodeOf(f))
	}
	if f.Error() != inner.Error() {
		t.Errorf("message = %q, want %q", f.Error(), inner.Error())
	}
	var typed *rigerr.Error
	if !errors.As(f, &typed) {
		t.Fatal("errors.As found no *rigerr.Error, so an error writer would call it internal")
	}
	if typed.Message != inner.Message {
		t.Errorf("message = %q, want %q", typed.Message, inner.Message)
	}
	if f.TenantID != uuid.Nil {
		t.Error("the zero tenant should be nil, not a tenant that does not exist")
	}

	// And a Failure with nothing underneath still has something to say, because
	// a String that panics turns a mistake in a hook into a crash in the
	// sign-in it was handling.
	bare := &oauth.Failure{Reason: oauth.ReasonCancelled}
	if bare.Error() == "" {
		t.Error("a Failure with no error underneath has no message")
	}
	if !strings.Contains(bare.Error(), string(oauth.ReasonCancelled)) {
		t.Errorf("message = %q, want the reason in it", bare.Error())
	}
}

// get fetches without following, because a redirect is what a hook writes and
// following it would test the sign-in page instead of the hook.
func get(t *testing.T, url string) *http.Response {
	t.Helper()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// stateFrom reads the state parameter out of the redirect to the provider, so a
// test can come back with a callback the cookie will accept.
func stateFrom(t *testing.T, res *http.Response) string {
	t.Helper()

	to, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := to.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in %q", res.Header.Get("Location"))
	}
	return state
}
