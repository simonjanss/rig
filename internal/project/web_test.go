package project_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

// A project with no web block gets no web block, which is the state that keeps
// every cross-origin line out of the generated code.
func TestNoWebBlockStaysNoWebBlock(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal))
	if diags.HasErrors() {
		t.Fatalf("unexpected:\n%s", diags.String())
	}
	if p.Config.ConfiguredWeb() {
		t.Error("a project that named no front end has one")
	}
	if p.Config.Web.IR(p.Config.ConfiguredWeb()) != nil {
		t.Error("the document carries a web block for a project without one")
	}
}

func TestWebDefaults(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal+
		"web:\n  origin: https://app.example.com/\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected:\n%s", diags.String())
	}

	w := p.Config.Web
	// The trailing slash goes: this value is compared against an Origin header,
	// which never has one, so keeping it would be an origin matching nothing.
	if w.Origin != "https://app.example.com" {
		t.Errorf("origin = %q, want the slash trimmed", w.Origin)
	}
	if w.CallbackPath != project.DefaultCallbackPath {
		t.Errorf("callback_path = %q, want %q", w.CallbackPath, project.DefaultCallbackPath)
	}
	if w.CORS.AllowedOriginsEnv != project.DefaultCORSOriginsEnv {
		t.Errorf("allowed_origins_env = %q, want %q",
			w.CORS.AllowedOriginsEnv, project.DefaultCORSOriginsEnv)
	}
	if w.CORS.MaxAge.Duration() != project.DefaultCORSMaxAge {
		t.Errorf("max_age = %s, want %s", w.CORS.MaxAge, project.DefaultCORSMaxAge)
	}

	doc := w.IR(p.Config.ConfiguredWeb())
	if doc == nil {
		t.Fatal("no web block in the document")
	}
	if doc.CORS.MaxAgeSeconds != 600 {
		t.Errorf("maxAgeSeconds = %d, want 600", doc.CORS.MaxAgeSeconds)
	}
}

// An origin from the environment is enough on its own: the whole point of the
// variable is a front end that differs per deployment.
func TestAWebBlockMayNameOnlyAVariable(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"web:\n  origin_env: APP_ORIGIN\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected:\n%s", diags.String())
	}
}

func TestWebValidation(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ name, web, want string }{
		{
			name: "a block naming nowhere",
			web:  "callback_path: /done",
			want: "names nowhere",
		},
		{
			name: "an origin with no scheme",
			web:  "origin: app.example.com",
			want: "has no scheme",
		},
		{
			name: "an origin with a path",
			web:  "origin: https://app.example.com/app",
			want: "carries a path",
		},
		{
			name: "a scheme no browser sends as an Origin",
			web:  "origin: ftp://app.example.com",
			want: "http or https",
		},
		{
			name: "a relative callback path",
			web:  "origin: https://app.example.com\n  callback_path: auth/callback",
			want: "begins with a single /",
		},
		{
			name: "a protocol-relative callback path",
			web:  "origin: https://app.example.com\n  callback_path: //evil.example",
			want: "begins with a single /",
		},
		{
			name: "an allowed origin that admits everything",
			web:  "origin: https://app.example.com\n  cors:\n    allowed_origins: ['*']",
			want: "admits every origin",
		},
		{
			name: "an allowed origin with a trailing path",
			web:  "origin: https://app.example.com\n  cors:\n    allowed_origins: ['https://a.example.com/x']",
			want: "carries a path",
		},
		{
			name: "a wildcard in the middle of a host",
			web:  "origin: https://app.example.com\n  cors:\n    allowed_origins: ['https://a*.example.com']",
			want: "wildcards part of a label",
		},
		{
			name: "two wildcards",
			web:  "origin: https://app.example.com\n  cors:\n    allowed_origins: ['https://*.*.example.com']",
			want: "more than one wildcard",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			_, diags := project.Parse("rig.yaml", []byte(minimal+"web:\n  "+c.web+"\n"))
			if !diags.HasErrors() {
				t.Fatalf("%s should be refused", c.name)
			}
			if !strings.Contains(diags.String(), c.want) {
				t.Errorf("expected a message about %q:\n%s", c.want, diags.String())
			}
		})
	}
}

