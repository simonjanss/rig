package tableconf

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	validator "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/message"

	"github.com/simonjanss/rig/internal/diag"
)

// Loaded is one configuration file, its syntax tree index, and where it came
// from. The index is what lets later stages report a problem at the exact key
// that caused it, long after the YAML has been decoded into structs.
type Loaded struct {
	Path  string
	File  *File
	Index *Index
}

// Set is every table configuration in a project, keyed by table name.
type Set struct {
	byTable map[string]*Loaded
	order   []string
}

// NewSet builds an empty set.
func NewSet() *Set {
	return &Set{byTable: make(map[string]*Loaded)}
}

// Add records a loaded file. The last file for a table wins; callers detect
// duplicates before adding.
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
//
// Validation runs against the same JSON Schema that [Schema] writes for
// editors, so a file an editor accepts is a file rig accepts. Every problem is
// reported with the position of the offending key; the returned file is nil
// only when the YAML could not be parsed at all.
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
// validate content that is not on disk.
func Parse(path string, data []byte) (*Loaded, diag.List) {
	var diags diag.List

	astFile, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		diags.Add(diag.CodeConfigSyntax, yamlErrorAnchor(path, err), "%s", yamlErrorMessage(err))
		return nil, diags
	}

	var root = rootNode(astFile)
	index := buildIndex(path, root)

	// An empty document is a file with nothing in it; report it as such rather
	// than letting it fail schema validation with a confusing type error.
	if root == nil {
		diags.Add(diag.CodeConfigSyntax, diag.Anchor{File: path, Line: 1, Column: 1},
			"table configuration is empty")
		return nil, diags
	}

	// Validate through JSON so the instance uses the exact value types the
	// schema validator expects, with no YAML-specific shapes left over.
	jsonBytes, err := yaml.YAMLToJSON(data)
	if err != nil {
		diags.Add(diag.CodeConfigSyntax, diag.Anchor{File: path, Line: 1, Column: 1},
			"cannot convert configuration to JSON for validation: %v", err)
		return nil, diags
	}

	instance, err := validator.UnmarshalJSON(bytes.NewReader(jsonBytes))
	if err != nil {
		diags.Add(diag.CodeConfigSyntax, diag.Anchor{File: path, Line: 1, Column: 1},
			"cannot read configuration for validation: %v", err)
		return nil, diags
	}

	sch, err := compiledSchema()
	if err != nil {
		diags.Add(diag.CodeInternal, diag.Anchor{File: path}, "%v", err)
		return nil, diags
	}

	if err := sch.Validate(instance); err != nil {
		var verr *validator.ValidationError
		if errors.As(err, &verr) {
			for _, leaf := range leafErrors(verr) {
				for _, path := range anchorPaths(leaf) {
					diags.Add(diag.CodeConfigInvalid, index.At(path), "%s", schemaMessage(leaf))
				}
			}
		} else {
			diags.Add(diag.CodeConfigInvalid, diag.Anchor{File: path, Line: 1, Column: 1}, "%v", err)
		}
		return nil, diags
	}

	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		diags.Add(diag.CodeConfigSyntax, yamlErrorAnchor(path, err), "%s", yamlErrorMessage(err))
		return nil, diags
	}

	return &Loaded{Path: path, File: &f, Index: index}, diags
}

// LoadDir reads every *.yaml and *.yml file matched by the given paths.
//
// A file whose declared table does not match its filename is an error: the
// filename is how `rig sync` finds the file to update, so a mismatch means one
// of the two is silently ignored from then on.
func LoadDir(paths []string) (*Set, diag.List) {
	var diags diag.List
	set := NewSet()

	seen := make(map[string]string, len(paths))
	for _, p := range paths {
		loaded, d := Load(p)
		diags.Append(d)
		if loaded == nil {
			continue
		}

		if loaded.File.Table == "" {
			diags.Add(diag.CodeConfigFile, loaded.Index.At("table"),
				"table configuration does not name a table")
			continue
		}

		if prev, dup := seen[loaded.File.Table]; dup {
			diags.Add(diag.CodeConfigFile, loaded.Index.At("table"),
				"table %q is already configured by %s", loaded.File.Table, prev)
			continue
		}
		seen[loaded.File.Table] = p

		if base := stem(p); base != loaded.File.Table {
			diags.Add(diag.CodeConfigFile, loaded.Index.At("table"),
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

func rootNode(f *ast.File) ast.Node {
	if f == nil || len(f.Docs) == 0 {
		return nil
	}
	for _, doc := range f.Docs {
		if doc != nil && doc.Body != nil {
			return doc.Body
		}
	}
	return nil
}

// leafErrors flattens a validation error tree down to the errors that actually
// describe a problem. The tree's interior nodes say things like "does not
// validate against #/$defs/Column", which repeats what the leaf already says
// with less precision.
func leafErrors(e *validator.ValidationError) []*validator.ValidationError {
	if len(e.Causes) == 0 {
		return []*validator.ValidationError{e}
	}
	var out []*validator.ValidationError
	for _, c := range e.Causes {
		out = append(out, leafErrors(c)...)
	}
	return out
}

var englishPrinter = message.NewPrinter(message.MatchLanguage("en"))

// anchorPaths returns the paths a validation error should be reported at.
//
// Most errors describe the value at their instance location. Two do not:
// an unknown key and a missing key are both reported against the enclosing
// object, and anchoring there would point a reader at the top of a block rather
// than at the word they mistyped. Naming the offending key turns "something in
// this table is wrong" into a cursor on the exact line.
func anchorPaths(e *validator.ValidationError) []string {
	base := instancePath(e.InstanceLocation)

	switch k := e.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		paths := make([]string, 0, len(k.Properties))
		for _, p := range k.Properties {
			paths = append(paths, Join(base, p))
		}
		return paths
	case *kind.Required:
		// A missing key has no position of its own, so the enclosing object is
		// the most precise place there is.
		return []string{base}
	default:
		return []string{base}
	}
}

// schemaMessage renders a validation error. The instance path is deliberately
// left out: the anchor already points at it, and repeating "operations.1" in
// the text adds an array index the reader does not need.
func schemaMessage(e *validator.ValidationError) string {
	return e.ErrorKind.LocalizedString(englishPrinter)
}

// instancePath converts a JSON-pointer-style location into the dotted path the
// anchor index is keyed by.
func instancePath(loc []string) string { return strings.Join(loc, ".") }

// yamlErrorAnchor recovers the position from a goccy parse or decode error.
// goccy formats positions into the message rather than exposing them, so the
// line is read back out of the rendered error.
func yamlErrorAnchor(path string, err error) diag.Anchor {
	a := diag.Anchor{File: path, Line: 1, Column: 1}
	var line, col int
	if n, _ := fmt.Sscanf(yaml.FormatError(err, false, false), "[%d:%d]", &line, &col); n == 2 {
		a.Line, a.Column = line, col
	}
	return a
}

func yamlErrorMessage(err error) string {
	msg := yaml.FormatError(err, false, false)
	// Drop the leading "[line:col] " that goccy prepends: the anchor already
	// carries the position, and repeating it reads as noise.
	if i := strings.Index(msg, "] "); i > 0 && strings.HasPrefix(msg, "[") {
		msg = msg[i+2:]
	}
	return strings.TrimSpace(strings.SplitN(msg, "\n", 2)[0])
}
