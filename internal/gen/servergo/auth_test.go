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
//
// web.gen.go and cors.gen.go join it when there is a `web:` block, because the
// front end's origin is the other half of how a provider sign-in ends: the two
// files have to agree about the callback path, and the third is the same origin
// read again for a preflight.
func authArtifact(t *testing.T, artifacts []gen.Artifact) []gen.Artifact {
	t.Helper()

	var out []gen.Artifact
	for _, a := range artifacts {
		switch a.Path {
		case "auth.gen.go", "web.gen.go", "cors.gen.go":
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		t.Fatal("no auth.gen.go was generated")
	}
	return out
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
	// The cache is a block of its own, and this fixture is "configure nothing" —
	// so it goes too, or the golden would be showing one thing that was
	// configured next to everything that was not. The web block goes for the
	// same reason: two words of auth configuration say nothing about a front
	// end somewhere else.
	doc.API.Cache = nil
	doc.API.Web = nil

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
		"Cache: auth.CacheOptions{",
		"TTL:        45 * time.Second",
		`Channel:    "rig_cache_authwired"`,
		"MaxEntries: 25000",
		"Logger: h.Logger",
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
		// And the front end, whose origin the ending redirects to and whose
		// callback path the cookie is scoped to.
		"Browser: &handoff.Config{Origin: web, CallbackPath: WebCallbackPath}",
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

	if !strings.Contains(got, "fail(srv, w, r, requestContext(srv, r), err)") {
		t.Error("the wiring should fail through this package's own path, unqualified")
	}
	if strings.Contains(got, "RequestContext{") {
		t.Error("the mapper builds a request context of its own, which is the second " +
			"answer requestContext exists to stop there being")
	}
	if !strings.Contains(got, "RequestID: h.RequestID") {
		t.Error("a project that labels its requests its own way does not reach the auth routes")
	}
	// Without it the Server reads apibase's default header, which is the right
	// one in most projects and the wrong one in exactly the projects that said
	// otherwise in rig.yaml — so /auth/* would look for a header the resource
	// routes in the same binary do not.
	if !strings.Contains(got, "RequestIDHeader: RequestIDHeader") {
		t.Error("the auth mapper reads a different header than every other route")
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

// TestTheOrdinaryOAuthDeploymentCompiles is the shape #130 arrived from: a
// provider sign-in, no reverse proxy to trust, and insecure off because this is
// not somebody's laptop.
//
// It is the one auth configuration in which nothing else in auth.gen.go emits a
// fmt call, so it is where an import collected outside the branch that uses it
// stops the package compiling. Every other oauth document in the repository
// sets trusted_proxies or insecure and consumes fmt by accident — including
// examples/auth_oauth, which is why make examples did not catch it either.
func TestTheOrdinaryOAuthDeploymentCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth.TrustedProxies = nil

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

// TestTheHooksSupplyEveryOAuthValue is #133: the origin, the signing key and
// each provider's pair are all readable from the environment, and now all three
// have a field in front of that read.
//
// Field first, the way serve.Config.fromEnvironment answers the same question
// for DatabaseURL and Addr. What is checked here is that every one of the three
// consults the hooks *before* os.Getenv, because a fallback in the other order
// is not a fallback — it is the environment still winning.
func TestTheHooksSupplyEveryOAuthValue(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	for _, want := range []string{
		// The fields themselves, and the struct a project names a provider on.
		"BaseURL string",
		"SigningKey []byte",
		"Credentials OAuthCredentials",
		"type OAuthCredentials struct {",
		"type OAuthClient struct {",
		"type OAuthMicrosoftClient struct {",

		// The two rules rig checked on base_url when it read the file, applied to
		// the origin it could not see, and the key it can only be handed.
		"Hooks.OAuth.BaseURL must be an absolute origin",
		"Hooks.OAuth.SigningKey holds %d bytes and needs at least 32",

		// The origin: the field, then what rig.yaml and the environment resolved.
		`base := strings.TrimRight(h.OAuth.BaseURL, "/")`,
		"if base, err = BaseURL(); err != nil {",
		"BaseURL:   base,",

		// The key, which reaches the variable only when nothing supplied one.
		"func signingKey(h OAuthHooks) ([]byte, error) {",
		"key := h.SigningKey",
		"key = []byte(os.Getenv(SigningKeyEnv))",

		// And each pair, per provider, including Microsoft's own tenant.
		`cmp.Or(h.Credentials.Google.ID, os.Getenv("GOOGLE_CLIENT_ID"))`,
		`cmp.Or(h.Credentials.Google.Secret, os.Getenv("GOOGLE_CLIENT_SECRET"))`,
		`cmp.Or(h.Credentials.Microsoft.ID, os.Getenv("MICROSOFT_CLIENT_ID"))`,
		`cmp.Or(h.Credentials.Microsoft.Tenant, os.Getenv("WORK_DIRECTORY"))`,
		`cmp.Or(h.Credentials.GitHub.ID, os.Getenv("GH_APP_ID"))`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the generated wiring does not contain %q", want)
		}
	}

	// The old shape, which is the one that had nowhere to disagree.
	for _, gone := range []string{
		"func signingKey() ([]byte, error) {",
		"BaseURL:   BaseURL(),",
		`if id, secret := os.Getenv("GOOGLE_CLIENT_ID")`,
	} {
		if strings.Contains(got, gone) {
			t.Errorf("the generated wiring still reads the environment first: %q", gone)
		}
	}
}

// TestARequiredProviderIsSatisfiedByAHook is the half of #133 that is easy to
// get wrong: `required` refuses to start when a provider's credentials are
// absent, and a pair that arrived through the hooks is not absent.
//
// It holds because the refusal is the else arm of the same if that reads the
// field, so there is no second condition to keep in step — which is what this
// asserts, since a rule with one expression cannot drift.
func TestARequiredProviderIsSatisfiedByAHook(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	guard, _, ok := strings.Cut(got, "provider google is required")
	if !ok {
		t.Fatal("the authwired fixture marks google required, so a refusal should be emitted")
	}
	if i := strings.LastIndex(guard, "if id, secret :="); i < 0 ||
		!strings.Contains(guard[i:], "h.Credentials.Google.ID") {
		t.Error("the required refusal is reachable without the hooks having been asked, " +
			"so a project that supplied the pair itself cannot start")
	}
	if !strings.Contains(got, "neither Hooks.OAuth.Credentials.Google nor "+
		"GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET") {
		t.Error("the refusal should name both ways in, the way serve's checkStated does")
	}
}

// TestCredentialsCarryOnlyTheConfiguredProviders is why this is a struct rather
// than a map keyed on the provider name, and it is the same reason the emitted
// Shutdown has fields: a project that does not offer Microsoft should have
// nowhere to name one, and a misspelling should cost a compilation.
func TestCredentialsCarryOnlyTheConfiguredProviders(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth.OAuth.Providers = doc.API.Auth.OAuth.Providers[:1]

	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	if !strings.Contains(got, "Google OAuthClient") {
		t.Error("the one configured provider should have a field")
	}
	for _, gone := range []string{
		"Microsoft OAuthMicrosoftClient", "GitHub OAuthClient",
		// The directory goes with the type that has it, so a project offering
		// only Google has nowhere to name one at all.
		"type OAuthMicrosoftClient struct {", "Tenant string",
	} {
		if strings.Contains(got, gone) {
			t.Errorf("only google is configured, so %q should not be nameable", gone)
		}
	}
	if !strings.Contains(got, "The only field is Google") {
		t.Error("the struct should say why its field set is the one it has")
	}
}

// TestMicrosoftsDirectoryIsOnMicrosoftsType is the field-per-provider rule one
// level down.
//
// Microsoft accepts a directory and nobody else does. On a shared OAuthClient
// that is Credentials.Google.Tenant: a field that compiles, that reads like a
// setting, and that nothing anywhere consults — which is the exact failure a
// struct with a field per provider was chosen over a keyed map to prevent.
func TestMicrosoftsDirectoryIsOnMicrosoftsType(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	if !strings.Contains(got, "Microsoft OAuthMicrosoftClient") ||
		!strings.Contains(got, "Google OAuthClient") {
		t.Fatal("Microsoft's registration should be a type of its own")
	}
	tenant := strings.Count(got, "Tenant string")
	if tenant != 1 {
		t.Errorf("the directory should be nameable exactly once, not %d times", tenant)
	}
	if i := strings.Index(got, "type OAuthMicrosoftClient struct {"); i < 0 ||
		strings.Index(got, "Tenant string") < i {
		t.Error("the directory is on the shared type, so Google and GitHub can name " +
			"one and nothing will read it")
	}
}

// TestAnUnsetOriginIsRefusedHere is the shape #133 was found in: base_url_env
// alone, with nothing in the variable.
//
// What used to happen was an empty string travelling into rig/auth to be
// refused by oauth.New — several frames below anything the project wrote, and
// naming nothing it could set. No fixture in the repository generates this
// branch, because all three set base_url as well.
func TestAnUnsetOriginIsRefusedHere(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth.OAuth.BaseURL = ""

	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	if !strings.Contains(got, "func BaseURL() (string, error) {") {
		t.Fatal("BaseURL should answer an error rather than the empty string")
	}
	if !strings.Contains(got, "BASE_URL must name this application's own origin") ||
		!strings.Contains(got, "Hooks.OAuth.BaseURL is the other way to supply it") {
		t.Error("the refusal should name the variable and the field, and come from here")
	}
	// And the field still short-circuits it: a project that supplies the origin
	// in Go named only a variable in rig.yaml, and must not meet this refusal.
	if !strings.Contains(got, `base := strings.TrimRight(h.OAuth.BaseURL, "/")`) {
		t.Error("Config should prefer the field without asking BaseURL at all")
	}

	// Compiled rather than only read, because this is the branch whose imports
	// differ: it takes errors and strings and, unlike the two-value shape, no
	// cmp — and an import collected outside the branch that uses it is #130.
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
		gentest.Package{Dir: "api", Artifacts: append(
			gentest.Run(t, servicego.New(), doc, gen.Options{Raw: map[string]any{
				"package": "api", "model_import": "rigtest/model", "store_import": "rigtest/store",
			}}),
			gentest.Run(t, servergo.New(), doc, authOpts())...)},
	)
}

// TestAFailedProviderSignInIsReachableFromTheHooks is rig#150's other half.
//
// oauth.Config.OnError is useless to a generated project unless the generated
// hooks carry it: a project never calls auth.New itself, so a field rig does not
// emit is a field nobody can set. OnSignIn has always been emitted this way and
// this is the same two lines — which is exactly why it was easy to add the hook
// and forget the wiring.
func TestAFailedProviderSignInIsReachableFromTheHooks(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	for _, want := range []string{
		"OnError func(w http.ResponseWriter, r *http.Request, f *oauth.Failure)",
		"OnError:           h.OAuth.OnError,",
		// The one rule a project has to be told, because getting it wrong is a
		// reflected-input bug rather than a compile error.
		"Never render Failure.ProviderError",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the generated hooks do not contain %q", want)
		}
	}

	// And it goes exactly where OnSignIn goes, in both halves: the field is on
	// the struct wherever that one is, and the assignment is behind the same
	// condition. Two hooks emitted by two rules is one rule to get out of step.
	doc.API.Auth.OAuth.Providers = nil
	bare := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")
	for _, pair := range [][2]string{
		{"OnSignIn func(", "OnError func("},
		{"OnSignIn:", "OnError:"},
	} {
		if strings.Contains(bare, pair[0]) != strings.Contains(bare, pair[1]) {
			t.Errorf("with no providers, %q and %q are emitted by different rules",
				pair[0], pair[1])
		}
	}
}

