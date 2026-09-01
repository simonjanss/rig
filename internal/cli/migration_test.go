package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What `rig migration check` refuses, and what `rig validate` says about the
// same directory.
//
// Two of the three rules are answerable from the names on disk, so they run in
// both places and neither needs a database. The third needs a git ref and runs
// only where one was named; it has a suite of its own below.

// gitIn runs a git command in root, with an identity and none of the caller's
// GIT_ variables, so the suite does not depend on the machine having an identity
// configured or on where it was started from.
//
// The environment matters more than it looks. `git push` exports GIT_DIR to the
// hooks it runs, and this repository's pre-push hook runs the suite: inherit it
// and every command here operates on rig's own repository from a temporary
// directory that is not its work tree. That is a failure nothing reproduces
// under `go test`, which is the worst kind to leave in.
//
// commit.gpgsign for the same reason as the identity: it comes from the
// machine's configuration, it is not this suite's business, and a signature it
// cannot produce fails the commit rather than the assertion.
func gitIn(t *testing.T, root string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(withoutGitEnv(os.Environ()),
		"GIT_AUTHOR_NAME=rig", "GIT_AUTHOR_EMAIL=rig@example.com",
		"GIT_COMMITTER_NAME=rig", "GIT_COMMITTER_EMAIL=rig@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func withoutGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

func migrationsProject(t *testing.T, names ...string) string {
	t.Helper()

	root := newProject(t)
	for _, name := range names {
		write(t, filepath.Join(root, "migrations", name), "-- +goose Up\n-- +goose Down\n")
	}
	return root
}

func TestMigrationCheckAcceptsAWellNumberedDirectory(t *testing.T) {
	root := migrationsProject(t, "00001_create_todo.sql", "00002_add_title.sql")

	_, stderr, code := run(t, "migration", "check", "-C", root)
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stderr)
	}
	if !strings.Contains(stderr, "2 migrations") {
		t.Errorf("stderr does not count the migrations: %s", stderr)
	}
}

// The command reads rig.yaml and a directory and stops there. No --schema, no
// container, no generators — which is the reason it exists beside `rig validate`
// rather than inside it.
func TestMigrationCheckNeedsNoSchema(t *testing.T) {
	root := migrationsProject(t, "00001_create_todo.sql")
	if err := os.Remove(filepath.Join(root, "schema.json")); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := run(t, "migration", "check", "-C", root); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stderr)
	}
}

func TestMigrationCheckReportsDuplicateVersions(t *testing.T) {
	// Padded and unpadded, which is the pair that looks like two versions and
	// is one.
	root := migrationsProject(t, "00002_add_title.sql", "2_add_body.sql")

	_, stderr, code := run(t, "migration", "check", "-C", root)
	if code == 0 {
		t.Fatalf("exit 0, want non-zero: %s", stderr)
	}
	if !strings.Contains(stderr, "RIG6051") {
		t.Errorf("stderr does not report RIG6051: %s", stderr)
	}
}

func TestMigrationCheckReportsBadNames(t *testing.T) {
	root := migrationsProject(t, "00001_ok.sql", "2_short.sql", "00003_MixedCase.sql")

	_, stderr, code := run(t, "migration", "check", "-C", root)
	if code == 0 {
		t.Fatalf("exit 0, want non-zero: %s", stderr)
	}
	for _, name := range []string{"2_short.sql", "00003_MixedCase.sql"} {
		if !strings.Contains(stderr, name) {
			t.Errorf("stderr does not name %s: %s", name, stderr)
		}
	}
	if strings.Contains(stderr, "00001_ok.sql") {
		t.Errorf("stderr blames a well-named migration: %s", stderr)
	}
}

// A directory that does not exist is a project between `rig init` and its first
// migration, which has nothing to check rather than something to complain about.
func TestMigrationCheckWithNoMigrationsDirectory(t *testing.T) {
	root := newProject(t)

	if _, stderr, code := run(t, "migration", "check", "-C", root); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stderr)
	}
}

