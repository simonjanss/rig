// Command release prepares a lockstep release of every published rig module.
//
// rig is ten modules in one repository, and they are not independent: the
// binary links auth, files, migrate, notify, presence and runtime, and it
// generates code that imports them. A rig that generates against a runtime API
// released under a different number is a rig nobody can use, so there is one
// version for all ten, and one commit that sets it everywhere.
//
// What this does, in order:
//
//  1. reads go.work for the modules to release — the use directives that are
//     not examples, so a new module is included by being buildable rather than
//     by being remembered here;
//  2. refuses to continue if any of them still replaces a sibling, because a
//     replace directive is exactly what a published module may not carry;
//  3. rewrites every intra-repository requirement to the new version;
//  4. commits if that rewrote anything, and tags either way: vX.Y.Z for the
//     root, <dir>/vX.Y.Z for the rest. A version already in place is not a
//     version already released — the tags are what release it.
//
// It does not push and it does not tidy. Not tidying is not an oversight: `go
// mod tidy` is a single-module operation that resolves from the proxy and
// ignores the workspace, so between the rewrite and the tag it would be asked
// for a version that does not exist yet and would fail. The order is rewrite,
// tag, push the tags, and only then tidy — which is what `make release` prints
// at the end.
//
//	go run ./internal/release v0.1.0 [--dry-run]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// tsPackages are the npm packages released alongside the Go modules, and they
// are released alongside them for the same reason the modules are released
// with each other: rig generates TypeScript that imports @rig-ts/client, so
// the generator and the package it generates imports of are one version.
//
// npm has no leading v, so the version written here is the tag without it.
var tsPackages = []string{
	"ts/packages/client/package.json",
	"ts/packages/electric/package.json",
	"ts/packages/presence/package.json",
}

// npmVersion matches the version field of a package.json — the first one, which
// is the package's own; a dependency's version is nested and indented further.
var npmVersion = regexp.MustCompile(`(?m)^(\s*"version":\s*")[^"]*(",?)$`)

// modulePrefix is the import path every module in this repository shares.
const modulePrefix = "github.com/simonjanss/rig"

// defaultBranch is the only branch a release may be cut from.
const defaultBranch = "main"

// usage is printed when no version is given, and it is the long version on
// purpose: this is where somebody who is about to release rig without having
// read AGENTS.md ends up, and a release is the one thing here that cannot be
// taken back.
const usage = `usage: make release VERSION=v0.1.0     (or: go run ./internal/release <version>)
       make release-dry VERSION=v0.1.0

Cuts one version across all ten published Go modules and all three npm
packages: rewrites every intra-repository requirement, sets the package.json
versions, commits, and tags.

DO NOT RUN THIS unless you were asked to cut this release, now. A tag the module
proxy has fetched is permanent — it cannot be moved, deleted or fixed, only
superseded by a higher number. Propose a version and wait.

Which number, on v0:
  minor (v0.3.1 -> v0.4.0)   a signature moved, a config key changed meaning, or
                             generated output stopped compiling against the last
                             release
  patch (v0.3.1 -> v0.3.2)   everything else

  A prerelease (v0.4.0-rc.1) is not what @latest selects, so it rehearses the
  whole mechanism for free. Use one when the release is the risky part.

Then, in this order — the tags go before the branch:

  git push origin --tags     nothing resolves the new versions until they exist
  make check                 now that ` + "`make deps`" + ` can tidy against them
  git push origin main       last, so a failure above costs a tag, not a release

Releasing in AGENTS.md has the reasoning.
`

// semver is the shape of a version this accepts: a release, or a prerelease of
// one. A prerelease is how a first release is rehearsed — the proxy caches a
// tag forever, so a bad v0.1.0 can only be superseded, never fixed, and
// v0.1.0-rc.1 is not what @latest selects.
var semver = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// mod is one module of the repository, as `go mod edit -json` describes it.
type mod struct {
	// Dir is relative to the repository root: "." for the root module.
	Dir string
	// Path is the module path.
	Path string
	// Requires are the sibling modules it depends on, by module path.
	Requires []string
	// Replaces are the sibling modules it replaces, which must be none.
	Replaces []string
}