// TestAProjectCanWriteTheOAuthHooksLiteral compiles the literal docs/auth.md
// prints, in the package the generator wrote it for.
//
// A golden file cannot notice this. It proves the bytes have not changed, which
// stays true after a field is renamed and every documented example of filling
// one in stops building — and these fields exist to be written by hand, so the
// hand-written form is the one worth checking.
func TestAProjectCanWriteTheOAuthHooksLiteral(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))

	api := gentest.Run(t, servicego.New(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "store_import": "rigtest/store",
	}})
	api = append(api, gentest.Run(t, servergo.New(), doc, authOpts())...)
	api = append(api, gen.Artifact{Path: "hooks_literal.go", Content: []byte(
		"package api\n\n" +
			"import (\n\t\"net/http\"\n\n\t\"github.com/simonjanss/rig/auth/oauth\"\n)\n\n" +
			"// What docs/auth.md shows a main function writing.\n" +
			"var _ = Hooks{OAuth: OAuthHooks{\n" +
			"\tBaseURL:    \"https://app.example.com\",\n" +
			"\tWebOrigin:  \"https://console.example.com\",\n" +
			"\tSigningKey: make([]byte, 32),\n" +
			"\tCredentials: OAuthCredentials{\n" +
			"\t\tGoogle:    OAuthClient{ID: \"id\", Secret: \"secret\"},\n" +
			"\t\tMicrosoft: OAuthMicrosoftClient{ID: \"id\", Secret: \"secret\", Tenant: \"common\"},\n" +
			"\t\tGitHub:    OAuthClient{ID: \"id\", Secret: \"secret\"},\n" +
			"\t},\n" +
			"\tOnError: func(w http.ResponseWriter, r *http.Request, f *oauth.Failure) {\n" +
			"\t\thttp.Redirect(w, r, \"/login?error=\"+string(f.Reason), http.StatusSeeOther)\n" +
			"\t},\n" +
			"}}\n")})

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

