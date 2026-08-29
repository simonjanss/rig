package tableconf

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/yamlconf"
)

// SchemaID is the identifier the emitted schema publishes itself under.
const SchemaID = "https://rig.dev/schema/table.v1.json"

// format describes the table configuration format to the shared loader.
var format = &yamlconf.Format{
	ID:    SchemaID,
	Title: "rig table configuration",
	Description: "Configuration for one table: documentation, exposed operations, " +
		"field naming, and any endpoints beyond the generated CRUD set.",
	New: func() any { return &File{} },
}

// Schema returns the JSON Schema for a table configuration file.
//
// Editors consume this for completion and inline errors, and the loader
// validates against the very same document.
func Schema() ([]byte, error) { return format.Schema() }

// Loaded is one configuration file, its syntax tree index, and where it came
// from. The index is what lets later stages report a problem at the exact key
// that caused it, long after the YAML has been decoded into structs.
type Loaded struct {
	Path  string
	File  *File
	Index *yamlconf.Index
	// Builtin says rig supplied this configuration rather than a project, so
	// [Loaded.Path] names no file anybody can open.
	//
	// It is the difference between "nobody has said what this column is for"
	// and "rig has, in prose it keeps in the migration that created it". The
	// `unmentioned_column` rule needs that difference: rig's own configurations
	// list no columns on purpose, so without this the rule would tell every
	// project to run `rig sync` against a file `rig sync` will not write.
	//
	// A project that writes its own file for one of rig's tables loses the
	// exemption with it, which is the intended reading: `rig sync` does keep
	// that file up to date, so the advice has somewhere to land.
	Builtin bool
}

// IsBuiltin reports whether rig supplied this configuration. Nil is a table
// with no configuration at all, which is nobody's answer rather than rig's.
func (l *Loaded) IsBuiltin() bool { return l != nil && l.Builtin }

// At returns the anchor for a dotted path within this file.
func (l *Loaded) At(segments ...string) diag.Anchor {
	if l == nil {
		return diag.At(yamlconf.Join(segments...))
	}
	return l.Index.At(yamlconf.Join(segments...))
}

// Set is every table configuration in a project, keyed by table name.
type Set struct {
	byTable map[string]*Loaded
	order   []string
	failed  map[string]bool
}

// NewSet builds an empty set.
func NewSet() *Set {
	return &Set{
		byTable: make(map[string]*Loaded),
		failed:  make(map[string]bool),
	}
}

// MarkFailed records that a table's configuration could not be read.
func (s *Set) MarkFailed(table string) {
	if s.failed == nil {
		s.failed = make(map[string]bool)
	}
	s.failed[table] = true
}

// Failed reports whether a table's configuration exists but could not be read.
//
// Such a table is not unconfigured — its intent is simply unknown, and that is
// a different thing. Rules that ask what the configuration says must stay quiet
// about it, or one mistyped key buries itself under a diagnostic for every
// column in the table.
func (s *Set) Failed(table string) bool {
	if s == nil {
		return false
	}
	return s.failed[table]
}

// Add records a loaded file.
func (s *Set) Add(l *Loaded) {
	if _, seen := s.byTable[l.File.Table]; !seen {
		s.order = append(s.order, l.File.Table)
	}
	s.byTable[l.File.Table] = l
}

// Get returns the configuration for a table, or nil.
func (s *Set) Get(table string) *Loaded {
	if s == nil {
		return nil
	}
	return s.byTable[table]
}

// Tables returns the configured table names in load order.
func (s *Set) Tables() []string {
	if s == nil {
		return nil
	}
	return s.order
}

// Len is the number of configured tables.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byTable)
}

// Load reads and validates one table configuration file.
func Load(path string) (*Loaded, diag.List) {
	var diags diag.List

	data, err := os.ReadFile(path)
	if err != nil {
		diags.Add(diag.CodeConfigFile, diag.Anchor{File: path}, "cannot read table configuration: %v", err)
		return nil, diags
	}
	return Parse(path, data)
}

// Parse is [Load] on bytes already in hand, so tests and the sync command can
// validate content that is not yet on disk.
func Parse(path string, data []byte) (*Loaded, diag.List) {
	var f File
	index, ok, diags := format.Decode(path, data, &f)
	if !ok {
		return nil, diags
	}
	return &Loaded{Path: path, File: &f, Index: index}, diags
}

// LoadDir reads every configuration file at the given paths.
//
// A file whose declared table does not match its filename is an error: the
// filename is how `rig sync` finds the file to update, so a mismatch means one
// of the two names is silently ignored from then on.
func LoadDir(paths []string) (*Set, diag.List) {
	var diags diag.List
	set := NewSet()

	seen := make(map[string]string, len(paths))
	for _, p := range paths {
		loaded, d := Load(p)
		diags.Append(d)
		if loaded == nil {
			// The file exists but could not be read. Recording the table it was
			// meant to configure keeps every downstream rule from piling on.
			set.MarkFailed(stem(p))
			continue
		}

		if loaded.File.Table == "" {
			diags.Add(diag.CodeConfigFile, loaded.At("table"),
				"table configuration does not name a table")
			continue
		}

		if prev, dup := seen[loaded.File.Table]; dup {
			diags.Add(diag.CodeConfigFile, loaded.At("table"),
				"table %q is already configured by %s", loaded.File.Table, prev)
			continue
		}
		seen[loaded.File.Table] = p

		if base := stem(p); base != loaded.File.Table {
			diags.Add(diag.CodeConfigFile, loaded.At("table"),
				"file is named %q but configures table %q; rig locates a table's configuration by filename",
				base, loaded.File.Table)
			continue
		}

		set.Add(loaded)
	}

	return set, diags
}

func stem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
