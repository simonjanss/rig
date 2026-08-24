package throttle_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/throttle"
)

func names(checks []throttle.Check) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Limit.Name)
	}
	return out
}

func has(checks []throttle.Check, name string) bool {
	for _, c := range checks {
		if c.Limit.Name == name {
			return true
		}
	}
	return false
}

// Once a request says who it is, the address stops being the key. An office
// behind one NAT is one address, and a phone is a different one every few
// minutes — so an identity is both fairer and harder to get more of.
func TestTheAddressLimitIsForAnonymousCallersOnly(t *testing.T) {
	t.Parallel()

	limits := throttle.StandardAPILimits()

	signedIn := limits.Checks(throttle.Caller{
		Account: "acct-1", Tenant: "tenant-1", IP: "203.0.113.7",
	}, "GET /todos")
	if has(signedIn, throttle.NameIP) {
		t.Errorf("a signed-in caller was limited by address as well: %v", names(signedIn))
	}
	if !has(signedIn, throttle.NameAccount) || !has(signedIn, throttle.NameTenant) {
		t.Errorf("a signed-in caller is missing an identity limit: %v", names(signedIn))
	}

	anon := limits.Checks(throttle.Caller{IP: "203.0.113.7"}, "GET /todos")
	if len(anon) != 1 || !has(anon, throttle.NameIP) {
		t.Errorf("an anonymous caller got %v, want only the address limit", names(anon))
	}
}

func TestAnAPIKeyIsLimitedAsAKey(t *testing.T) {
	t.Parallel()

	checks := throttle.StandardAPILimits().Checks(throttle.Caller{
		APIKey: "key-1", Account: "acct-1", Tenant: "tenant-1", IP: "203.0.113.7",
	}, "GET /todos")

	if !has(checks, throttle.NameAPIKey) {
		t.Fatalf("no key limit: %v", names(checks))
	}
	if has(checks, throttle.NameIP) {
		t.Errorf("a key was also limited by address: %v", names(checks))
	}
	// And not as the account it acts as. The account limit is the tighter of the
	// two on any ladder worth writing, so counting both would mean the key's own
	// number — the whole point of allowing an integration more — never applies.
	if has(checks, throttle.NameAccount) {
		t.Errorf("a key was also limited as its account, so %d/min is the real key limit: %v",
			throttle.StandardAPILimits().ByAccount.Max, names(checks))
	}
	// The tenant ceiling is a different question and still applies.
	if !has(checks, throttle.NameTenant) {
		t.Errorf("the tenant ceiling did not apply to a key: %v", names(checks))
	}
}

func TestARouteLimitAppliesOnTopOfTheIdentityOnes(t *testing.T) {
	t.Parallel()

	limits := throttle.StandardAPILimits()
	limits.Routes = map[string]throttle.Limit{
		"POST /todos": {Name: throttle.NameRoutePrefix + "POST /todos", Max: 60, Window: time.Minute},
	}

	caller := throttle.Caller{Account: "acct-1", Tenant: "tenant-1"}

	on := limits.Checks(caller, "POST /todos")
	if !has(on, throttle.NameRoutePrefix+"POST /todos") {
		t.Fatalf("the route limit did not apply: %v", names(on))
	}
	if !has(on, throttle.NameAccount) {
		t.Errorf("the route limit replaced the identity limits instead of adding to them: %v", names(on))
	}

	off := limits.Checks(caller, "GET /todos")
	if has(off, throttle.NameRoutePrefix+"POST /todos") {
		t.Errorf("the route limit applied to another route: %v", names(off))
	}
}

func TestAnExemptRouteIsCheckedAtAll(t *testing.T) {
	t.Parallel()

	limits := throttle.StandardAPILimits()
	limits.Exempt = map[string]bool{"GET /todos/{id}/live_stream": true}

	checks := limits.Checks(throttle.Caller{Account: "acct-1"}, "GET /todos/{id}/live_stream")
	if len(checks) != 0 {
		t.Fatalf("an exempt route produced %v", names(checks))
	}
}

func TestAnUnsetLimitIsNotAnUnlimitedOne(t *testing.T) {
	t.Parallel()

	// A zero Max would refuse everything if it were treated as a number, and a
	// zero Window has no bucket to count in. Both mean "not configured".
	var limits throttle.APILimits
	if limits.Configured() {
		t.Fatal("the zero value claims to be configured")
	}
	if got := limits.Checks(throttle.Caller{Account: "a", IP: "203.0.113.7"}, "GET /x"); len(got) != 0 {
		t.Fatalf("the zero value produced %v", names(got))
	}
}

