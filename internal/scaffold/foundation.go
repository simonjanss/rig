package scaffold

import (
	"fmt"
	"slices"
	"strings"
)

// Foundation parts. Each is one migration and its table configuration, so
// skipping one leaves a project that still applies and still validates.
const (
	PartTenancy       = "tenancy"
	PartSessions      = "sessions"
	PartAPIKeys       = "apikeys"
	PartOAuth         = "oauth"
	PartFiles         = "files"
	PartNotifications = "notifications"
)

// Parts in the order they must be applied.
//
// Sessions come before API keys because the key table is optional and the token
// table is not: keys add a column to tokens rather than tokens depending on
// keys, so a project that skips keys is still coherent.
//
// Files come after those and depend on nothing. They are the one part that is
// useful in a project with no authentication at all, which is why rig_file's
// tenant column carries no reference to rig_tenant.
//
// Notifications come last and are the opposite case: a notification is addressed
// to an account, so they need the tenancy part and say so in [Requires]. They do
// not need sessions — an application that reads its inbox through a header is a
// perfectly ordinary one — which is why the dependency is the narrow half of the
// foundation rather than all of it.
func Parts() []string {
	return []string{PartTenancy, PartAPIKeys, PartSessions, PartOAuth, PartFiles, PartNotifications}
}

// FoundationOptions describe what to scaffold.
type FoundationOptions struct {
	// Skip names parts to leave out.
	Skip []string
	// Expose names foundation tables to write a table configuration for.
	//
	// Empty is the ordinary case and writes none. The foundation's tables belong
	// to the rig/auth module: their Go types, their stores and their endpoints
	// are imported from there, so generating a model and a repository for each
	// of them would be a few thousand lines of code in the project that nothing
	// in the project calls.
	//
	// A table listed here gets its configuration back, and with it everything a
	// configuration asks for. It is for the case where an application genuinely
	// wants CRUD over one — an administration screen listing the people in a
	// tenant, most often.
	Expose []string
	// FirstNumber is the migration number to start from.
	FirstNumber int
	// MigrationsDir and ConfigPath place the files.
	MigrationsDir string
	ConfigPath    func(table string) string
	// Existing is the migration files already present, by base name.
	//
	// A part whose migration is already there is skipped entirely. Without
	// this, a second run would write the same tables again under fresh
	// numbers, and `rig db up` would fail on the first CREATE TABLE — which is
	// a poor way to learn that the command was not idempotent.
	Existing []string
}

// Foundation returns the files `rig setup-project` writes.
//
// Migrations, and — only for a table named in [FoundationOptions.Expose] — a
// table configuration.
//
// The tables are ordinary rig: real Postgres tables following the same column
// conventions any other table follows, so `rig sync` can read them and every
// rule applies to them. What is deliberately not ordinary is that rig generates
// no code for them. The Go types, the stores and the endpoints all live in the
// rig/auth module, and a second copy projected into the project would be a few
// thousand lines nothing calls. The migrations stay here because they are the
// project's own schema history: a library that silently owns a dozen tables is
// harder to reason about than a dozen tables you can read.
func Foundation(opt FoundationOptions) []File {
	var (
		files  []File
		number = opt.FirstNumber
	)
	if number == 0 {
		number = 1
	}

	for _, part := range Parts() {
		if slices.Contains(opt.Skip, part) {
			continue
		}
		p := foundationPart(part)

		// The migration is written once. The configuration is a separate
		// question: exposing a table is something an application decides later,
		// long after the migration landed, so `--expose rig_account` on a project
		// that already has the foundation has to write that one file rather than
		// finding the part done and doing nothing.
		if !alreadyApplied(opt.Existing, part) {
			files = append(files, File{
				Path:    MigrationFilename(opt.MigrationsDir, number, "rig_"+part),
				Content: p.migration,
			})
			number++
		}

		for _, tc := range p.configs {
			if !slices.Contains(opt.Expose, tc.table) {
				continue
			}
			files = append(files, File{
				Path:    opt.ConfigPath(tc.table),
				Content: tc.content,
			})
		}
	}

	return files
}

