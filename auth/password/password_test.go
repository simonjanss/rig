package password_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// cheap keeps the suite fast. The cost parameters are what make argon2id worth
// using and what make a test that uses the real ones take a minute.
func cheap() password.Params {
	return password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	h := password.New(cheap())

	c, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	ok, rehash, err := h.Verify(c.Encoded, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("the password it was just given should verify")
	}
	if rehash {
		t.Error("a hash written at the current cost is not out of date")
	}
}

func TestAWrongPasswordDoesNotVerify(t *testing.T) {
	t.Parallel()

	h := password.New(cheap())
	c, _ := h.Hash("correct horse battery staple")

	ok, _, err := h.Verify(c.Encoded, "correct horse battery stapler")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a different password must not verify")
	}
}

// Two hashes of the same password must differ, or the salt is not doing its
// job and one rainbow table covers every account at once.
func TestTheSaltIsPerHash(t *testing.T) {
	t.Parallel()

	h := password.New(cheap())
	a, _ := h.Hash("correct horse battery staple")
	b, _ := h.Hash("correct horse battery staple")

	if a.Encoded == b.Encoded {
		t.Error("two hashes of one password are identical: the salt is not random")
	}
}

func TestTheEncodingIsAPHCString(t *testing.T) {
	t.Parallel()

	c, _ := password.New(cheap()).Hash("correct horse battery staple")

	// The format is what lets a hash be verified by anything that speaks it,
	// including a future version of this package at a different cost.
	if !strings.HasPrefix(c.Encoded, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("encoded = %q", c.Encoded)
	}
	if c.Algorithm != password.Algorithm {
		t.Errorf("algorithm = %q", c.Algorithm)
	}
	if c.Params.Memory != 8*1024 {
		t.Errorf("params were not recorded alongside the hash: %+v", c.Params)
	}
}

// A cost you can raise but never apply to existing accounts is a cost you have
// not raised. Login is the only moment the plaintext exists to rehash from.
func TestAHashBelowTheCurrentCostAsksToBeRehashed(t *testing.T) {
	t.Parallel()

	old := password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1})
	c, _ := old.Hash("correct horse battery staple")

	raised := password.New(password.Params{Memory: 16 * 1024, Iterations: 1, Parallelism: 1})

	ok, rehash, err := raised.Verify(c.Encoded, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("raising the cost must not lock anybody out: the old hash still verifies")
	}
	if !rehash {
		t.Error("a hash cheaper than the current cost should ask to be rehashed")
	}

	// And the other way round: a hash written at a higher cost than the
	// current one is not stale, it is generous.
	c2, _ := raised.Hash("correct horse battery staple")
	if _, rehash, _ := old.Verify(c2.Encoded, "correct horse battery staple"); rehash {
		t.Error("a more expensive hash should not be downgraded")
	}
}

