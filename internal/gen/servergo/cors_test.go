package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// web is a block for a document that has none, for the tests below whose
// subject is a header list rather than the block's own resolution.
func web() *ir.Web {
	return &ir.Web{
		Origin:       "https://app.example.com",
		CallbackPath: "/auth/callback",
		CORS: ir.WebCORS{
			AllowedOriginsEnv: "CORS_ORIGINS",
			MaxAgeSeconds:     600,
		},
	}
}

// withWeb loads a fixture and gives it a front end on another origin.
func withWeb(t *testing.T, fixtureName string) []gen.Artifact {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixtureName))
	doc.API.Web = web()
	return gentest.Run(t, servergo.New(), doc, opts())
}

// The rule every optional block follows. Getting this wrong costs a project
// that answers no browser an import of runtime/cors and a wrapper around every
// request it serves.
func TestNoWebBlockWritesNoPolicy(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Web = nil

	for _, a := range gentest.Run(t, servergo.New(), doc, authOpts()) {
		if a.Path == "cors.gen.go" {
			t.Fatal("a project with no web block should get no cors.gen.go")
		}
		src := string(a.Content)
		for _, forbidden := range []string{
			"runtime/cors", "AllowedOrigins", "policy.Wrap", "parts.CORS",
		} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s names %q in a project with no web block", a.Path, forbidden)
			}
		}
	}
}

// QUERY is the entry whose absence has no symptom to read: the client falls
// back to POST on a 405 or a 501, and a preflight that omits a method fails as
// a network error rather than with a status.
func TestQueryIsAllowedOnlyWhereARouteUsesIt(t *testing.T) {
	t.Parallel()

	with := find(t, withWeb(t, "authwired.ir.json"), "cors.gen.go")
	if !strings.Contains(with, `"QUERY"`) {
		t.Error("no QUERY method for a project whose search is a QUERY route")
	}

	// And the other half, so this cannot pass by the entry being unconditional.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Web = web()
	for i := range doc.API.Resources {
		for j := range doc.API.Resources[i].Endpoints {
			ep := &doc.API.Resources[i].Endpoints[j]
			ep.Pattern = strings.Replace(ep.Pattern, "QUERY ", "POST ", 1)
			for k := range ep.AliasPatterns {
				ep.AliasPatterns[k] = strings.Replace(ep.AliasPatterns[k], "QUERY ", "POST ", 1)
			}
		}
	}
	without := find(t, gentest.Run(t, servergo.New(), doc, opts()), "cors.gen.go")
	if strings.Contains(without, `"QUERY"`) {
		t.Error("QUERY is allowed on a project with no QUERY route")
	}
}

// The tenant header is this project's own spelling, so the policy names the
// constant. Under the same predicate that emits it, or the file would not
// compile.
func TestTheTenantHeaderIsAllowedOnlyWhenThereIsOne(t *testing.T) {
	t.Parallel()

	with := find(t, withWeb(t, "authwired.ir.json"), "cors.gen.go")
	if !strings.Contains(with, "TenantHeader,") {
		t.Error("the tenant header is not allowed for a project that reads one")
	}

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "authwired.ir.json"))
	doc.API.Web = web()
	doc.API.Auth = nil
	without := find(t, gentest.Run(t, servergo.New(), doc, opts()), "cors.gen.go")
	if strings.Contains(without, "TenantHeader") {
		t.Error("the tenant header is named by a project with no auth block")
	}
}

// The two both SDKs act on unconditionally, and the reason each matters is in
// the generated comment rather than here.
func TestTheHeadersEverySDKUsesAreAlwaysThere(t *testing.T) {
	t.Parallel()

	got := find(t, withWeb(t, "authwired.ir.json"), "cors.gen.go")
	for _, want := range []string{
		`"Idempotency-Key"`, `"Idempotency-Replayed"`,
		`"Retry-After", "RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"`,
		"RequestIDHeader,", "RevisionHeader,",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the policy does not contain %s", want)
		}
	}
}

