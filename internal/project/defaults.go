package project

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
)

// Defaults every project inherits. They are the shape `rig init` writes, so a
// generated rig.yaml can stay short: anything left out here means the same
// thing as spelling it out.
const (
	DefaultTableDir   = "services/{table}"
	DefaultConfigFile = "{table_dir}/{table}.yaml"

	DefaultAPIVersion   = "v1"
	DefaultSearchMethod = SearchBoth
	DefaultOpenAPI      = "3.1"

	DefaultImage    = "postgres:17-alpine"
	DefaultPort     = 55432
	DefaultDBName   = "rig"
	DefaultDBUser   = "rig"
	DefaultDBPass   = "rig"
	DefaultDBSchema = "public"

	DefaultMigrationsDir   = "migrations"
	DefaultMigrationsTable = "rig_migrations"

	DefaultJSONCase = "camel"
)

func (p *Project) applyDefaults() {
	c := p.Config

	if c.Version == 0 {
		c.Version = 1
	}

	setDefault(&c.Layout.TableDir, DefaultTableDir)
	setDefault(&c.Layout.ConfigFile, DefaultConfigFile)

	setDefault(&c.API.Name, c.Project.Name)
	setDefault(&c.API.Version, DefaultAPIVersion)
	if c.API.BasePath == "" {
		c.API.BasePath = "/api/" + c.API.Version
	}
	c.API.BasePath = "/" + strings.Trim(c.API.BasePath, "/")
	if c.API.SearchMethod == "" {
		c.API.SearchMethod = DefaultSearchMethod
	}

	setDefault(&c.OpenAPI.Version, DefaultOpenAPI)

	setDefault(&c.Database.Image, DefaultImage)
	setDefault(&c.Database.Name, DefaultDBName)
	setDefault(&c.Database.User, DefaultDBUser)
	setDefault(&c.Database.Password, DefaultDBPass)
	setDefault(&c.Database.Schema, DefaultDBSchema)
	if c.Database.ContainerName == "" {
		name := c.Project.Name
		if name == "" {
			name = "rig"
		}
		c.Database.ContainerName = name + "-db"
	}
	if c.Database.Port == 0 {
		c.Database.Port = DefaultPort
	}

	setDefault(&c.Migrations.Dir, DefaultMigrationsDir)
	setDefault(&c.Migrations.Table, DefaultMigrationsTable)

	setDefault(&c.Naming.JSONCase, DefaultJSONCase)
}

func setDefault(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

// DatabaseURL is the connection string for this project.
//
// An explicit URL wins; otherwise one is built for the throwaway container.
func (p *Project) DatabaseURL() string {
	d := p.Config.Database
	if d.URL != "" {
		return d.URL
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable",
		url.QueryEscape(d.User), url.QueryEscape(d.Password), d.Port, d.Name)
}

// UsesContainer reports whether rig manages the database itself.
func (p *Project) UsesContainer() bool { return p.Config.Database.URL == "" }

// check validates what the JSON Schema cannot: relationships between values,
// and templates that must be able to name a table.
func (p *Project) check() diag.List {
	var diags diag.List
	c := p.Config

	// project.name and project.module are required by the schema, so there is
	// no check for them here.

	// A layout that cannot distinguish one table from another would make every
	// table share a single configuration file.
	if !hasTablePlaceholder(c.Layout.ConfigFile) && !hasTablePlaceholder(c.Layout.TableDir) {
		diags.Add(diag.CodeConfigInvalid, p.At("layout", "config_file"),
			"layout must name a table somewhere: use {table}, {Table} or {tables} in config_file or table_dir")
	}

	seen := make(map[string]int, len(c.Generators))
	for i, g := range c.Generators {
		at := p.At("generators", fmt.Sprint(i), "name")
		if g.Name == "" {
			diags.Add(diag.CodeConfigInvalid, at, "generator %d has no name", i)
			continue
		}
		if prev, dup := seen[g.Name]; dup {
			diags.Add(diag.CodeConfigInvalid, at,
				"generator %q is already configured at generators.%d", g.Name, prev)
			continue
		}
		seen[g.Name] = i
	}

	return diags
}

func hasTablePlaceholder(s string) bool {
	return strings.Contains(s, "{table}") ||
		strings.Contains(s, "{Table}") ||
		strings.Contains(s, "{tables}")
}