// Tag is the tag that releases this module at v. Go's rule for a module in a
// subdirectory is the subdirectory, then the version.
func (m mod) Tag(v string) string {
	if m.Dir == "." {
		return v
	}
	return m.Dir + "/" + v
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "release: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var version string
	dryRun := false
	for _, a := range args {
		switch {
		case a == "--dry-run":
			dryRun = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %s", a)
		case version != "":
			return fmt.Errorf("more than one version given: %s and %s", version, a)
		default:
			version = a
		}
	}
	if version == "" {
		fmt.Print(usage)
		return fmt.Errorf("no version given")
	}
	if !semver.MatchString(version) {
		return fmt.Errorf("%q is not a version: want vMAJOR.MINOR.PATCH, optionally with a prerelease suffix", version)
	}

	mods, err := modules()
	if err != nil {
		return err
	}

	// A replace left in a published module is the failure this whole scheme
	// exists to prevent: `go install pkg@version` refuses a module whose go.mod
	// has one, and a consumer resolving a sibling gets a version that was never
	// published. Better to stop here than to publish it.
	for _, m := range mods {
		if len(m.Replaces) > 0 {
			return fmt.Errorf("%s replaces %s — a published module may not, "+
				"local resolution is go.work's job",
				m.Dir+"/go.mod", strings.Join(m.Replaces, ", "))
		}
	}

	// Where a release may be cut from, checked before anything is written. A
	// tag on a feature branch is a version of rig that nothing on main
	// contains, and it cannot be withdrawn once the proxy has it. In a dry run
	// this is a warning: previewing a release from a branch is how you find out
	// what one would do.
	if err := checkBranch(); err != nil {
		if !dryRun {
			return err
		}
		fmt.Printf("warning: %v\n\n", err)
	}

	if err := checkTags(mods, version); err != nil {
		return err
	}
	// Only the real thing needs a clean tree: what makes the commit is `git
	// commit -am`, which would carry anything else uncommitted into the
	// release. A dry run writes nothing, and refusing to preview a release
	// until the tree is clean would make the preview useless.
	if !dryRun {
		if err := checkClean(); err != nil {
			return err
		}
	}

	for _, m := range mods {
		for _, req := range m.Requires {
			fmt.Printf("%-12s require %s@%s\n", m.Dir, strings.TrimPrefix(req, modulePrefix+"/"), version)
			if dryRun {
				continue
			}
			if err := git("go", "mod", "edit", "-require="+req+"@"+version, filepath.Join(m.Dir, "go.mod")); err != nil {
				return err
			}
		}
	}

	for _, p := range tsPackages {
		fmt.Printf("%-12s version %s\n", filepath.Base(filepath.Dir(p)), strings.TrimPrefix(version, "v"))
		if dryRun {
			continue
		}
		if err := setNPMVersion(p, strings.TrimPrefix(version, "v")); err != nil {
			return err
		}
	}

	fmt.Println()
	for _, m := range mods {
		fmt.Printf("%-12s tag %s\n", m.Dir, m.Tag(version))
	}

	if dryRun {
		fmt.Println("\n--dry-run: nothing was written")
		return nil
	}

	// Nothing to commit is not nothing to do. The tree was clean going in, so
	// the only changes here are the ones just written — and a rewrite that
	// wrote none means the version is already what it should be. That is the
	// ordinary state of a first release, whose versions were set in the commit
	// that added this command, and of a second attempt after one failed
	// between the commit and the tags. `git commit -am` fails on an empty
	// commit, so asking it for one would abort the release with every tag
	// still uncreated, which is the one outcome worth avoiding here.
	dirty, err := changed()
	if err != nil {
		return err
	}
	if dirty {
		if err := git("git", "commit", "-am", "release "+version); err != nil {
			return err
		}
	} else {
		fmt.Printf("\nnothing to rewrite — %s is already what the tree says; tagging HEAD\n", version)
	}
	for _, m := range mods {
		if err := git("git", "tag", "-a", m.Tag(version), "-m", m.Path+" "+version); err != nil {
			return err
		}
	}

	fmt.Printf(`
Tagged %s across %d modules. Next, in this order:

  git push origin --tags     the versions have to exist before anything resolves them
  make check                 now that `+"`make deps`"+` can tidy against them
  git push origin main       last, so a failure above costs a tag and not a release

A tag the proxy has seen cannot be changed. If `+"`make check`"+` fails after the
push, release the fix as the next patch version rather than moving %s.
`, version, len(mods), version)
	return nil
}

