package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/migcheck"
)

// addedMigrations names the migration files this branch adds to dir, relative
// to the project root.
//
// The merge base and not the base ref's tip, which is the opposite of what
// [baseMigrations] asks and for the opposite reason. A file that merged to the
// base branch after this one was created is already in the base tree, and a
// plain two-dot diff would call it deleted here or added there depending on
// which side it looked from. Neither is true: it is somebody else's migration,
// and this branch has nothing to answer for it.
//
// Against the working tree rather than HEAD, which is what `--merge-base <ref>`
// says and `<ref>...HEAD` does not. A migration that has been written and not
// yet committed is the ordinary state of the thing five seconds after
// `rig migration new` wrote it, and it is on disk for the two file rules and for
// the count this command prints. Reading it for two rules and not the third
// would answer "no problems found" about a directory holding one.
//
// Untracked files are asked for separately because a diff cannot see them: git
// has nothing to diff a file it does not know about against. That is the whole
// of the second invocation, and it is why the ordinary local run costs two
// rather than one. --exclude-standard so that a project whose migrations are
// generated and ignored is not told about files it deliberately does not keep.
//
// A rename counts as an addition when the number changed, and not when it did
// not — which is the whole reason the status is read rather than a plain list of
// additions. Move 00030_x.sql to 00002_x.sql and git reports one rename, so a
// filter on additions alone sees an empty diff while goose sees a file claiming a
// version it applied long ago. Give a merged migration a clearer description and
// git reports a rename too, and calling that one an addition would tell somebody
// to renumber a file whose number is the one correct thing about it.
//
// -M so that both readings come from git's answer rather than from the machine's
// `diff.renames`, which a project may have turned off. git pairs a rename by
// content, so two migrations with the same bytes can be paired either way and a
// renumbering between them reads as a rename. Two migrations with the same bytes
// are interchangeable, though, and that pairing is as true as the other one.
//
// --relative because a diagnostic's path is the project's, and git's is the
// repository's. They are the same string only when the project is the repository
// root; below it, an anchor of sub/migrations/00002_x.sql would disagree with the
// migrations/00002_x.sql that the two file rules produce for that same file, and
// an annotation carrying one of the two would land somewhere that does not exist.
func addedMigrations(root, base, dir string) ([]string, error) {
	out, err := git(root, "diff", "--name-status", "--relative", "-M",
		"--merge-base", base, "--", dir)
	if err != nil {
		return nil, err
	}

	var added []string
	for _, line := range lines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		switch fields[0][0] {
		case 'A':
			if _, ok := migcheck.Version(fields[1]); ok {
				added = append(added, fields[1])
			}
		case 'R':
			// R is `status<TAB>from<TAB>to`, and only a move between two
			// different numbers is this branch claiming one it did not have.
			if len(fields) < 3 {
				continue
			}
			to, ok := migcheck.Version(fields[2])
			if !ok {
				continue
			}
			if from, ok := migcheck.Version(fields[1]); !ok || from != to {
				added = append(added, fields[2])
			}
		}
	}

	out, err = git(root, "ls-files", "--others", "--exclude-standard", "--", dir)
	if err != nil {
		return nil, err
	}
	for _, name := range lines(out) {
		if _, ok := migcheck.Version(name); ok {
			added = append(added, name)
		}
	}
	return added, nil
}

// baseMigrations lists dir's files as the base ref has them.
//
// The tip, and not the merge base, which is the half of this that is easy to get
// backwards. The question the ordering rule asks is "what number will already be
// taken when this merges", and the answer is whatever is on the branch it merges
// into now — including every migration that landed there while this branch was
// being written. Ask the merge base instead and the check passes on exactly the
// case it exists for: two branches cut from the same commit, one merged, the
// other still numbered as though it had not been.
//
// A directory that does not exist on the base ref lists nothing and is not an
// error. That is the first migration in a new project, and it passes.
func baseMigrations(root, base, dir string) ([]string, error) {
	out, err := git(root, "ls-tree", "-r", "--name-only", base, "--", dir+"/")
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// git runs one git command in dir and returns its standard output.
//
// The directory is the project root and is not optional. `-C` does not chdir the
// process — it tells rig where to look for rig.yaml and nothing more — so a
// command run without this asks whatever repository the shell happened to be
// standing in, which in a pipeline is a plausible-looking answer about the wrong
// tree. It also settles the pathspec: git resolves a relative one against the
// current directory, so `migrations` means the project's migrations whether or
// not the project root is the repository root.
//
// The environment is stripped of the variables that name a repository, because
// those beat the directory and would undo the paragraph above. `git push`
// exports GIT_DIR to the hooks it runs, and a pre-push hook is a reasonable
// place to put this check — inherit it and rig asks about the repository the
// hook belongs to, from a process standing somewhere else. The failure is not
// even shaped like a wrong answer: git with a GIT_DIR it cannot reconcile falls
// back to `diff --no-index` and reports `--merge-base` as an unknown option.
//
// git writes the useful half of a failure to stderr and exec discards it, so it
// is put back into the error: "bad revision" is the whole diagnosis for a base
// ref nobody fetched, and "exit status 128" is not.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = withoutRepoEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if msg != "" {
				return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
			}
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// repoEnv names the environment variables that decide which repository git
// answers about, and therefore the ones [git] has to drop for cmd.Dir to mean
// anything. Everything else in the environment is left alone: git needs PATH to
// be found and HOME to read the configuration a project may depend on.
var repoEnv = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_COMMON_DIR",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
}

func withoutRepoEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(repoEnv, name) {
			out = append(out, kv)
		}
	}
	return out
}

func lines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
