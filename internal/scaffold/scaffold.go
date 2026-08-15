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
	// Quoted: a project named "2026" or "001" — a directory name is where this
	// usually comes from — is a YAML number, and the loader would refuse the
	// file rig had just written.
	fmt.Fprintf(&b, "project:\n  name: %q\n  module: %q\n\n", opt.Name, opt.Module)
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
	b.WriteString("  boolean_prefix: warn\n\n")
	b.WriteString(generatorsYAML(opt.Module))
	return b.String()
}

// generatorsYAML wires up the three Go layers.
//
// They are configured together because they only work together: the API layer
// calls the repository and the HTTP layer calls the API layer, so a project
// with one of the three is a project that does not build.
func generatorsYAML(module string) string {
	var b strings.Builder
	b.WriteString("generators:\n")
	b.WriteString("  # The entity, its enums, its query types, and its inputs. Both of the\n")
	b.WriteString("  # layers below import this one, so there is one definition of a row\n")
	b.WriteString("  # rather than a copy on each side and a conversion between them.\n")
	b.WriteString("  - name: model-go\n")
	b.WriteString("    out_dir: internal/model\n")
	b.WriteString("    options:\n")
	b.WriteString("      package: model\n\n")
	b.WriteString("  # The repository interface and its pgx implementation.\n")
	b.WriteString("  - name: persist-go\n")
	b.WriteString("    out_dir: internal/store\n")
	b.WriteString("    options:\n")
	b.WriteString("      package: store\n")
	fmt.Fprintf(&b, "      model_import: %s/internal/model\n\n", module)
	b.WriteString("  # Wire types, the interface your service layer implements, and a\n")
	b.WriteString("  # working default implementation of every operation.\n")
	b.WriteString("  - name: service-go\n")
	b.WriteString("    out_dir: internal/api\n")
	b.WriteString("    options:\n")
	b.WriteString("      package: api\n")
	fmt.Fprintf(&b, "      model_import: %s/internal/model\n", module)
	fmt.Fprintf(&b, "      store_import: %s/internal/store\n", module)
	fmt.Fprintf(&b, "      api_import: %s/internal/api\n", module)
	b.WriteString("      # Where your service layer goes. Written once, then yours.\n")
	b.WriteString("      stub_dir: services/{table}\n\n")
	b.WriteString("  # Routing and handlers.\n")
	b.WriteString("  - name: server-go\n")
	b.WriteString("    out_dir: internal/api\n")
	b.WriteString("    options:\n")
	b.WriteString("      package: api\n")
	fmt.Fprintf(&b, "      model_import: %s/internal/model\n\n", module)
	b.WriteString("  # Live-sync shape endpoints. It writes nothing until a table asks\n")
	b.WriteString("  # for one with `electric: {enabled: true}`, so leaving it here costs\n")
	b.WriteString("  # nothing and turning it on is one line in a table's configuration.\n")
	b.WriteString("  - name: electric\n")
	b.WriteString("    out_dir: internal/electric\n")
	b.WriteString("    options:\n")
	b.WriteString("      package: electric\n")
	b.WriteString("      electric_url: http://localhost:3000\n")
	fmt.Fprintf(&b, "      shape_import: %s/internal/electric\n", module)
	b.WriteString("      # Where your extra scoping goes. Written once, then yours.\n")
	b.WriteString("      stub_dir: services/{table}\n")
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

## Four layers, you write one

    generated            YOU WRITE THIS           generated
  ┌──────────────┐   ┌──────────────────────┐   ┌──────────────┐
  │  repository  │ ← │    service layer     │ → │  API layer   │
  │  pgx, SQL    │   │  business logic      │   │  handlers    │
  │              │   │  rules, hooks        │   │  routing     │
  └──────┬───────┘   └──────────────────────┘   └──────┬───────┘
         │             ┌──────────────────┐            │
         └────────────►│      model       │◄───────────┘
                       │   (generated)    │  entities, enums, queries,
                       └──────────────────┘  inputs, validation

Both outer layers speak the model's types, so nothing is converted between
them: a field is defined once and returned as it is stored.

The service layer under `+"`services/<table>/`"+` is the only place to write code.
It implements a generated interface and calls a generated repository. A table
with no business logic needs nothing but its constructor and an empty set of
rules: the generated default implementation already satisfies the interface.

Every service says what it owes, in a `+"`contract()`"+` function the stub writes. It
is passed to the constructor, so there is no service whose rules were never
attached, and every field is listed there even when it is nil — adding a column
shows up as a field nobody filled in.

What goes in the contract:

- **A rule about a field** — a function in the create validator, the update
  validator, or both. Each has one entry per field that operation can set, so
  an update has no hook for a column it cannot touch. The hook sees the row the
  request would produce. Return a `+"`model.FieldError`"+` and the client is
  answered 422 with that field named.
- **Something that must happen with the write** — a hook: `+"`Hooks.Create.Before`"+`,
  `+"`.After`"+`, `+"`.AfterCommit`"+`, and the same for Update, Delete and Restore. Before
  and After run inside the write's transaction, so returning an error undoes it.
  AfterCommit runs once it has landed, which is the only safe place to tell
  anything outside the database.
- **An endpoint the table configuration declares** — a method on the same type,
  handed over as `+"`Endpoints`"+`. rig has no default for one, so the set is an
  interface: declare an endpoint and forget to write it and the build fails.

Anything else — taking over a generated operation entirely — is a method on the
service that overrides the embedded default.

What the schema already declares — NOT NULL, lengths, enum membership — is
checked by the generated code. Do not write it again.

## Which files you may edit

| Pattern | Who owns it |
|---|---|
| `+"`rig.yaml`"+` | you — the project's whole configuration |
| `+"`migrations/*.sql`"+` | you |
| `+"`services/<table>/<table>.yaml`"+` | you, via `+"`rig sync`"+` |
| `+"`main.go`"+` | you |
| `+"`services/<table>/<table>.go`"+` | you |
| `+"`*.gen.go`"+`, `+"`*.gen.ts`"+` | rig — rewritten on every run, never edit |

## Migrations

    rig db up          development: a throwaway Postgres, migrated
    go run . migrate   anywhere else: the binary applies what it embeds

They are the same files read by the same library, so the two cannot disagree
about what the schema is. Applying them from the binary rather than from the
rig CLI is deliberate: the build that carries the code carries the schema it
expects, where a CLI on a deployment machine is whatever version was installed
there.

Run it as its own step before a rollout. Migrating at boot is one line
(`+"`cfg.Migrate`"+`) and fine for a single instance; with replicas it means every
one of them migrating at once, a slow migration holding the rollout open, and a
bad one taking down the fleet instead of one job.

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
| `+"`deleted_at`"+` | makes the table soft-deletable, and adds two routes |
| `+"`version_type`"+` + `+"`snapshot_from_<table>_id`"+` + `+"`_at`"+` | keeps prior versions |

Add them in a migration; do not try to configure them.

## Who may call what

Every endpoint requires the claims lookup to succeed. `+"`public:`"+` in a
table's configuration names the operations that do not:

    public: [Get, List]

It covers generated operations and custom endpoints alike — they share one
namespace. A custom endpoint can also say `+"`public: true`"+` in its own
block. Anything unnamed needs a credential, so forgetting to add to the list
leaves an endpoint protected rather than open.

The last two carry routes of their own, because a deletion that stamps the row
has somewhere it went and a way back, and a resource with a history has one to
read:

    GET  /_deleted           what was retired and is still restorable
    POST /{id}/_restore      bring one back
    GET  /{id}/_versions     the previous versions, newest first
    POST /{id}/_revert       put one of them back

All four are generated, so do not declare them in `+"`endpoints:`"+` — that is
for the operations rig has no default for.
`, opt.Name)
}
