package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/gen"
)

// generated is one file out of a run, by base name.
func generated(t *testing.T, o gen.Options, name string) string {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for _, a := range gentest.Run(t, servergo.New(), doc, o) {
		if filepath.Base(a.Path) == name {
			return string(a.Content)
		}
	}
	t.Fatalf("no %s in the run", name)
	return ""
}

// electric_required is one boolean and it decides three things: whether a sync
// service that is not answering stops the boot, whether it is in the readiness
// check, and — because both of those are behind the same constant — nothing
// else. Emitting it wrongly is a project that says it cannot run without live
// sync and starts anyway, which no test at runtime would catch: by then it is a
// constant that was compiled in.
func TestElectricRequiredDecidesWhatABadAnswerCosts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  any
		want string
	}{
		{"said nothing", nil, "const ElectricRequired = false"},
		{"required", true, "const ElectricRequired = true"},
		{"stated false", false, "const ElectricRequired = false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := opts()
			if tc.set != nil {
				o.Raw["electric_required"] = tc.set
			}
			got := generated(t, o, "electric.gen.go")
			if !strings.Contains(got, tc.want) {
				t.Errorf("no %q in electric.gen.go", tc.want)
			}
		})
	}
}

// The boot check is generated, not written, so what has to hold is that it is
// reachable: Mount calls it, and it asks the sync service rather than assuming.
func TestTheBootChecksTheSyncService(t *testing.T) {
	t.Parallel()

	elec := generated(t, opts(), "electric.gen.go")
	for _, want := range []string{
		"func CheckSyncService(",
		// The probe itself, and both answers to a bad one.
		"proxy.Health(probe)",
		"if ElectricRequired {",
		"the sync service is not answering",
		"syncHint",
		// And the readiness registration, behind the same constant.
		`app.Ready("sync service", proxy.Health)`,
	} {
		if !strings.Contains(elec, want) {
			t.Errorf("no %q in electric.gen.go", want)
		}
	}

	// Reachable: Parts carries the proxy and Mount is what asks it. Without
	// this the whole file is dead code that still compiles.
	run := generated(t, opts(), "run.gen.go")
	for _, want := range []string{
		"Proxy *electric.Proxy",
		"CheckSyncService(ctx, app, parts.Proxy)",
		// A nil one is said rather than refused, because rig cannot tell a
		// project that has not built a front end yet from one that forgot.
		"no sync service in this server",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("no %q in run.gen.go", want)
		}
	}
}

// A project with no shapes gets none of it: no Parts.Proxy, no check, and no
// import of rig/electric in the file that would otherwise name it.
func TestAProjectWithNoShapesGetsNoSyncCheck(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "relations.ir.json"))
	for _, a := range gentest.Run(t, servergo.New(), doc, opts()) {
		if filepath.Base(a.Path) != "run.gen.go" {
			continue
		}
		if body := string(a.Content); strings.Contains(body, "CheckSyncService") {
			t.Error("run.gen.go checks a sync service this project has no shapes for")
		}
		return
	}
	t.Fatal("no run.gen.go in the run")
}
