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
// It does not tidy, and that is not an oversight: `go mod tidy` is a
// single-module operation that resolves from the proxy and ignores the
// workspace, so between the rewrite and the tag it would be asked for a version
// that does not exist yet and would fail. The order is rewrite, tag, push the
// tags, and only then tidy — which is what `make release` prints at the end.
//
// Pushing and verifying are separate modes rather than separate commands,
// because both need the same list of modules this reads out of go.work:
//
//	--push     the submodule tags in one push, then the root tag by itself.
//	           GitHub creates no push event for a batch of more than three
//	           tags, so pushing all ten fires no release workflow at all.
//	--verify   whether the tag produced a release: the tags on origin, the run
//	           it was supposed to trigger, the GitHub release and its archives,
//	           npm, and a real `go install`. A workflow that never started looks
//	           exactly like one that succeeded, and nothing else here would
//	           notice.
//
//	go run ./internal/release v0.1.0 [--dry-run | --push | --verify]
package main

import (
	"encoding/json"
	"errors"
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

// releaseWorkflow is the workflow a root tag is supposed to trigger, and the
// one `--verify` asks about by name.
const releaseWorkflow = "release.yaml"

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

  make release-push VERSION=v0.1.0   the submodule tags, then the root tag alone
  make tidy && git commit            the hashes, which only now resolve
  make check                         now that ` + "`make deps`" + ` can tidy against them
  git push origin main               last, so a failure costs a tag, not a release
  make release-verify VERSION=v0.1.0 what the tag was supposed to produce

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
	pushing := false
	verifying := false
	for _, a := range args {
		switch {
		case a == "--dry-run":
			dryRun = true
		case a == "--push":
			pushing = true
		case a == "--verify":
			verifying = true
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

	// The flags, before anything is read or run — a combination that means two
	// things should not get as far as doing one of them.
	if pushing && verifying {
		return fmt.Errorf("--push and --verify are separate steps: push, tidy, check, push main, then verify")
	}
	// --dry-run is what somebody cautious types before an irreversible command,
	// and --push is the irreversible one: a tag the proxy has fetched cannot be
	// moved. Ignoring the flag rather than refusing it would make the careful
	// spelling the dangerous one.
	if dryRun && (pushing || verifying) {
		return fmt.Errorf("--dry-run previews cutting a release; --push and --verify have nothing to preview")
	}

	mods, err := modules()
	if err != nil {
		return err
	}

	// Both of these run after a release rather than making one, so none of the
	// guards below apply: the tags they are asking about are exactly the ones
	// `checkTags` refuses to let a release overwrite, and verifying is a
	// read-only question about a version that is already published.
	if pushing {
		return pushTags(mods, version)
	}
	if verifying {
		return verifyRelease(mods, version)
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

  make release-push VERSION=%s
  make tidy && git commit -am "go.sum: record the %s sibling hashes"
  make check
  git push origin main
  make release-verify VERSION=%s

A tag the proxy has seen cannot be changed. If `+"`make check`"+` fails after the
push, release the fix as the next patch version rather than moving %s.
`, version, len(mods), version, version, version, version)
	return nil
}

// pushTags pushes what `make release` created: the submodule tags in one push,
// and then the root tag by itself.
//
// The order is the whole point, and it is not tidiness. GitHub creates no push
// event for a batch of more than three tags, and `release.yaml` triggers on
// `v*` — so `git push origin --tags`, which is what this command used to print,
// sends all ten at once and fires nothing. The tags land, the release looks
// done, and there are no binaries, no GitHub release and nothing on npm. That
// is what happened to v0.2.0, which had to be superseded by v0.2.1 to get a
// build.
//
// Pushing the root tag alone, last, makes the trigger a single-ref event no
// batching rule can swallow. Last rather than first because the nine are what
// the root's go.mod requires: by the time anything reacts to `v*`, the versions
// it resolves are already there.
func pushTags(mods []mod, version string) error {
	root, siblings, err := tagsToPush(mods, version)
	if err != nil {
		return err
	}
	for _, tag := range append(siblings, root) {
		if err := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/tags/"+tag).Run(); err != nil {
			return fmt.Errorf("no tag %s — run `make release VERSION=%s` first", tag, version)
		}
	}

	fmt.Printf("pushing %d submodule tags\n", len(siblings))
	if err := git("git", append([]string{"push", "origin"}, siblings...)...); err != nil {
		return err
	}

	fmt.Printf("\npushing %s on its own — this is what triggers release.yaml\n", root)
	if err := git("git", "push", "origin", root); err != nil {
		return err
	}

	fmt.Printf(`
Pushed. The release workflow runs on %s; `+"`make release-verify VERSION=%s`"+` says
whether it produced anything.

Next: `+"`make tidy`"+` and commit the hashes — they resolve now and did not before.
`, root, version)
	return nil
}

// tagsToPush separates the tag that triggers the release workflow from the ones
// that only have to exist before it does.
//
// Kept apart from the pushing so the rule that matters can be tested without a
// remote: the root tag is pushed by itself, and it is pushed second.
func tagsToPush(mods []mod, version string) (root string, siblings []string, err error) {
	for _, m := range mods {
		if m.Dir == "." {
			root = m.Tag(version)
			continue
		}
		siblings = append(siblings, m.Tag(version))
	}
	if root == "" {
		return "", nil, fmt.Errorf("go.work names no root module, so there is no tag to trigger the release workflow")
	}
	return root, siblings, nil
}

// verifyRelease answers the question the release procedure never asked: did the
// tag produce anything?
//
// Everything up to `git push origin main` proves the repository is consistent,
// and none of it proves a release exists. What publishes rig is a GitHub
// Actions run reacting to a tag, and a run that never started looks exactly
// like a run that succeeded — v0.2.0 has ten correct tags, is installable from
// the proxy, and has no binaries, no GitHub release and nothing on npm,
// because nothing ever triggered. This is what would have said so in seconds.
//
// It checks the three surfaces a release has, because they fail separately: the
// proxy (which the tags alone satisfy), the GitHub release (goreleaser), and
// npm (trusted publishing, which was misconfigured for v0.1.0 and silently
// worked around by hand). Every failure is collected rather than returned, so
// one run names everything that is wrong.
//
// And it asks first whether the run exists at all, because an unfinished one
// leaves those three surfaces looking exactly like a tag that triggered
// nothing. Waiting is the fix for the one and a new version is the fix for the
// other, so a report that cannot tell them apart is worse than none.
func verifyRelease(mods []mod, version string) error {
	var root mod
	for _, m := range mods {
		if m.Dir == "." {
			root = m
		}
	}
	if root.Path == "" {
		return fmt.Errorf("go.work names no root module")
	}

	var failed []string
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		failed = append(failed, msg)
		fmt.Printf("  FAIL  %s\n", msg)
	}
	ok := func(format string, a ...any) {
		fmt.Printf("  ok    %s\n", fmt.Sprintf(format, a...))
	}

	fmt.Printf("tags on origin\n")
	remote, err := output("git", "ls-remote", "--tags", "origin")
	if err != nil {
		return err
	}
	for _, m := range mods {
		if strings.Contains(string(remote), "refs/tags/"+m.Tag(version)+"\n") {
			ok("%s", m.Tag(version))
		} else {
			fail("%s is not on origin", m.Tag(version))
		}
	}

	// Asked before the surfaces it explains, because it is the difference
	// between the two ways they come up empty: a tag that triggered nothing,
	// and a run that is six minutes into a job that publishes at the end.
	fmt.Printf("\nthe release workflow\n")
	running := false
	switch status, conclusion, err := workflowRun(version); {
	case err != nil:
		fail("asking whether %s ran for %s — %v", releaseWorkflow, version, err)
	case status == "":
		fail("no run of %s for %s — the tag triggered nothing", releaseWorkflow, version)
	case status != "completed":
		running = true
		fmt.Printf("  wait  a run is %s — everything below may be failing only because of that\n", status)
	case conclusion != "success":
		fail("the run for %s %s", version, conclusion)
	default:
		ok("%s succeeded", releaseWorkflow)
	}

	fmt.Printf("\nGitHub release\n")
	verifyGitHubRelease(version, ok, fail)

	fmt.Printf("\nnpm\n")
	want := strings.TrimPrefix(version, "v")
	for _, p := range tsPackages {
		name := "@rig-ts/" + filepath.Base(filepath.Dir(p))
		out, err := quiet("npm", "view", name+"@"+want, "version")
		switch got := strings.TrimSpace(string(out)); {
		case err != nil:
			fail("%s is not published at %s — %v", name, want, err)
		case got != want:
			fail("%s@%s answers %q", name, want, got)
		default:
			ok("%s@%s", name, want)
		}
	}

	fmt.Printf("\ninstallable from the proxy\n")
	verifyInstall(root.Path, version, ok, fail)

	fmt.Println()
	if len(failed) > 0 {
		if running {
			return fmt.Errorf("%d checks failed while the run for %s is still going — "+
				"nothing here is settled until it finishes, so run this again rather than acting on it",
				len(failed), version)
		}
		return fmt.Errorf("%d checks failed — %s is not fully released; "+
			"a missing GitHub release or npm package is fixed by the next version, never by moving this tag",
			len(failed), version)
	}
	if running {
		fmt.Printf("%s looks right so far, but its run has not finished — check again when it has.\n", version)
		return nil
	}
	fmt.Printf("%s is released: tags, binaries and npm all agree.\n", version)
	return nil
}

// workflowRun reports the state of the most recent release.yaml run for a tag:
// its status, and its conclusion once it has one. No run at all is no error —
// it is an answer, and the empty status says so.
//
// This is the question that separates the two ways a release comes up empty.
// v0.2.0 had no run because a ten-tag push produces no push event, and the fix
// for that is another version. A run six minutes into a twenty-minute
// goreleaser job has published nothing either, and the fix for that is to wait.
// Everything below reports them identically, and the advice for one is the
// worst possible advice for the other.
//
// `gh run list --branch` takes a tag: a run triggered by a tag push records the
// tag as its head branch.
func workflowRun(version string) (status, conclusion string, err error) {
	out, err := quiet("gh", "run", "list",
		"--workflow", releaseWorkflow, "--branch", version, "--limit", "1",
		"--json", "status,conclusion")
	if err != nil {
		return "", "", err
	}
	var runs []struct{ Status, Conclusion string }
	if err := json.Unmarshal(out, &runs); err != nil {
		return "", "", fmt.Errorf("reading the run list: %w", err)
	}
	if len(runs) == 0 {
		return "", "", nil
	}
	return runs[0].Status, runs[0].Conclusion, nil
}

// verifyGitHubRelease checks what goreleaser was supposed to leave behind.
//
// The assets are checked against checksums.txt rather than against a list
// written here, because the archive names are a compatibility contract —
// `.github/actions/setup-rig` downloads `rig_${version}_${os}_${arch}.tar.gz`
// by name — and a second copy of that template would be a second thing to keep
// right. checksums.txt names exactly what shipped, so asking whether every file
// in it is an asset asks the contract about itself.
func verifyGitHubRelease(version string, ok, fail func(string, ...any)) {
	out, err := quiet("gh", "release", "view", version, "--json", "isDraft,isPrerelease,assets")
	if err != nil {
		// With the error rather than a diagnosis of it: "release not found" is
		// the ordinary answer here, and "gh: not logged in" reaches the same
		// branch while meaning nothing about the release at all.
		fail("no GitHub release for %s — %v", version, err)
		return
	}
	var rel struct {
		IsDraft      bool
		IsPrerelease bool
		Assets       []struct{ Name string }
	}
	if err := json.Unmarshal(out, &rel); err != nil {
		fail("reading the release: %v", err)
		return
	}
	if rel.IsDraft {
		fail("the release is a draft, so nothing can download it")
	}

	assets := map[string]bool{}
	for _, a := range rel.Assets {
		assets[a.Name] = true
	}
	if !assets["checksums.txt"] {
		fail("the release has no checksums.txt, so there is nothing to verify an archive against")
		return
	}

	dir, err := os.MkdirTemp("", "rig-release-verify")
	if err != nil {
		fail("%v", err)
		return
	}
	defer os.RemoveAll(dir)
	if err := git("gh", "release", "download", version, "--pattern", "checksums.txt", "--dir", dir); err != nil {
		fail("downloading checksums.txt: %v", err)
		return
	}
	sums, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		fail("%v", err)
		return
	}

	named := 0
	for line := range strings.SplitSeq(string(sums), "\n") {
		// `<sha256>  <filename>`, which is what shasum -c reads.
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		named++
		if assets[fields[1]] {
			ok("%s", fields[1])
		} else {
			fail("checksums.txt names %s, which is not on the release", fields[1])
		}
	}
	if named == 0 {
		fail("checksums.txt names no files")
	}

	// A prerelease is meant not to be `latest`: it is the rehearsal, and
	// `setup-rig` resolves `latest` through this endpoint.
	latest, err := output("gh", "api", "repos/{owner}/{repo}/releases/latest", "--jq", ".tag_name")
	if err != nil {
		fail("asking which release is latest: %v", err)
		return
	}
	switch got := strings.TrimSpace(string(latest)); {
	case rel.IsPrerelease && got == version:
		fail("%s is a prerelease and is also `latest`, which is what setup-rig installs", version)
	case rel.IsPrerelease:
		ok("a prerelease, and `latest` is still %s", got)
	case got != version:
		fail("`latest` is %s, not %s — setup-rig would install the older one", got, version)
	default:
		ok("`latest` is %s", version)
	}
}

// verifyInstall installs the binary the way somebody outside this repository
// would, into a directory of its own, and asks it what version it is.
//
// It is the local half of the `consumable` job, and worth having twice: that
// job runs on the tag, so a release whose workflow never started never runs it
// either. The version has to be the tag exactly, because the documented way to
// pin the libraries is `go get .../runtime@$(rig version)`.
func verifyInstall(rootPath, version string, ok, fail func(string, ...any)) {
	dir, err := os.MkdirTemp("", "rig-release-install")
	if err != nil {
		fail("%v", err)
		return
	}
	defer os.RemoveAll(dir)

	install := exec.Command("go", "install", rootPath+"/cmd/rig@"+version)
	install.Env = append(os.Environ(), "GOBIN="+dir, "GOFLAGS=")
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		fail("go install %s/cmd/rig@%s: %v", rootPath, version, err)
		return
	}

	out, err := output(filepath.Join(dir, "rig"), "version")
	if err != nil {
		fail("running the installed binary: %v", err)
		return
	}
	if got := strings.TrimSpace(string(out)); got != version {
		fail("the installed binary says %s, not %s", got, version)
		return
	}
	ok("go install %s/cmd/rig@%s, and it says %s", rootPath, version, version)
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

// quiet is output for a question whose answer may well be no.
//
// `npm view` on an unpublished version prints nine lines about a 404, and
// `gh release view` on a tag with no release prints its own — which is the
// ordinary case during a verify, and letting it through buries the report it
// is part of under the noise of the thing it just found.
//
// Captured rather than discarded, though, and the first line of it becomes the
// error. "release not found" and "gh: not logged in" reach the same branch of
// every caller here, and a verify that reported the second as the first would
// be saying a release failed because nobody ran `gh auth login`.
func quiet(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).Output()
	if err == nil {
		return out, nil
	}
	// The line as the command wrote it, without a prefix naming the command
	// again: every caller here already says what it was asking, and both tools
	// label their own errors anyway ("npm error code E404"). A failure that is
	// not an exit — no such binary — describes itself.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if line := firstLine(exit.Stderr); line != "" {
			return nil, errors.New(line)
		}
	}
	return nil, err
}

// firstLine is the first non-blank line of a command's stderr, which for both
// `gh` and `npm` is the sentence saying what went wrong; the rest is registry
// URLs and the path to a log file.
func firstLine(b []byte) string {
	for line := range strings.SplitSeq(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}
