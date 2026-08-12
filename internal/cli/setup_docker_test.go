//go:build docker

// The authentication foundation, from an empty directory to compiling code.
//
//	go test -tags docker ./internal/cli/
//
// `rig setup-project` writes fourteen tables' worth of migrations, and the only
// honest way to know they are right is to apply them, introspect them back,
// validate them, and build what comes out. Every step here found a real bug the
// first time it ran.
//
// It also pins the other half of the arrangement: none of those tables are
// generated from, because everything they would provide already exists in the
// rig/auth module.
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/scaffold"
)

func TestSetupProject(t *testing.T) {
	root := t.TempDir()

	const (
		projectName   = "rigSetup"
		containerName = projectName + "-db"
	)

	removeContainer(t, containerName)
	t.Cleanup(func() { removeContainer(t, containerName) })

	step(t, "init", func() {
		if _, stderr, code := run(t, "init", root,
			"--name", projectName, "--module", "example.com/setup"); code != 0 {
			t.Fatalf("init failed:\n%s", stderr)
		}
	})
	appendTo(t, filepath.Join(root, "rig.yaml"), "\ndatabase:\n  port: 55496\n")

	step(t, "setup-project", func() {
		_, stderr, code := run(t, "setup-project", "-C", root)
		if code != 0 {
			t.Fatalf("setup-project failed:\n%s", stderr)
		}

		for _, want := range []string{
			"00001_rig_tenancy.sql",
			// Keys before sessions: everything after them can record which key
			// changed a row, including the account table itself.
			"00002_rig_apikeys.sql",
			"00003_rig_sessions.sql",
			"00004_rig_oauth.sql",
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("expected %s to be written:\n%s", want, stderr)
			}
		}

		// Migrations and nothing else. The tables belong to the rig/auth module,
		// so a table configuration for one would ask rig to generate a model and
		// a repository the project never calls.
		if strings.Contains(stderr, ".yaml") {
			t.Errorf("setup-project should write no table configuration:\n%s", stderr)
		}
	})

	step(t, "it is idempotent", func() {
		_, stderr, code := run(t, "setup-project", "-C", root)
		if code != 0 {
			t.Fatalf("a second setup-project failed:\n%s", stderr)
		}
		if !strings.Contains(stderr, "already in place") {
			t.Errorf("a second run should say it had nothing to do:\n%s", stderr)
		}
	})

	step(t, "the migrations apply", func() {
		// Fourteen tables, seven enums, and a self-referencing foreign key. If
		// any of it is wrong this is where it shows.
		if _, stderr, code := run(t, "db", "up", "-C", root); code != 0 {
			t.Fatalf("db up failed:\n%s", stderr)
		}
	})

	step(t, "sync fills in the rest", func() {
		if _, stderr, code := run(t, "sync", "-C", root); code != 0 {
			t.Fatalf("sync failed:\n%s", stderr)
		}
	})

	// The point of carrying COMMENT ON in the migrations: a project that has
	// just been set up validates without anybody filling in a TODO.
	step(t, "it validates as written", func() {
		_, stderr, code := run(t, "validate", "-C", root)
		if code != 0 {
			t.Fatalf("the foundation should validate as scaffolded:\n%s", stderr)
		}
		if !strings.Contains(stderr, "no problems found") {
			t.Errorf("expected a clean summary:\n%s", stderr)
		}
	})

	step(t, "generate", func() {
		if _, stderr, code := run(t, "generate", "-C", root); code != 0 {
			t.Fatalf("generate failed:\n%s", stderr)
		}
	})

	// The whole point: the foundation is eleven tables and none of them are
	// projected. Everything they would provide — the types, the stores, the
	// endpoints, the permission checks — is imported from the rig/auth module,
	// and a copy here would be a few thousand lines nothing calls.
	step(t, "the foundation is not generated from", func() {
		for _, table := range scaffold.Tables() {
			for _, path := range []string{
				filepath.Join("internal", "model", table+".gen.go"),
				filepath.Join("internal", "store", table+"_repository.gen.go"),
				filepath.Join("internal", "api", table+"_routes.gen.go"),
			} {
				if _, err := os.Stat(filepath.Join(root, path)); err == nil {
					t.Errorf("%s should not have been generated", path)
				}
			}
		}

		// Nor are the enum types those tables use: auth_event and the rest are
		// already Go constants in the auth module.
		for _, enum := range []string{"auth_event", "account_kind", "oauth_provider"} {
			path := filepath.Join("internal", "model", enum+".gen.go")
			if _, err := os.Stat(filepath.Join(root, path)); err == nil {
				t.Errorf("%s belongs to an unprojected table and should not exist", path)
			}
		}
	})

	// And the escape hatch, for an application that wants an administration
	// screen listing the people in a tenant after all.
	step(t, "a table can be exposed", func() {
		if _, stderr, code := run(t, "setup-project", "-C", root, "--expose", "account"); code != 0 {
			t.Fatalf("--expose failed:\n%s", stderr)
		}
		appendTo(t, filepath.Join(root, "rig.yaml"), "\nauth:\n  expose: [account]\n")

		if _, stderr, code := run(t, "generate", "-C", root); code != 0 {
			t.Fatalf("generate after exposing failed:\n%s", stderr)
		}
		for _, want := range []string{
			filepath.Join("internal", "model", "account.gen.go"),
			filepath.Join("internal", "store", "account_repository.gen.go"),
			filepath.Join("internal", "api", "account_routes.gen.go"),
		} {
			if _, err := os.Stat(filepath.Join(root, want)); err != nil {
				t.Errorf("exposing account should have generated %s: %v", want, err)
			}
		}

		// Its neighbours stay out: exposing one table is exposing one table.
		if _, err := os.Stat(filepath.Join(root, "internal", "store", "api_key_repository.gen.go")); err == nil {
			t.Error("exposing account should not have brought api_key with it")
		}
	})

	// An account created through plain CRUD would have no credential and no
	// verification mail, so registration is an auth endpoint instead.
	step(t, "account has no Create route", func() {
		routes := read(t, filepath.Join(root, "internal", "api", "account_routes.gen.go"))
		if strings.Contains(routes, `"POST /api/v1/accounts"`) {
			t.Error("account should not be creatable through CRUD")
		}
		if !strings.Contains(routes, `"GET /api/v1/accounts/{id}"`) {
			t.Error("account should still be readable")
		}
	})

	step(t, "it compiles", func() {
		writeFile(t, filepath.Join(root, "go.mod"), `module example.com/setup

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/simonjanss/rig/runtime v0.0.0
)

replace github.com/simonjanss/rig/runtime => `+runtimeDir(t)+`
`)
		env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := goRun(root, env, "mod", "tidy"); err != nil {
			t.Fatalf("go mod tidy:\n%s", out)
		}
		if out, err := goRun(root, env, "build", "./..."); err != nil {
			t.Fatalf("the foundation does not compile:\n%s", out)
		}
		if out, err := goRun(root, env, "vet", "./..."); err != nil {
			t.Fatalf("the foundation does not vet cleanly:\n%s", out)
		}
	})

	step(t, "check is clean", func() {
		if _, stderr, code := run(t, "check", "-C", root); code != 0 {
			t.Fatalf("check should pass right after generate:\n%s", stderr)
		}
	})
}