// Live sync's cursor travels in these, and a browser hides a response header
// from script until it is exposed — so without them a subscription ends after
// one response, which reads as a stream that stopped.
func TestTheSyncHeadersAreExposedOnlyWithShapes(t *testing.T) {
	t.Parallel()

	with := find(t, withWeb(t, fixture), "cors.gen.go")
	for _, want := range []string{
		`"electric-handle", "electric-offset", "electric-schema"`,
		`"electric-cursor", "electric-up-to-date", "electric-has-data"`,
		`"X-Rig-Sync-Fallback"`,
	} {
		if !strings.Contains(with, want) {
			t.Errorf("a project with shapes does not expose %s", want)
		}
	}

	without := find(t, withWeb(t, "authwired.ir.json"), "cors.gen.go")
	if strings.Contains(without, "electric-") {
		t.Error("a project with no shapes exposes the sync headers")
	}
}

// A download is read with three headers a browser hides, and asked for with
// one it does not allow by default.
func TestTheFileHeadersAreThereOnlyWithFiles(t *testing.T) {
	t.Parallel()

	with := find(t, withWeb(t, "files.ir.json"), "cors.gen.go")
	for _, want := range []string{
		`"Range"`, `"ETag"`,
		`"Content-Disposition", "Accept-Ranges", "Content-Range"`,
	} {
		if !strings.Contains(with, want) {
			t.Errorf("a project with files does not name %s", want)
		}
	}

	without := find(t, withWeb(t, "authwired.ir.json"), "cors.gen.go")
	for _, forbidden := range []string{"Range", "Content-Disposition", "Accept-Ranges"} {
		if strings.Contains(without, forbidden) {
			t.Errorf("a project with no files names %q", forbidden)
		}
	}
}

// The wrap is the last thing mountWith does, after every attach and after the
// OpenAPI line. A policy applied before them would be a policy some of the
// routes are outside.
func TestTheWrapIsTheLastThingMountDoes(t *testing.T) {
	t.Parallel()

	got := find(t, withWeb(t, "authwired.ir.json"), "run.gen.go")

	wrap := strings.Index(got, "parts.Handler = policy.Wrap(parts.Handler)")
	if wrap < 0 {
		t.Fatalf("nothing wraps the handler:\n%s", got)
	}
	ret := strings.Index(got[wrap:], "return parts.Handler, nil")
	if ret < 0 {
		t.Error("the wrap does not come before the handler is returned")
	}
	for _, before := range []string{"AttachAuth(app, parts.Auth)", "parts, err := build("} {
		if at := strings.Index(got, before); at < 0 || at > wrap {
			t.Errorf("%q does not come before the wrap", before)
		}
	}
}

// Both lines, because a policy that answers nobody is a decision worth seeing
// in a startup log rather than a silence.
func TestBothCrossOriginLinesAreEmitted(t *testing.T) {
	t.Parallel()

	got := find(t, withWeb(t, "authwired.ir.json"), "run.gen.go")
	for _, want := range []string{
		`"answering cross-origin requests", "origins", policy.AllowedOrigins`,
		`"not answering cross-origin requests", "cost"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the mount does not log %s", want)
		}
	}
}

// nil is the generated policy and an empty one is "none, I wrapped my own",
// which is the whole reason the field is a pointer.
func TestPartsCORSIsAPointerWithThreeMeanings(t *testing.T) {
	t.Parallel()

	got := find(t, withWeb(t, "authwired.ir.json"), "run.gen.go")
	for _, want := range []string{
		"CORS *cors.Policy",
		"case parts.CORS != nil:",
		"policy = *parts.CORS",
		"policy = CORS(origins)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("run.gen.go does not contain %q", want)
		}
	}
}

// The environment replaces the list rather than adding to it, and the origin
// itself still comes from WebOrigin — so a deployment that named only a
// variable and set nothing is refused here too.
func TestTheOriginListResolvesFromTheEnvironmentFirst(t *testing.T) {
	t.Parallel()

	got := find(t, withWeb(t, "authwired.ir.json"), "cors.gen.go")
	for _, want := range []string{
		`const AllowedOriginsEnv = "CORS_ORIGINS"`,
		"if raw := os.Getenv(AllowedOriginsEnv); raw != \"\" {",
		"return cors.Split(raw), nil",
		"origin, err := WebOrigin()",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cors.gen.go does not contain %q", want)
		}
	}
}
