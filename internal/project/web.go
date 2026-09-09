package project

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
	"golang.org/x/net/publicsuffix"
)

// Web is where this API's browser front end is served, when that is not this
// API.
//
// A top-level block rather than a key under `auth.oauth`, because two different
// things need the same fact and only one of them is authentication. A finished
// provider sign-in has to redirect somewhere, and a preflight has to be answered
// for some origin — and a project serving a single-page application with no
// provider sign-in at all still has the second question. `servers:` is the
// precedent: a deployment fact that several generators read and none owns.
//
// Presence of the block is the switch, so there is no `enabled`. A project that
// serves its front end from this same origin writes nothing here and gets no
// cross-origin code emitted at all.
type Web struct {
	// Origin is where the front end is served, as scheme://host[:port] with
	// nothing after the host.
	Origin string `yaml:"origin,omitempty" json:"origin,omitempty" jsonschema_description:"Where the browser front end is served, for example https://app.example.com. Scheme and host only. Either this or origin_env is required."`

	// OriginEnv names the environment variable the origin comes from, for a
	// deployment whose front end differs per environment.
	//
	// This exists here and deliberately not beside `servers[].url`, and the
	// asymmetry is the same one `auth.oauth.base_url_env` makes: this value is
	// read by *this* server as it starts, so it can see its own environment. A
	// server URL is a constant compiled into somebody else's program, which
	// cannot.
	OriginEnv string `yaml:"origin_env,omitempty" json:"origin_env,omitempty" jsonschema_description:"Environment variable holding the front end's origin, for a deployment where it differs per environment. Set beside origin, the variable wins and origin is the default."`

	// CallbackPath is the path on the front end that receives a finished
	// sign-in. Default /auth/callback.
	CallbackPath string `yaml:"callback_path,omitempty" json:"callback_path,omitempty" jsonschema_description:"Path on the front end that receives a finished provider sign-in. Defaults to /auth/callback."`

	// CORS is what a preflight from that front end is answered with.
	CORS WebCORS `yaml:"cors,omitempty" json:"cors,omitempty" jsonschema_description:"What a browser's preflight is answered with. The origin above is always allowed; this is for the rest."`
}

// WebCORS is the part of a cross-origin policy a project has to say out loud.
//
// Which methods and headers are allowed is not here, because it is not a
// decision: it follows from what this API's own endpoints read and answer with,
// which the document already describes. A project maintaining its own list is
// how the header a live-sync cursor needs ends up missing, and a subscription
// that stops after one response is a bug nobody attributes to a CORS list.
type WebCORS struct {
	// AllowedOrigins are origins besides [Web.Origin] that may call this API —
	// an administrative front end, most often.
	//
	// Each is scheme://host[:port]. One leading `*.` label is allowed, so
	// `https://*.example.com` matches any single subdomain; a bare `*` is not,
	// because this API answers bearer credentials and a policy that admits every
	// origin is one somebody should have to write out.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" json:"allowed_origins,omitempty" jsonschema_description:"Origins besides web.origin that may call this API. Each is scheme://host, optionally with one leading *. label. A bare * is refused."`

	// AllowedOriginsEnv names the environment variable that replaces the list
	// above, as a comma-separated string. Default CORS_ORIGINS.
	//
	// Replaces rather than extends, which is how `base_url_env` behaves: a
	// deployment overriding a value should not have to know what it is
	// overriding.
	AllowedOriginsEnv string `yaml:"allowed_origins_env,omitempty" json:"allowed_origins_env,omitempty" jsonschema_description:"Environment variable holding a comma-separated origin list. Set, it replaces the baked-in cors.allowed_origins rather than adding to it. web.origin is always allowed either way. Defaults to CORS_ORIGINS."`

	// MaxAge is how long a browser may cache a preflight. Default 10m, which is
	// as long as Safari will honour.
	MaxAge Duration `yaml:"max_age,omitempty" json:"max_age,omitempty" jsonschema_description:"How long a browser may cache a preflight. Defaults to 10m, which is the longest Safari honours."`
}