// Skipping a part something else depends on produces SQL that fails halfway
// through `rig db up`, which is a worse way to find out.
func TestSetupProjectSkip(t *testing.T) {
	root := t.TempDir()

	const containerName = "rigSkip-db"
	removeContainer(t, containerName)
	t.Cleanup(func() { removeContainer(t, containerName) })

	if _, stderr, code := run(t, "init", root,
		"--name", "rigSkip", "--module", "example.com/skip"); code != 0 {
		t.Fatalf("init failed:\n%s", stderr)
	}
	appendTo(t, filepath.Join(root, "rig.yaml"), "\ndatabase:\n  port: 55497\n")

	if _, stderr, code := run(t, "setup-project", "-C", root, "--skip", "tenancy"); code == 0 {
		t.Errorf("skipping tenancy should be refused: everything references it\n%s", stderr)
	}
	if _, stderr, code := run(t, "setup-project", "-C", root, "--skip", "nonsense"); code == 0 {
		t.Errorf("an unknown part should be refused\n%s", stderr)
	}

	// Sessions name the key a request arrived with, so dropping the keys alone
	// would leave a column referencing a table nobody created. Refused before
	// anything is written, rather than discovered by psql.
	if _, stderr, code := run(t, "setup-project", "-C", root, "--skip", "apikeys"); code == 0 {
		t.Errorf("skipping keys but keeping sessions should be refused\n%s", stderr)
	}

	// A real skip: no OAuth, no roles, no keys — and so no sessions either. What
	// is left still applies.
	if _, stderr, code := run(t, "setup-project", "-C", root,
		"--skip", "oauth,apikeys,sessions"); code != 0 {
		t.Fatalf("setup-project failed:\n%s", stderr)
	}
	for _, gone := range []string{"rig_oauth", "rig_apikeys", "rig_sessions"} {
		matches, _ := filepath.Glob(filepath.Join(root, "migrations", "*"+gone+"*"))
		if len(matches) > 0 {
			t.Errorf("%s should have been skipped, found %v", gone, matches)
		}
	}

	if _, stderr, code := run(t, "db", "up", "-C", root); code != 0 {
		t.Fatalf("a skipped foundation should still apply:\n%s", stderr)
	}
	if _, stderr, code := run(t, "sync", "-C", root); code != 0 {
		t.Fatalf("sync failed:\n%s", stderr)
	}
	if _, stderr, code := run(t, "validate", "-C", root); code != 0 {
		t.Fatalf("a skipped foundation should still validate:\n%s", stderr)
	}
}
