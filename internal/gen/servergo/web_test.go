package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/ir"
)

// The rule every optional block follows, and here the cost of getting it wrong
// is a project that never mentioned a front end importing rig/auth's handoff
// package and answering redirects nobody asked for.
func TestNoWebBlockWritesNoWebFile(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Web = nil

	for _, a := range gentest.Run(t, servergo.New(), doc, authOpts()) {
		if a.Path == "web.gen.go" {
			t.Fatal("a project with no web block should get no web.gen.go")
		}
		src := string(a.Content)
		for _, forbidden := range []string{
			"auth/handoff", "WebOrigin", "WebCallbackPath", "Browser:",
		} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s names %q in a project with no web block", a.Path, forbidden)
			}
		}
	}
}

// And the ending is untouched, which is what makes the block additive: a
// project without one still gets the JSON ending it always had.
func TestNoWebBlockKeepsTheJSONEnding(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Web = nil

	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")
	if !strings.Contains(got, "OnSignIn:          h.OAuth.OnSignIn,") {
		t.Error("the OnSignIn hook is no longer wired straight through")
	}
}

// A front end and no sign-in at all, which is why `web:` is a top-level block:
// the cross-origin half of the fact has nothing to do with authentication.
func TestAWebBlockWithoutAuthStillWritesTheWebFile(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Auth = nil

	artifacts := gentest.Run(t, servergo.New(), doc, authOpts())
	got := find(t, artifacts, "web.gen.go")
	if !strings.Contains(got, "func WebOrigin() (string, error)") {
		t.Error("no WebOrigin in a project that named a front end")
	}
	for _, a := range artifacts {
		if a.Path == "auth.gen.go" {
			t.Fatal("a project with no auth block got authentication wiring")
		}
	}
}

// The one configuration that can produce no origin at all, and the refusal has
// to name the variable: the reader is looking at a process that will not start.
func TestAWebOriginOnlyInTheEnvironmentCanRefuse(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Web = &ir.Web{
		OriginEnv:    "APP_ORIGIN",
		CallbackPath: "/auth/callback",
		CORS:         ir.WebCORS{AllowedOriginsEnv: "CORS_ORIGINS", MaxAgeSeconds: 600},
	}

	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "web.gen.go")
	for _, want := range []string{
		`const WebOriginEnv = "APP_ORIGIN"`,
		"raw := os.Getenv(WebOriginEnv)",
		"APP_ORIGIN must name the origin",
		"Hooks.OAuth.WebOrigin is the other way to supply it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the generated file does not contain %q:\n%s", want, got)
		}
	}
	// The other branch's imports must not be here — cmp is what a configuration
	// with both a literal and a variable reaches for, and an unused import does
	// not compile.
	if strings.Contains(got, `"cmp"`) {
		t.Error("cmp is imported by a configuration with nothing to fall back to")
	}
}

// A literal and nothing else needs neither os nor a refusal.
func TestAWebOriginOnlyInTheFileIsAConstantAnswer(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Web = &ir.Web{
		Origin:       "https://app.example.com",
		CallbackPath: "/auth/callback",
		CORS:         ir.WebCORS{AllowedOriginsEnv: "CORS_ORIGINS", MaxAgeSeconds: 600},
	}

	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "web.gen.go")
	if !strings.Contains(got, `return "https://app.example.com", nil`) {
		t.Errorf("a literal origin is not answered directly:\n%s", got)
	}
	for _, forbidden := range []string{"WebOriginEnv", `"os"`, `"errors"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("%q is in a file with nothing to read from the environment", forbidden)
		}
	}
}

// The callback path is one string used twice — the redirect's destination and
// the cookie's Path — so it comes from rig.yaml rather than from each end.
func TestTheCallbackPathReachesBothPlacesThatNeedIt(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	artifacts := gentest.Run(t, servergo.New(), doc, authOpts())

	if got := find(t, artifacts, "web.gen.go"); !strings.Contains(
		got, `const WebCallbackPath = "/auth/finish"`) {
		t.Errorf("the configured callback path is not a constant:\n%s", got)
	}
	if got := find(t, artifacts, "auth.gen.go"); !strings.Contains(
		got, "CallbackPath: WebCallbackPath") {
		t.Error("the handoff is built with a path of its own rather than the constant")
	}
}
