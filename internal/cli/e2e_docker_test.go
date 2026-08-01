//go:build docker

// The whole loop, from an empty directory to a validated project, against a
// real Postgres.
//
//	go test -tags docker ./internal/cli/
//
// This is the test that would catch a scaffold that does not apply, a sync that
// writes configuration its own loader rejects, or an introspection change that
// breaks the compiler. Each of those is invisible to the unit tests, because
// each lives in the seam between two pieces that are individually fine.
package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEndToEnd(t *testing.T) {
	root := t.TempDir()

	// The container name is derived from the project name, so a distinctive
	// one keeps this from fighting with a developer's own database.
	const (
		projectName   = "rigE2E"
		containerName = projectName + "-db"
	)

	// rig leaves its container running between commands, which is what makes
	// the inner loop fast — and what makes this test non-hermetic if it does
	// not say otherwise. A schema left behind by an earlier run would make the
	// later steps see columns they were supposed to add themselves.
	removeContainer(t, containerName)
	t.Cleanup(func() { removeContainer(t, containerName) })

	step(t, "init", func() {
		_, stderr, code := run(t, "init", root, "--name", projectName, "--module", "example.com/e2e")
		if code != 0 {
			t.Fatalf("init failed:\n%s", stderr)
		}
	})

	// A distinct port, so a real project's container is never touched.
	appendTo(t, filepath.Join(root, "rig.yaml"), "\ndatabase:\n  port: 55493\n")

	step(t, "migration new", func() {
		_, stderr, code := run(t, "migration", "new", "create_team",
			"--table", "team", "--soft-delete", "--snapshot", "-C", root)
		if code != 0 {
			t.Fatalf("migration new failed:\n%s", stderr)
		}
	})

	// Fill in the placeholder the scaffold leaves, the way a developer would.
	migration := filepath.Join(root, "migrations", "00001_create_team.sql")
	replace(t, migration, "    -- Add your columns here.",
		"    name        text NOT NULL,\n    is_active   boolean NOT NULL DEFAULT true,")

	step(t, "db up", func() {
		// The scaffolded SQL has to apply as written. A trailing comma or an
		// unbalanced CHECK constraint fails right here.
		_, stderr, code := run(t, "db", "up", "-C", root)
		if code != 0 {
			t.Fatalf("db up failed:\n%s", stderr)
		}
	})

	step(t, "sync", func() {
		_, stderr, code := run(t, "sync", "-C", root)
		if code != 0 {
			t.Fatalf("sync failed:\n%s", stderr)
		}
		if !strings.Contains(stderr, "create") {
			t.Errorf("sync should have created a file:\n%s", stderr)
		}
	})

	config := filepath.Join(root, "services", "team", "team.yaml")
	if _, err := os.Stat(config); err != nil {
		t.Fatalf("sync wrote no configuration for team: %v", err)
	}

	step(t, "validate reports the placeholders", func() {
		// A file full of TODOs must not pass: a rule a placeholder satisfies is
		// a rule nobody ever acts on.
		_, stderr, code := run(t, "validate", "-C", root)
		if code == 0 {
			t.Fatalf("a freshly synced project should not validate:\n%s", stderr)
		}
		if !strings.Contains(stderr, "RIG6002") {
			t.Errorf("expected missing-comment diagnostics:\n%s", stderr)
		}
	})

	step(t, "fill in the comments", func() {
		replace(t, config,
			"  name:\n    comment: 'TODO: describe this.'",
			"  name:\n    comment: Display name shown in league tables.")
		replace(t, config,
			"  is_active:\n    comment: 'TODO: describe this.'",
			"  is_active:\n    comment: Whether the team is still competing.")
		fillPlaceholders(t, config)
	})

	step(t, "validate passes", func() {
		_, stderr, code := run(t, "validate", "-C", root)
		if code != 0 {
			t.Fatalf("a completed project should validate:\n%s", stderr)
		}
		if !strings.Contains(stderr, "no problems found") {
			t.Errorf("expected the clean summary:\n%s", stderr)
		}
	})

	step(t, "sync is idempotent", func() {
		_, stderr, code := run(t, "sync", "-C", root)
		if code != 0 {
			t.Fatalf("sync failed:\n%s", stderr)
		}
		if !strings.Contains(stderr, "already up to date") {
			t.Errorf("a second sync should have nothing to do:\n%s", stderr)
		}
		// And it must not have disturbed the comments just written.
		if strings.Contains(read(t, config), "TODO") {
			t.Error("sync reintroduced placeholders over hand-written comments")
		}
	})

	step(t, "ir is stable", func() {
		first, stderr, code := run(t, "ir", "-C", root)
		if code != 0 {
			t.Fatalf("ir failed:\n%s", stderr)
		}
		second, _, _ := run(t, "ir", "-C", root)
		if first != second {
			t.Error("two runs produced different documents")
		}
		for _, want := range []string{
			`"soft_delete"`,
			`"restore_window_days": 30`,
			`"snapshot"`,
			`"QUERY /api/v1/teams"`,
			`"POST /api/v1/teams/_search"`,
		} {
			if !strings.Contains(first, want) {
				t.Errorf("the document is missing %s", want)
			}
		}
	})

	step(t, "a new migration flows through", func() {
		writeFile(t, filepath.Join(root, "migrations", "00002_add_tier.sql"), `-- +goose Up
-- +goose StatementBegin
CREATE TYPE team_tier AS ENUM ('amateur', 'professional');
ALTER TABLE team ADD COLUMN tier team_tier NOT NULL DEFAULT 'amateur';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE team DROP COLUMN tier;
DROP TYPE team_tier;
-- +goose StatementEnd
`)

		_, stderr, code := run(t, "sync", "-C", root)
		if code != 0 {
			t.Fatalf("sync failed:\n%s", stderr)
		}
		if !strings.Contains(stderr, "added tier") {
			t.Errorf("sync should have picked up the new column:\n%s", stderr)
		}

		got := read(t, config)
		// The hand-written comments survive an incremental sync.
		if !strings.Contains(got, "Display name shown in league tables.") {
			t.Error("sync lost a hand-written comment")
		}
		if !strings.Contains(got, "  team_tier:") {
			t.Error("sync did not add the new enum")
		}

		// The new entries arrive as placeholders, so the project stops
		// validating until someone says what they mean. Describe them, the way
		// the loop expects.
		if _, _, code := run(t, "validate", "-C", root); code == 0 {
			t.Error("newly synced placeholders should fail validation")
		}
		fillPlaceholders(t, config)
		if _, stderr, code := run(t, "validate", "-C", root); code != 0 {
			t.Fatalf("the project should validate once described:\n%s", stderr)
		}
	})

	step(t, "db reset rebuilds from the migrations", func() {
		_, stderr, code := run(t, "db", "reset", "-C", root)
		if code != 0 {
			t.Fatalf("db reset failed:\n%s", stderr)
		}
		_, stderr, code = run(t, "ir", "-C", root, "-o", filepath.Join(root, "ir.json"))
		if code != 0 {
			t.Fatalf("ir after reset failed:\n%s", stderr)
		}
	})
}