// A corrupted row is not somebody typing the wrong password. Conflating them
// leaves an account permanently unable to log in with no sign of why.
func TestAMalformedHashIsAnError(t *testing.T) {
	t.Parallel()

	h := password.New(cheap())

	for _, tc := range []struct{ name, encoded string }{
		{"empty", ""},
		{"not phc", "hunter2"},
		{"wrong algorithm", "$bcrypt$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA"},
		{"wrong version", "$argon2id$v=16$m=8192,t=1,p=1$c2FsdA$aGFzaA"},
		{"truncated", "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA"},
		{"bad base64", "$argon2id$v=19$m=8192,t=1,p=1$!!!!$aGFzaA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, err := h.Verify(tc.encoded, "correct horse battery staple")
			if ok {
				t.Fatal("a hash that cannot be read must not verify")
			}
			if !errors.Is(err, password.ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

// Response time must not say whether an account exists.
func TestDummyIsAVerifiableHashOfNothing(t *testing.T) {
	t.Parallel()

	h := password.New(cheap())
	dummy := h.Dummy()

	if dummy == "" {
		t.Fatal("there is nothing to spend the time on")
	}
	if ok, _, err := h.Verify(dummy, "correct horse battery staple"); err != nil || ok {
		t.Errorf("the dummy must be real work that always fails: ok=%v err=%v", ok, err)
	}
	// Stable, so the work is done once rather than on every unknown address.
	if h.Dummy() != dummy {
		t.Error("the dummy should be derived once")
	}
}

func TestPolicyLength(t *testing.T) {
	t.Parallel()

	p := password.DefaultPolicy()
	ctx := context.Background()

	if err := p.Check(ctx, "short"); err == nil {
		t.Error("five characters should be refused")
	} else if !rigerr.Is(err, rigerr.CodeUnprocessableEntity) {
		t.Errorf("err = %v, want an invalid-input error", err)
	}

	if err := p.Check(ctx, "correct horse battery staple"); err != nil {
		t.Errorf("a long passphrase should pass: %v", err)
	}

	// Length is counted in characters, so a passphrase in a script that needs
	// several bytes per character is not penalized for it.
	if err := p.Check(ctx, "日本語のパスワードですよ"); err != nil {
		t.Errorf("eleven characters is still short, but this is twelve: %v", err)
	}

	// The maximum is not about strength. argon2id over a huge input is a
	// denial of service anybody can send.
	if err := p.Check(ctx, strings.Repeat("a", 2000)); err == nil {
		t.Error("an unbounded password is free work for an attacker")
	}
}

func TestPolicyHasNoCompositionRules(t *testing.T) {
	t.Parallel()

	// Twelve lowercase letters and nothing else. Demanding a symbol here is
	// what produces Password1! and a sticky note.
	if err := password.DefaultPolicy().Check(context.Background(), "abcdefghijkl"); err != nil {
		t.Errorf("a long lowercase password should pass: %v", err)
	}
}

func TestPolicyRejectsABreachedPassword(t *testing.T) {
	t.Parallel()

	p := password.DefaultPolicy()
	p.Breached = stub{count: 3_645_804}

	err := p.Check(context.Background(), "correct horse battery staple")
	if err == nil {
		t.Fatal("a password known to have leaked should be refused")
	}
	// The count invites an argument and helps nobody choose a better password.
	if strings.Contains(err.Error(), "3645804") {
		t.Errorf("the message should not quote the count: %v", err)
	}
}

// A third party being down is not the person's fault. Refusing every password
// because a breach service is unreachable trades a small risk for a total one.
func TestPolicyFailsOpen(t *testing.T) {
	t.Parallel()

	p := password.DefaultPolicy()
	p.Breached = stub{err: errors.New("connection refused")}

	if err := p.Check(context.Background(), "correct horse battery staple"); err != nil {
		t.Errorf("an unreachable breach service should not block a sign-up: %v", err)
	}
}

type stub struct {
	count int
	err   error
}

func (s stub) Count(context.Context, string) (int, error) { return s.count, s.err }

// The password never leaves the process: only five hex digits of its SHA-1 do.
func TestHIBPSendsOnlyAPrefix(t *testing.T) {
	t.Parallel()

	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		// The suffix of SHA-1("correct horse battery staple"), plus noise.
		w.Write([]byte("0000000000000000000000000000000000:12\r\n" +
			"AD6438836DBE526AA231ABDE2D0EEF74D42:99\r\n"))
	}))
	defer srv.Close()

	h := &password.HIBP{Client: srv.Client(), BaseURL: srv.URL}
	n, err := h.Count(context.Background(), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	// SHA-1("correct horse battery staple") = ABF7...; only the first five
	// characters may appear in the request.
	if want := "/range/ABF7A"; asked != want {
		t.Errorf("asked for %q, want %q", asked, want)
	}
	if len(strings.TrimPrefix(asked, "/range/")) != 5 {
		t.Errorf("more than a prefix was sent: %q", asked)
	}
	if n != 99 {
		t.Errorf("count = %d, want the matching suffix's count", n)
	}
}

func TestHIBPReportsAMissAsZero(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("0000000000000000000000000000000000:12\r\n"))
	}))
	defer srv.Close()

	h := &password.HIBP{Client: srv.Client(), BaseURL: srv.URL}
	n, err := h.Count(context.Background(), "a password nobody has ever used before")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// Reporting an outage as "not breached" is worse than having no check at all,