// TestASuppliedSigningKeyBeatsTheDevelopmentOne is the one precedence question
// with two wrong answers rather than one.
//
// Under auth.oauth.insecure an empty key is minted per process. A key the
// application supplied must win over that branch rather than race it — and a
// supplied key that is too short must reach the refusal, because what a project
// stated is not something to quietly substitute for.
func TestASuppliedSigningKeyBeatsTheDevelopmentOne(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth.OAuth.Insecure = true

	got := find(t, gentest.Run(t, servergo.New(), doc, authOpts()), "auth.gen.go")

	key := got[strings.Index(got, "func signingKey(h OAuthHooks)"):]
	mint := strings.Index(key, "case len(key) == 0:")
	if mint < 0 {
		t.Fatal("insecure is set, so a development key should be minted for an empty one")
	}
	if !strings.Contains(key[:mint], "key := h.SigningKey") {
		t.Error("the development key is minted before the hooks are read, so a supplied " +
			"key races the one this process invented")
	}
	// Which is what makes the mint unreachable for a short supplied key: the
	// branch asks for nothing at all, not for something too small.
	if !strings.Contains(key[:mint], "case len(key) >= 32:") {
		t.Error("a supplied key of fewer than 32 bytes should reach the refusal")
	}
}

// TestTheOriginHookAndTheOriginFieldAreBothEmitted covers the configuration no
// fixture in the repository has: origin_from_host off, which is the only shape
// where OAuthHooks carries both an Origin function and a BaseURL string.
//
// They are different questions — one origin the callback routes are built from,
// and an override per request — so the emitted file has to say which is which,
// and it has to compile with both on the struct.
func TestTheOriginHookAndTheOriginFieldAreBothEmitted(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	doc.API.Auth.OAuth.OriginFromHost = false

	api := gentest.Run(t, servicego.New(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "store_import": "rigtest/store",
	}})
	api = append(api, gentest.Run(t, servergo.New(), doc, authOpts())...)

	got := find(t, api, "auth.gen.go")
	for _, want := range []string{
		"Origin func(r *http.Request) string",
		"BaseURL string",
		"h.OAuth.Origin,",
		"Not [OAuthHooks.Origin], which answers per request.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the generated wiring does not contain %q", want)
		}
	}

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
