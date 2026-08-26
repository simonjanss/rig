// Package gobuf emits Go source.
//
// Two decisions shape it. Imports are collected as they are used rather than
// declared up front, so adding a line that needs time.Time cannot leave the
// file missing an import or carrying an unused one. And the result goes through
// go/format, so emitted whitespace is irrelevant — which means the emitting
// code can stay readable instead of being contorted around indentation, and
// golden files diff on substance rather than on spacing.
package gobuf

import (
	"bytes"
	"fmt"
	"go/format"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/pkg/gen"
)

// Buf accumulates one Go file.
type Buf struct {
	pkg       string
	doc       string
	handOwned bool
	body      bytes.Buffer
	imports   map[string]string // path -> alias, empty alias means none
}

// New starts a file in the given package.
func New(pkg string) *Buf {
	return &Buf{pkg: pkg, imports: make(map[string]string)}
}

// NewHandOwned starts a file that is written once and then belongs to the
// developer.
//
// It carries no "DO NOT EDIT" banner, because editing it is the entire point,
// and because tools that recognize the banner would otherwise skip a file that
// is ordinary source.
func NewHandOwned(pkg string) *Buf {
	return &Buf{pkg: pkg, handOwned: true, imports: make(map[string]string)}
}

// Doc sets the package comment. It takes plain text rather than a format,
// because a package comment is prose and prose contains percent signs.
func (b *Buf) Doc(text string) { b.doc = text }

// packageName is the identifier an import path is referred to by.
//
// A module's major version is part of its path but not part of its package
// name: github.com/jackc/pgx/v5 is imported as pgx, and taking the last
// segment would produce code that refers to a package called v5.
func packageName(importPath string) string {
	base := path.Base(importPath)
	if len(base) > 1 && base[0] == 'v' && isDigits(base[1:]) {
		if parent := path.Base(path.Dir(importPath)); parent != "." && parent != "/" {
			return parent
		}
	}
	return base
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// Import records an import and returns the name to qualify with.
//
// It returns the name rather than nothing so that the call site reads as the
// thing it is about to write — `b.L("%s.Time", b.Import("time"))` cannot get
// out of step the way a separate import list can.
func (b *Buf) Import(importPath string) string {
	if alias, ok := b.imports[importPath]; ok {
		if alias != "" {
			return alias
		}
		return packageName(importPath)
	}

	name := packageName(importPath)
	alias := ""

	// Two packages with the same base name need one of them aliased. The
	// alias is derived from the path so it is stable across runs.
	for existing, existingAlias := range b.imports {
		if existing == importPath {
			continue
		}
		used := existingAlias
		if used == "" {
			used = packageName(existing)
		}
		if used == name {
			alias = aliasFor(importPath)
			break
		}
	}

	b.imports[importPath] = alias
	if alias != "" {
		return alias
	}
	return name
}

// aliasFor builds a stable alias from the last two path segments.
func aliasFor(importPath string) string {
	parts := strings.Split(importPath, "/")
	if len(parts) < 2 {
		return sanitize(importPath)
	}
	return sanitize(parts[len(parts)-2] + parts[len(parts)-1])
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// L writes one line.
func (b *Buf) L(format string, args ...any) {
	fmt.Fprintf(&b.body, format, args...)
	b.body.WriteByte('\n')
}

// P writes without a trailing newline, for building a line in pieces.
func (b *Buf) P(format string, args ...any) {
	fmt.Fprintf(&b.body, format, args...)
}

// NL writes a blank line.
func (b *Buf) NL() { b.body.WriteByte('\n') }

// Comment writes a doc comment, wrapping at a readable width and preserving
// paragraph breaks.
//
// A line beginning with a tab is godoc's code block and survives as written.
// Prose is reflowed because the width it was typed at is nobody's business;
// an example is not, because reflowing one changes what it says.
func (b *Buf) Comment(text string) {
	for i, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		if i > 0 {
			b.L("//")
		}
		b.paragraph(para)
	}
}

// paragraph writes one paragraph, keeping indented lines out of the flow.
func (b *Buf) paragraph(para string) {
	var flow []string
	flush := func() {
		for _, line := range Wrap(strings.Join(flow, " "), 76) {
			b.L("// %s", line)
		}
		flow = nil
	}

	for _, line := range strings.Split(para, "\n") {
		if strings.HasPrefix(line, "\t") {
			flush()
			b.L("//%s", strings.TrimRight(line, " \t"))
			continue
		}
		flow = append(flow, strings.Fields(line)...)
	}
	flush()
}

// Wrap breaks text into lines no longer than width, without splitting words.
//
// Exported for tsbuf, which lays out comments the same way and is otherwise
// unrelated to this package. Two copies of a word-wrapper would be two comment
// layouts sooner or later, and the generated Go and the generated TypeScript sit
// in the same review.
func Wrap(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var (
		lines []string
		cur   = words[0]
	)
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	return append(lines, cur)
}

// Quote renders a Go string literal.
func Quote(s string) string { return strconv.Quote(s) }

// Bytes renders the finished file.
//
// Formatting failure is reported with the unformatted source attached. A
// syntax error in emitted code otherwise surfaces as an opaque complaint from
// gofmt about a file nobody can see.
func (b *Buf) Bytes() ([]byte, error) {
	var out bytes.Buffer

	if !b.handOwned {
		out.WriteString(gen.Banner + "\n")
		out.WriteString("//\n")
		out.WriteString("// This file is rewritten on every run. Put changes in the service layer.\n\n")
	}

	if b.doc != "" {
		for _, line := range Wrap(b.doc, 76) {
			out.WriteString("// " + line + "\n")
		}
	}
	out.WriteString("package " + b.pkg + "\n\n")

	if len(b.imports) > 0 {
		out.WriteString("import (\n")
		for _, group := range groupImports(b.imports) {
			for _, imp := range group {
				// A package whose name does not match its path's last
				// segment is aliased explicitly, so a reader of the import
				// block does not have to know the convention.
				switch alias := b.imports[imp]; {
				case alias != "":
					fmt.Fprintf(&out, "\t%s %q\n", alias, imp)
				case packageName(imp) != path.Base(imp):
					fmt.Fprintf(&out, "\t%s %q\n", packageName(imp), imp)
				default:
					fmt.Fprintf(&out, "\t%q\n", imp)
				}
			}
			out.WriteString("\n")
		}
		out.WriteString(")\n\n")
	}

	out.Write(b.body.Bytes())

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("generated source does not parse: %w\n\n%s", err, numbered(out.String()))
	}
	return formatted, nil
}

// groupImports separates the standard library from everything else, which is
// what gofmt would do anyway and what a reader expects.
func groupImports(imports map[string]string) [][]string {
	var std, third []string
	for _, p := range slices.Sorted(maps.Keys(imports)) {
		if isStdlib(p) {
			std = append(std, p)
			continue
		}
		third = append(third, p)
	}

	var groups [][]string
	if len(std) > 0 {
		groups = append(groups, std)
	}
	if len(third) > 0 {
		groups = append(groups, third)
	}
	return groups
}

// isStdlib guesses by the shape of the path: a standard-library import has no
// dot in its first segment, because it has no domain.
func isStdlib(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// numbered prefixes each line with its number, so the parse error's position
// can be found in the attached source.
func numbered(src string) string {
	var b strings.Builder
	for i, line := range strings.Split(src, "\n") {
		fmt.Fprintf(&b, "%4d| %s\n", i+1, line)
	}
	return b.String()
}
