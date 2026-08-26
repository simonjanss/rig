// Package tsbuf builds one TypeScript file.
//
// It is [github.com/simonjanss/rig/internal/gen/gobuf] for TypeScript, and the
// differences are the ones the language forces. There is no gofmt here: Go
// generators can emit approximately correct source and let the formatter settle
// the layout, and nothing in the Go toolchain formats TypeScript. So this emits
// what it means, already laid out — four spaces, one import per module, a
// trailing comma where Prettier would put one — and the output is compared
// against a golden file rather than against a formatter.
//
// The other difference is imports. A Go import is a path and a package name; a
// TypeScript import is a module specifier and a set of names, and the same
// module can be imported twice over — once for values and once for types. So
// [Buf.Import] and [Buf.ImportType] collect names per module and the two are
// emitted as separate statements, because `import type` is what tells a bundler
// the import disappears at build time.
package tsbuf

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// Buf accumulates one file.
type Buf struct {
	doc     string
	banner  bool
	values  map[string]map[string]bool
	types   map[string]map[string]bool
	body    bytes.Buffer
	indent  int
	pending bool
}

// New builds a buffer for a generated file, which carries the do-not-edit
// banner.
func New() *Buf {
	return &Buf{banner: true, values: map[string]map[string]bool{}, types: map[string]map[string]bool{}}
}

// NewHandOwned builds a buffer for a file a developer owns, which carries no
// banner and is written once.
func NewHandOwned() *Buf {
	return &Buf{values: map[string]map[string]bool{}, types: map[string]map[string]bool{}}
}

// Doc sets the file's leading comment, emitted above the imports.
func (b *Buf) Doc(text string) { b.doc = text }

// Import records a value import and returns the name, so a caller can write the
// name it was given rather than the one it asked for.
func (b *Buf) Import(module, name string) string {
	if b.values[module] == nil {
		b.values[module] = map[string]bool{}
	}
	b.values[module][name] = true
	return name
}

// ImportType records a type-only import. Separate from [Buf.Import] because
// `import type` is what says the import is erased at build time — and a type
// imported as a value is a runtime dependency on a module that may have no
// runtime at all.
func (b *Buf) ImportType(module, name string) string {
	if b.types[module] == nil {
		b.types[module] = map[string]bool{}
	}
	b.types[module][name] = true
	return name
}

// Indent shifts everything written after it one level right.
func (b *Buf) Indent() { b.indent++ }

// Outdent undoes one [Buf.Indent].
func (b *Buf) Outdent() {
	if b.indent > 0 {
		b.indent--
	}
}

// L writes one line at the current indentation.
func (b *Buf) L(format string, args ...any) {
	b.P(format, args...)
	b.body.WriteByte('\n')
	b.pending = false
}

// P writes without ending the line, for a line assembled in pieces.
func (b *Buf) P(format string, args ...any) {
	if !b.pending {
		b.body.WriteString(strings.Repeat("    ", b.indent))
		b.pending = true
	}
	if len(args) == 0 {
		b.body.WriteString(format)
		return
	}
	fmt.Fprintf(&b.body, format, args...)
}

// NL writes a blank line.
func (b *Buf) NL() {
	b.body.WriteByte('\n')
	b.pending = false
}

// Comment writes a JSDoc block, wrapped.
//
// A block rather than `//`, because this is the form an editor shows on hover
// and the form TypeDoc reads: a description the document carries is
// documentation, and a `//` comment above a member is invisible to everything
// that would display it.
func (b *Buf) Comment(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	lines := b.commentLines(text)
	if len(lines) == 1 {
		b.L("/** %s */", lines[0])
		return
	}

	b.L("/**")
	for _, line := range lines {
		if line == "" {
			b.L(" *")
			continue
		}
		b.L(" * %s", line)
	}
	b.L(" */")
}

// commentLines wraps each paragraph to the width left after the indentation and
// the ` * ` prefix.
func (b *Buf) commentLines(text string) []string {
	width := max(80-(b.indent*4)-3, 32)

	var out []string
	for i, para := range strings.Split(text, "\n\n") {
		if i > 0 {
			out = append(out, "")
		}
		// Asked before trimming, or the trim removes the indentation this is
		// looking for and every code block in every doc comment gets reflowed
		// into one line. Which is what happened.
		if strings.HasPrefix(para, "    ") || strings.HasPrefix(para, "\t") {
			out = append(out, strings.Split(strings.Trim(para, "\n"), "\n")...)
			continue
		}
		out = append(out, gobuf.Wrap(strings.Join(strings.Fields(para), " "), width)...)
	}
	return out
}

// Quote renders a string as a TypeScript literal.
//
// Double quotes, because that is what Prettier's default produces and the
// generated files sit beside hand-written ones that went through it.
func Quote(s string) string { return strconv.Quote(s) }

// Ident reports whether a name can be written bare as an object key, or has to
// be quoted. A column called `order` is fine; one called `full-name` is not.
func Ident(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '$':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Key renders a name as an object or interface key, quoting it when it is not a
// bare identifier.
func Key(s string) string {
	if Ident(s) {
		return s
	}
	return Quote(s)
}

// Bytes renders the file.
func (b *Buf) Bytes() ([]byte, error) {
	var out bytes.Buffer

	if b.banner {
		out.WriteString(gen.Banner + "\n")
		out.WriteString("//\n")
		out.WriteString("// This file is rewritten on every run. Put changes in the code that calls it.\n")
		out.WriteString("\n")
	}

	if b.doc != "" {
		tmp := &Buf{}
		tmp.Comment(b.doc)
		out.Write(tmp.body.Bytes())
		out.WriteString("\n")
	}

	// Types first, then values, each group sorted by module. Two groups rather
	// than one interleaved list because that is what the hand-written packages
	// look like, and a generated file that sorts its imports differently from
	// every file beside it reads as a different codebase.
	for _, group := range []struct {
		kind    string
		modules map[string]map[string]bool
	}{
		{"import type ", b.types},
		{"import ", b.values},
	} {
		for _, module := range slices.Sorted(maps.Keys(group.modules)) {
			names := slices.Sorted(maps.Keys(group.modules[module]))
			out.WriteString(importLine(group.kind, names, module))
		}
		if len(group.modules) > 0 {
			out.WriteString("\n")
		}
	}

	out.Write(b.body.Bytes())
	return out.Bytes(), nil
}

// importLine renders one import, on several lines when one would run long.
//
// The threshold and the layout are Prettier's, because these files sit beside
// hand-written ones that went through it and a generated file laid out
// differently reads as a different codebase. Nothing checks this — there is no
// formatter in the Go toolchain that would — so it is done here or not at all.
func importLine(kind string, names []string, module string) string {
	oneLine := kind + "{ " + strings.Join(names, ", ") + " } from " + Quote(module) + ";\n"
	if len(oneLine) <= 81 {
		return oneLine
	}

	var b strings.Builder
	b.WriteString(kind)
	b.WriteString("{\n")
	for _, name := range names {
		b.WriteString("    ")
		b.WriteString(name)
		b.WriteString(",\n")
	}
	b.WriteString("} from ")
	b.WriteString(Quote(module))
	b.WriteString(";\n")
	return b.String()
}
