package gen

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Orphans finds files on disk that rig wrote and no generator claims any more.
//
// The manifest cannot answer this on its own. It is gitignored by default, so a
// clean checkout — which is what CI is — has no record of what rig wrote there.
// What does survive a clone is the output itself: the `.gen.` in a name and
// [Banner] on the first line are rig's claim on a file, and a claim no generator
// renews is a leftover. A renamed table that left its old file behind is caught
// on a machine that has never generated anything.
//
// The two signals are both needed. The OpenAPI documents carry no banner,
// because YAML that begins with a comment is not what anyone wants to read; the
// TypeScript client's index.ts carries one but cannot be renamed, because a
// package entry point is called index.ts. Neither signal covers a hand-owned
// stub, which is the point: a stub has no banner and no `.gen.`, so it is never
// mistaken for rig's.
func Orphans(root string, claimed map[string]bool) ([]Delta, error) {
	var out []Delta

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory rig cannot read is not evidence of a leftover file, and
			// failing the whole run over it would make `check` depend on the
			// permissions of everything beside the code it checks.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipDir(path, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}

		// Symlinks are not followed: a link into a generated tree would be
		// reported at both of its names, and removing one of them removes
		// something else's file.
		if !d.Type().IsRegular() {
			return nil
		}

		if claimed[relSlash(root, path)] {
			return nil
		}
		if !rigWrote(path) {
			return nil
		}

		out = append(out, Delta{
			Path:   path,
			Status: Stale,
			Reason: "no generator produces this any more",
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	return out, nil
}

// skipDir reports whether a scan should stop at this directory.
//
// A directory with its own rig.yaml is another project, and every generated file
// in it belongs to the generators that project configures rather than to these.
// Dot directories are skipped wholesale — .git and .rig are not generator output
// and walking node_modules is minutes rig does not have.
func skipDir(path, name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "build", "testdata":
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	if st, err := os.Stat(filepath.Join(path, "rig.yaml")); err == nil && !st.IsDir() {
		return true
	}
	return false
}

// bannerExts are the extensions rig writes [Banner] into. Sniffing only these
// keeps the scan from opening every image and lockfile in the project.
var bannerExts = map[string]bool{".go": true, ".ts": true, ".tsx": true}

// rigWrote reports whether a file is rig's own output.
func rigWrote(path string) bool {
	if strings.Contains(filepath.Base(path), ".gen.") {
		return true
	}
	if !bannerExts[filepath.Ext(path)] {
		return false
	}

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	// The banner is the first thing both emitters write, so this is a prefix
	// check rather than a search.
	head := make([]byte, len(Banner))
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return string(head) == Banner
}
