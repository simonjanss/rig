package project

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// goModule is a Go module found on disk, as far as a directory inside it is
// concerned.
type goModule struct {
	// Declared is the module path its go.mod states.
	Declared string
	// File is the go.mod itself, for a diagnostic to name.
	File string
	// Sub is the directory that was asked about, relative to the directory
	// holding File, in slash form. "." is the module root.
	Sub string
}

// goModuleAt locates the Go module a project-relative directory belongs to: the
// nearest go.mod at or above it, the module path that file declares, and the
// directory expressed relative to that go.mod's own directory.
//
// This is the one fact rig.yaml does not carry. `out_dir` is a path from the
// directory holding rig.yaml, and an import path is a path from the module
// root; those are the same string only when the two directories are the same
// one. A project whose go.mod is under api/ — the two-half layout
// examples/linearlite demonstrates — has an offset between them that nothing in
// the configuration states, and go.mod is the only thing that does.
//
// The directory itself need not exist. `rig generate` creates it, and this is
// asked before that happens; only go.mod files are looked for, so a directory
// that is not there yet simply carries the walk upward.
func (p *Project) goModuleAt(rel string) (goModule, bool) {
	start := p.Path(rel)

	for dir := start; ; {
		file := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(file); err == nil {
			if declared, ok := moduleDirective(data); ok {
				sub, err := filepath.Rel(dir, start)
				if err != nil {
					return goModule{}, false
				}
				return goModule{Declared: declared, File: file, Sub: filepath.ToSlash(sub)}, true
			}
			// A go.mod with no module directive names no module, so it is not a
			// root — and the directory above it may still be one.
		}

		// A repository boundary is as far as the search goes, for the reason
		// [Find] stops at one. Unlike Find this takes a .git file as well as a
		// directory: that is what a worktree and a submodule have, and a lookup
		// that walked out of one would answer with an unrelated module.
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return goModule{}, false
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return goModule{}, false
		}
		dir = parent
	}
}

// moduleDirective reads the module path out of a go.mod.
//
// Read here rather than with golang.org/x/mod/modfile, which the root module
// does not depend on: one directive is wanted, its grammar is two forms, and a
// dependency added to a published module is one every project that installs rig
// acquires. Shelling out to `go mod edit -json` the way internal/release does is
// the other way, and it would make the binary need a Go toolchain beside it to
// answer a question about a text file.
func moduleDirective(data []byte) (string, bool) {
	var block bool
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)

		if block {
			switch {
			case len(fields) == 0:
				continue
			case fields[0] == ")":
				return "", false
			}
			return unquotePath(fields[0])
		}

		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		// go.mod's grammar allows every directive in a parenthesised block, and
		// nothing writes this one that way — but a file that does still names a
		// module, and refusing to read it would be a diagnostic about nothing.
		if fields[1] == "(" {
			block = true
			continue
		}
		return unquotePath(fields[1])
	}
	return "", false
}

// unquotePath takes the quotes off a module path that has them, which go.mod
// allows anywhere a path is expected.
func unquotePath(s string) (string, bool) {
	if unquoted, err := strconv.Unquote(s); err == nil {
		s = unquoted
	}
	if s == "" {
		return "", false
	}
	return s, true
}