// ConfiguredWeb reports whether the project named a front end at all, which is
// the switch every generated cross-origin line hangs off.
func (c *Config) ConfiguredWeb() bool { return configured(c.Web, Web{}) }

// IR is the resolved block, or nil for a project that named no front end.
//
// Nil is the question a generator asks — does this project have a browser front
// end somewhere else — rather than an empty struct it has to interpret.
func (w Web) IR(present bool) *ir.Web {
	if !present {
		return nil
	}
	return &ir.Web{
		Origin:       w.Origin,
		OriginEnv:    w.OriginEnv,
		CallbackPath: w.CallbackPath,
		CORS: ir.WebCORS{
			// Deduplicated rather than reported: naming one origin twice means
			// exactly what naming it once does, so it is not worth a
			// diagnostic — but it should not reach a generated list twice.
			AllowedOrigins:    dedupe(w.CORS.AllowedOrigins),
			AllowedOriginsEnv: w.CORS.AllowedOriginsEnv,
			MaxAgeSeconds:     int64(w.CORS.MaxAge.Duration().Seconds()),
		},
	}
}

// checkWeb validates the block, and the one relationship it has with another.
func (p *Project) checkWeb() diag.List {
	var diags diag.List
	if !p.Config.ConfiguredWeb() {
		return diags
	}

	w := p.Config.Web
	at := func(key ...string) diag.Anchor { return p.At(append([]string{"web"}, key...)...) }

	if w.Origin == "" && w.OriginEnv == "" {
		diags.Add(diag.CodeWebWithoutOrigin, at(),
			"the `web:` block says where the browser front end is served, and this "+
				"one names nowhere. Write `origin: https://app.example.com`, or "+
				"`origin_env: APP_ORIGIN` for a front end that differs per deployment")
		return diags
	}

	if w.Origin != "" {
		diags.Append(checkWebOrigin(w.Origin, "web.origin", at("origin")))
	}
	if !strings.HasPrefix(w.CallbackPath, "/") || strings.HasPrefix(w.CallbackPath, "//") {
		diags.Add(diag.CodeConfigInvalid, at("callback_path"),
			"callback path %q is a path on the front end, so it begins with a "+
				"single /: `/auth/callback`", w.CallbackPath)
	}

	for i, origin := range w.CORS.AllowedOrigins {
		diags.Append(checkWebOrigin(origin, "allowed origin",
			at("cors", "allowed_origins", fmt.Sprint(i))))
	}
	if w.CORS.MaxAge < 0 {
		diags.Add(diag.CodeConfigInvalid, at("cors", "max_age"),
			"a preflight cannot be cached for %s. Leave it unset for the default, "+
				"or write a positive duration", w.CORS.MaxAge.Duration())
	}

	diags.Append(p.checkWebReachesTheAPI())
	return diags
}

// checkWebReachesTheAPI is RIG3013: two hosts that share no registrable domain.
//
// Only when both are literal and there are providers to sign in with. An origin
// arriving from the environment is unknown here and is checked by the process
// that reads it, and a project with no provider sign-in writes no handoff cookie
// for the rule to be about.
func (p *Project) checkWebReachesTheAPI() diag.List {
	var diags diag.List

	w, o := p.Config.Web, p.Config.Auth.OAuth
	if len(o.Providers) == 0 || w.Origin == "" || o.BaseURL == "" {
		return diags
	}

	web, api := hostOf(w.Origin), hostOf(o.BaseURL)
	if web == "" || api == "" || web == api {
		return diags
	}

	webDomain, webErr := publicsuffix.EffectiveTLDPlusOne(web)
	apiDomain, apiErr := publicsuffix.EffectiveTLDPlusOne(api)
	if webErr == nil && apiErr == nil && webDomain == apiDomain {
		return diags
	}

	diags.Add(diag.CodeWebUnreachableCookie, p.At("web", "origin"),
		"the front end is served on %s and this API on %s (`auth.oauth.base_url`), "+
			"which share no registrable domain. A provider sign-in ends by leaving "+
			"its tokens in a cookie for the front end to read, and a browser will "+
			"not keep one written across unrelated domains — it drops it without a "+
			"word, so the sign-in would fail with nothing written anywhere",
		web, api)
	return diags
}

