package tablesync

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// update brings an existing file in step with the schema, returning nil when
// nothing needs to change.
func update(t *ir.Table, loaded *tableconf.Loaded, enums map[string]*ir.PgEnum, opt Options) (*Change, error) {
	source, err := os.ReadFile(loaded.Path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", loaded.Path, err)
	}

	file, err := parser.ParseBytes(source, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", loaded.Path, err)
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return nil, fmt.Errorf("%s is empty", loaded.Path)
	}

	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		// A single-key document parses as the pair itself. Wrapping it lets the
		// rest of this function treat every file the same way.
		pair, isPair := file.Docs[0].Body.(*ast.MappingValueNode)
		if !isPair {
			return nil, fmt.Errorf("%s is not a mapping", loaded.Path)
		}
		root = ast.Mapping(pair.GetToken(), false, pair)
		file.Docs[0].Body = root
	}

	n := opt.Namer
	if n == nil {
		n = naming.New(naming.Config{})
	}

	var notes []string

	columnNotes, err := syncColumns(root, t, loaded, opt)
	if err != nil {
		return nil, err
	}
	notes = append(notes, columnNotes...)

	enumNotes, err := syncEnums(root, t, loaded, enums, n, opt)
	if err != nil {
		return nil, err
	}
	notes = append(notes, enumNotes...)

	if len(notes) == 0 {
		return nil, nil
	}

	return &Change{
		Kind:    ChangeUpdate,
		Table:   t.Name,
		Path:    loaded.Path,
		Content: file.String(),
		Notes:   notes,
	}, nil
}

func syncColumns(root *ast.MappingNode, t *ir.Table, loaded *tableconf.Loaded, opt Options) ([]string, error) {
	want := configurableColumns(t)
	have := loaded.File.Columns

	var missing []*ir.Column
	for _, c := range want {
		if _, ok := have[c.Name]; !ok {
			missing = append(missing, c)
		}
	}

	var dead []string
	if opt.Prune {
		for name := range have {
			if t.Column(name) == nil {
				dead = append(dead, name)
			}
		}
		slices.Sort(dead)
	}

	if len(missing) == 0 && len(dead) == 0 {
		return nil, nil
	}

	var notes []string

	if len(missing) > 0 {
		var snippet strings.Builder
		for _, c := range missing {
			snippet.WriteString(renderColumn(c, "  "))
		}
		if err := appendUnder(root, "columns", snippet.String()); err != nil {
			return nil, err
		}
		notes = append(notes, fmt.Sprintf("added %s", names(missing)))
	}

	if len(dead) > 0 {
		removeUnder(root, "columns", dead)
		notes = append(notes, fmt.Sprintf("pruned %s", strings.Join(dead, ", ")))
	}

	return notes, nil
}

func syncEnums(
	root *ast.MappingNode,
	t *ir.Table,
	loaded *tableconf.Loaded,
	enums map[string]*ir.PgEnum,
	n *naming.Namer,
	opt Options,
) ([]string, error) {
	used := enumsUsedBy(t, enums)
	if len(used) == 0 {
		return nil, nil
	}

	var (
		notes   []string
		missing []*ir.PgEnum
	)

	for _, e := range used {
		configured, ok := loaded.File.Enums[e.Name]
		if !ok {
			missing = append(missing, e)
			continue
		}

		// The type is configured but may be missing labels a migration added.
		var newLabels []ir.PgEnumValue
		for _, v := range e.Values {
			if _, ok := configured.Values[v.Value]; !ok {
				newLabels = append(newLabels, v)
			}
		}
		if len(newLabels) == 0 {
			continue
		}

		var snippet strings.Builder
		for _, v := range newLabels {
			fmt.Fprintf(&snippet, "  %s:\n", v.Value)
			fmt.Fprintf(&snippet, "    name: %s\n", n.Go(v.Value))
			fmt.Fprintf(&snippet, "    description: %s\n", quote(todoComment))
		}

		enumNode, err := findValue(root, "enums", e.Name)
		if err != nil || enumNode == nil {
			continue
		}
		if err := appendUnderNode(enumNode, "values", snippet.String()); err != nil {
			return nil, err
		}

		var labels []string
		for _, v := range newLabels {
			labels = append(labels, v.Value)
		}
		notes = append(notes, fmt.Sprintf("added %s values %s", e.Name, strings.Join(labels, ", ")))
	}

	if len(missing) > 0 {
		var snippet strings.Builder
		for _, e := range missing {
			snippet.WriteString(renderEnum(e, n, "  "))
		}
		if err := appendUnder(root, "enums", snippet.String()); err != nil {
			return nil, err
		}

		var added []string
		for _, e := range missing {
			added = append(added, e.Name)
		}
		notes = append(notes, fmt.Sprintf("added enum %s", strings.Join(added, ", ")))
	}

	return notes, nil
}

