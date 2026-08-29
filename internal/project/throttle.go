package project

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// Throttle is how many calls one caller may make.
//
// It is fair-use limiting and the documentation says so in those words. What it
// stops is a client stuck in a retry loop, one tenant's batch job crowding out
// the others, scraping, and an API that fans out to something metered. What it
// does not stop is a volumetric attack: by the time a request is here it has
// already cost a connection, a handshake and a goroutine, and the defence for
// that lives in front of the application rather than inside it.
//
// The numbers are configuration rather than a Go literal for the reason
// `auth.limits` is: they are answered to clients in the RateLimit-* headers and
// quoted in the generated documentation, so the number a caller is told is the
// number the server enforces.
type Throttle struct {
	// Enabled is what makes `server-go` write the wiring. Off by default: an API
	// that starts refusing calls after an upgrade nobody asked for is worse than
	// one that does not limit yet.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether the generated server limits API calls. The server-go generator writes the wiring only when it is set."`

	// APIKey, Account and Tenant apply to a caller who said who they were.
	APIKey  AuthLimit `yaml:"api_key,omitempty" json:"api_key,omitempty" jsonschema_description:"Calls per window per API key. Defaults to 5000 per 1m."`
	Account AuthLimit `yaml:"account,omitempty" json:"account,omitempty" jsonschema_description:"Calls per window per signed-in account. Defaults to 1000 per 1m."`
	Tenant  AuthLimit `yaml:"tenant,omitempty" json:"tenant,omitempty" jsonschema_description:"Calls per window per tenant, across every account and key they have. What stops one customer crowding out the rest. Defaults to 10000 per 1m."`

	// IP applies only to a caller who did not say who they were.
	IP AuthLimit `yaml:"ip,omitempty" json:"ip,omitempty" jsonschema_description:"Calls per window per source address, for callers who are not signed in. Only for those: once a request carries an identity that identity is the better key. Defaults to 300 per 1m."`

	// Interval is how long one replica may hold its own count before publishing
	// it. It is the accuracy of the whole feature, stated as time.
	Interval Duration `yaml:"interval,omitempty" json:"interval,omitempty" jsonschema_description:"How long a replica may count locally before publishing to the database. The limiter can miss up to one interval of traffic per replica, and a shorter one costs writes. Defaults to 1s."`

	// Routes are extra limits on particular routes, on top of the per-caller
	// ones above.
	Routes []ThrottleRoute `yaml:"routes,omitempty" json:"routes,omitempty" jsonschema_description:"Extra limits on particular route patterns, counted against the caller and on top of the per-caller limits."`

	// Exempt are route patterns nothing applies to.
	Exempt []string `yaml:"exempt,omitempty" json:"exempt,omitempty" jsonschema_description:"Route patterns no limit applies to, for example a long-lived stream. Written as METHOD /path, matching the pattern the router registers."`
}

// ThrottleRoute is one route's own limit.
type ThrottleRoute struct {
	// Pattern is the route as the router registers it, for example
	// "POST /api/v1/todos". It is matched exactly against what net/http
	// reports, not against the request path.
	Pattern string   `yaml:"pattern" json:"pattern" jsonschema_description:"The route pattern, as the router registers it — for example POST /api/v1/todos. Matched against the pattern net/http reports, not against the path."`
	Max     int      `yaml:"max,omitempty" json:"max,omitempty" jsonschema:"minimum=1" jsonschema_description:"How many calls are allowed in one window."`
	Window  Duration `yaml:"window,omitempty" json:"window,omitempty" jsonschema_description:"How long the window is."`
}

// DefaultThrottleInterval is how long a replica holds its count by default.
//
// A second is chosen from what it costs rather than from what it buys: it turns
// a write per request into a write per second per caller, and the traffic it can
// miss in that second is small next to limits measured in thousands per minute.
const DefaultThrottleInterval = time.Second

// Configured reports whether the block says anything beyond being switched on.
func (t Throttle) Configured() bool {
	return configured(t, Throttle{Enabled: t.Enabled})
}