// because it looks like it is working.
func TestHIBPReportsAnOutage(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := &password.HIBP{Client: srv.Client(), BaseURL: srv.URL}
	if _, err := h.Count(context.Background(), "correct horse battery staple"); err == nil {
		t.Error("an unreachable service should be an error, not a clean bill of health")
	}
}

// The zero-valued checker is the one an application gets from a struct literal
// that only set BaseURL, and it has to work rather than panic on a nil client.
func TestHIBPFillsInItsOwnDefaults(t *testing.T) {
	t.Parallel()

	h := password.NewHIBP()
	if h.Client == nil || h.BaseURL == "" {
		t.Fatalf("NewHIBP = %+v, want a client and the public API", h)
	}
	// A password check sits on the sign-up path, and a hung request there is
	// worse than a skipped check.
	if h.Client.Timeout == 0 {
		t.Error("the default client should have a timeout")
	}

	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("Add-Padding")
		_, _ = w.Write([]byte("0000000000000000000000000000000000:12\r\n"))
	}))
	defer srv.Close()

	// No Client at all: the zero value has to reach the network anyway.
	bare := &password.HIBP{BaseURL: srv.URL}
	if _, err := bare.Count(context.Background(), "anything at all"); err != nil {
		t.Fatal(err)
	}
	// Padding asks for a variable number of decoy entries, so the response
	// size does not narrow down which prefix was asked for.
	if asked != "true" {
		t.Errorf("Add-Padding = %q, want true", asked)
	}
}

// A line the service cannot have meant is not a count of zero: answering zero
// would report a breached password as clean.
func TestHIBPRefusesAnUnreadableCount(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The suffix of SHA-1("correct horse battery staple") with nonsense
		// where the count belongs.
		_, _ = w.Write([]byte("AD6438836DBE526AA231ABDE2D0EEF74D42:lots\r\n"))
	}))
	defer srv.Close()

	h := &password.HIBP{Client: srv.Client(), BaseURL: srv.URL}
	if _, err := h.Count(context.Background(), "correct horse battery staple"); err == nil {
		t.Error("an unreadable count should be an error")
	}
}

// A checker whose host does not resolve is the ordinary outage, and the policy
// above it is what decides that an outage is not a failed check.
func TestHIBPReportsAConnectionFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	h := &password.HIBP{Client: &http.Client{Timeout: time.Second}, BaseURL: url}
	if _, err := h.Count(context.Background(), "correct horse battery staple"); err == nil {
		t.Error("a connection that cannot be made should be an error")
	}
}

// The hasher writes its own cost into every credential, so a caller who raises
// one parameter has to get that one raised and the rest left alone.
func TestNewFillsInOnlyWhatWasNotGiven(t *testing.T) {
	t.Parallel()

	defaults := password.DefaultParams()

	h := password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1})
	got := h.Params()

	if got.Memory != 8*1024 || got.Iterations != 1 || got.Parallelism != 1 {
		t.Errorf("params = %+v, want what was asked for", got)
	}
	// The two nobody usually thinks about still have to be set, or the salt is
	// zero bytes long.
	if got.SaltLength != defaults.SaltLength || got.KeyLength != defaults.KeyLength {
		t.Errorf("params = %+v, want the defaults for the rest", got)
	}

	if all := password.New(password.Params{}).Params(); all != defaults {
		t.Errorf("params = %+v, want %+v", all, defaults)
	}
}

// A password that is only spaces passes a length check and is not a password.
func TestAPasswordOfNothingButSpaceIsRefused(t *testing.T) {
	t.Parallel()

	p := password.DefaultPolicy()

	if err := p.Check(context.Background(), strings.Repeat(" ", 20)); err == nil {
		t.Error("whitespace is not a password")
	}
	// But a password with spaces in it is exactly what a passphrase looks like,
	// and it is stored as typed rather than trimmed: a value that worked at
	// sign-up has to keep working.
	if err := p.Check(context.Background(), " correct horse battery staple "); err != nil {
		t.Errorf("a passphrase should be allowed: %v", err)
	}
}
