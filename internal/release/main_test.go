package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The version is rewritten textually, so the thing worth proving is that
// nothing else moves: a release that reindented three package.json files would
// bury its own one-line change.
func TestSetNPMVersion(t *testing.T) {
	t.Parallel()

	const before = `{
    "name": "@rig-ts/electric",
    "version": "0.1.0",
    "type": "module",
    "peerDependencies": {
        "@rig-ts/client": "workspace:^"
    },
    "devDependencies": {
        "typescript": "5.9.3"
    }
}
`
	const want = `{
    "name": "@rig-ts/electric",
    "version": "0.2.0",
    "type": "module",
    "peerDependencies": {
        "@rig-ts/client": "workspace:^"
    },
    "devDependencies": {
        "typescript": "5.9.3"
    }
}
`

	path := filepath.Join(t.TempDir(), "package.json")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setNPMVersion(path, "0.2.0"); err != nil {
		t.Fatalf("setNPMVersion: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("rewrote more than the version:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// A package.json with no version is a release that would otherwise publish
// whatever was there before.
func TestSetNPMVersionRefusesAFileWithout(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "package.json")
	if err := os.WriteFile(path, []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setNPMVersion(path, "0.2.0"); err == nil {
		t.Fatal("no error for a package.json with no version field")
	}
}

// The rule this proves is the one that cost a version. `git push origin --tags`
// sends all ten at once, GitHub creates no push event for a batch of more than
// three, and v0.2.0 landed ten correct tags that triggered nothing — no
// binaries, no GitHub release, nothing on npm. A published tag cannot be
// re-pushed, so the only way to get a build was v0.2.1.
func TestTagsToPushKeepsTheRootTagOnItsOwn(t *testing.T) {
	t.Parallel()

	mods := []mod{
		{Dir: ".", Path: modulePrefix},
		{Dir: "runtime", Path: modulePrefix + "/runtime"},
		{Dir: "auth", Path: modulePrefix + "/auth"},
	}

	root, siblings, err := tagsToPush(mods, "v0.3.0")
	if err != nil {
		t.Fatalf("tagsToPush: %v", err)
	}
	if root != "v0.3.0" {
		t.Errorf("root tag is %q, want v0.3.0 — this is the one release.yaml triggers on", root)
	}
	want := []string{"runtime/v0.3.0", "auth/v0.3.0"}
	if len(siblings) != len(want) {
		t.Fatalf("siblings are %v, want %v", siblings, want)
	}
	for i, w := range want {
		if siblings[i] != w {
			t.Errorf("sibling %d is %q, want %q", i, siblings[i], w)
		}
	}
	for _, s := range siblings {
		if s == root {
			t.Errorf("the root tag is in the batch push, which is what stops it triggering anything")
		}
	}
}

// go.work naming no root module would leave a release with nine tags and
// nothing to trigger the workflow — worth failing on rather than pushing.
func TestTagsToPushNeedsARootModule(t *testing.T) {
	t.Parallel()

	_, _, err := tagsToPush([]mod{{Dir: "runtime", Path: modulePrefix + "/runtime"}}, "v0.3.0")
	if err == nil {
		t.Fatal("no error when go.work names no root module")
	}
}
