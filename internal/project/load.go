package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/internal/yamlconf"
)

// SchemaID is the identifier the emitted project schema publishes itself under.
const SchemaID = "https://rig.dev/schema/project.v1.json"

// ConfigNames are the filenames rig looks for, in order.
var ConfigNames = []string{"rig.yaml", "rig.yml"}

var format = &yamlconf.Format{
	ID:    SchemaID,
	Title: "rig project configuration",
	Description: "Project-wide configuration: layout, database, naming, " +
		"convention severities, and the generators to run.",
	New: func() any { return &Config{} },
}

// Schema returns the JSON Schema for rig.yaml.
func Schema() ([]byte, error) { return format.Schema() }

// ErrNotFound is returned by [Find] when no configuration file exists.
var ErrNotFound = errors.New("no rig.yaml found in this directory or any parent")

// Project is a loaded configuration together with where it came from.
type Project struct {
	// Root is the directory holding rig.yaml. Every relative path in the
	// configuration is resolved against it.
	Root string
	// ConfigPath is the configuration file itself.
	ConfigPath string

	Config *Config
	Index  *yamlconf.Index

	namer *naming.Namer
}

// Find locates the configuration file by walking up from start.
//
// The walk stops at the first match, at a directory containing .git, or at the
// filesystem root. Stopping at a repository boundary keeps a stray rig.yaml in
// a home directory from silently capturing an unrelated project.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}

	for {
		for _, name := range ConfigNames {
			candidate := filepath.Join(dir, name)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("looking for %s: %w", name, err)
			}
		}

		// A repository boundary is as far as the search goes.
		if st, err := os.Stat(filepath.Join(dir, ".git")); err == nil && st.IsDir() {
			return "", ErrNotFound
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}

// Load finds and reads the configuration for the project containing start.
func Load(start string) (*Project, diag.List) {
	var diags diag.List

	path, err := Find(start)
	if err != nil {
		diags.Add(diag.CodeConfigFile, diag.Anchor{}, "%v", err)
		return nil, diags
	}
	return LoadFile(path)
}

// LoadFile reads a specific configuration file.
func LoadFile(path string) (*Project, diag.List) {
	var diags diag.List

	data, err := os.ReadFile(path)
	if err != nil {
		diags.Add(diag.CodeConfigFile, diag.Anchor{File: path}, "cannot read project configuration: %v", err)
		return nil, diags
	}
	return Parse(path, data)
}