// checkThrottle validates what the JSON Schema cannot.
func (p *Project) checkThrottle() diag.List {
	var diags diag.List
	c := p.Config.Throttle

	if !c.Enabled {
		// The same failure the auth, files, notifications and presence blocks
		// refuse, and for the same reason: numbers somebody set and believed in,
		// which nothing reads.
		if c.Configured() {
			diags.Add(diag.CodeConfigInvalid, p.At("throttle", "enabled"),
				"throttle is configured but throttle.enabled is false, so none of it is "+
					"read; set `enabled: true` or remove the block")
		}
		return diags
	}

	for name, l := range map[string]AuthLimit{
		"api_key": c.APIKey, "account": c.Account, "tenant": c.Tenant, "ip": c.IP,
	} {
		checkThrottleLimit(&diags, p.At("throttle", name), "throttle."+name, l.Max, l.Window)
	}

	// A tenant ceiling below what one account may spend is a ceiling nobody can
	// reach past a single user, which is almost certainly a transposition rather
	// than a decision.
	if acct, tenant := c.Account, c.Tenant; usableLimit(acct) && usableLimit(tenant) &&
		perSecond(tenant) < perSecond(acct) {
		diags.AddSeverity(diag.CodeConfigInvalid, diag.SeverityWarning, p.At("throttle", "tenant"),
			"throttle.tenant allows %d per %s and throttle.account allows %d per %s, so the "+
				"tenant ceiling is below what one of its accounts may spend — the account "+
				"limit can never be reached",
			tenant.Max, tenant.Window, acct.Max, acct.Window)
	}

	seen := map[string]bool{}
	for i, r := range c.Routes {
		where := p.At("throttle", "routes", itoa(i))
		if r.Pattern == "" {
			diags.Add(diag.CodeConfigInvalid, where,
				"throttle.routes[%d] has no pattern, so there is no route for it to limit", i)
			continue
		}
		if seen[r.Pattern] {
			diags.Add(diag.CodeConfigInvalid, where,
				"throttle.routes[%d]: %q is limited twice, and only one of the two would "+
					"apply", i, r.Pattern)
		}
		seen[r.Pattern] = true

		checkThrottlePattern(&diags, where, fmt.Sprintf("throttle.routes[%d].pattern", i), r.Pattern)
		checkThrottleLimit(&diags, where, fmt.Sprintf("throttle.routes[%d]", i), r.Max, r.Window)
		if r.Max == 0 || r.Window.Duration() == 0 {
			diags.Add(diag.CodeConfigInvalid, where,
				"throttle.routes[%d] sets no %s, and unlike the per-caller limits a route "+
					"limit has no default to fall back on — it would be a route somebody "+
					"listed and nothing limits",
				i, missingWord(r.Max == 0, r.Window.Duration() == 0))
		}
	}

	for i, pattern := range c.Exempt {
		where := p.At("throttle", "exempt", itoa(i))
		checkThrottlePattern(&diags, where, fmt.Sprintf("throttle.exempt[%d]", i), pattern)
		if seen[pattern] {
			diags.Add(diag.CodeConfigInvalid, where,
				"throttle.exempt[%d]: %q also has a limit in throttle.routes, and exempt "+
					"wins — so the limit is written down and never applied", i, pattern)
		}
	}

	if iv := c.Interval.Duration(); iv < 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("throttle", "interval"),
			"throttle.interval is %s; a negative interval is not a shorter one", c.Interval)
	} else if iv > time.Minute {
		diags.AddSeverity(diag.CodeConfigInvalid, diag.SeverityWarning, p.At("throttle", "interval"),
			"throttle.interval is %s, so a replica can count that long before any other "+
				"replica hears about it. With several replicas the limit that actually "+
				"applies is much looser than the numbers above", c.Interval)
	}

	return diags
}

func checkThrottleLimit(diags *diag.List, where diag.Anchor, name string, maxN int, window Duration) {
	if maxN < 0 {
		diags.Add(diag.CodeConfigInvalid, where, "%s.max is %d", name, maxN)
	}
	if d := window.Duration(); d < 0 {
		diags.Add(diag.CodeConfigInvalid, where, "%s.window is %s", name, window)
	} else if maxN > 0 && d > 0 && d < time.Second {
		// The window is the bucket, and a bucket under a second is arithmetic
		// nobody can reason about against a clock that is only approximately
		// shared between replicas.
		diags.Add(diag.CodeConfigInvalid, where,
			"%s.window is %s; the window is also the counting bucket, and under a second "+
				"it is shorter than the clock skew between two replicas", name, window)
	}
}

// checkThrottlePattern refuses the mistake this whole surface invites: writing a
// path where a pattern goes.
func checkThrottlePattern(diags *diag.List, where diag.Anchor, name, pattern string) {
	method, rest, found := strings.Cut(pattern, " ")
	if !found {
		diags.Add(diag.CodeConfigInvalid, where,
			"%s is %q, which has no method. Patterns are matched against what net/http "+
				"reports for the route it dispatched — `GET /todos`, not `/todos` — so this "+
				"would never match anything",
			name, pattern)
		return
	}
	if method != strings.ToUpper(method) || !knownMethod(method) {
		diags.Add(diag.CodeConfigInvalid, where,
			"%s is %q, and %q is not an HTTP method the router registers", name, pattern, method)
	}
	if !strings.HasPrefix(rest, "/") {
		diags.Add(diag.CodeConfigInvalid, where,
			"%s is %q, and the path has to start with /", name, pattern)
	}
	if strings.Contains(rest, "*") {
		diags.Add(diag.CodeConfigInvalid, where,
			"%s is %q; patterns are matched exactly rather than globbed, and a wildcard "+
				"segment is written {name} the way the router writes it", name, pattern)
	}
}

func knownMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, "QUERY":
		return true
	}
	return false
}

func missingWord(noMax, noWindow bool) string {
	switch {
	case noMax && noWindow:
		return "max or window"
	case noMax:
		return "max"
	default:
		return "window"
	}
}

func usableLimit(l AuthLimit) bool { return l.Max > 0 && l.Window.Duration() > 0 }

func perSecond(l AuthLimit) float64 { return float64(l.Max) / l.Window.Duration().Seconds() }

// IR is the resolved block, as a document carries it.
//
// Nil for a project that does not throttle, so a generator asks one question
// rather than reading a flag and then a dozen numbers that may mean nothing.
func (t Throttle) IR() *ir.Throttle {
	if !t.Enabled {
		return nil
	}

	out := &ir.Throttle{
		Enabled:         true,
		APIKey:          throttleLimitIR(t.APIKey),
		Account:         throttleLimitIR(t.Account),
		Tenant:          throttleLimitIR(t.Tenant),
		IP:              throttleLimitIR(t.IP),
		IntervalSeconds: t.Interval.Duration().Seconds(),
		Exempt:          append([]string(nil), t.Exempt...),
	}
	for _, r := range t.Routes {
		out.Routes = append(out.Routes, ir.ThrottleRoute{
			Pattern:       r.Pattern,
			Max:           r.Max,
			WindowSeconds: int64(r.Window.Duration().Seconds()),
		})
	}
	return out
}

func throttleLimitIR(l AuthLimit) ir.ThrottleLimit {
	return ir.ThrottleLimit{Max: l.Max, WindowSeconds: int64(l.Window.Duration().Seconds())}
}
