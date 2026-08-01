package gen

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Status is what would happen, or did happen, to one file.
type Status string

const (
	// Added is a file that does not exist yet.
	Added Status = "add"
	// Changed is a generated file whose content differs.
	Changed Status = "change"
	// Unchanged is already correct.
	Unchanged Status = "ok"
	// Stale is a file rig wrote that no generator claims any more, left behind
	// by a renamed table or a removed generator.
	Stale Status = "stale"
	// Conflict is a generated file someone has edited by hand. Overwriting it
	// would throw that work away without asking.
	Conflict Status = "conflict"
	// Kept is a create-once file that already exists and is left alone.
	Kept Status = "keep"
)

// Delta is one file's worth of difference between what a generator produced and
// what is on disk.
type Delta struct {
	Generator string
	Path      string
	Status    Status
	// Reason explains a conflict.
	Reason string
	// content is what would be written; nil for a stale file.
	content []byte
	mode    WriteMode
	perm    fs.FileMode
}

// Diff compares generated artifacts against the filesystem.
//
// The manifest is what makes two of these statuses possible at all. Without a
// record of what rig wrote, a hand-edited generated file is indistinguishable
// from one rig has not caught up with, and a file left behind by a renamed
// table is indistinguishable from one somebody added on purpose.
func Diff(root string, results []Result, m *Manifest) ([]Delta, error) {
	var deltas []Delta
	claimed := map[string]bool{}

	for _, res := range results {
		for _, a := range res.Artifacts {
			rel := relSlash(root, a.Path)
			claimed[rel] = true

			d := Delta{
				Generator: res.Generator,
				Path:      a.Path,
				content:   a.Content,
				mode:      a.Mode,
				perm:      a.Perm,
			}

			existing, err := os.ReadFile(a.Path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				d.Status = Added

			case err != nil:
				return nil, fmt.Errorf("read %s: %w", a.Path, err)

			case a.Mode == CreateOnce:
				// It exists, so it is the developer's now. What the generator
				// would have written is irrelevant.
				d.Status = Kept

			case string(existing) == string(a.Content):
				d.Status = Unchanged

			default:
				d.Status = Changed
				// A generated file that has been edited is reported rather than
				// silently overwritten. The manifest is the only way to tell
				// the two cases apart.
				if prev, recorded := m.Lookup(rel); recorded && prev.SHA256 != Sum(existing) {
					d.Status = Conflict
					d.Reason = "edited by hand since rig wrote it"
				}
			}

			deltas = append(deltas, d)
		}
	}

	// Anything rig recorded writing that no generator claims any more.
	for _, e := range m.Files {
		if claimed[e.Path] {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(e.Path))
		if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
			continue // already gone
		}
		deltas = append(deltas, Delta{
			Generator: e.Generator,
			Path:      abs,
			Status:    Stale,
			Reason:    "no generator produces this any more",
		})
	}

	slices.SortFunc(deltas, func(a, b Delta) int { return cmp.Compare(a.Path, b.Path) })
	return deltas, nil
}

// WriteOptions tune how deltas are applied.
type WriteOptions struct {
	// Force overwrites files that were edited by hand.
	Force bool
	// Prune deletes files no generator claims any more.
	Prune bool
}

// Write applies the deltas and returns the manifest describing the result.
//
// A conflict stops the whole run before anything is written. Applying half a
// generation and then refusing the rest would leave the tree in a state neither
// the tool nor the developer can reason about.
func Write(root string, results []Result, deltas []Delta, opt WriteOptions) (*Manifest, error) {
	if !opt.Force {
		var conflicts []string
		for _, d := range deltas {
			if d.Status == Conflict {
				conflicts = append(conflicts, relSlash(root, d.Path))
			}
		}
		if len(conflicts) > 0 {
			return nil, fmt.Errorf(
				"refusing to overwrite %d hand-edited generated file(s):\n  %s\n\n"+
					"Generated files are rewritten on every run, so edits to them are lost. "+
					"Move the change into a file you own, or pass --force to discard it",
				len(conflicts), strings.Join(conflicts, "\n  "))
		}
	}

	next := NewManifest()
	for _, res := range results {
		next.Generators[res.Generator] = res.Version
	}

	for _, d := range deltas {
		switch d.Status {
		case Stale:
			if opt.Prune {
				if err := os.Remove(d.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("remove %s: %w", d.Path, err)
				}
				continue
			}
			// Not pruned, so it is still on disk and still rig's to remember.
			next.Files = append(next.Files, Entry{
				Path:      relSlash(root, d.Path),
				Generator: d.Generator,
				Mode:      Overwrite.String(),
				SHA256:    sumFile(d.Path),
			})

		case Kept:
			// Hand-owned now, but still recorded so it is not reported as new
			// on the next run.
			next.Files = append(next.Files, Entry{
				Path:      relSlash(root, d.Path),
				Generator: d.Generator,
				Mode:      CreateOnce.String(),
				SHA256:    sumFile(d.Path),
			})

		default:
			if err := writeArtifact(d); err != nil {
				return nil, err
			}
			next.Files = append(next.Files, Entry{
				Path:      relSlash(root, d.Path),
				Generator: d.Generator,
				Mode:      d.mode.String(),
				SHA256:    Sum(d.content),
			})
		}
	}

	return next, nil
}

func writeArtifact(d Delta) error {
	if err := os.MkdirAll(filepath.Dir(d.Path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(d.Path), err)
	}

	perm := d.perm
	if perm == 0 {
		perm = 0o644
	}
	if err := os.WriteFile(d.Path, d.content, perm); err != nil {
		return fmt.Errorf("write %s: %w", d.Path, err)
	}
	return nil
}

// sumFile digests a file already on disk, returning empty when it cannot be
// read — a missing digest only means the next run reports it as changed.
func sumFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return Sum(b)
}

func relSlash(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// NeedsWork reports whether any delta represents a difference, which is what
// `rig check` exits non-zero on.
func NeedsWork(deltas []Delta) bool {
	for _, d := range deltas {
		switch d.Status {
		case Unchanged, Kept:
		default:
			return true
		}
	}
	return false
}
