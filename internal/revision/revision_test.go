package revision_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/simonjanss/rig/internal/revision"
)

func date(s string) time.Time {
	t, err := time.Parse(revision.Layout, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestResolveKeepsTheDateWhileTheSurfaceHoldsStill(t *testing.T) {
	t.Parallel()

	prev := revision.File{Revision: "2026-03-01", Hash: "sha256:aaa"}

	got, moved := revision.Resolve(prev, "sha256:aaa", date("2026-08-18"))
	if moved {
		t.Error("an unchanged hash reported a move")
	}
	if got != prev {
		t.Errorf("resolved to %+v, want the recorded %+v", got, prev)
	}
}

func TestResolveMovesWhenTheSurfaceDoes(t *testing.T) {
	t.Parallel()

	prev := revision.File{Revision: "2026-03-01", Hash: "sha256:aaa"}

	got, moved := revision.Resolve(prev, "sha256:bbb", date("2026-08-18"))
	if !moved {
		t.Error("a changed hash did not report a move")
	}
	if got.Revision != "2026-08-18" {
		t.Errorf("revision = %q, want today", got.Revision)
	}
	if got.Hash != "sha256:bbb" {
		t.Errorf("hash = %q, want the new one", got.Hash)
	}
}

func TestResolveStampsAProjectThatHasNoRecord(t *testing.T) {
	t.Parallel()

	got, moved := revision.Resolve(revision.File{}, "sha256:aaa", date("2026-08-18"))
	if !moved {
		t.Error("a project with no record did not report a move")
	}
	if got.Revision != "2026-08-18" {
		t.Errorf("revision = %q, want today", got.Revision)
	}
}

// A hash that matches but a date that does not exist is a half-written record,
// and half a record is worth nothing: the date is the thing clients carry.
func TestResolveStampsAMatchingHashWithNoDate(t *testing.T) {
	t.Parallel()

	got, moved := revision.Resolve(revision.File{Hash: "sha256:aaa"}, "sha256:aaa", date("2026-08-18"))
	if !moved {
		t.Error("a record with no date did not report a move")
	}
	if got.Revision != "2026-08-18" {
		t.Errorf("revision = %q, want today", got.Revision)
	}
}

// The clock is UTC because the file is committed: two people generating on the
// same day either side of a date line should not disagree about what day it is.
func TestResolveIsUTC(t *testing.T) {
	t.Parallel()

	// Late evening in Auckland, which is still the previous day in UTC.
	nz := time.FixedZone("NZST", 12*3600)
	local := time.Date(2026, 8, 19, 6, 0, 0, 0, nz)

	got, _ := revision.Resolve(revision.File{}, "sha256:aaa", local)
	if got.Revision != "2026-08-18" {
		t.Errorf("revision = %q, want the UTC date", got.Revision)
	}
}

func TestLoadOfAMissingFileIsNotAnError(t *testing.T) {
	t.Parallel()

	got, err := revision.Load(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != (revision.File{}) {
		t.Errorf("loaded %+v from an empty directory", got)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	want := revision.File{Revision: "2026-08-18", Hash: "sha256:aaa"}
	if err := want.Save(root); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := revision.Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %+v, want %+v", got, want)
	}

	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(revision.Name)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	const wantJSON = "{\n  \"revision\": \"2026-08-18\",\n  \"hash\": \"sha256:aaa\"\n}\n"
	if string(raw) != wantJSON {
		t.Errorf("wrote %q, want %q", raw, wantJSON)
	}
}

// Forgetting a corrupt record would stamp today over a date that is already in
// clients that have shipped, so it is reported instead.
func TestLoadReportsACorruptRecord(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(revision.Name)), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := revision.Load(root); err == nil {
		t.Fatal("expected an error for a corrupt record")
	}
}