func removeContainer(t *testing.T, name string) {
	t.Helper()
	// A container that was never created is not an error.
	_ = exec.Command("docker", "rm", "-f", "-v", name).Run()
}

// step runs one stage of the loop. The stages build on each other, so a
// failure stops the sequence rather than cascading into confusing failures
// downstream.
func step(t *testing.T, name string, fn func()) {
	t.Helper()
	if !t.Run(name, func(t *testing.T) { fn() }) {
		t.Fatalf("stopping: %q failed and the later steps depend on it", name)
	}
}

// fillPlaceholders describes whatever sync left as a TODO, standing in for the
// developer who would otherwise write the sentences themselves.
func fillPlaceholders(t *testing.T, path string) {
	t.Helper()

	content := read(t, path)
	for _, r := range []struct{ from, to string }{
		{"'TODO: what is this table for?'", "A group of players that competes as a unit."},
		{"'TODO: what does this enumeration represent?'", "A closed set of values."},
		{"'TODO: describe this.'", "Self-explanatory."},
	} {
		content = strings.ReplaceAll(content, r.from, r.to)
	}
	writeFile(t, path, content)
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func replace(t *testing.T, path, old, new string) {
	t.Helper()
	content := read(t, path)
	if !strings.Contains(content, old) {
		t.Fatalf("%s does not contain %q:\n%s", filepath.Base(path), old, content)
	}
	writeFile(t, path, strings.Replace(content, old, new, 1))
}

func appendTo(t *testing.T, path, extra string) {
	t.Helper()
	writeFile(t, path, read(t, path)+extra)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