// `validate.migration_filename` has been a documented key with nothing behind it
// for as long as it has existed. These are the two ends of it working.
func TestMigrationFilenameSeverityIsConfigurable(t *testing.T) {
	t.Run("off reports nothing", func(t *testing.T) {
		root := migrationsProject(t, "2_short.sql")
		write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  migration_filename: off
`)
		if _, stderr, code := run(t, "migration", "check", "-C", root); code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stderr)
		}
	})

	t.Run("warn does not fail the run, and --strict makes it", func(t *testing.T) {
		root := migrationsProject(t, "2_short.sql")
		write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
validate:
  migration_filename: warn
`)
		_, stderr, code := run(t, "migration", "check", "-C", root)
		if code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stderr)
		}
		if !strings.Contains(stderr, "warning[RIG6050]") {
			t.Errorf("stderr does not warn: %s", stderr)
		}
		// And a run that reported something does not also claim it found
		// nothing.
		if strings.Contains(stderr, "no problems found") {
			t.Errorf("stderr says both: %s", stderr)
		}

		if _, stderr, code := run(t, "migration", "check", "-C", root, "--strict"); code == 0 {
			t.Fatalf("--strict exited 0: %s", stderr)
		}
	})
}

// The file rules are part of the ordinary validation pass too, so a project that
// never runs the new command still finds out.
func TestValidateReportsMigrationProblems(t *testing.T) {
	root := migrationsProject(t, "00002_add_title.sql", "2_add_body.sql")

	_, stderr, code := runWithSchema(t, root, "validate", "-C", root)
	if code == 0 {
		t.Fatalf("exit 0, want non-zero: %s", stderr)
	}
	for _, code := range []string{"RIG6050", "RIG6051"} {
		if !strings.Contains(stderr, code) {
			t.Errorf("stderr does not report %s: %s", code, stderr)
		}
	}
}

// --format github is what a pull request annotation is made of, and the anchor
// has to be the migration rather than rig.yaml for it to land anywhere useful.
func TestMigrationCheckAnnotatesTheFile(t *testing.T) {
	root := migrationsProject(t, "2_short.sql")

	_, stderr, _ := run(t, "--format", "github", "migration", "check", "-C", root)
	if !strings.Contains(stderr, "::error file=migrations/2_short.sql,title=RIG6050::") {
		t.Errorf("stderr is not an annotation on the migration: %s", stderr)
	}
}