// modules reads go.work and returns the modules a release covers: the ones this
// repository publishes, which is every use directive that is not an example.
func modules() ([]mod, error) {
	out, err := output("go", "work", "edit", "-json")
	if err != nil {
		return nil, err
	}
	var work struct {
		Use []struct{ DiskPath string }
	}
	if err := json.Unmarshal(out, &work); err != nil {
		return nil, fmt.Errorf("reading go.work: %w", err)
	}

	var mods []mod
	for _, u := range work.Use {
		dir := filepath.Clean(u.DiskPath)
		// The examples are in the workspace so that they compile against the
		// working tree, and they are not published. Their replace directives
		// are correct and stay.
		if dir == "examples" || strings.HasPrefix(dir, "examples"+string(filepath.Separator)) {
			continue
		}
		m, err := readMod(dir)
		if err != nil {
			return nil, err
		}
		mods = append(mods, m)
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("go.work names no publishable module")
	}
	return mods, nil
}

// readMod describes one module's go.mod: its path, the siblings it requires,
// and the siblings it replaces.
func readMod(dir string) (mod, error) {
	out, err := output("go", "mod", "edit", "-json", filepath.Join(dir, "go.mod"))
	if err != nil {
		return mod{}, err
	}
	var f struct {
		Module  struct{ Path string }
		Require []struct{ Path string }
		Replace []struct {
			Old struct{ Path string }
		}
	}
	if err := json.Unmarshal(out, &f); err != nil {
		return mod{}, fmt.Errorf("reading %s/go.mod: %w", dir, err)
	}

	m := mod{Dir: dir, Path: f.Module.Path}
	for _, r := range f.Require {
		if sibling(r.Path) {
			m.Requires = append(m.Requires, r.Path)
		}
	}
	for _, r := range f.Replace {
		if sibling(r.Old.Path) {
			m.Replaces = append(m.Replaces, r.Old.Path)
		}
	}
	return m, nil
}

// sibling reports whether path names another module of this repository.
func sibling(path string) bool {
	return strings.HasPrefix(path, modulePrefix+"/")
}

// checkTags fails if any tag this release would create already exists. Half a
// release is worse than none: the modules that did get tagged are published,
// and the ones that did not are not.
func checkTags(mods []mod, version string) error {
	for _, m := range mods {
		if err := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/tags/"+m.Tag(version)).Run(); err == nil {
			return fmt.Errorf("tag %s already exists — %s has been released", m.Tag(version), version)
		}
	}
	return nil
}

// checkBranch fails unless this is main, and main as the remote last saw it.
//
// Both halves matter, and for different reasons. A tag off a feature branch
// publishes code that main does not contain, and the proxy keeps it forever. A
// tag off a stale main publishes an old tree under a new number, which is worse
// than either: it looks like a release and is a revert.
func checkBranch() error {
	out, err := output("git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch := strings.TrimSpace(string(out)); branch != defaultBranch {
		return fmt.Errorf("on %s, and a release is cut from %s — merge first", branch, defaultBranch)
	}

	remote := "origin/" + defaultBranch
	if err := exec.Command("git", "rev-parse", "--verify", "--quiet", remote).Run(); err != nil {
		return fmt.Errorf("no %s to compare against — run `git fetch origin`", remote)
	}
	if err := exec.Command("git", "merge-base", "--is-ancestor", remote, "HEAD").Run(); err != nil {
		return fmt.Errorf("HEAD is behind %s — this would release an older tree under a newer number; "+
			"run `git pull` first", remote)
	}
	return nil
}

// checkClean fails on a dirty tree, because the commit this makes is `git
// commit -am` and would carry whatever else is uncommitted into the release.
func checkClean() error {
	out, err := output("git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return fmt.Errorf("the working tree is not clean:\n%s", out)
	}
	return nil
}

// changed reports whether there is anything to commit. Called after the
// rewrite and only there, where — because [checkClean] passed first — the
// answer is exactly whether the rewrite wrote anything.
func changed() (bool, error) {
	out, err := output("git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// setNPMVersion rewrites a package.json's own version in place.
//
// A textual edit rather than a decode and re-encode, because re-encoding would
// reorder and reindent a file nobody asked it to touch, and the diff a release
// leaves should be the version and nothing else.
func setNPMVersion(path, version string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	loc := npmVersion.FindSubmatchIndex(b)
	if loc == nil {
		return fmt.Errorf("%s has no version field", path)
	}
	out := append([]byte{}, b[:loc[0]]...)
	out = append(out, b[loc[2]:loc[3]]...)
	out = append(out, version...)
	out = append(out, b[loc[4]:loc[5]]...)
	out = append(out, b[loc[1]:]...)
	return os.WriteFile(path, out, 0o644)
}

// git runs a command, letting its output through, and fails with what it said.
func git(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// output runs a command and returns its standard output.
func output(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}
