package throttle

import (
	"context"
	"net/http"
	"time"
)

// The names the API limits count under. They are part of the counter's primary
// key, so they are constants rather than anything derived: changing one silently
// hands everybody a fresh budget.
const (
	NameAPIKey  = "api.api_key"
	NameAccount = "api.account"
	NameTenant  = "api.tenant"
	NameIP      = "api.ip"
	// NameRoutePrefix is joined with the route pattern, so two routes with
	// limits are two budgets rather than one shared one.
	NameRoutePrefix = "api.route:"
)

// Caller is who a request is from, as the API limits key on them.
//
// Strings rather than the claims themselves, so that this package does not
// depend on tenancy — a limiter should not need to know what a tenant is to
// count against one. Empty means the request did not carry that identity.
type Caller struct {
	APIKey  string
	Account string
	Tenant  string
	IP      string
}

// Identified reports whether the request said who it was.
func (c Caller) Identified() bool { return c.APIKey != "" || c.Account != "" }

// narrowest is the most specific key this caller has, for a limit that applies
// to a route rather than to an identity.
func (c Caller) narrowest() (Key, bool) {
	switch {
	case c.APIKey != "":
		return APIKey(c.APIKey), true
	case c.Account != "":
		return Account(c.Account), true
	case c.Tenant != "":
		return Tenant(c.Tenant), true
	case c.IP != "":
		return IP(c.IP), true
	}
	return Key{}, false
}

// APILimits is what an API allows one caller, per window.
//
// The zero value limits nothing, which is what a project that configured no
// throttle block gets.
type APILimits struct {
	// ByAPIKey, ByAccount and ByTenant apply to a caller who said who they
	// were. A machine integration is usually allowed the most, one person the
	// least, and the tenant ceiling is what stops one customer's batch job
	// starving the others.
	//
	// The first two are alternatives rather than a pair: a request made with a
	// key is counted as that key and not also as the account the key acts as.
	// Otherwise the tighter of the two — the account, on any sane ladder —
	// decides every key-authenticated request and the key's own number is
	// decoration. ByTenant applies either way, because it is a different
	// question: one customer's whole share.
	ByAPIKey  Limit
	ByAccount Limit
	ByTenant  Limit

	// ByIP applies only to a caller who did not say who they were.
	//
	// Only, and deliberately. An address is a poor name for a person — an
	// office behind one NAT is one address and a phone changes address between
	// requests — so once a request carries an identity, that identity is the
	// better key and the address is noise. It is also the weakest key there is,
	// which is worth saying out loud: addresses are cheap, and an attacker with
	// many of them is limited many times over rather than once.
	ByIP Limit

	// Routes are extra limits on particular route patterns, counted against the
	// caller's narrowest identity and on top of whatever the limits above
	// allow. The key is the pattern net/http matched, for example
	// "POST /api/v1/todos".
	Routes map[string]Limit

	// Exempt are patterns nothing applies to. A route whose whole job is to be
	// called constantly — a health check somebody put behind the API, a
	// long-lived stream — is better named here than given a number that has to
	// be guessed.
	Exempt map[string]bool
}

// Configured reports whether any limit is set. A gate over nothing should not
// be built, rather than built and then skipped on every request.
func (l APILimits) Configured() bool {
	if len(l.Routes) > 0 {
		return true
	}
	for _, lim := range []Limit{l.ByAPIKey, l.ByAccount, l.ByTenant, l.ByIP} {
		if usable(lim) {
			return true
		}
	}
	return false
}

func usable(l Limit) bool { return l.Max > 0 && l.Window > 0 }

// Checks are the limits that apply to one request.
func (l APILimits) Checks(c Caller, pattern string) []Check {
	if l.Exempt[pattern] {
		return nil
	}

	var out []Check
	add := func(lim Limit, k Key, have bool) {
		if have && usable(lim) {
			out = append(out, Check{Limit: lim, Key: k})
		}
	}

	if c.Identified() {
		// A key or an account, not both. A key's claims name the account it acts
		// as, so counting both would put every key-authenticated request under
		// the account ceiling as well — and since the tightest check is the one
		// that decides, the key's own allowance could never be reached. The
		// ladder is only a ladder if each rung is the whole answer for the
		// caller standing on it.
		if c.APIKey != "" {
			add(l.ByAPIKey, APIKey(c.APIKey), true)
		} else {
			add(l.ByAccount, Account(c.Account), c.Account != "")
		}
		add(l.ByTenant, Tenant(c.Tenant), c.Tenant != "")
	} else {
		add(l.ByIP, IP(c.IP), c.IP != "")
	}

	if lim, ok := l.Routes[pattern]; ok {
		k, have := c.narrowest()
		add(lim, k, have)
	}
	return out
}

// Gate is the API limiter as a server holds it.
//
// It is here rather than written into each generated server because the two
// decisions it encodes — which limits apply, and what to do when the counter
// cannot answer — should have one implementation and one set of tests.
type Gate struct {
	limiter *Limiter
	limits  APILimits
	onError func(context.Context, error)
}

// NewGate builds a gate, or nil if nothing is configured.
//
// Nil is usable: [Gate.Check] on a nil gate allows. That is what lets the
// generated server hold one field whether or not the project configured a
// throttle block.
func NewGate(limiter *Limiter, limits APILimits, onError func(context.Context, error)) *Gate {
	if limiter == nil || !limits.Configured() {
		return nil
	}
	return &Gate{limiter: limiter, limits: limits, onError: onError}
}

// Check spends the request's slots, describes the limit in the headers, and
// returns the refusal to answer with — or nil to go ahead.
//
// It fails open. If the counter cannot be reached the request is served, and
// the error goes to OnError rather than to the caller. This is the opposite of
// what the auth limits do, and the asymmetry is the point: a login limiter that
// fails open is a credential-stuffing window held open by whoever can make the
// database slow, while an API limiter that fails closed turns a database blip
// into a total outage — which is precisely the availability it was added to
// protect. The cost is stated rather than hidden: somebody who can degrade the
// database can also switch this off.
func (g *Gate) Check(ctx context.Context, c Caller, pattern string, h http.Header) error {
	if g == nil {
		return nil
	}

	checks := g.limits.Checks(c, pattern)
	if len(checks) == 0 {
		return nil
	}

	d, err := g.limiter.Take(ctx, checks...)
	if err != nil {
		if g.onError != nil {
			g.onError(ctx, err)
		}
		return nil
	}

	// On the way out either way: a client that can see it is at 900 of 1000
	// can slow down before it is refused, which is the whole point of telling
	// it anything.
	d.SetHeaders(h)
	return d.Err()
}

// StandardAPILimits are the numbers a project starts with.
//
// Deliberately loose. These are a backstop against a client in a retry loop and
// against one tenant crowding out the rest, not a quota — a limit tight enough
// to be interesting is one somebody has to choose for their own API, and a
// default that broke a working integration on upgrade would be worse than no
// default at all.
func StandardAPILimits() APILimits {
	return APILimits{
		ByAPIKey:  Limit{Name: NameAPIKey, Max: 5000, Window: time.Minute},
		ByAccount: Limit{Name: NameAccount, Max: 1000, Window: time.Minute},
		ByTenant:  Limit{Name: NameTenant, Max: 10000, Window: time.Minute},
		ByIP:      Limit{Name: NameIP, Max: 300, Window: time.Minute},
	}
}