// The ordering rule, which is the only part that needs a repository.
//
// These build one rather than mocking git, because the two invocations are the
// whole of the rule and both of them are easy to get subtly wrong: which ref
// supplies the ceiling, and which comparison supplies the added set.
func TestMigrationCheckAgainstABaseRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// repo returns a project on a `feature` branch cut from `main`, with main's
	// migrations committed first. No onBranch leaves the branch at main's tip
	// for a subtest that makes its own commit.
	repo := func(t *testing.T, onMain, onBranch []string) string {
		t.Helper()

		root := migrationsProject(t, onMain...)

		gitIn(t, root, "init", "-q", "-b", "main")
		gitIn(t, root, "add", "-A")
		gitIn(t, root, "commit", "-qm", "main")

		gitIn(t, root, "switch", "-qc", "feature")
		if len(onBranch) == 0 {
			return root
		}
		for _, name := range onBranch {
			write(t, filepath.Join(root, "migrations", name), "-- +goose Up\n")
		}
		gitIn(t, root, "add", "-A")
		gitIn(t, root, "commit", "-qm", "feature")
		return root
	}

	t.Run("a migration above the base passes", func(t *testing.T) {
		root := repo(t, []string{"00001_a.sql"}, []string{"00002_b.sql"})

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main")
		if code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stderr)
		}
	})

	t.Run("a migration at or below the base is refused", func(t *testing.T) {
		root := repo(t, []string{"00001_a.sql", "00002_b.sql"}, []string{"00002_late.sql"})

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main")
		if code == 0 {
			t.Fatalf("exit 0, want non-zero: %s", stderr)
		}
		if !strings.Contains(stderr, "RIG6052") {
			t.Errorf("stderr does not report RIG6052: %s", stderr)
		}
		if !strings.Contains(stderr, "3 or higher") {
			t.Errorf("stderr does not suggest the next number: %s", stderr)
		}
	})

	// The case the whole tip-versus-merge-base decision exists for.
	//
	// The branch's 00002 was free when it was written; somebody else's 00002
	// merged to main afterwards. Ask the merge base what the ceiling is and this
	// passes, because at the merge base main was still at 00001 — and the pair
	// merges into a directory with two files claiming version 2.
	t.Run("a migration merged to the base after the branch was cut still counts", func(t *testing.T) {
		root := repo(t, []string{"00001_a.sql"}, []string{"00002_mine.sql"})

		gitIn(t, root, "switch", "-q", "main")
		write(t, filepath.Join(root, "migrations", "00002_theirs.sql"), "-- +goose Up\n")
		gitIn(t, root, "add", "-A")
		gitIn(t, root, "commit", "-qm", "theirs")
		gitIn(t, root, "switch", "-q", "feature")

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main")
		if code == 0 {
			t.Fatalf("exit 0, want non-zero: %s", stderr)
		}
		if !strings.Contains(stderr, "RIG6052") {
			t.Errorf("stderr does not report RIG6052: %s", stderr)
		}
		// And theirs is not reported as something this branch added: the added
		// set is the three-dot diff, so it holds only this branch's own work.
		if strings.Contains(stderr, "00002_theirs.sql") {
			t.Errorf("stderr blames the other branch's migration: %s", stderr)
		}
	})

	// A migration five seconds old is not committed yet, and the two file rules
	// already read it off disk. The ordering rule reading HEAD instead would
	// answer "no problems found" about a directory holding the problem.
	t.Run("a migration that is written but not committed counts", func(t *testing.T) {
		root := repo(t, []string{"00005_a.sql"}, nil)

		for _, name := range []string{"00003_untracked.sql", "00004_staged.sql"} {
			write(t, filepath.Join(root, "migrations", name), "-- +goose Up\n")
		}
		gitIn(t, root, "add", "migrations/00004_staged.sql")

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main")
		if code == 0 {
			t.Fatalf("exit 0, want non-zero: %s", stderr)
		}
		for _, name := range []string{"00003_untracked.sql", "00004_staged.sql"} {
			if !strings.Contains(stderr, name) {
				t.Errorf("stderr does not name %s: %s", name, stderr)
			}
		}
	})

	// The other side of it: an uncommitted migration numbered above the base is
	// no more a problem than a committed one.
	t.Run("an uncommitted migration above the base passes", func(t *testing.T) {
		root := repo(t, []string{"00005_a.sql"}, nil)
		write(t, filepath.Join(root, "migrations", "00006_new.sql"), "-- +goose Up\n")

		if _, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main"); code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stderr)
		}
	})

	// The two things a rename can be, which is why the rule reads the status
	// rather than a plain list of additions. Each migration gets a body of its
	// own here, because git pairs a rename by content and a directory of
	// identical files can be paired any way at all.
	renameRepo := func(t *testing.T, onMain ...string) string {
		t.Helper()

		root := newProject(t)
		for _, name := range onMain {
			write(t, filepath.Join(root, "migrations", name),
				"-- +goose Up\nCREATE TABLE "+strings.TrimSuffix(name, ".sql")+" ();\n")
		}
		gitIn(t, root, "init", "-q", "-b", "main")
		gitIn(t, root, "add", "-A")
		gitIn(t, root, "commit", "-qm", "main")
		gitIn(t, root, "switch", "-qc", "feature")
		return root
	}

	// Renumbering an existing migration downward is the same mistake as writing
	// a new one below the base: git reports `00003_c.sql` moved to
	// `00002_late.sql` as a rename rather than a delete and an add, and a filter
	// on additions alone would see an empty diff.
	t.Run("a migration renumbered downward is reported", func(t *testing.T) {
		root := renameRepo(t, "00001_a.sql", "00002_b.sql", "00003_c.sql")

		gitIn(t, root, "mv", "migrations/00003_c.sql", "migrations/00002_late.sql")
		gitIn(t, root, "rm", "-q", "migrations/00002_b.sql")
		gitIn(t, root, "commit", "-qm", "renumber")

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main")
		if code == 0 {
			t.Fatalf("exit 0, want non-zero: %s", stderr)
		}
		if !strings.Contains(stderr, "RIG6052") {
			t.Errorf("stderr does not report RIG6052: %s", stderr)
		}
		if !strings.Contains(stderr, "00002_late.sql") {
			t.Errorf("stderr does not name the renumbered file: %s", stderr)
		}
	})

	// The other half. A better description on a migration the base branch
	// already has keeps its number, which is the one correct thing about it —
	// goose keys on the version and never reads the name. Reporting it would ask
	// for a renumbering that would re-run applied SQL.
	t.Run("a migration renamed without renumbering is not reported", func(t *testing.T) {
		root := renameRepo(t, "00001_a.sql", "00002_b.sql", "00003_c.sql")

		gitIn(t, root, "mv", "migrations/00001_a.sql", "migrations/00001_a_clearer_name.sql")
		gitIn(t, root, "commit", "-qm", "rename")

		if _, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main"); code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stderr)
		}
	})

	// A project below the repository root reports one path for a file, not two.
	// git answers about the repository and the other two rules answer about the
	// project, and an anchor that alternated between them would name the same
	// file twice and land at most one of the two annotations.
	t.Run("the anchor is relative to the project root", func(t *testing.T) {
		repoRoot := t.TempDir()
		root := filepath.Join(repoRoot, "sub")
		write(t, filepath.Join(root, "rig.yaml"), `project:
  name: demo
  module: example.com/demo
`)
		write(t, filepath.Join(root, "migrations", "00002_a.sql"), "-- +goose Up\n")

		gitIn(t, repoRoot, "init", "-q", "-b", "main")
		gitIn(t, repoRoot, "add", "-A")
		gitIn(t, repoRoot, "commit", "-qm", "main")

		gitIn(t, repoRoot, "switch", "-qc", "feature")
		write(t, filepath.Join(root, "migrations", "00001_late.sql"), "-- +goose Up\n")
		gitIn(t, repoRoot, "add", "-A")
		gitIn(t, repoRoot, "commit", "-qm", "feature")

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main")
		if code == 0 {
			t.Fatalf("exit 0, want non-zero: %s", stderr)
		}
		if !strings.Contains(stderr, "migrations/00001_late.sql") {
			t.Errorf("stderr does not anchor on the project's path: %s", stderr)
		}
		if strings.Contains(stderr, "sub/migrations") {
			t.Errorf("stderr anchors on the repository's path: %s", stderr)
		}
	})

	// The same variable the pre-push hook exports, which is a real place to run
	// this: `rig migration check --base` inside a hook inherits a GIT_DIR naming
	// somebody else's repository, and it beats the directory rig sets. git does
	// not fail cleanly on it either — it falls back to `diff --no-index` and
	// calls --merge-base an unknown option.
	t.Run("a GIT_DIR in the environment does not decide the repository", func(t *testing.T) {
		root := repo(t, []string{"00001_a.sql", "00002_b.sql"}, []string{"00002_late.sql"})
		t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere.git"))

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "main")
		if code == 0 {
			t.Fatalf("exit 0, want non-zero: %s", stderr)
		}
		if !strings.Contains(stderr, "RIG6052") {
			t.Errorf("stderr does not report RIG6052: %s", stderr)
		}
	})

	t.Run("a base ref nobody fetched says so", func(t *testing.T) {
		root := repo(t, []string{"00001_a.sql"}, []string{"00002_b.sql"})

		_, stderr, code := run(t, "migration", "check", "-C", root, "--base", "origin/nope")
		if code == 0 {
			t.Fatalf("exit 0, want non-zero: %s", stderr)
		}
		// The useful half of a git failure is on its stderr, and a bare "exit
		// status 128" is not a diagnosis.
		if !strings.Contains(stderr, "bad revision") {
			t.Errorf("stderr does not carry git's own message: %s", stderr)
		}
	})

	// Without --base the rule does not run at all, so the command works outside
	// a repository — which is most of the ways somebody runs it locally.
	t.Run("without --base no git is consulted", func(t *testing.T) {
		root := migrationsProject(t, "00001_a.sql")

		if _, stderr, code := run(t, "migration", "check", "-C", root); code != 0 {
			t.Fatalf("exit %d, want 0: %s", code, stderr)
		}
	})
}
