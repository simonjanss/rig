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

// DiffOptions scope what a comparison is entitled to call stale.
type DiffOptions struct {
	// Partial says some of the configured generators were held back, as --only
	// does.
	//
	// Staleness is relative to the run. A generator that did not run still owns
	// every file it wrote, and calling those leftovers would turn a narrow
	// regeneration into a proposal to delete most of the project. Which
	// generators ran is read from the results; that a generator was held back
	// is the one thing the results cannot say, because a generator nobody asked
	// leaves nothing behind to be missing from.
	Partial bool
}

// Diff compares generated artifacts against the filesystem.
//
// Three things can be wrong with a file, and they are found three ways. What a
// generator produces now is compared against what is on disk. What the manifest
// records is what makes a hand edit distinguishable from output rig has not
// caught up with. And what is on disk under rig's own naming — see [Orphans] —
// is what makes a leftover visible in a checkout that has no manifest, which is
// every checkout CI makes.
func Diff(root string, results []Result, m *Manifest, opt DiffOptions) ([]Delta, error) {
	var deltas []Delta
	claimed := map[string]bool{}
	ran := map[string]bool{}

	for _, res := range results {
		ran[res.Generator] = true

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

	// Anything rig recorded writing that no generator claims any more. A
	// generator held back by --only claims nothing, so its record is left alone
	// rather than read as a shelf of leftovers; one that has been deleted from
	// rig.yaml is a different thing, and what it wrote is a leftover like any
	// other.
	reported := map[string]bool{}
	for _, e := range m.Files {
		if claimed[e.Path] || (opt.Partial && !ran[e.Generator]) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(e.Path))
		if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
			continue // already gone
		}
		reported[e.Path] = true
		deltas = append(deltas, Delta{
			Generator: e.Generator,
			Path:      abs,
			Status:    Stale,
			Reason:    "no generator produces this any more",
		})
	}

	// And anything on disk that rig wrote, whether or not there is a manifest
	// left to say so. The manifest is the only thing that can attribute such a
	// file to a generator, so one found this way carries no generator name.
	if !opt.Partial {
		orphans, err := Orphans(root, claimed)
		if err != nil {
			return nil, err
		}
		for _, d := range orphans {
			if !reported[relSlash(root, d.Path)] {
				deltas = append(deltas, d)
			}
		}
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
	// Previous is the manifest this run started from, if there was one.
	//
	// The new manifest is built from this run's deltas, and a run narrowed by
	// --only has no deltas for the generators it held back. Without their old
	// entries carried across, a narrow regeneration would cost every other
	// generator's files the one record that tells a hand edit from output rig
	// has not caught up with.
	Previous *Manifest
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
	ran := map[string]bool{}
	for _, res := range results {
		next.Generators[res.Generator] = res.Version
		ran[res.Generator] = true
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
			// A file found by scanning has no generator to attribute it to, and
			// an entry naming none would claim rig wrote it on the strength of
			// its name. The scan finds it again next run either way.
			if d.Generator == "" {
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

	carryForward(root, next, opt.Previous, ran)
	return next, nil
}

// carryForward copies the record of the generators that did not run into the
// new manifest.
//
// Staleness is relative to the run — see [DiffOptions] — and so is the record:
// a file this run had no opinion about keeps the one it had. A file that is no
// longer on disk is the exception, since there is nothing left to remember.
func carryForward(root string, next, prev *Manifest, ran map[string]bool) {
	if prev == nil {
		return
	}

	recorded := make(map[string]bool, len(next.Files))
	for _, e := range next.Files {
		recorded[e.Path] = true
	}

	for _, e := range prev.Files {
		if ran[e.Generator] || recorded[e.Path] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(e.Path))); err != nil {
			continue
		}
		next.Files = append(next.Files, e)

		// The version that wrote them comes along, since it is what the next
		// full run compares against. Only for a generator whose files survived:
		// one that left nothing behind is a generator this project no longer
		// has, and remembering its version forever is how a manifest fills up
		// with names nobody recognizes.
		if v, ok := prev.Generators[e.Generator]; ok {
			next.Generators[e.Generator] = v
		}
	}
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