// checkWebOrigin refuses anything that is not scheme://host, allowing one
// leading `*.` label.
//
// The wildcard is accepted for a CORS list and nowhere else in rig, because a
// preflight is the one place a project genuinely does not know the host in
// advance — a preview deployment per branch is the case.
func checkWebOrigin(raw, what string, at diag.Anchor) diag.List {
	return checkOrigin(raw, what, true, at)
}

// checkExactOrigin is the same rules without the wildcard, for a list that is
// compared with an equality test rather than matched.
func checkExactOrigin(raw, what string, at diag.Anchor) diag.List {
	return checkOrigin(raw, what, false, at)
}

func checkOrigin(raw, what string, wildcards bool, at diag.Anchor) diag.List {
	var diags diag.List

	if strings.TrimSpace(raw) != raw {
		diags.Add(diag.CodeConfigInvalid, at,
			"%s %q has whitespace around it, and an Origin header will never "+
				"match it", what, raw)
		return diags
	}

	scheme, rest, found := strings.Cut(raw, "://")
	switch {
	case raw == "*" && wildcards:
		diags.Add(diag.CodeConfigInvalid, at,
			"%s is `*`, which admits every origin on the internet. This API "+
				"answers bearer credentials, so that is a policy worth writing out: "+
				"name the origins, or `https://*.example.com` for a wildcard within "+
				"one domain", what)
		return diags
	case !found:
		diags.Add(diag.CodeConfigInvalid, at,
			"%s %q has no scheme. An Origin header is always scheme://host, and it "+
				"is compared exactly: `https://app.example.com`", what, raw)
		return diags
	case scheme != "http" && scheme != "https":
		diags.Add(diag.CodeConfigInvalid, at,
			"%s %q needs an http or https scheme", what, raw)
		return diags
	case rest == "":
		diags.Add(diag.CodeConfigInvalid, at, "%s %q names no host", what, raw)
		return diags
	}

	host := rest
	if strings.Contains(host, "*") {
		if !wildcards {
			diags.Add(diag.CodeConfigInvalid, at,
				"%s %q has a wildcard in it, and this list is compared with an "+
					"equality test rather than matched — so it would never match "+
					"anything. Name the origins", what, raw)
			return diags
		}
		wildcard, after, ok := strings.Cut(host, ".")
		switch {
		case !ok || wildcard != "*":
			diags.Add(diag.CodeConfigInvalid, at,
				"%s %q wildcards part of a label. One whole leading `*.` label is "+
					"the most a browser's single-label match can mean", what, raw)
			return diags
		case strings.Contains(after, "*"):
			diags.Add(diag.CodeConfigInvalid, at,
				"%s %q has more than one wildcard. One leading `*.` label is the "+
					"most a browser's single-label match can mean", what, raw)
			return diags
		case after == "":
			diags.Add(diag.CodeConfigInvalid, at,
				"%s %q wildcards a host that is not there", what, raw)
			return diags
		}
		host = after
	}

	u, err := url.Parse(scheme + "://" + host)
	switch {
	case err != nil:
		diags.Add(diag.CodeConfigInvalid, at, "%s %q cannot be parsed: %v", what, raw, err)
	case u.Host == "":
		diags.Add(diag.CodeConfigInvalid, at, "%s %q names no host", what, raw)
	case u.Path != "" || u.RawQuery != "" || u.Fragment != "":
		// The mistake that is accepted silently and then never matches: an
		// Origin header carries no path, so an entry with one — a trailing
		// slash included — is dead configuration.
		diags.Add(diag.CodeConfigInvalid, at,
			"%s %q carries a path, query or fragment. An Origin header is scheme "+
				"and host only, and this is compared against one exactly, so it "+
				"would never match anything", what, raw)
	}
	return diags
}

// dedupe keeps the first of each entry, so the order a project wrote stays the
// order a generated list has.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// hostOf takes the host out of an origin.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
