// Package yamlconf loads YAML configuration and keeps track of where every key
// came from.
//
// Both of rig's configuration formats — the project file and the per-table
// files — are validated against a JSON Schema generated from their Go structs,
// and both report problems at the exact line and column of the offending key.
// That machinery lives here so the two cannot drift apart in how strictly they
// read a file or how precisely they report a mistake.
package yamlconf

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
// "endpoints.0.name" for sequences, which is also how schema violations report
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

// Has reports whether the exact path appears in the file. Callers use it to
// tell "set to the zero value" from "not set at all".
func (ix *Index) Has(path string) bool {
	if ix == nil {
		return false
	}
	_, ok := ix.positions[path]
	return ok
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

// BuildIndex walks a parsed document and records where every key sits.
func BuildIndex(file string, doc ast.Node) *Index {
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
			ix.record(p, startNode(item))
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

// startNode finds the token a reader would call the beginning of a node. A
// mapping's own token is its colon, which is a strange place for a cursor to
// land; its first key is what someone scanning the file actually looks at.
func startNode(n ast.Node) ast.Node {
	switch v := n.(type) {
	case *ast.MappingNode:
		if len(v.Values) > 0 {
			return startNode(v.Values[0])
		}
	case *ast.MappingValueNode:
		if v.Key != nil {
			return startNode(v.Key)
		}
	}
	return n
}

func keyName(n ast.Node) string {
	switch k := n.(type) {
	case *ast.StringNode:
		return k.Value
	default:
		if tok := n.GetToken(); tok != nil {
			return tok.Value
		}
		return ""
	}
}
