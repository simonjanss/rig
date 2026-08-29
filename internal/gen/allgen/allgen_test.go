package allgen_test

import (
	"slices"
	"testing"

	_ "github.com/simonjanss/rig/internal/gen/allgen"
	"github.com/simonjanss/rig/pkg/gen"
)

// The registry is populated by init, from one blank import per generator. The
// failure this guards against is silent: drop an import and the generator
// simply stops running, and every project that configured it gets a "no such
// generator" long after the change that caused it.
func TestEveryBuiltInIsRegistered(t *testing.T) {
	registered := make([]string, 0, 8)
	for _, g := range gen.Default.All() {
		registered = append(registered, g.Name())
	}
	slices.Sort(registered)

	want := []string{
		"go-client", "model-go", "openapi",
		"persist-go", "server-go", "service-go", "ts-client",
	}
	if !slices.Equal(registered, want) {
		t.Errorf("registered = %v, want %v", registered, want)
	}

	// A generator that went away is not simply absent. A rig.yaml is a file
	// somebody wrote once and does not reread, so the one that used to write the
	// shape endpoints answers for itself rather than leaving a reader with "no
	// generator named electric" and nowhere to look.
	for name := range gen.Retired {
		if slices.Contains(registered, name) {
			t.Errorf("%s is registered and also listed as retired", name)
		}
	}
	if _, ok := gen.Retired["electric"]; !ok {
		t.Error("the electric generator was folded into server-go and should say so")
	}
}

// Everything the CLI prints about a generator, and everything a project's
// configuration is validated against, comes from these three.
func TestEveryGeneratorDescribesItself(t *testing.T) {
	for _, g := range gen.Default.All() {
		if g.Name() == "" {
			t.Errorf("%T has no name", g)
		}
		if g.Description() == "" {
			t.Errorf("%s has no description, so `rig generators` cannot say what it does", g.Name())
		}
		if g.Version() == "" {
			t.Errorf("%s has no version, so the manifest cannot record what wrote a file", g.Name())
		}
	}
}
