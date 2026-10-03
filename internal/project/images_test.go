package project_test

import (
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

// An image whose address is a fact about the puller rather than about the
// project.
//
// A mirror in a private registry is reached at a name carrying somebody's
// account — <account>.dkr.ecr.<region>.amazonaws.com/… — and committing one into
// rig.yaml puts that account in a file every laptop and every fork reads. The
// case stopped being hypothetical when `electricsql/electric` went behind a
// login: every tag of it, so no version in rig.yaml could be pulled anonymously
// and a continuous integration job that had just authenticated to its own mirror
// had no way to say so.
//
// Hence the one setting rig reads from the environment in preference to the
// file. Both halves are asserted here, because an override that could not be
// turned off would be worse than none: a project that names its own image has
// to keep it everywhere the variable is unset.
func TestAnImageMayComeFromTheEnvironment(t *testing.T) {
	const (
		mirror   = "111122223333.dkr.ecr.eu-north-1.amazonaws.com/procss/electric:1.7.12"
		postgres = "111122223333.dkr.ecr.eu-north-1.amazonaws.com/procss/postgres:17-alpine"
	)

	t.Run("the environment wins over the file", func(t *testing.T) {
		t.Setenv(project.ElectricImageEnv, mirror)
		t.Setenv(project.ImageEnv, postgres)

		p, out := parseFiles(t, "database:\n  image: postgres:17-alpine\n  electric:\n    enabled: true\n    image: electricsql/electric:1.6.9\n")
		if out != "" {
			t.Fatalf("this should be valid:\n%s", out)
		}
		if got := p.Config.Database.Electric.Image; got != mirror {
			t.Errorf("electric image = %q, want the mirror %q", got, mirror)
		}
		if got := p.Config.Database.Image; got != postgres {
			t.Errorf("database image = %q, want the mirror %q", got, postgres)
		}
	})

	t.Run("the file wins when the environment says nothing", func(t *testing.T) {
		p, out := parseFiles(t, "database:\n  image: postgres:16\n  electric:\n    enabled: true\n    image: electricsql/electric:9.9.9\n")
		if out != "" {
			t.Fatalf("this should be valid:\n%s", out)
		}
		if got := p.Config.Database.Electric.Image; got != "electricsql/electric:9.9.9" {
			t.Errorf("electric image = %q, want the one rig.yaml names", got)
		}
		if got := p.Config.Database.Image; got != "postgres:16" {
			t.Errorf("database image = %q, want the one rig.yaml names", got)
		}
	})

	// A variable set to nothing is a shell that expanded something to nothing —
	// a registry lookup that failed, most often. Taking it literally would start
	// a container named "", which fails later and further from the cause.
	t.Run("an empty variable is not an image", func(t *testing.T) {
		t.Setenv(project.ElectricImageEnv, "")
		t.Setenv(project.ImageEnv, "")

		p, out := parseFiles(t, "database:\n  electric:\n    enabled: true\n")
		if out != "" {
			t.Fatalf("this should be valid:\n%s", out)
		}
		if got := p.Config.Database.Electric.Image; got != project.DefaultElectricImage {
			t.Errorf("electric image = %q, want rig's own pin", got)
		}
		if got := p.Config.Database.Image; got != project.DefaultImage {
			t.Errorf("database image = %q, want rig's own pin", got)
		}
	})
}