// Parse reads configuration bytes as though they came from path.
func Parse(path string, data []byte) (*Project, diag.List) {
	var cfg Config
	index, ok, diags := format.Decode(path, data, &cfg)
	if !ok {
		return nil, diags
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	p := &Project{
		Root:       filepath.Dir(abs),
		ConfigPath: abs,
		Config:     &cfg,
		Index:      index,
	}
	p.applyDefaults()
	diags.Append(p.check())

	return p, diags
}

// At returns the anchor for a dotted path within rig.yaml.
func (p *Project) At(segments ...string) diag.Anchor {
	if p == nil {
		return diag.At(yamlconf.Join(segments...))
	}
	return p.Index.At(yamlconf.Join(segments...))
}

// Path resolves a project-relative path against the project root. An absolute
// path is returned unchanged.
func (p *Project) Path(rel string) string {
	if rel == "" {
		return p.Root
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(p.Root, filepath.FromSlash(rel))
}

// Rel expresses an absolute path relative to the project root, for reporting.
// Paths outside the project are returned unchanged.
func (p *Project) Rel(abs string) string {
	rel, err := filepath.Rel(p.Root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return abs
	}
	return filepath.ToSlash(rel)
}

// Namer returns the naming rules for this project.
func (p *Project) Namer() *naming.Namer {
	if p.namer == nil {
		p.namer = naming.New(naming.Config{
			ExtraInitialisms: p.Config.Naming.Initialisms,
			Plurals:          p.Config.Naming.Plurals,
			JSONCase:         naming.Case(p.Config.Naming.JSONCase),
		})
	}
	return p.namer
}

// MigrationsDir is the absolute path to the migration directory.
func (p *Project) MigrationsDir() string { return p.Path(p.Config.Migrations.Dir) }

// TableDir is the absolute directory for one table's configuration and code.
func (p *Project) TableDir(table string) string {
	return p.Path(p.expand(p.Config.Layout.TableDir, table))
}

// TableConfigPath is the absolute path to one table's configuration file.
func (p *Project) TableConfigPath(table string) string {
	tmpl := p.Config.Layout.ConfigFile
	// {table_dir} lets the two templates stay in step: change the directory and
	// the config file follows it.
	tmpl = strings.ReplaceAll(tmpl, "{table_dir}", p.Config.Layout.TableDir)
	return p.Path(p.expand(tmpl, table))
}

// expand fills the layout placeholders for one table.
func (p *Project) expand(tmpl, table string) string {
	n := p.Namer()
	return strings.NewReplacer(
		"{table}", table,
		"{Table}", n.Go(table),
		"{tables}", naming.Snake(n.Plural(table)),
	).Replace(tmpl)
}

// configTemplate is the config-file layout with {table_dir} already folded in.
func (p *Project) configTemplate() string {
	return strings.ReplaceAll(p.Config.Layout.ConfigFile, "{table_dir}", p.Config.Layout.TableDir)
}

// TableConfigPaths returns every existing table configuration file, sorted.
//
// It globs the layout template rather than walking the tree, then keeps only
// the paths the template can actually reproduce. That second step matters:
// under the default layout of services/{table}/{table}.yaml a plain glob also
// matches services/lesson/notes.yaml, and treating an unrelated YAML file as
// table configuration produces a baffling error.
func (p *Project) TableConfigPaths() ([]string, error) {
	tmpl := p.configTemplate()

	pattern := strings.NewReplacer(
		"{table}", "*",
		"{Table}", "*",
		"{tables}", "*",
	).Replace(tmpl)

	matches, err := filepath.Glob(p.Path(pattern))
	if err != nil {
		return nil, fmt.Errorf("looking for table configuration in %q: %w", pattern, err)
	}

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if st, err := os.Stat(m); err != nil || st.IsDir() {
			continue
		}
		if _, ok := p.TableForConfigPath(m); !ok {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// TableForConfigPath recovers the table a configuration path belongs to.
//
// The name is read out of the first {table} placeholder, and then the whole
// template is re-expanded and compared: a path only belongs to a table if the
// layout would have produced exactly that path for it. Re-expanding is what
// makes {Table} and {tables} fall out correctly without inverting them.
//
// A layout with no {table} placeholder cannot be inverted, so every glob match
// is accepted and the filename check at load time catches any mismatch.
func (p *Project) TableForConfigPath(path string) (string, bool) {
	tmpl := p.configTemplate()
	if !strings.Contains(tmpl, "{table}") {
		return "", true
	}

	rel := p.Rel(path)
	re, err := templateRegexp(tmpl)
	if err != nil {
		return "", false
	}
	m := re.FindStringSubmatch(rel)
	if m == nil {
		return "", false
	}

	table := m[1]
	if p.Rel(p.TableConfigPath(table)) != rel {
		return "", false
	}
	return table, true
}

// templateRegexp compiles a layout template into a pattern that captures the
// {table} placeholder. Other placeholders match loosely; the re-expansion check
// in [Project.TableForConfigPath] is what actually pins them down.
func templateRegexp(tmpl string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")

	rest := tmpl
	for rest != "" {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(regexp.QuoteMeta(rest))
			break
		}
		close := strings.IndexByte(rest[open:], '}')
		if close < 0 {
			b.WriteString(regexp.QuoteMeta(rest))
			break
		}
		close += open

		b.WriteString(regexp.QuoteMeta(rest[:open]))
		switch rest[open : close+1] {
		case "{table}":
			b.WriteString(`([^/]+)`)
		default:
			b.WriteString(`[^/]+`)
		}
		rest = rest[close+1:]
	}

	b.WriteString("$")
	return regexp.Compile(b.String())
}

// SearchMethod returns the configured Search exposure.
func (p *Project) SearchMethod() SearchMethod { return p.Config.API.SearchMethod }

// Severity resolves a configurable rule's severity. An unset rule uses the
// code's own default, and "off" reports nothing at all.
func (p *Project) Severity(configured string, code diag.Code) diag.Severity {
	if strings.TrimSpace(configured) == "" {
		return code.Severity
	}
	sev, ok := diag.ParseSeverity(configured)
	if !ok {
		// "off" and anything else the schema would have rejected.
		return ""
	}
	return sev
}
