// Package scaffold writes the files a new rig project starts from.
//
// Everything here is generated once and then owned by the developer. Nothing in
// this package overwrites a file that already exists: a scaffold that clobbers
// your work on the second run is worse than no scaffold at all.
package scaffold

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File is one file to write.
type File struct {
	Path    string
	Content string
}

// Write creates files that do not already exist and reports which ones it made.
// Existing files are left alone and reported as skipped.
func Write(files []File) (written, skipped []string, err error) {
	for _, f := range files {
		exists, err := fileExists(f.Path)
		if err != nil {
			return written, skipped, err
		}
		if exists {
			skipped = append(skipped, f.Path)
			continue
		}
		if err := writeFile(f.Path, f.Content); err != nil {
			return written, skipped, err
		}
		written = append(written, f.Path)
	}
	return written, skipped, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ProjectOptions describe a new project.
type ProjectOptions struct {
	Name   string
	Module string
	Image  string
}

// Project returns the files a new project starts with.
func Project(opt ProjectOptions) []File {
	return []File{
		{Path: "rig.yaml", Content: rigYAML(opt)},
		{Path: ".gitignore", Content: gitignore},
		{Path: "AGENTS.md", Content: agents(opt)},
		{Path: filepath.Join("migrations", ".keep"), Content: ""},
	}
}

func rigYAML(opt ProjectOptions) string {
	var b strings.Builder
	b.WriteString("# yaml-language-server: $schema=.rig/rig.schema.json\n")
	b.WriteString("version: 1\n\n")
	fmt.Fprintf(&b, "project:\n  name: %s\n  module: %s\n\n", opt.Name, opt.Module)
	b.WriteString("api:\n")
	b.WriteString("  version: v1\n")
	b.WriteString("  base_path: /api/v1\n\n")
	if opt.Image != "" {
		fmt.Fprintf(&b, "database:\n  image: %s\n\n", opt.Image)
	}
	b.WriteString("# Every table gets a configuration file. `rig sync` creates and updates them.\n")
	b.WriteString("layout:\n")
	b.WriteString("  table_dir: services/{table}\n")
	b.WriteString("  config_file: \"{table_dir}/{table}.yaml\"\n\n")
	b.WriteString("# Convention rules. Each is off, warn, or error.\n")
	b.WriteString("validate:\n")
	b.WriteString("  unmentioned_column: warn\n")
	b.WriteString("  missing_comment: error\n")
	b.WriteString("  fk_needs_index: error\n")
	b.WriteString("  tenant_id_leading_index: error\n")
	b.WriteString("  boolean_prefix: warn\n")
	return b.String()
}

const gitignore = `# Generated code. Everything with .gen. in its name is rewritten on every run.
*.gen.go
*.gen.ts
*.gen.json

# rig's working directory.
.rig/manifest.json

# Binaries
/bin/
`

// agents documents the project for whoever joins it next, human or otherwise.
//
// An agent dropped into an unfamiliar repository has to work out which files it
// may edit before it can do anything useful, and guessing wrong means editing a
// file that is about to be overwritten. Saying it plainly costs one file.
func agents(opt ProjectOptions) string {
	return fmt.Sprintf(`# %s

Generated with [rig](https://github.com/simonjanss/rig).

## Three layers, you write one

    generated            YOU WRITE THIS           generated
  ┌──────────────┐   ┌──────────────────────┐   ┌──────────────┐
  │  repository  │ ← │    service layer     │ → │  API layer   │
  │  models      │   │  business logic      │   │  handlers    │
  │  queries     │   │  validation, rules   │   │  routing     │
  └──────────────┘   └──────────────────────┘   └──────────────┘

The service layer under `+"`services/<table>/`"+` is the only place to write code.
It implements a generated interface and calls a generated repository. A table
with no business logic needs nothing but a constructor: the generated default
implementation already satisfies the interface.

## Which files you may edit

| Pattern | Who owns it |
|---|---|
| `+"`migrations/*.sql`"+` | you |
| `+"`services/<table>/<table>.yaml`"+` | you, via `+"`rig sync`"+` |
| `+"`services/<table>/<table>.go`"+` | you |
| `+"`*.gen.go`"+`, `+"`*.gen.ts`"+` | rig — rewritten on every run, never edit |

## The loop

    rig migration new <name>   write a migration
    rig sync                   read the database into the table configuration
    rig validate               check the schema and the configuration
    rig generate               write the code

`+"`rig validate`"+` reports every problem in one pass, each anchored to the exact
line. `+"`rig codes RIG3101`"+` explains any code it prints.

## Schema conventions

rig infers behavior from column names, so the schema is the source of truth:

| Column | Effect |
|---|---|
| `+"`id uuid primary key`"+` | required on every table |
| `+"`tenant_id uuid not null`"+` | every generated query is scoped by it |
| `+"`created_at`"+`, `+"`created_by_account_id`"+` | stamped automatically |
| `+"`updated_at`"+`, `+"`updated_by_account_id`"+` | stamped automatically |
| `+"`deleted_at`"+` | makes the table soft-deletable |
| `+"`version_type`"+` + `+"`snapshot_from_<table>_id`"+` + `+"`_at`"+` | keeps prior versions |

Add them in a migration; do not try to configure them.
`, opt.Name)
}
