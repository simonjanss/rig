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
	b.WriteString("  base_path: /api/v1\n")
	// Commented for the reason `servers:` below is: turning it on is only half
	// the wiring. The other half is server-go's openapi_import, which needs this
	// project's module path joined to the openapi generator's out_dir — and a
	// key on by default whose partner is missing would make `rig generate`
	// refuse the first time anybody ran it. docs/api.md has both halves.
	b.WriteString("  # The document describing this API, served beside the routes it\n")
	b.WriteString("  # describes, at /api/v1/openapi.json and /api/v1/openapi.yaml. It\n")
	b.WriteString("  # needs server-go's openapi_import as well, because the document is\n")
	b.WriteString("  # embedded in the package the openapi generator writes it to. Nothing\n")
	b.WriteString("  # in main.go. See docs/api.md.\n")
	b.WriteString("  # openapi:\n")
	b.WriteString("  #   serve: true\n\n")
	// Commented rather than filled in, because rig cannot know where this
	// project will run — and written out in full rather than as a one-line
	// example, because the shape is what somebody uncommenting it needs. The
	// scaffold test parses this block with the `# ` stripped, so a suggestion
	// that does not decode fails the build rather than the reader.
	b.WriteString("# Where this API answers. Every SDK generator and the OpenAPI document\n")
	b.WriteString("# read this one list, so a document saying the API is at api.example.com\n")
	b.WriteString("# cannot ship beside a client pointing somewhere else. The entry marked\n")
	b.WriteString("# default is what a client that names no URL gets; a caller pointing at a\n")
	b.WriteString("# mock server passes their own, and nothing generated argues.\n")
	b.WriteString("# servers:\n")
	b.WriteString("#   - name: local\n")
	b.WriteString("#     url: http://localhost:8080\n")
	b.WriteString("#     default: true\n")
	b.WriteString("#   - name: production\n")
	b.WriteString("#     url: https://api.example.com\n\n")
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

// generatorsYAML wires up the three Go layers, and the document written from
// the same compiled schema beside them.
//
// The three layers are configured together because they only work together: the
// API layer calls the repository and the HTTP layer calls the API layer, so a
// project with one of the three is a project that does not build. The OpenAPI
// document is configured beside them because it is free — it describes whatever
// the layers above turned out to be.
//
// Live sync has no block of its own. It used to, and what that bought was a
// second package with a second server in it, whose claims lookup and error
// writer an application had to remember to fill in twice. The shape endpoints
// are server-go's now: a table asks for one with `electric: {enabled: true}`
// and the routes appear beside the rest of its API.
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
	b.WriteString("  # Routing and handlers, and the live-sync shape endpoints beside\n")
	b.WriteString("  # them. A table gets a stream with `electric: {enabled: true}`;\n")
	b.WriteString("  # until one asks, none of the shape options below writes anything.\n")
	b.WriteString("  - name: server-go\n")
	b.WriteString("    out_dir: internal/api\n")
	b.WriteString("    options:\n")
	b.WriteString("      package: api\n")
	fmt.Fprintf(&b, "      model_import: %s/internal/model\n", module)
	fmt.Fprintf(&b, "      api_import: %s/internal/api\n", module)
	b.WriteString("      electric_url: http://localhost:3000\n")
	b.WriteString("      # Whether a sync service that is not answering stops this\n")
	b.WriteString("      # server starting. False, the default, says so once at boot\n")
	b.WriteString("      # with a hint and serves anyway. Turn it on for an\n")
	b.WriteString("      # application whose pages are shapes, where starting is a\n")
	b.WriteString("      # server that answers 502 to everything that matters.\n")
	b.WriteString("      # electric_required: true\n")
	b.WriteString("      # Where your extra shape scoping goes, beside the service layer\n")
	b.WriteString("      # for the same table. Written once, then yours.\n")
	b.WriteString("      stub_dir: services/{table}\n")
	b.WriteString("      # Nothing here decides what happens when the sync service is\n")
	b.WriteString("      # unreachable. Pass DB to electric.Config where you build the\n")
	b.WriteString("      # proxy and every shape answers from its own filter instead of\n")
	b.WriteString("      # 502; leave it out and a sync outage is a subscriber with no\n")
	b.WriteString("      # rows. See docs/electric.md.\n\n")
	b.WriteString("  # The OpenAPI document. It describes exactly the endpoints above,\n")
	b.WriteString("  # because it is written from the same compiled document they are.\n")
	b.WriteString("  - name: openapi\n")
	b.WriteString("    out_dir: docs\n")
	b.WriteString("    options:\n")
	b.WriteString("      formats: [json, yaml]\n")
	return b.String()
}

const gitignore = `# Generated code. Everything with .gen. in its name is rewritten on every run.
# Delete these lines to commit it instead. Either way "rig check" is the CI gate,
# and it recognizes rig's own output rather than trusting the record below.
*.gen.go
*.gen.ts
*.gen.json
*.gen.yaml

# rig's working directory. The manifest is a local note about what rig wrote,
# not something to share: it is rewritten on every run, and "rig check" does not
# need it to notice a file left behind by a renamed table.
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
