package project_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

// parseServers parses a configuration that may be invalid and returns both
// halves: most of what is worth checking here is what gets refused.
func parseServers(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	p, diags := project.Parse("rig.yaml", []byte(minimal+body))
	return p, diags.String()
}

const threeServers = `servers:
  - name: local
    url: http://localhost:8080
    description: A machine on somebody's desk.
  - name: production
    url: https://api.example.com
    default: true
  - name: staging_eu
    url: https://staging.eu.example.com
`

// Nil until it is asked for, which is the one question a generator asks: does
// this project say where it runs.
func TestNoServersBlockResolvesToNothing(t *testing.T) {
	t.Parallel()

	p, out := parseServers(t, "")
	if out != "" {
		t.Fatalf("a project naming no deployment complained:\n%s", out)
	}
	if got := p.Config.Servers.IR(); got != nil {
		t.Errorf("want nil, got %#v", got)
	}
}

// Order survives, because it is data: a documentation viewer sends its trial
// request to the first entry.
func TestServersKeepTheOrderTheyWereWrittenIn(t *testing.T) {
	t.Parallel()

	p, out := parseServers(t, threeServers)
	if out != "" {
		t.Fatalf("a valid block complained:\n%s", out)
	}

	got := p.Config.Servers.IR()
	want := []string{"local", "production", "staging_eu"}
	if len(got) != len(want) {
		t.Fatalf("want %d deployments, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Errorf("servers[%d] = %q, want %q", i, got[i].Name, want[i])
		}
	}
	if !got[1].Default {
		t.Error("the entry marked default did not come back marked")
	}
	if got[0].Default || got[2].Default {
		t.Error("a deployment nobody marked came back as the default")
	}
	if got[0].Description != "A machine on somebody's desk." {
		t.Errorf("the description did not survive: %q", got[0].Description)
	}
}

// The other half of the rule, and the reason a project naming one deployment
// writes no marker at all.
func TestWithNobodyClaimingItTheFirstEntryIsTheDefault(t *testing.T) {
	t.Parallel()

	p, out := parseServers(t, "servers:\n  - name: production\n    url: https://api.example.com\n"+
		"  - name: local\n    url: http://localhost:8080\n")
	if out != "" {
		t.Fatalf("a block with no marker complained:\n%s", out)
	}

	got := p.Config.Servers.IR()
	if !got[0].Default {
		t.Error("nobody claimed the default and the first entry did not get it")
	}
	if got[1].Default {
		t.Error("two deployments came back as the default")
	}
}

// A trailing slash is normalized rather than reported: both client runtimes trim
// one and the OpenAPI document does not, so it is the document that ends up
// describing an origin with a doubled slash in it.
func TestATrailingSlashIsTrimmed(t *testing.T) {
	t.Parallel()

	p, out := parseServers(t, "servers:\n  - name: production\n    url: https://api.example.com/\n")
	if out != "" {
		t.Fatalf("a trailing slash was refused rather than trimmed:\n%s", out)
	}
	if got := p.Config.Servers.IR()[0].URL; got != "https://api.example.com" {
		t.Errorf("url = %q, want the slash gone", got)
	}
}

// The refusals, each of them something somebody could write and believe in.
func TestServersRefuseWhatNoSDKCouldCall(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		what string
		body string
	}{
		{"two deployments with one name", "servers:\n  - name: production\n    url: https://a.example.com\n" +
			"  - name: production\n    url: https://b.example.com\n"},
		{"two claims on the default", "servers:\n  - name: a\n    url: https://a.example.com\n    default: true\n" +
			"  - name: b\n    url: https://b.example.com\n    default: true\n"},
		{"a deployment with no url", "servers:\n  - name: production\n"},
		{"a name that is not an identifier", "servers:\n  - name: Staging-EU\n    url: https://a.example.com\n"},
		{"a protocol-relative url", "servers:\n  - name: production\n    url: //api.example.com\n"},
		{"a relative url", "servers:\n  - name: production\n    url: /api\n"},
		{"a scheme nothing speaks", "servers:\n  - name: production\n    url: ftp://api.example.com\n"},
		{"a url with no host", "servers:\n  - name: production\n    url: https://\n"},
		{"a url carrying a query", "servers:\n  - name: production\n    url: https://api.example.com?key=1\n"},
	} {
		if _, out := parseServers(t, c.body); !strings.Contains(out, "RIG3002") {
			t.Errorf("%s was accepted:\n%s", c.what, out)
		}
	}
}

// The superseded option keeps working, so a project is never broken by an
// upgrade it did not ask for — and it says so, once, where the option is
// written rather than after somebody wonders why their SDK has no constant.
func TestTheDeprecatedOptionWarnsAndStillResolves(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		what string
		body string
	}{
		{"go-client", "generators:\n  - name: go-client\n    options:\n      default_base_url: http://localhost:8080\n"},
		{"ts-client", "generators:\n  - name: ts-client\n    options:\n      default_base_url: http://localhost:8080\n"},
		{"openapi", "generators:\n  - name: openapi\n    options:\n      servers: [{url: https://api.example.com}]\n"},
	} {
		p, diags := project.Parse("rig.yaml", []byte(minimal+c.body))
		if diags.HasErrors() {
			t.Errorf("%s: a deprecated key should warn, not refuse:\n%s", c.what, diags.String())
			continue
		}
		if !strings.Contains(diags.String(), "RIG3010") {
			t.Errorf("%s: the deprecated key said nothing at all:\n%s", c.what, diags.String())
		}
		if len(p.Config.Generators) != 1 {
			t.Errorf("%s: a warned-about generator still has to resolve", c.what)
		}
	}
}

// Both at once is a refusal rather than a precedence rule. They are two answers
// to where this API is, and choosing one silently is how a document and the SDK
// beside it end up disagreeing.
func TestABlockBesideTheDeprecatedOptionIsRefused(t *testing.T) {
	t.Parallel()

	_, out := parseServers(t, threeServers+
		"generators:\n  - name: go-client\n    options:\n      default_base_url: http://localhost:8080\n")
	if !strings.Contains(out, "RIG3002") {
		t.Errorf("two answers to where the API is were accepted:\n%s", out)
	}
}

// A generator with no such option is left alone, so the peek at the options map
// cannot start warning about a key that means something else.
func TestAnUnrelatedGeneratorOptionIsNotDeprecated(t *testing.T) {
	t.Parallel()

	if _, out := parseServers(t,
		"generators:\n  - name: server-go\n    options:\n      electric_url: http://localhost:3000\n"); out != "" {
		t.Errorf("an unrelated option was reported:\n%s", out)
	}
}
