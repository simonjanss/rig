// Command godoccheck fails on an exported symbol with no doc comment.
//
// It runs over the modules somebody else imports — runtime, auth, migrate,
// rigclient — where the godoc is the documentation for the Go surface rather
// than commentary on it. `make godoc-check` is the entry point.
//
// It parses files by path rather than loading packages, which is what lets a
// program in the root module check directories belonging to four other modules
// without a type checker, a build, or a module resolution step. That also rules
// out go/doc, whose ast.Package has been deprecated since Go 1.22.
//
// What it asks is only whether a symbol has a doc, never what the doc says. The
// form of a comment is a decision this repository already made — prose, not
// "Foo does..." boilerplate, which is why ST1000 and ST1020-ST1022 are off in
// .golangci.yml — and struct fields are documented where the name leaves a
// question open, which is a judgement no tool makes well. A check that produced
// a hundred "// ID is the ID." lines would leave the next reader trusting the
// doc comments less, not more.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: godoccheck <dir>...")
		os.Exit(2)
	}

	var findings []string
	for _, root := range os.Args[1:] {
		found, err := checkTree(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "godoccheck:", err)
			os.Exit(2)
		}
		findings = append(findings, found...)
	}
	if len(findings) == 0 {
		return
	}

	sort.Strings(findings)
	for _, f := range findings {
		fmt.Fprintln(os.Stderr, f)
	}
	noun := "symbols"
	if len(findings) == 1 {
		noun = "symbol"
	}
	fmt.Fprintf(os.Stderr, "\n%d exported %s without a doc comment\n", len(findings), noun)
	os.Exit(1)
}

// checkTree walks root and checks every Go file that godoc would read.
func checkTree(root string) ([]string, error) {
	var findings []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata is Go by extension and not by intent, and a leading dot
			// or underscore is already invisible to the go tool.
			name := d.Name()
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || skipFile(path) {
			return nil
		}
		found, err := checkFile(path)
		if err != nil {
			return err
		}
		findings = append(findings, found...)
		return nil
	})
	return findings, err
}

// skipFile reports whether a file's contents are nobody's to document here. A
// test does not render, and a generated file is the generator's output — a
// missing comment in one is a finding about the generator, and it belongs there.
func skipFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".gen.go")
}

func checkFile(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	var findings []string
	report := func(pos token.Pos, name, kind string) {
		p := fset.Position(pos)
		findings = append(findings, fmt.Sprintf("%s:%d: %s %s is exported and undocumented", p.Filename, p.Line, kind, name))
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			checkFunc(d, report)
		case *ast.GenDecl:
			checkGenDecl(d, report)
		}
	}
	return findings, nil
}

func checkFunc(d *ast.FuncDecl, report func(token.Pos, string, string)) {
	if !d.Name.IsExported() || d.Doc != nil {
		return
	}
	if d.Recv == nil {
		report(d.Pos(), d.Name.Name, "func")
		return
	}
	// A method on an unexported type does not render, however exported its own
	// name is.
	recv, ok := receiverName(d.Recv)
	if !ok || !ast.IsExported(recv) {
		return
	}
	report(d.Pos(), recv+"."+d.Name.Name, "method")
}

// receiverName is the type a method is on, with any pointer and type parameters
// stripped: T, *T, T[U] and *T[U] all answer T.
func receiverName(recv *ast.FieldList) (string, bool) {
	if len(recv.List) == 0 {
		return "", false
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name, true
	case *ast.IndexExpr: // T[U]
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name, true
		}
	case *ast.IndexListExpr: // T[U, V]
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name, true
		}
	}
	return "", false
}

func checkGenDecl(d *ast.GenDecl, report func(token.Pos, string, string)) {
	if d.Tok == token.IMPORT {
		return
	}
	// A doc on the block covers every name in it, which is how a set of related
	// constants is documented once rather than a dozen times.
	if d.Doc != nil {
		return
	}

	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if s.Name.IsExported() && s.Doc == nil && s.Comment == nil {
				report(s.Pos(), s.Name.Name, "type")
			}
		case *ast.ValueSpec:
			if s.Doc != nil || s.Comment != nil {
				continue
			}
			for _, name := range s.Names {
				if name.IsExported() {
					report(name.Pos(), name.Name, strings.ToLower(d.Tok.String()))
				}
			}
		}
	}
}