// alreadyApplied reports whether a part's migration is already present.
//
// It matches on the name rather than the full path because the number is
// assigned at write time: the same part written on two different days would sit
// at two different numbers and look like two different files.
func alreadyApplied(existing []string, part string) bool {
	suffix := "_rig_" + part + ".sql"
	for _, name := range existing {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// PartTables are the tables a part creates.
//
// Every name carries the `rig_` prefix the migrations create them under, so a
// project can tell at a glance which tables arrived with the foundation and is
// free to have an `account` or a `tenant` of its own.
//
// Written out rather than parsed from the SQL, so that adding a table to the
// foundation is a decision somebody makes here as well — a heuristic would
// quietly start or stop treating one as the auth module's, and which side of
// that line a table falls on decides whether rig generates code for it.
func PartTables(part string) []string {
	switch part {
	case PartTenancy:
		return []string{
			"rig_tenant", "rig_identity", "rig_identity_credential",
			"rig_identity_verification", "rig_account",
		}
	case PartAPIKeys:
		return []string{"rig_api_key"}
	case PartSessions:
		return []string{"rig_account_token", "rig_auth_log", "rig_identity_session"}
	case PartOAuth:
		return []string{"rig_identity_oauth"}
	case PartFiles:
		return []string{"rig_file"}
	case PartNotifications:
		return []string{
			"rig_notification", "rig_notification_recipient",
			"rig_notification_device", "rig_notification_setting",
			"rig_notification_delivery",
		}
	default:
		return nil
	}
}

// Tables are every table the foundation creates.
func Tables() []string {
	var out []string
	for _, part := range Parts() {
		out = append(out, PartTables(part)...)
	}
	return out
}

// Managed are the foundation tables a project has actually applied.
//
// The evidence is the migration files: a part rig wrote is named for itself, so
// a project that ran `setup-project` says so in its own migrations directory.
// That matters because the answer decides whether rig generates a model and a
// repository for a table — and a project with a `rig_account` table of its own,
// which nobody scaffolded, must keep getting them.
func Managed(existing []string) []string {
	var out []string
	for _, part := range Parts() {
		if !alreadyApplied(existing, part) {
			continue
		}
		out = append(out, PartTables(part)...)
	}
	return out
}

// Requires reports the parts a part depends on, so a skip list that would
// leave a dangling reference can be refused rather than discovered by psql.
func Requires(part string) []string {
	switch part {
	case PartSessions:
		// Sessions and the log name the key a request arrived with, and they
		// declare those columns where the tables are created rather than by
		// altering them afterwards — so rig_api_key has to exist by then. Keeping
		// them separable would mean two shapes of the same table, and the
		// keys are two hundred lines of the foundation nobody regrets having.
		return []string{PartTenancy, PartAPIKeys}
	case PartAPIKeys, PartOAuth:
		return []string{PartTenancy}
	case PartNotifications:
		// An inbox line names an account, and rig_account is the tenancy part.
		// Not sessions: reading your own inbox needs claims, and where those
		// come from is not this foundation's business.
		return []string{PartTenancy}
	default:
		return nil
	}
}

type part struct {
	migration string
	configs   []tableConfig
}

type tableConfig struct {
	table   string
	content string
}

func foundationPart(name string) part {
	switch name {
	case PartTenancy:
		return part{migration: tenancySQL, configs: tenancyConfigs()}
	case PartSessions:
		return part{migration: sessionsSQL, configs: sessionConfigs()}
	case PartAPIKeys:
		return part{migration: apiKeysSQL, configs: apiKeyConfigs()}
	case PartOAuth:
		return part{migration: oauthSQL, configs: oauthConfigs()}
	case PartFiles:
		return part{migration: filesSQL, configs: fileConfigs()}
	case PartNotifications:
		return part{migration: notificationsSQL, configs: notificationConfigs()}
	default:
		return part{}
	}
}

// config renders a table configuration file.
//
// The resource name is written out rather than derived, because the physical
// name carries the `rig_` prefix and the API name should not: the prefix is
// there so a project can tell its own tables from the foundation's in psql, and
// a client has no business knowing which library created a table. Without it an
// exposed table would arrive as RigAccount on /rig-accounts.
//
// Column comments are not repeated here: the migrations carry COMMENT ON for
// every one of them, so they arrive through introspection. Saying them twice
// would mean two places to edit and one place to forget.
func config(table, resource, schemaDepth string, body ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# yaml-language-server: $schema=%s\n", schemaDepth)
	fmt.Fprintf(&b, "table: %s\n", table)
	fmt.Fprintf(&b, "resource: %s\n", resource)
	for _, s := range body {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(s, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// schemaRef is the editor directive's path back to the generated JSON Schema.
// It is written relative to the configuration file, which is two directories
// down under the default layout.
const schemaRef = "../../.rig/table.schema.json"

// notExposed is the stanza that keeps a table's model and repository while
// giving it no API at all.
const notExposed = `# The model and the repository are generated; the API is not. A REST interface
# for this table would be a way to bypass the rules the auth package enforces.
expose: false`