// One leading wildcard label is the form a browser's single-label match can
// mean, and it is accepted here because a preview deployment per branch is a
// real case.
func TestAWildcardSubdomainIsAllowed(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"web:\n  origin: https://app.example.com\n  cors:\n"+
		"    allowed_origins: ['https://*.example.com']\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected:\n%s", diags.String())
	}
}

// The whole reason RIG3013 exists: a cookie written by one of these hosts can
// never be read by the other, and a browser drops it without a word.
func TestAFrontEndTheCookieCannotReachIsRefused(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"web:\n  origin: https://app.other.com\n"+
		"auth:\n  enabled: true\n  oauth:\n    base_url: https://api.example.com\n"+
		"    providers:\n      - name: google\n"))
	if !diags.HasErrors() {
		t.Fatal("two unrelated domains were accepted")
	}
	got := diags.String()
	for _, want := range []string{"RIG3013", "registrable domain", "app.other.com", "api.example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected a message naming %q:\n%s", want, got)
		}
	}
}

// A multi-label public suffix is where a label-counting heuristic goes wrong,
// and getting it right is the reason the public suffix list is a dependency.
func TestTwoSubdomainsOfOneRegistrableDomainAreAccepted(t *testing.T) {
	t.Parallel()

	for _, hosts := range []struct{ web, api string }{
		{"https://app.example.com", "https://api.example.com"},
		{"https://app.example.co.uk", "https://api.example.co.uk"},
		{"http://localhost:3000", "http://localhost:8080"},
	} {
		_, diags := project.Parse("rig.yaml", []byte(minimal+
			"web:\n  origin: "+hosts.web+"\n"+
			"auth:\n  enabled: true\n  oauth:\n    base_url: "+hosts.api+"\n"+
			"    providers:\n      - name: google\n"))
		if diags.HasErrors() {
			t.Errorf("%s beside %s was refused:\n%s", hosts.web, hosts.api, diags.String())
		}
	}
}

// An origin that arrives from the environment is not knowable here, so the
// check belongs to the process that reads it rather than to rig.
func TestAnOriginFromTheEnvironmentIsNotCheckedAgainstTheAPIHost(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"web:\n  origin_env: APP_ORIGIN\n"+
		"auth:\n  enabled: true\n  oauth:\n    base_url: https://api.example.com\n"+
		"    providers:\n      - name: google\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected:\n%s", diags.String())
	}
}

// allowed_return_to was documented three ways and validated nowhere, so an
// entry that could never match read as a working allow-list.
func TestAllowedReturnToValidation(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ name, entry, want string }{
		{"a path", "/welcome", "is a path"},
		{"a trailing slash", "https://app.example.com/", "carries a path"},
		{"no scheme", "app.example.com", "has no scheme"},
		{"a wildcard", "https://*.example.com", "equality test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			_, diags := project.Parse("rig.yaml", []byte(minimal+
				"auth:\n  enabled: true\n  oauth:\n"+
				"    base_url: https://api.example.com\n"+
				"    allowed_return_to: ['"+c.entry+"']\n"+
				"    providers:\n      - name: google\n"))
			if !diags.HasErrors() {
				t.Fatalf("%s should be refused", c.name)
			}
			if !strings.Contains(diags.String(), c.want) {
				t.Errorf("expected a message about %q:\n%s", c.want, diags.String())
			}
		})
	}
}

func TestAnAllowedReturnToOriginIsAccepted(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"auth:\n  enabled: true\n  oauth:\n"+
		"    base_url: https://api.example.com\n"+
		"    allowed_return_to: ['https://app.example.com', 'http://localhost:3000']\n"+
		"    providers:\n      - name: google\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected:\n%s", diags.String())
	}
}
