// Package tablesync keeps table configuration in step with the database.
//
// Editing YAML by hand and having a tool rewrite it are usually in tension: the
// tool reformats, drops comments, and reorders keys, so people stop trusting it
// and stop running it. This package edits the syntax tree and prints it back,
// which preserves comments, blank lines, and key order exactly. The only thing
// that changes is what was asked for.
package tablesync

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// ChangeKind is what a [Change] does to a file.
type ChangeKind string

const (
	// ChangeCreate writes a configuration file for a table that has none.
	ChangeCreate ChangeKind = "create"
	// ChangeUpdate adds or removes entries in an existing file.
	ChangeUpdate ChangeKind = "update"
	// ChangeOrphan reports a file whose table is gone. rig does not delete it:
	// it may hold endpoint definitions that took real work, and a flag is not
	// enough authority to throw those away.
	ChangeOrphan ChangeKind = "orphan"
)

// Change is one file's worth of work.
type Change struct {
	Kind  ChangeKind
	Table string
	Path  string
	// Content is the file to write. Empty for an orphan.
	Content string
	// Notes describe what changed, for reporting.
	Notes []string
}

// Options tune a sync.
type Options struct {
	Namer *naming.Namer
	// Prune removes entries for columns and enum values that no longer exist.
	Prune bool
	// Only limits the sync to one table.
	Only string
}

// Plan works out what needs to change without writing anything.
func Plan(schema ir.Schema, set *tableconf.Set, pathFor func(table string) string, opt Options) ([]Change, error) {
	n := opt.Namer
	if n == nil {
		n = naming.New(naming.Config{})
	}

	enumsByName := make(map[string]*ir.PgEnum, len(schema.Enums))
	for i := range schema.Enums {
		enumsByName[schema.Enums[i].Name] = &schema.Enums[i]
	}

	var changes []Change
	live := make(map[string]bool, len(schema.Tables))

	for i := range schema.Tables {
		t := &schema.Tables[i]
		if !configurable(t) {
			continue
		}
		live[t.Name] = true
		if opt.Only != "" && opt.Only != t.Name {
			continue
		}

		path := pathFor(t.Name)
		loaded := set.Get(t.Name)

		if loaded == nil {
			changes = append(changes, Change{
				Kind:    ChangeCreate,
				Table:   t.Name,
				Path:    path,
				Content: render(t, enumsByName, n),
				Notes:   []string{fmt.Sprintf("%d columns", len(configurableColumns(t)))},
			})
			continue
		}

		change, err := update(t, loaded, enumsByName, opt)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// A file whose table is gone is reported wherever it is found, even when
	// the sync was limited to one table: it is the strongest available signal
	// that a migration renamed something.
	for _, table := range set.Tables() {
		if live[table] {
			continue
		}
		loaded := set.Get(table)
		changes = append(changes, Change{
			Kind:  ChangeOrphan,
			Table: table,
			Path:  loaded.Path,
			Notes: []string{"no table named " + table + " exists"},
		})
	}

	return changes, nil
}

// Apply writes the planned changes.
func Apply(changes []Change) error {
	for _, c := range changes {
		if c.Kind == ChangeOrphan {
			continue
		}
		if err := writeFile(c.Path, c.Content); err != nil {
			return err
		}
	}
	return nil
}

// configurable reports whether a table gets a configuration file. A join table
// is a relation on the resources it links, so it has nothing of its own to say.
func configurable(t *ir.Table) bool {
	return t.Kind == ir.TableKindBase && t.LinkTable == nil
}

// configurableColumns are the columns worth writing an entry for. The ones rig
// manages carry generated documentation and take no configuration, so listing
// them would be forty lines of noise per table.
func configurableColumns(t *ir.Table) []*ir.Column {
	var out []*ir.Column
	for i := range t.Columns {
		c := &t.Columns[i]
		if compile.IsManagedColumn(t.Name, c.Name) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// enumsUsedBy are the enum types a table's columns refer to.
func enumsUsedBy(t *ir.Table, enums map[string]*ir.PgEnum) []*ir.PgEnum {
	seen := map[string]bool{}
	var out []*ir.PgEnum

	for i := range t.Columns {
		name := t.Columns[i].EnumType
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if e, ok := enums[name]; ok {
			out = append(out, e)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func dir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// parseSnippet turns correctly-indented YAML into nodes that can be grafted
// into a document.
//
// Building nodes directly is possible but they carry no position information,
// so the renderer puts them at column zero. Parsing text that is already
// indented the way it should appear gives the tokens real positions, and the
// output comes out formatted like everything around it.
func parseSnippet(text string) ([]*ast.MappingValueNode, error) {
	f, err := parser.ParseBytes([]byte(text), parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("build configuration snippet: %w", err)
	}
	if len(f.Docs) == 0 || f.Docs[0].Body == nil {
		return nil, nil
	}

	switch b := f.Docs[0].Body.(type) {
	case *ast.MappingNode:
		return b.Values, nil
	case *ast.MappingValueNode:
		return []*ast.MappingValueNode{b}, nil
	default:
		return nil, fmt.Errorf("unexpected snippet shape %T", b)
	}
}
