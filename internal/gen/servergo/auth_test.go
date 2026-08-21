package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/internal/gen/servicego"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

const authFixture = "authwired.ir.json"

func authOpts() gen.Options {
	return gen.Options{OutDir: ".", Raw: map[string]any{
		"package":      "api",
		"model_import": "rigtest/model",
	}}
}

// authArtifact is the wiring on its own.
//
// The goldens below are about the auth block, so they are compared against that
// file rather than against everything server-go writes: a route added to an
// unrelated resource is not a change to how a token is configured.
func authArtifact(t *testing.T, artifacts []gen.Artifact) []gen.Artifact {
	t.Helper()

	for _, a := range artifacts {
		if a.Path == "auth.gen.go" {
			return []gen.Artifact{a}
		}
	}
	t.Fatal("no auth.gen.go was generated")
	return nil
}

// TestAuthGolden covers the configuration with everything turned on: three
// tenant sources, three providers, trusted proxies, a breach check, and limits
// that differ from the defaults.
func TestAuthGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	artifacts := gentest.Run(t, servergo.New(), doc, authOpts())

	gentest.Golden(t, filepath.Join("testdata", "authwired"), authArtifact(t, artifacts), *update)
}

// TestAuthGoldenDefaults covers the other end: a project whose whole
// authentication block is `enabled: true`.
//
// The document is the same fixture with its auth block replaced by what the
// project loader resolves from those two words, so this golden is the answer to
// "what do I get if I configure nothing" — and it fails if a default changes
// without somebody deciding to change it.
func TestAuthGoldenDefaults(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth = defaultAuth(t)

	artifacts := gentest.Run(t, servergo.New(), doc, authOpts())

	gentest.Golden(t, filepath.Join("testdata", "authdefaults"), authArtifact(t, artifacts), *update)
}

func TestAuthDeterministic(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	gentest.Deterministic(t, servergo.New(), doc, authOpts())
}

// TestNoAuthBlockWritesNoWiring is what makes the wiring safe to fold into
// server-go: a project without authentication gets no file, so its API package
// — and its module — never reaches rig/auth at all.
func TestNoAuthBlockWritesNoWiring(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth = nil

	for _, a := range gentest.Run(t, servergo.New(), doc, authOpts()) {
		if a.Path == "auth.gen.go" {
			t.Fatal("a project with no auth block should get no authentication wiring")
		}
		if strings.Contains(string(a.Content), "simonjanss/rig/auth\"") {
			t.Errorf("%s imports rig/auth without an auth block", a.Path)
		}
	}
}

// TestTheConfiguredValuesReachTheOutput checks the numbers themselves rather
// than the shape of the file.
//
// A golden file proves the output has not changed; it does not prove the output
// says what the configuration said. These are the values somebody would go and
// look for after editing rig.yaml.
func TestTheConfiguredValuesReachTheOutput(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	for _, want := range []string{
		"AccessTTL:          5 * time.Minute",
		"RefreshTTL:         8 * time.Hour",
		"RememberTTL:        14 * 24 * time.Hour",
		"RotationLeeway:     45 * time.Second",
		"IdentitySessionTTL: 20 * time.Minute",
		"SessionCacheTTL: 2 * time.Second",
		"MinLength: 14, MaxLength: 512",
		"password.NewHIBP()",
		"d.LoginByEmail.Max, d.LoginByEmail.Window = 3, 10*time.Minute",
		"AllowRegistration:    true",
		"AllowTenantCreation:  true",
		"RequireVerifiedEmail: true",
		`"10.0.0.0/8", "192.168.0.0/16"`,
		"StateTTL:          8 * time.Minute",
		`os.Getenv("GH_APP_ID")`,
		`os.Getenv("WORK_DIRECTORY")`,
		// The host source, which is the one that carries real code.
		"SELECT id FROM rig_tenant WHERE lower(slug) = $1",
		`os.Getenv(DefaultSlugEnv)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the generated wiring does not contain %q", want)
		}
	}
}

// TestTheErrorMapperIsThisPackages is the reason the wiring is generated into
// the API package rather than beside it: the mapper is reached by name, with no
// import and no option naming the package it lives in.
//
// Through fail rather than the mapper directly, which is the other half: fail
// is where the cause of a 500 is recorded, and an auth route is the one route a
// project cannot wrap to record it itself.
func TestTheErrorMapperIsThisPackages(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	if !strings.Contains(got, "fail(srv, w, r, RequestContext{") {
		t.Error("the wiring should fail through this package's own path, unqualified")
	}
	if !strings.Contains(got, `r.Header.Get("X-Request-Id")`) {
		t.Error("the failure should carry the same request identifier as every other one")
	}
}

// TestDefaultsUseRigAuthsOwnResolver checks that a project configuring nothing
// gets no generated tenant resolver at all: rig/auth already reads the header,
// and a generated copy of it would be a second thing to keep in step.
func TestDefaultsUseRigAuthsOwnResolver(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth = defaultAuth(t)

	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	if strings.Contains(got, "func tenant(") {
		t.Error("the default header resolution should not emit a resolver of its own")
	}
	if strings.Contains(got, "ConfiguredProviders") {
		t.Error("no providers are configured, so nothing about OAuth should be emitted")
	}
}

// TestTheWiredAPICompiles builds the whole stack for a project that has
// authentication, because the wiring and the handlers are now one package and
// either can break the other.
func TestTheWiredAPICompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))

	api := gentest.Run(t, servicego.New(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "store_import": "rigtest/store",
	}})
	api = append(api, gentest.Run(t, servergo.New(), doc, authOpts())...)

	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{
			Dir: "store",
			Artifacts: gentest.Run(t, persistgo.New(), doc, gen.Options{Raw: map[string]any{
				"package": "store", "model_import": "rigtest/model",
			}}),
		},
		gentest.Package{Dir: "api", Artifacts: api},
	)
}

// defaultAuth is what `auth: {enabled: true}` and nothing else resolves to.
//
// It comes from the project loader rather than from a literal here, so this test
// cannot disagree with what a real rig.yaml would produce.
func defaultAuth(t *testing.T) *ir.Auth {
	t.Helper()

	p, diags := project.Parse("rig.yaml", []byte(
		"project:\n  name: demo\n  module: example.com/demo\nauth:\n  enabled: true\n"))
	if diags.HasErrors() {
		t.Fatalf("the minimal auth configuration does not load:\n%s", diags.String())
	}
	return p.Config.Auth.IR()
}
