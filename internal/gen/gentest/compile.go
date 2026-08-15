package gentest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/simonjanss/rig/pkg/gen"
)

// MustCompile writes the artifacts into a throwaway module and builds them.
//
// Golden files prove the output has not changed; they say nothing about whether
// it is valid Go. A generator can emit a file that formats cleanly, matches its
// golden exactly, and refers to a method that does not exist — and nothing else
// in the suite would notice. This is the check that does.
//
// The module replaces rig/runtime with the local copy, so a change to the
// runtime and a change to the generator are checked against each other rather
// than against whatever is published.
func MustCompile(t *testing.T, artifacts []gen.Artifact, pkg string) {
	t.Helper()
	MustCompileAll(t, Package{Dir: pkg, Artifacts: artifacts})
}

// Package is one directory in the throwaway module.
//
// Dir is relative to the module root, so a package at "store" is imported as
// "rigtest/store". Artifacts land in it by base name: the layout inside the
// module is what the test asks for, not what the generator happened to choose,
// because that is what lets one test compile several generators together.
type Package struct {
	Dir       string
	Artifacts []gen.Artifact
}

// MustCompileAll builds several packages together.
//
// The layers only make sense against each other — the API layer calls the
// repository, the stub embeds the default service — so compiling one in
// isolation would miss exactly the mismatches worth catching.
func MustCompileAll(t *testing.T, pkgs ...Package) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping compile check in short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	dir := t.TempDir()
	for _, p := range pkgs {
		pkgDir := filepath.Join(dir, filepath.FromSlash(p.Dir))
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, a := range p.Artifacts {
			path := filepath.Join(pkgDir, filepath.Base(a.Path))
			if err := os.WriteFile(path, a.Content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Both local modules are replaced, whether or not this particular set of
	// artifacts imports them: a replace for a module nothing requires is inert,
	// and `go mod tidy` sorts out which requirements are real.
	gomod := fmt.Sprintf(`module rigtest

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/simonjanss/rig/runtime v0.0.0
)

replace github.com/simonjanss/rig/runtime => %s

replace github.com/simonjanss/rig/auth => %s
`, moduleDir(t, "runtime"), moduleDir(t, "auth"))

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// GOFLAGS=-mod=mod lets the toolchain resolve from the module cache, which
	// is warm because the repository itself depends on these. GOWORK=off keeps
	// rig's own tenant from capturing the throwaway module.
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")

	if out, err := runIn(dir, env, "go", "mod", "tidy"); err != nil {
		t.Fatalf("go mod tidy on the generated module:\n%s", out)
	}
	if out, err := runIn(dir, env, "go", "build", "./..."); err != nil {
		t.Fatalf("the generated code does not compile:\n%s", out)
	}
	if out, err := runIn(dir, env, "go", "vet", "./..."); err != nil {
		t.Fatalf("the generated code does not vet cleanly:\n%s", out)
	}
}

func runIn(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// moduleDir locates one of rig's own modules relative to this source file, so
// the check works wherever the repository is cloned.
func moduleDir(t *testing.T, name string) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("cannot locate the %s module", name)
	}
	// .../internal/gen/gentest/compile.go -> repository root
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	dir := filepath.Join(root, name)

	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("no %s module at %s: %v", name, dir, err)
	}
	return dir
}
