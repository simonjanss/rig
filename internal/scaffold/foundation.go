package scaffold

import (
	"fmt"
	"slices"
	"strings"

	authfnd "github.com/simonjanss/rig/auth/foundation"
	filesfnd "github.com/simonjanss/rig/files/foundation"
	notifyfnd "github.com/simonjanss/rig/notify/foundation"
	presencefnd "github.com/simonjanss/rig/presence/foundation"
	"github.com/simonjanss/rig/runtime/dbschema"
	runtimefnd "github.com/simonjanss/rig/runtime/foundation"
)

// Foundation parts. Each is one migration and its table configuration, so
// skipping one leaves a project that still applies and still validates.
//
// A part's name is the name of the migration that creates it, inside the module
// that owns it — see [Sets]. That is what makes a vendored `00009_rig_tenancy.sql`
// recognisable as auth's `tenancy` however it was numbered on the way in, and it
// is why [Managed] can read a project's migrations directory and say which parts
// arrived with the foundation.
const (
	PartTenancy       = "tenancy"
	PartSessions      = "sessions"
	PartAPIKeys       = "apikeys"
	PartOAuth         = "oauth"
	PartFiles         = "files"
	PartNotifications = "notifications"
	PartPresence      = "presence"
	PartIdempotency   = "idempotency"
)

// Sets are the foundation's migration sets, in the order they apply.
//
// One per module that writes SQL against the schema, and each records how far it
// has been applied in a table of its own. The private table is what lets those
// modules be tagged separately: a shared one would mean a shared numbering
// sequence, and two modules adding a migration in the same release would collide
// on a version number.
//
// The order is the dependency order, and each set says why in its own package
// documentation. auth first because everything references its tenancy migration;
// files next because rig_file references nothing at all, deliberately, so that
// uploads work in a project with no authentication; notify after that because an
// inbox line names an account, which auth creates.
//
// runtime and presence are last, and the rule is the same one: **a set is
// appended, never inserted.** Neither had to be here rather than earlier —
// rig_idempotency references nothing, and rig_presence references only what auth
// already created — so both went at the end because that is where a new set
// belongs. A project that vendored the foundation before one existed then finds
// it as a migration to add rather than as a renumbering of the ones it has
// already applied.
func Sets() []dbschema.Set {
	return []dbschema.Set{
		authfnd.Set(), filesfnd.Set(), notifyfnd.Set(), runtimefnd.Set(), presencefnd.Set(),
	}
}

// Parts in the order they must be applied.
//
// Derived from [Sets] rather than listed, so a migration added to one of those
// modules is a part here without anybody having to say so twice. The consts above
// name the parts that exist today; this is the list, and it is the modules that
// decide what is on it.
func Parts() []string {
	var out []string
	for _, s := range Sets() {
		for _, m := range s.Migrations {
			out = append(out, m.Name)
		}
	}
	return out
}

// SetOf names the set that carries a part, and whether any does.
//
// Callers that have to apply a part, or write down which module a project depends
// on for it, ask here rather than matching on the part name — the mapping is the
// sets' to state and this is the one place that reads it.
func SetOf(part string) (dbschema.Set, bool) {
	for _, s := range Sets() {
		if _, ok := s.Find(part); ok {
			return s, true
		}
	}
	return dbschema.Set{}, false
}