// appendUnder adds entries to a top-level mapping, creating the key when it is
// not there yet.
func appendUnder(root *ast.MappingNode, key, snippet string) error {
	target := findPair(root, key)
	if target == nil {
		// The key does not exist, so the whole block is new. Rendering it at
		// top level and appending gives the same result as adding to an
		// existing one.
		pairs, err := parseSnippet(key + ":\n" + snippet)
		if err != nil {
			return err
		}
		root.Values = append(root.Values, pairs...)
		return nil
	}
	return appendToPair(target, snippet)
}

// appendUnderNode adds entries under a nested key, for enum values.
func appendUnderNode(parent ast.Node, key, snippet string) error {
	m, ok := parent.(*ast.MappingNode)
	if !ok {
		if pair, isPair := parent.(*ast.MappingValueNode); isPair {
			m = ast.Mapping(pair.GetToken(), false, pair)
		} else {
			return fmt.Errorf("cannot add %s under a %T", key, parent)
		}
	}

	target := findPair(m, key)
	if target == nil {
		pairs, err := parseSnippet("  " + key + ":\n" + indentSnippet(snippet, "  "))
		if err != nil {
			return err
		}
		m.Values = append(m.Values, pairs...)
		return nil
	}
	return appendToPair(target, indentSnippet(snippet, "  "))
}

func appendToPair(pair *ast.MappingValueNode, snippet string) error {
	pairs, err := parseSnippet(snippet)
	if err != nil {
		return err
	}

	switch v := pair.Value.(type) {
	case *ast.MappingNode:
		v.Values = append(v.Values, pairs...)
	case *ast.MappingValueNode:
		pair.Value = ast.Mapping(v.GetToken(), false, append([]*ast.MappingValueNode{v}, pairs...)...)
	case *ast.NullNode:
		// An empty `columns:` with nothing under it.
		pair.Value = ast.Mapping(pair.GetToken(), false, pairs...)
	default:
		return fmt.Errorf("cannot add entries to a %T", pair.Value)
	}
	return nil
}

// removeUnder deletes named entries from a mapping.
func removeUnder(root *ast.MappingNode, key string, names []string) {
	pair := findPair(root, key)
	if pair == nil {
		return
	}
	m, ok := pair.Value.(*ast.MappingNode)
	if !ok {
		return
	}
	m.Values = slices.DeleteFunc(m.Values, func(v *ast.MappingValueNode) bool {
		return slices.Contains(names, keyOf(v))
	})
}

// findPair returns the top-level entry with the given key.
func findPair(m *ast.MappingNode, key string) *ast.MappingValueNode {
	for _, pair := range m.Values {
		if keyOf(pair) == key {
			return pair
		}
	}
	return nil
}

// findValue returns the value node at parent.child.
func findValue(root *ast.MappingNode, parent, child string) (ast.Node, error) {
	p := findPair(root, parent)
	if p == nil {
		return nil, nil
	}
	m, ok := p.Value.(*ast.MappingNode)
	if !ok {
		if pair, isPair := p.Value.(*ast.MappingValueNode); isPair {
			m = ast.Mapping(pair.GetToken(), false, pair)
		} else {
			return nil, nil
		}
	}
	c := findPair(m, child)
	if c == nil {
		return nil, nil
	}
	return c.Value, nil
}

func keyOf(pair *ast.MappingValueNode) string {
	if pair == nil || pair.Key == nil {
		return ""
	}
	if s, ok := pair.Key.(*ast.StringNode); ok {
		return s.Value
	}
	if tok := pair.Key.GetToken(); tok != nil {
		return tok.Value
	}
	return ""
}

func indentSnippet(snippet, indent string) string {
	lines := strings.Split(strings.TrimRight(snippet, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = indent + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func names(cols []*ir.Column) string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return strings.Join(out, ", ")
}
