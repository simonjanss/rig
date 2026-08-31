// Package migcheck checks a project's migration file names before they are
// applied to anything.
//
// goose decides two things from a file name alone — which version a migration
// is, and what order the versions run in — and it reports a problem with either
// at boot, against a live database, having already applied whatever came
// before. That is the wrong moment and the wrong machine. Everything here is
// answerable from the names on disk plus what the base branch already has, so
// it is answerable in a pull request.
//
// Three rules:
//
//   - The name is NNNNN_snake_case.sql (RIG6050). This is rig's convention, not
//     goose's, and it is the one of the three a project may turn off.
//   - No two files in the directory claim the same version (RIG6051). goose
//     records one version number per applied migration, so a second file with
//     the same number is one that never runs and never will.
//   - A migration added on this branch is numbered above everything on the base
//     ref (RIG6052). Merged below, it is a migration goose has already stepped
//     past, and it reports it as missing rather than applying it.
//
// # Two parses, on purpose
//
// [WellNamed] demands exactly five digits; [Version] accepts any number of
// them. The difference is not laxity. rig's convention is the five-digit form,
// so 25_late.sql fails RIG6050 — but goose reads that file as version 25
// regardless, so it can still collide with 00025_early.sql and still land out of
// order. A collision check that only looked at well-named files would let the
// badly-named one through the two rules that describe what actually breaks.
//
// So the naming rule is strict and everything about version numbers is lenient,
// and a single bad file can earn all three diagnostics. That is the honest
// count: three separate things are wrong with it.
//
// # No I/O
//
// Every check takes the names it is to judge. Reading the directory belongs to
// the caller, and so does asking git what the base ref has — which is what lets
// the whole of this be tested without a repository, and what keeps one rule from
// deciding how another one's files were found.
package migcheck

import (
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
)

// wellNamed is the form rig writes and requires: a five-digit sequence, an
// underscore, then a snake_case description.
//
// Fixed width rather than "some digits" so that the directory sorts in the
// order it applies. A directory holding 9_a.sql and 10_b.sql lists the second
// one first in every tool anybody reads it with, and the order a migration runs
// in is the one thing about a migrations directory that has to be obvious.
var wellNamed = regexp.MustCompile(`^\d{5}_[a-z0-9_]+\.sql$`)

// numbered is what goose itself will read: a numeric prefix and an underscore.
// Anything this matches is a migration that will be applied, whatever rig
// thinks of the name.
var numbered = regexp.MustCompile(`^([0-9]+)_[^/]*\.sql$`)

// Version returns the version goose will give a migration file. The second
// return is false when the name is not one goose would apply at all.
//
// The argument may be a bare name or a path; only the last element is read.
func Version(name string) (int64, bool) {
	m := numbered.FindStringSubmatch(fileName(name))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		// A prefix too long for an int64. Not a version anybody meant.
		return 0, false
	}
	return v, true
}

// WellNamed reports whether a name is the NNNNN_snake_case.sql form rig
// requires. A name that is not is still a migration goose applies; see
// [Version].
func WellNamed(name string) bool {
	return wellNamed.MatchString(fileName(name))
}

// MaxVersion is the highest version among names, and zero when none of them is
// a migration. Zero is a safe floor because goose numbers from one.
func MaxVersion(names []string) int64 {
	var highest int64
	for _, n := range names {
		if v, ok := Version(n); ok && v > highest {
			highest = v
		}
	}
	return highest
}

// CheckNames reports every file in dir whose name is not the form rig requires.
//
// Only files that look like a migration attempt are judged: a README.md beside
// the migrations is not a badly-named migration, and reporting it would make the
// rule impossible to leave on. What counts as an attempt is a .sql file, which
// is the extension goose reads and the only thing in the directory that will be
// run.
//
// sev is the severity from the project's `validate.migration_filename`. An empty
// severity reports nothing, which is how the rule is turned off.
func CheckNames(dir string, names []string, sev diag.Severity) diag.List {
	var diags diag.List
	if sev == "" {
		return diags
	}
	for _, name := range sorted(names) {
		name = fileName(name)
		if !strings.HasSuffix(name, ".sql") || WellNamed(name) {
			continue
		}
		diags.AddSeverity(diag.CodeMigrationFilename, sev, diag.Anchor{File: join(dir, name)},
			"%s is not named NNNNN_snake_case.sql", name)
	}
	return diags
}

// CheckDuplicates reports every version number in dir claimed by more than one
// file.
//
// Padding does not save two files from being the same version: goose parses the
// prefix as a number, so 00025_a.sql and 25_b.sql are both version 25 and only
// one of them will ever run. Which one is whichever goose sorted first, and the
// other is a migration that is in the repository, is not in the database, and
// says nothing about it.
func CheckDuplicates(dir string, names []string) diag.List {
	byVersion := make(map[int64][]string)
	for _, name := range names {
		if v, ok := Version(name); ok {
			byVersion[v] = append(byVersion[v], fileName(name))
		}
	}

	var diags diag.List
	for _, v := range slices.Sorted(maps.Keys(byVersion)) {
		claimants := sorted(byVersion[v])
		if len(claimants) < 2 {
			continue
		}
		// Anchored at the first, so the diagnostic has a file to point at and
		// the message names the rest. Anchoring each of them would report one
		// mistake as two.
		diags.Add(diag.CodeMigrationDuplicate, diag.Anchor{File: join(dir, claimants[0])},
			"version %d is claimed by %d files (%s); goose applies one of them and "+
				"never runs the others", v, len(claimants), strings.Join(claimants, ", "))
	}
	return diags
}

// CheckOutOfOrder reports every added migration numbered at or below baseMax,
// the highest version the base ref already has.
//
// At or below, not below: a migration equal to the base's highest is either a
// duplicate of it or a renumbering of something already applied, and neither is
// a migration that runs.
//
// baseMax of zero — nothing on the base ref, or no migrations directory there —
// passes everything, which is the first migration in a new project.
func CheckOutOfOrder(added []string, base string, baseMax int64) diag.List {
	var diags diag.List
	for _, f := range sorted(added) {
		v, ok := Version(f)
		if !ok || v > baseMax {
			continue
		}
		diags.Add(diag.CodeMigrationOutOfOrder, diag.Anchor{File: f},
			"version %d is at or below %s, which is already at version %d; renumber "+
				"this migration to %d or higher", v, base, baseMax, baseMax+1)
	}
	return diags
}

// join builds the anchor path for a file in the migrations directory. Slashes,
// because a diagnostic path is read by GitHub and by an editor, not by the
// filesystem.
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return path.Join(slashed(dir), name)
}

// fileName is the last element of a name or a path, on either separator. Every
// rule here judges the file name, so a caller may pass whichever it has.
func fileName(name string) string { return path.Base(slashed(name)) }

// slashed normalizes Windows separators, so one anchor form comes out of every
// platform.
func slashed(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// sorted is a sorted copy, so that a caller's slice is not reordered under it
// and the diagnostics come out in a fixed order whatever the directory read
// returned.
func sorted(in []string) []string { return slices.Sorted(slices.Values(in)) }
