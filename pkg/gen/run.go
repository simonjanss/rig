package gen

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/simonjanss/rig/pkg/ir"
)

// Spec selects one generator and configures it.
type Spec struct {
	Name    string
	OutDir  string
	Options map[string]any
}

// Result is what one generator produced. Paths are absolute by this point.
type Result struct {
	Generator string
	Version   string
	Artifacts []Artifact
}

// Run executes the requested generators against a document.
//
// A document that failed validation is refused outright. Generating from a
// schema rig knows to be wrong would produce code that compiles and misbehaves,
// which is far worse than producing nothing.
func Run(ctx context.Context, r *Registry, doc *ir.Document, root string, specs []Spec) ([]Result, error) {
	if !doc.Valid {
		return nil, fmt.Errorf("the document did not validate; fix the reported problems first")
	}

	results := make([]Result, 0, len(specs))
	for _, spec := range specs {
		g, ok := r.Get(spec.Name)
		if !ok {
			return nil, fmt.Errorf("no generator named %q; `rig generators` lists the ones there are", spec.Name)
		}

		outDir := spec.OutDir
		if outDir == "" {
			outDir = "."
		}
		if !filepath.IsAbs(outDir) {
			outDir = filepath.Join(root, filepath.FromSlash(outDir))
		}

		artifacts, err := g.Generate(ctx, doc, Options{OutDir: outDir, Root: root, Raw: spec.Options})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", spec.Name, err)
		}

		// Paths are resolved once, here, so a generator only ever thinks in
		// terms of names relative to its own output directory. An absolute path
		// is left alone: that is how a generator places a hand-owned scaffold
		// somewhere other than beside its own output.
		for i := range artifacts {
			p := filepath.FromSlash(artifacts[i].Path)
			if !filepath.IsAbs(p) {
				p = filepath.Join(outDir, p)
			}
			artifacts[i].Path = p
		}
		slices.SortFunc(artifacts, func(a, b Artifact) int { return cmp.Compare(a.Path, b.Path) })

		if err := checkDuplicates(spec.Name, artifacts); err != nil {
			return nil, err
		}

		results = append(results, Result{
			Generator: spec.Name,
			Version:   g.Version(),
			Artifacts: artifacts,
		})
	}

	return results, nil
}

// checkDuplicates catches a generator emitting the same path twice, which would
// otherwise resolve to whichever one was written last.
func checkDuplicates(name string, artifacts []Artifact) error {
	for i := 1; i < len(artifacts); i++ {
		if artifacts[i].Path == artifacts[i-1].Path {
			return fmt.Errorf("%s produced %s twice", name, artifacts[i].Path)
		}
	}
	return nil
}

// Report writes a human-readable summary of a set of deltas.
func Report(w io.Writer, root string, deltas []Delta, verbose bool) {
	counts := map[Status]int{}
	for _, d := range deltas {
		counts[d.Status]++
	}

	for _, d := range deltas {
		if d.Status == Unchanged && !verbose {
			continue
		}
		fmt.Fprintf(w, "%-9s %s\n", d.Status, relTo(root, d.Path))
	}

	var parts []string
	for _, s := range []Status{Added, Changed, Unchanged, Stale, Conflict} {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "%s\n", strings.Join(parts, ", "))
	}
}

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}
