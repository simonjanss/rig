package project

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// Servers are the deployments this API answers on: production, staging, a
// machine on somebody's desk.
//
// It is a project block rather than a generator option because every generator
// that names a URL has the same question to answer, and three copies of the
// answer are three things to keep in step. The OpenAPI document, the Go client
// and the TypeScript client all read this one list, so a document saying the API
// is at api.example.com cannot ship beside an SDK pointing somewhere else — and
// the language added next gets the deployments without growing an option of its
// own, which is the argument [Generator] already makes for selecting generators
// by name.
//
// A list rather than a map keyed by name, because the order is data: the first
// entry is where a documentation viewer sends its trial request, and a map
// decoded into Go arrives in whatever order it likes.
//
// There is no url_env beside url, and the asymmetry with auth.oauth.base_url_env
// is deliberate. That one exists because a server reads its own origin when it
// starts. These are constants compiled into somebody else's program, which
// cannot see your deployment's environment — and the program wanting a different
// one passes its own URL, which every generated constructor still accepts.
type Servers []Server

// Server is one deployment.
type Server struct {
	// Name identifies the deployment, and becomes an identifier in every
	// generated SDK: production is Go's ServerProduction and TypeScript's
	// servers.production.
	//
	// Lower snake case, which is the one spelling that becomes an ordinary name
	// in both languages without a rule anybody has to remember.
	Name string `yaml:"name" json:"name" jsonschema:"pattern=^[a-z][a-z0-9_]*$" jsonschema_description:"Name of the deployment: production, staging, local. It becomes an identifier in every generated SDK, so it is lower snake case."`

	// URL is where that deployment answers, origin and any prefix it sits
	// behind. The API's own base_path is appended to it, so it is not repeated
	// here.
	URL string `yaml:"url" json:"url" jsonschema:"minLength=1" jsonschema_description:"Where the deployment answers, for example https://api.example.com. It is absolute, and the API's base_path is appended to it."`

	// Description is one line about what this deployment is for. It reaches the
	// OpenAPI document and the doc comment beside the generated constant.
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"One line about what this deployment is for. It reaches the OpenAPI document and the doc comment on the generated constant."`

	// Default is the deployment a caller who names none gets. At most one entry
	// may claim it; with nobody claiming it the first entry is the default, so a
	// project naming a single deployment writes no marker at all.
	Default bool `yaml:"default,omitempty" json:"default,omitempty" jsonschema_description:"The deployment a client that names no URL gets. At most one entry may set it; with none set, the first entry is the default."`
}

// IR is the resolved block, as a document carries it.
//
// The default is resolved here rather than in each generator. With three
// consumers asking "which deployment does a caller who named none get", the rule
// that an unclaimed default falls to the first entry has to be written once — in
// the place that can also refuse two claims.
//
// Nil for a project that named no deployment, so a generator asks the document
// one question — does this project say where it runs — rather than reading a
// list and then deciding what an empty one implies.
func (s Servers) IR() []ir.Server {
	if len(s) == 0 {
		return nil
	}

	claimed := slices.ContainsFunc(s, func(v Server) bool { return v.Default })
	out := make([]ir.Server, 0, len(s))
	for i, v := range s {
		out = append(out, ir.Server{
			Name:        v.Name,
			URL:         v.URL,
			Description: v.Description,
			Default:     v.Default || (!claimed && i == 0),
		})
	}
	return out
}

// checkServers validates what the schema cannot.
//
// A missing url and a name that is not lower snake case are struct tags, and
// yamlconf reports them before any of this runs. What is left is the
// relationships between entries, and the URL shapes that parse but produce a
// client nobody can use.
func (p *Project) checkServers() diag.List {
	var diags diag.List

	seen := make(map[string]int, len(p.Config.Servers))
	claimed := -1

	for i, s := range p.Config.Servers {
		at := func(key string) diag.Anchor { return p.At("servers", fmt.Sprint(i), key) }

		if prev, dup := seen[s.Name]; dup {
			diags.Add(diag.CodeConfigInvalid, at("name"),
				"server %q is already configured at servers.%d. The name becomes a "+
					"constant in every generated SDK, and two deployments cannot be the "+
					"same one", s.Name, prev)
		}
		seen[s.Name] = i

		if s.Default {
			if claimed >= 0 {
				diags.Add(diag.CodeConfigInvalid, at("default"),
					"servers.%d and servers.%d both set `default: true`. A client that "+
						"names no URL gets exactly one deployment, so only one entry may "+
						"claim it", claimed, i)
			} else {
				claimed = i
			}
		}

		diags.Append(checkServerURL(s.URL, at("url")))
	}

	return diags
}

// checkServerURL refuses the shapes that produce a client nobody can use.
//
// Absolute, because that is what a generated SDK is for: rigclient refuses a URL
// with no scheme and host, and a browser's same-origin answer is the empty
// string rather than "/". The relative server an OpenAPI document wants is what
// a project naming no deployment already gets, and it stays the openapi
// generator's business.
func checkServerURL(raw string, at diag.Anchor) diag.List {
	var diags diag.List

	switch {
	case raw == "":
		// The schema already said so.
		return diags
	case strings.HasPrefix(raw, "//"):
		diags.Add(diag.CodeConfigInvalid, at,
			"server url %q is protocol-relative, which means one thing in a browser "+
				"and nothing at all in a Go client. Write the scheme", raw)
		return diags
	case strings.HasPrefix(raw, "/"):
		diags.Add(diag.CodeConfigInvalid, at,
			"server url %q is relative, and a generated SDK cannot call it: a Go "+
				"client needs a scheme and a host, and a browser's same-origin answer "+
				"is the empty string rather than a path. A project that names no "+
				"deployment already gets a relative server in its OpenAPI document", raw)
		return diags
	}

	u, err := url.Parse(raw)
	switch {
	case err != nil:
		diags.Add(diag.CodeConfigInvalid, at, "server url %q cannot be parsed: %v", raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		diags.Add(diag.CodeConfigInvalid, at,
			"server url %q needs an http or https scheme, for example "+
				"https://api.example.com", raw)
	case u.Host == "":
		diags.Add(diag.CodeConfigInvalid, at, "server url %q names no host", raw)
	case u.RawQuery != "" || u.Fragment != "":
		diags.Add(diag.CodeConfigInvalid, at,
			"server url %q carries a query or a fragment. The API's own paths are "+
				"appended to it, so neither survives the first request", raw)
	}

	return diags
}

// deprecatedServerKey is the option a generator still uses to say where the API
// is, or "" for a generator that has none.
func deprecatedServerKey(g Generator) string {
	var key string
	switch g.Name {
	case "go-client", "ts-client":
		key = "default_base_url"
	case "openapi":
		key = "servers"
	default:
		return ""
	}
	if _, set := g.Options[key]; !set {
		return ""
	}
	return key
}

// checkDeprecatedServerOptions reports the per-generator keys the servers block
// replaced.
//
// It is here and not in the generators because a generator returns artifacts and
// an error, and has no way to say "this worked, and stop writing it that way".
// The options block is readable from here, which makes this the only place the
// question can be asked at all.
//
// Both at once is a refusal rather than a precedence rule. They are two answers
// to where this API is, and choosing one silently is how a document and the SDK
// beside it end up disagreeing — which is the failure the block exists to
// prevent. It only fires during a migration the warning has already asked for,
// so no upgrade breaks on it.
func (p *Project) checkDeprecatedServerOptions() diag.List {
	var diags diag.List

	for i, g := range p.Config.Generators {
		key := deprecatedServerKey(g)
		if key == "" {
			continue
		}
		at := p.At("generators", fmt.Sprint(i), "options", key)

		if len(p.Config.Servers) > 0 {
			diags.Add(diag.CodeConfigInvalid, at,
				"generators.%d (%s) sets `%s`, and rig.yaml has a `servers:` block. That "+
					"is two answers to where this API is. Remove the option; the block is "+
					"what every generator reads", i, g.Name, key)
			continue
		}

		diags.Add(diag.CodeConfigDeprecated, at,
			"generators.%d (%s) sets `%s`, which has moved to the project's `servers:` "+
				"block — one list read by every SDK generator and by the OpenAPI "+
				"document, so a generator added later gets the deployments without an "+
				"option of its own. It still works", i, g.Name, key)
	}

	return diags
}
