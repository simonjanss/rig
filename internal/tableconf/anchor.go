package tableconf

import (
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/simonjanss/rig/internal/diag"
)

// Index maps a dotted path within a configuration file to the position of the
// key that declares it.
//
// Paths use the shape "columns.title.comment" for mappings and
// "endpoints.0.name" for sequences, which is also how validation errors report
// their location, so the two line up without translation.
type Index struct {
	file      string
	positions map[string]diag.Anchor
}

// File returns the path of the file this index describes.
func (ix *Index) File() string {
	if ix == nil {
		return ""
	}
	return ix.file
}

// At returns the anchor for a path.
//
// When the exact path is not in the file — a missing required key, say — the
// lookup walks up to the nearest ancestor that is. Pointing at the enclosing
// key is far more useful than pointing at line 1, and it is what a reader
// expects: the problem is *in* that block.
func (ix *Index) At(path string) diag.Anchor {
	if ix == nil {
		return diag.At(path)
	}
	if a, ok := ix.positions[path]; ok {
		a.Path = path
		return a
	}
	for rest := path; rest != ""; {
		i := strings.LastIndexByte(rest, '.')
		if i < 0 {
			break
		}
		rest = rest[:i]
		if a, ok := ix.positions[rest]; ok {
			// Report the path that was asked about, at the position of the
			// nearest enclosing key that does exist.
			a.Path = path
			return a
		}
	}
	return diag.Anchor{File: ix.file, Line: 1, Column: 1, Path: path}
}

// Join builds a path from segments, skipping empty ones.
func Join(segments ...string) string {
	parts := make([]string, 0, len(segments))
	for _, s := range segments {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ".")
}

// buildIndex walks a parsed document and records where every key sits.
func buildIndex(file string, doc ast.Node) *Index {
	ix := &Index{file: file, positions: make(map[string]diag.Anchor)}
	ix.walk(doc, "")
	return ix
}

func (ix *Index) record(path string, node ast.Node) {
	if path == "" || node == nil {
		return
	}
	tok := node.GetToken()
	if tok == nil || tok.Position == nil {
		return
	}
	// The first occurrence wins. A nested node can report the same token as its
	// parent, and the outer path is the more useful of the two.
	if _, exists := ix.positions[path]; exists {
		return
	}
	ix.positions[path] = diag.Anchor{
		File:   ix.file,
		Line:   tok.Position.Line,
		Column: tok.Position.Column,
		Path:   path,
	}
}

func (ix *Index) walk(n ast.Node, path string) {
	switch v := n.(type) {
	case *ast.DocumentNode:
		ix.walk(v.Body, path)

	case *ast.MappingNode:
		for _, pair := range v.Values {
			ix.walkPair(pair, path)
		}

	case *ast.MappingValueNode:
		// A single-pair mapping parses as the value node itself rather than as
		// a MappingNode wrapping it.
		ix.walkPair(v, path)

	case *ast.SequenceNode:
		for i, item := range v.Values {
			p := Join(path, strconv.Itoa(i))
			ix.record(p, item)
			ix.walk(item, p)
		}

	case *ast.AnchorNode:
		ix.walk(v.Value, path)

	case *ast.TagNode:
		ix.walk(v.Value, path)
	}
}

func (ix *Index) walkPair(pair *ast.MappingValueNode, path string) {
	if pair == nil || pair.Key == nil {
		return
	}
	key := keyName(pair.Key)
	if key == "" {
		return
	}
	p := Join(path, key)
	// Anchor on the key, not the value: that is the token a reader is looking
	// for when they jump to the diagnostic.
	ix.record(p, pair.Key)
	ix.walk(pair.Value, p)
}

func keyName(n ast.Node) string {
	switch k := n.(type) {
	case *ast.StringNode:
		return k.Value
	case *ast.IntegerNode:
		return k.GetToken().Value
	case *ast.BoolNode:
		return k.GetToken().Value
	default:
		if tok := n.GetToken(); tok != nil {
			return tok.Value
		}
		return ""
	}
}
