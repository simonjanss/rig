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
