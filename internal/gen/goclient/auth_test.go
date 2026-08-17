package goclient_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/goclient"
)

const authFixture = "authwired.ir.json"

func TestAuthGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	artifacts := gentest.Run(t, goclient.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "authwired"), artifacts, *update)
}

func TestAuthenticatedClientCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", authFixture))
	gentest.MustCompile(t, gentest.Run(t, goclient.New(), doc, opts()), "client")
}

// The lifetimes are the server's, and they are emitted as something a reviewer
// can check against rig.yaml rather than as a nanosecond count.
func TestTheProfileCarriesTheProjectsOwnLifetimes(t *testing.T) {
	t.Parallel()

	src := collapse(findIn(t, authFixture, "auth.gen.go"))

	for _, want := range []string{
		`BasePath: "/auth"`,
		"AccessTTL: 5 * time.Minute",
		"RefreshTTL: 8 * time.Hour",
		"RememberTTL: 14 * 24 * time.Hour",
		"RotationLeeway: 45 * time.Second",
		"IdentityTTL: 20 * time.Minute",
		"CacheTTL: 2 * time.Second",
		// The project allows both, so the client knows the routes are there.
		"HasRegistration: true",
		"HasTenantCreation: true",
		// And how a sign-in names its tenant, since that is the one call that
		// cannot read it from a credential.
		`TenantHeader: "X-Tenant-Id"`,
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, src)
		}
	}
}

// A permission is a string the server checks. Naming them means a caller minting
// an API key asks for one by identifier rather than by spelling.
func TestThePermissionsAreNamed(t *testing.T) {
	t.Parallel()

	src := collapse(findIn(t, authFixture, "auth.gen.go"))

	for _, want := range []string{
		`PermissionTodoRead = "todo.read"`,
		`PermissionTodoWrite = "todo.write"`,
		`PermissionTodoDelete = "todo.delete"`,
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, src)
		}
	}
}

// The client only knows about authentication when the document does, and then
// it is wired in one place.
func TestTheClientReadsTheProfile(t *testing.T) {
	t.Parallel()

	src := collapse(findIn(t, authFixture, "client.gen.go"))

	for _, want := range []string{
		"Auth *rigclient.Auth",
		"rigclient.API{ BasePath: BasePath, Auth: &AuthProfile, }",
		"c.Auth = rt.Auth()",
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, src)
		}
	}
}

func findIn(t *testing.T, fixtureName, artifact string) string {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixtureName))
	for _, a := range gentest.Run(t, goclient.New(), doc, opts()) {
		if filepath.Base(a.Path) == artifact {
			return string(a.Content)
		}
	}
	t.Fatalf("no artifact named %s", artifact)
	return ""
}