// SetsFor are the sets a list of parts needs, in apply order.
//
// A set appears once however many of its parts are named, because a migration set
// is applied whole: goose reads the directory, so asking for `sessions` and not
// `oauth` is not something a set can express. That is a real limit of leaving the
// schema to the modules, and the honest place to state it — a project that wants
// a subset of auth's tables vendors the foundation and skips the parts it does not
// want.
func SetsFor(parts []string) []dbschema.Set {
	var out []dbschema.Set
	for _, s := range Sets() {
		for _, m := range s.Migrations {
			if slices.Contains(parts, m.Name) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// ImportPath is where a set's Go package lives, for generated code that has to
// name it in an import.
//
// Derived from the module rather than listed, because the layout is a convention
// this repository keeps: `rig/auth`'s set is `rig/auth/foundation`. Deriving it
// means a fourth module carrying a set needs nothing here.
func ImportPath(s dbschema.Set) string {
	return modulePrefix + s.Module + "/foundation"
}

// modulePrefix is where rig's modules live. Spelled once so that the derived
// import paths and a future move stay one edit.
const modulePrefix = "github.com/simonjanss/"

// BookkeepingTables are the tables the foundation's sets record themselves in.
//
// They carry the `rig_` prefix like everything else rig creates, and introspection
// returns them like any other table — but they arrive through a different door:
// goose writes them, no migration creates them. So two rules have to know about
// them. The reserved prefix would otherwise refuse them as tables under rig's
// prefix that no migration created, and the ignore list would otherwise let rig
// project a CRUD resource over its own migration history.
//
// The project's own bookkeeping table is not here. It is named in rig.yaml and
// [github.com/simonjanss/rig/internal/compile] reads it from there.
func BookkeepingTables() []string {
	var out []string
	for _, s := range Sets() {
		out = append(out, s.Table)
	}
	return out
}

// Wanted says which of the foundation's features a project's configuration turns
// on. It is the input to [Wanted.Parts].
type Wanted struct {
	// Auth is an `auth:` block, which needs identities, tokens and keys.
	Auth bool
	// OAuth is a provider configured under `auth.oauth`.
	//
	// It brings no part [Wanted.Auth] does not: oauth is a migration in auth's
	// set, and a set is applied whole. Read rather than assumed all the same,
	// because it is what the configuration says.
	OAuth bool
	// Files is `files.enabled`.
	Files bool
	// Notifications is `notifications.enabled`.
	Notifications bool
	// Presence is `presence.enabled`.
	Presence bool

	// There is no Idempotency field. Every project brings that part — see
	// [Wanted.Parts] — so a bool here could only ever be true, and a knob with
	// one setting is a question somebody has to answer for no reason.
}

// Parts names the parts these features bring, in apply order, with [Requires]
// expanded and every named set taken whole.
//
// This is the other answer to the question [Managed] answers. A project that
// vendored the foundation says which parts it has by having their migrations; a
// project that left them to the modules has only its configuration to say so, and
// this is that reading. `rig setup-project` decides what to write the same way,
// which is what keeps the two from disagreeing about a project it set up.
//
// **Brings, not asks for, and the difference is the whole of it.** A set is
// applied whole — goose reads a directory, so `sessions but not oauth` is not
// something a set can express — so an `auth:` block with no provider configured
// still ends up with rig_identity_oauth in the database, and a project with an
// inbox and no authentication ends up with all of auth's. Answering with the
// narrower list would have rig report RIG2005 on a table it created itself one
// command earlier, and then, with the table missing from the ignore list, project
// a resource over it. So the widening happens here rather than in each caller:
// there is one answer to "which of rig's tables does this project have", and no
// way to reach for the wrong one.
func (w Wanted) Parts() []string {
	want := map[string]bool{}
	// Declared before it is assigned because it recurses: a part pulls in what it
	// requires, and what those require in turn.
	var add func(parts ...string)
	add = func(parts ...string) {
		for _, p := range parts {
			if want[p] {
				continue
			}
			want[p] = true
			add(Requires(p)...)
		}
	}

	if w.Auth {
		// Keys and sessions come with authentication rather than as questions of
		// their own: the token table and the log declare their key columns where
		// they are created, so a project with sessions and no rig_api_key would
		// need a second shape of the same table.
		add(PartTenancy, PartAPIKeys, PartSessions)
	}
	if w.OAuth {
		add(PartOAuth)
	}
	if w.Files {
		add(PartFiles)
	}
	if w.Notifications {
		add(PartNotifications)
	}
	if w.Presence {
		add(PartPresence)
	}

	// Unconditionally, and it is the only part that is. Every generated project
	// has write endpoints, and what rig_idempotency does for them is engaged by
	// a request header rather than by a configuration — so a project that had to
	// turn it on is a project whose clients cannot rely on it being on, which is
	// most of the value gone. It costs one table nobody reads until a caller
	// sends a key.
	add(PartIdempotency)

	// To a fixed point rather than once, because a part that arrives with its set
	// may require one whose set is not here yet — which is exactly the shape of
	// the cross-module edge notifications already has.
	for {
		before := len(want)
		for _, s := range SetsFor(inApplyOrder(want)) {
			for _, m := range s.Migrations {
				add(m.Name)
			}
		}
		if len(want) == before {
			return inApplyOrder(want)
		}
	}
}

// inApplyOrder turns a set of part names into the order they apply in.
func inApplyOrder(want map[string]bool) []string {
	var out []string
	for _, p := range Parts() {
		if want[p] {
			out = append(out, p)
		}
	}
	return out
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
	//
	// It is also the upgrade path. The sets are append-only, so a module that has
	// gained a migration since this project was set up has a part nothing here
	// matches, and it is written at the next free number — leaving what is already
	// applied exactly as it was.
	Existing []string
	// ConfigsOnly leaves the migrations out and writes only the table
	// configurations for whatever [FoundationOptions.Expose] names.
	//
	// It is `migrations.foundation: embedded`: the modules carry the schema, so
	// there is no migration for this project to keep. Exposing a table is still a
	// question for the project, because it is about rig.yaml rather than about
	// DDL — a project can leave the tables to the modules and still want a model
	// and a repository for one of them.
	//
	// False is vendored, which is the default, so the zero value writes the
	// migrations as it always has.
	ConfigsOnly bool
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
		if !opt.ConfigsOnly && !alreadyApplied(opt.Existing, part) {
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
// project can tell at a glance which tables arrived with the foundation. Both
// halves of that name are rig's and rig refuses a project either: the prefix
// itself ([TablePrefix]) and the API name the prefix strips to
// ([ReservedResources]), so no project has an `account` or a `tenant` of its
// own and exposing this one later is never a rename.
//
// The names come from the owning module's manifest, where they are written out
// rather than parsed from the SQL — so that adding a table to the foundation is a
// decision somebody makes there as well. A heuristic would quietly start or stop
// treating one as the module's, and which side of that line a table falls on
// decides whether rig generates code for it.
func PartTables(part string) []string {
	for _, s := range Sets() {
		if m, ok := s.Find(part); ok {
			return m.Tables
		}
	}
	return nil
}

// PartOf names the part that creates a table, or "" for a table the foundation
// does not create.
//
// The inverse of [PartTables], and it exists for a diagnostic. A project whose
// rig_account arrived through a migration [Managed] cannot recognise has to be
// told which name that migration needs, and the part is that name: `Managed`
// matches on the `_rig_<part>.sql` suffix and nothing else.
func PartOf(table string) string {
	for _, part := range Parts() {
		if slices.Contains(PartTables(part), table) {
			return part
		}
	}
	return ""
}

// Tables are every table the foundation creates.
func Tables() []string {
	var out []string
	for _, part := range Parts() {
		out = append(out, PartTables(part)...)
	}
	return out
}

// AppliedParts are the parts a project's own migration files show it has, in
// apply order.
//
// The evidence is the filenames: a part rig wrote is named for itself, so a
// project that vendored the foundation says which parts it has in its own
// migrations directory. That matters twice over. It decides whether rig generates
// a model and a repository for a table, and it is what tells a `rig_account` rig
// created from one somebody wrote by hand — which is refused rather than
// generated for, now that [TablePrefix] is reserved.
//
// A name alone cannot answer either question, which is why this reads the
// directory. [Wanted.Parts] is the other reading, for a project whose modules
// carry their own schema and which therefore has no files here to read.
func AppliedParts(existing []string) []string {
	var out []string
	for _, part := range Parts() {
		if alreadyApplied(existing, part) {
			out = append(out, part)
		}
	}
	return out
}

// Managed are the foundation tables a project has actually applied, read from its
// own migration files. It is [AppliedParts] in terms of tables.
//
// `auth.own` is the exception and it turns the whole thing off: a project that has
// taken the schema over owns those tables and their names.
func Managed(existing []string) []string {
	return TablesFor(AppliedParts(existing))
}

// TablesFor are the tables a list of parts creates, in apply order.
func TablesFor(parts []string) []string {
	var out []string
	for _, part := range Parts() {
		if slices.Contains(parts, part) {
			out = append(out, PartTables(part)...)
		}
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
	case PartPresence:
		// A presence row names an account and a tenant, both of which the
		// tenancy part creates. Not sessions, for the reason above: being
		// present needs claims and this schema does not care where they came
		// from — and deliberately not a foreign key to a session either, because
		// the identity of a presence is the *tab*, and one sign-in has several.
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
	table    string
	resource string
	content  string
}

// migrationSQL is every part's DDL, read once out of the sets.
//
// Read at init because a part named in a manifest with no file behind it is a
// build-time mistake rather than a condition to handle: both halves ship in the
// same package, and every module carrying a set calls [dbschema.Set.Validate] in
// a test of its own. Reaching the panic would mean somebody shipped a set that
// could not have passed it.
var migrationSQL = func() map[string]string {
	out := map[string]string{}
	for _, s := range Sets() {
		for _, m := range s.Migrations {
			b, err := s.Read(m.Name)
			if err != nil {
				panic(fmt.Sprintf("scaffold: %v", err))
			}
			out[m.Name] = string(b)
		}
	}
	return out
}()

// foundationPart pairs a part's DDL with the table configurations that describe
// it.
//
// The two halves come from different places on purpose. The migration belongs to
// the module whose code depends on the schema, so it travels with that module.
// The configuration is rig.yaml — how a project would expose the table if it
// wanted to — so it stays here, next to the resource names [ReservedResources]
// reads back out of it.
func foundationPart(name string) part {
	p := part{migration: migrationSQL[name]}
	switch name {
	case PartTenancy:
		p.configs = tenancyConfigs()
	case PartSessions:
		p.configs = sessionConfigs()
	case PartAPIKeys:
		p.configs = apiKeyConfigs()
	case PartOAuth:
		p.configs = oauthConfigs()
	case PartFiles:
		p.configs = fileConfigs()
	case PartNotifications:
		p.configs = notificationConfigs()
	case PartPresence:
		p.configs = presenceConfigs()
	case PartIdempotency:
		p.configs = idempotencyConfigs()
	}
	return p
}

// config renders a table configuration file, and is the one place either name
// is written.
//
// The resource name is written out rather than derived, because the physical
// name carries the `rig_` prefix and the API name should not: the prefix is
// there so a project can tell its own tables from the foundation's in psql, and
// a client has no business knowing which library created a table. Without it an
// exposed table would arrive as RigAccount on /rig-accounts.
//
// It returns the whole [tableConfig] rather than the file's text so that the
// resource name survives as a value. [ReservedResources] reads it back, which is
// what makes a name reserved by being written here and nowhere else — a second
// list would drift the first time somebody added a part.
//
// Column comments are not repeated here: the migrations carry COMMENT ON for
// every one of them, so they arrive through introspection. Saying them twice
// would mean two places to edit and one place to forget.
func config(table, resource string, body ...string) tableConfig {
	var b strings.Builder
	fmt.Fprintf(&b, "# yaml-language-server: $schema=%s\n", schemaRef)
	fmt.Fprintf(&b, "table: %s\n", table)
	fmt.Fprintf(&b, "resource: %s\n", resource)
	for _, s := range body {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(s, "\n"))
		b.WriteString("\n")
	}
	return tableConfig{table: table, resource: resource, content: b.String()}
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