func TestNewGateOverNothingIsNil(t *testing.T) {
	t.Parallel()

	if g := throttle.NewGate(throttle.NewRecording(newTally()), throttle.APILimits{}, nil); g != nil {
		t.Fatal("a gate was built over no limits")
	}
	if g := throttle.NewGate(nil, throttle.StandardAPILimits(), nil); g != nil {
		t.Fatal("a gate was built over no limiter")
	}
	// And a nil gate is usable, which is what lets the generated server hold
	// one field either way.
	var g *throttle.Gate
	if err := g.Check(context.Background(), throttle.Caller{IP: "203.0.113.7"}, "GET /x", http.Header{}); err != nil {
		t.Fatalf("a nil gate refused: %v", err)
	}
}

func TestTheGateRefusesPastTheLimitAndSaysSo(t *testing.T) {
	t.Parallel()

	c := newClock()
	limits := throttle.APILimits{ByIP: throttle.Limit{Name: throttle.NameIP, Max: 3, Window: time.Minute}}
	gate := throttle.NewGate(
		throttle.NewRecording(throttle.NewLocal(newTally(), throttle.LocalConfig{Interval: 0})).WithClock(c.now),
		limits, nil)

	ctx := context.Background()
	caller := throttle.Caller{IP: "203.0.113.7"}

	for i := 1; i <= 3; i++ {
		if err := gate.Check(ctx, caller, "GET /todos", http.Header{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	h := http.Header{}
	err := gate.Check(ctx, caller, "GET /todos", h)
	if err == nil {
		t.Fatal("the call past the limit was allowed")
	}
	if _, ok := throttle.RefusalOf(err); !ok {
		t.Fatalf("the error is not a refusal: %v", err)
	}
	if h.Get("Retry-After") == "" {
		t.Error("no Retry-After, which leaves a client with nothing to do but guess")
	}
	if h.Get("RateLimit-Limit") != "3" {
		t.Errorf("RateLimit-Limit is %q", h.Get("RateLimit-Limit"))
	}
}

// The headers go out on the way through too, so a well-behaved client can slow
// down before it is refused rather than after.
func TestTheGateDescribesTheLimitWhileAllowing(t *testing.T) {
	t.Parallel()

	c := newClock()
	gate := throttle.NewGate(
		throttle.NewRecording(throttle.NewLocal(newTally(), throttle.LocalConfig{Interval: 0})).WithClock(c.now),
		throttle.APILimits{ByIP: throttle.Limit{Name: throttle.NameIP, Max: 10, Window: time.Minute}}, nil)

	h := http.Header{}
	if err := gate.Check(context.Background(), throttle.Caller{IP: "203.0.113.7"}, "GET /todos", h); err != nil {
		t.Fatal(err)
	}
	if h.Get("RateLimit-Remaining") != "9" {
		t.Fatalf("RateLimit-Remaining is %q after one call of ten", h.Get("RateLimit-Remaining"))
	}
}

// The asymmetry with the auth limits, pinned: a counter that cannot answer must
// not take the API down with it.
func TestTheGateFailsOpen(t *testing.T) {
	t.Parallel()

	rec := newTally()
	rec.err = errors.New("connection refused")

	var seen error
	gate := throttle.NewGate(
		throttle.NewRecording(rec),
		throttle.APILimits{ByIP: throttle.Limit{Name: throttle.NameIP, Max: 1, Window: time.Minute}},
		func(_ context.Context, err error) { seen = err })

	for range 5 {
		if err := gate.Check(context.Background(), throttle.Caller{IP: "203.0.113.7"}, "GET /todos", http.Header{}); err != nil {
			t.Fatalf("a database outage refused a request: %v", err)
		}
	}
	if seen == nil {
		t.Error("the failure was swallowed without telling anybody")
	}
}

func TestACallerWithNoIdentityAtAllIsNotChecked(t *testing.T) {
	t.Parallel()

	// No account, no key, no address — a Unix socket, or a test. There is
	// nothing to count against, and counting them all together would put every
	// such request in one budget.
	got := throttle.StandardAPILimits().Checks(throttle.Caller{}, "GET /todos")
	if len(got) != 0 {
		t.Fatalf("got %v", names(got))
	}
}
