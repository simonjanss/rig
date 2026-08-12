package persistgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// filterScopeType emits what a condition needs besides the filter itself.
//
// A filter is a tree, and rendering one is a walk: at each level the columns
// belong to a different table, the caller is the same, and the depth is one
// greater. Carrying that as a value is what lets a relation hand a changed copy
// down without every builder taking four parameters.
func (e *emitter) filterScopeType(b *gobuf.Buf) {
	var (
		queryPkg = b.Import(runtimeModule + "/query")
		errPkg   = b.Import(runtimeModule + "/rigerr")
		tenPkg   = b.Import(runtimeModule + "/tenancy")
		strPkg   = b.Import("strconv")
	)

	b.Comment("MaxFilterDepth bounds how far a filter may reach across " +
		"relations.\n\n" +
		"The filter types are mutually recursive, so a client can nest one as " +
		"deep as it likes, and each level is another correlated subquery. A bound " +
		"is the difference between an expensive query somebody asked for and an " +
		"arbitrarily expensive one they were allowed to.")
	b.L("const MaxFilterDepth = 3")
	b.NL()

	b.Comment("filterScope is what rendering one level of a filter needs.")
	b.L("type filterScope struct {")
	b.L("claims %s.Claims", tenPkg)
	b.Comment("as qualifies this level's columns. Inside a subquery two tables " +
		"are in scope, and an unqualified name there resolves to whichever one " +
		"happens to have it — a silent correlation to the wrong row rather than " +
		"an error.")
	b.L("as string")
	b.Comment("link qualifies the join table of a many-to-many at this level.")
	b.L("link  string")
	b.L("depth int")
	b.L("}")
	b.NL()

	b.Comment("newFilterScope starts at the table being read.")
	b.L("func newFilterScope(claims %s.Claims, table string) filterScope {", tenPkg)
	b.L("return filterScope{claims: claims, as: table}")
	b.L("}")
	b.NL()

	b.Comment("ok refuses a filter nested deeper than this will render.")
	b.L("func (s filterScope) ok() error {")
	b.L("if s.depth > MaxFilterDepth {")
	b.L("return %s.Invalid(\"a filter may reach across at most %%s relations\", %s.Itoa(MaxFilterDepth))",
		errPkg, strPkg)
	b.L("}")
	b.L("return nil")
	b.L("}")
	b.NL()

	b.Comment("at qualifies a condition with this level's alias.")
	b.L("func (s filterScope) at(c %s.Cond) %s.Cond {", queryPkg, queryPkg)
	b.L("c.Table = s.as")
	b.L("return c")
	b.L("}")
	b.NL()

	b.Comment("into opens the next level down, with aliases of its own so that a " +
		"self-reference at two depths does not shadow itself.")
	b.L("func (s filterScope) into() filterScope {")
	b.L("d := s.depth + 1")
	b.L("return filterScope{claims: s.claims, as: \"r\" + %s.Itoa(d), link: \"l\" + %s.Itoa(d), depth: d}",
		strPkg, strPkg)
	b.L("}")
	b.NL()

	b.Comment("belongsTo opens a subquery on the row this one points at.")
	b.L("func (s filterScope) belongsTo(farTable, farColumn, localColumn string) (filterScope, string, string) {")
	b.L("in := s.into()")
	b.L("return in, farTable + \" \" + in.as, in.as + \".\" + farColumn + \" = \" + s.as + \".\" + localColumn")
	b.L("}")
	b.NL()

	b.Comment("hasMany opens a subquery on the rows that point at this one.")
	b.L("func (s filterScope) hasMany(farTable, farColumn, pk string) (filterScope, string, string) {")
	b.L("in := s.into()")
	b.L("return in, farTable + \" \" + in.as, in.as + \".\" + farColumn + \" = \" + s.as + \".\" + pk")
	b.L("}")
	b.NL()

	b.Comment("throughLink opens a subquery on the far side of a join table.\n\n" +
		"The condition is on the far resource and the link is only how you get " +
		"there, which is why this is a chain rather than a table name.")
	b.L("func (s filterScope) throughLink(linkTable, nearColumn, farTable, farColumn, pk string) (filterScope, string, string) {")
	b.L("in := s.into()")
	b.L("from := linkTable + \" \" + in.link +")
	b.L("\" JOIN \" + farTable + \" \" + in.as +")
	b.L("\" ON \" + in.as + \".id = \" + in.link + \".\" + farColumn")
	b.L("return in, from, in.link + \".\" + nearColumn + \" = \" + s.as + \".\" + pk")
	b.L("}")
	b.NL()

	b.Comment("tenant scopes a subquery to the caller's tenant.\n\n" +
		"This is the predicate the whole design turns on. Without it a condition " +
		"on a related table is answered from every tenant's rows: the caller " +
		"cannot read the far row, but learns it exists from which of their own " +
		"rows come back.")
	b.L("func (s filterScope) tenant(column string) %s.Cond {", queryPkg)
	b.L("return s.at(%s.Eq(column, s.claims.TenantID))", queryPkg)
	b.L("}")
	b.NL()

	b.Comment("live excludes rows somebody retired, so a deleted row is not a way " +
		"to find its neighbours.")
	b.L("func (s filterScope) live(column string) %s.Cond {", queryPkg)
	b.L("return s.at(%s.IsNull(column))", queryPkg)
	b.L("}")
	b.NL()

	b.Comment("original excludes the history, which is never what a condition on " +
		"a relation means.")
	b.L("func (s filterScope) original(column string, value any) %s.Cond {", queryPkg)
	b.L("return s.at(%s.Eq(column, value))", queryPkg)
	b.L("}")
	b.NL()

	b.Comment("aliased is this scope under a different name, for the far side of a " +
		"join.\n\n" +
		"The alias space is separate from the one a subquery uses — o1 here, r1 " +
		"there — because a join lives in the same statement as the conditions and " +
		"a subquery that reused the name would resolve its own columns against it.")
	b.L("func (s filterScope) aliased(alias string) filterScope {")
	b.L("return filterScope{claims: s.claims, as: alias, depth: s.depth}")
	b.L("}")
	b.NL()

	b.Comment("orderJoin reaches a related row so that its columns can be ordered " +
		"by, and returns the scope its predicates belong to.\n\n" +
		"LEFT rather than INNER, and that is the whole reason this is a join at " +
		"all: it is here to read a value, not to decide anything. An inner join " +
		"would drop every row whose foreign key is null, so asking for an order " +
		"would quietly shorten the list.")
	b.L("func (s filterScope) orderJoin(farTable, alias, farColumn, localColumn string) (filterScope, %s.Join) {",
		queryPkg)
	b.L("return s.aliased(alias), %s.Join{", queryPkg)
	b.L("Table: farTable + \" \" + alias,")
	b.L("On:    alias + \".\" + farColumn + \" = \" + s.as + \".\" + localColumn,")
	b.L("}")
	b.L("}")
	b.NL()
}

// storeFile emits the object that holds the connection and hands out
// repositories, plus the helpers every generated method shares.
func (e *emitter) storeFile() (gen.Artifact, error) {
	b := gobuf.New(e.pkg)
	b.Doc("Package " + e.pkg + " is the generated persistence layer. " +
		"Business logic belongs in the service layer above it, not here: every " +
		"file in this package is rewritten whenever the schema changes.")

	e.storeType(b)
	e.helpers(b)

	return artifact("store.gen.go", b)
}

func (e *emitter) storeType(b *gobuf.Buf) {
	var (
		poolPkg = b.Import("github.com/jackc/pgx/v5/pgxpool")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		ctxPkg  = b.Import("context")
	)

	b.Comment("Pagination limits. A read without a limit is a production " +
		"incident waiting for the table to grow, so one is always applied.")
	b.L("const (")
	b.L("DefaultLimit = 50")
	b.L("MaxLimit     = 500")
	b.L(")")
	b.NL()

	e.filterScopeType(b)

	b.Comment("Config is what a Store needs beyond a connection.\n\n" +
		"It is empty today. It is still taken, and taken by value, so that " +
		"giving a store something to hold is a new field rather than a " +
		"signature change every caller has to follow.")
	b.L("type Config struct{}")
	b.NL()

	b.Comment("Store holds the connection pool and hands out repositories.")
	b.L("type Store struct {")
	b.L("pool *%s.Pool", poolPkg)
	b.NL()
	for _, res := range e.resources() {
		b.L("%s %sRepository", res.Plural, res.Name)
	}
	b.L("}")
	b.NL()

	b.Comment("New builds a store over a connection pool.")
	b.L("func New(pool *%s.Pool, _ Config) *Store {", poolPkg)
	b.L("s := &Store{pool: pool}")
	for _, res := range e.resources() {
		b.L("s.%s = &%s{db: s}", res.Plural, repoTypeName(res))
	}
	b.L("return s")
	b.L("}")
	b.NL()

	b.Comment("conn returns the transaction on the context when there is one, and " +
		"the pool otherwise.\n\n" +
		"This is what lets a repository method work the same whether it was " +
		"called directly or inside a transaction someone else opened.")
	b.L("func (s *Store) conn() %s.Conn { return s.connFor(%s.Background()) }", dbxPkg, ctxPkg)
	b.NL()
	b.L("func (s *Store) connFor(ctx %s.Context) %s.Conn {", ctxPkg, dbxPkg)
	b.L("if tx, ok := %s.Tx(ctx); ok { return tx }", dbxPkg)
	b.L("return s.pool")
	b.L("}")
	b.NL()

	b.Comment("Pool exposes the underlying pool, for the rare query the generated " +
		"repositories do not cover.")
	b.L("func (s *Store) Pool() *%s.Pool { return s.pool }", poolPkg)
	b.NL()

	b.Comment("InTx runs fn inside one transaction. Repository calls made with the " +
		"context it provides join that transaction rather than opening their own.")
	b.L("func (s *Store) InTx(ctx %s.Context, fn func(ctx %s.Context) error) error {", ctxPkg, ctxPkg)
	b.L("return %s.InTx(ctx, s.pool, func(ctx %s.Context, _ %s.Conn) error { return fn(ctx) })",
		dbxPkg, ctxPkg, dbxPkg)
	b.L("}")
	b.NL()
}

// resources are the resources with storage, in document order.
func (e *emitter) resources() []*ir.Resource {
	var out []*ir.Resource
	for i := range e.doc.API.Resources {
		if e.doc.API.Resources[i].Storage != nil {
			out = append(out, &e.doc.API.Resources[i])
		}
	}
	return out
}

// helpers emit the small shared functions the repositories call, so the same
// four lines are not written into every method.
func (e *emitter) helpers(b *gobuf.Buf) {
	var (
		strPkg  = b.Import("strings")
		strcPkg = b.Import("strconv")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
	)

	b.Comment("joinColumns renders a column list. The names come from generated " +
		"constants, never from a request.")
	b.L("func joinColumns(columns []string) string { return %s.Join(columns, \", \") }", strPkg)
	b.NL()

	b.Comment("placeholders renders $1, $2, ... for n values.")
	b.L("func placeholders(n int) string {")
	b.L("parts := make([]string, n)")
	b.L("for i := range parts { parts[i] = \"$\" + %s.Itoa(i+1) }", strcPkg)
	b.L("return %s.Join(parts, \", \")", strPkg)
	b.L("}")
	b.NL()

	b.Comment("assignments renders `col = $1, col = $2, ...` for an update.")
	b.L("func assignments(columns []string) string {")
	b.L("parts := make([]string, len(columns))")
	b.L("for i, c := range columns { parts[i] = c + \" = $\" + %s.Itoa(i+1) }", strcPkg)
	b.L("return %s.Join(parts, \", \")", strPkg)
	b.L("}")
	b.NL()

	b.Comment("writeError turns a database failure into one a client can act on.\n\n" +
		"A constraint violation is the caller's mistake and deserves a status " +
		"that says so; anything else is the server's problem and says nothing " +
		"more, because the detail of an internal failure is exactly the kind of " +
		"thing that leaks a table name.")
	b.L("func writeError(err error, table string) error {")
	b.L("switch {")
	b.L("case %s.IsUniqueViolation(err):", dbxPkg)
	b.L("return %s.Conflict(\"that %%s already exists\", table).Wrap(err)", errPkg)
	b.L("case %s.IsForeignKeyViolation(err):", dbxPkg)
	b.L("return %s.Conflict(\"a related row is missing or still in use\").Wrap(err)", errPkg)
	b.L("case %s.IsCheckViolation(err):", dbxPkg)
	b.L("return %s.Invalid(\"the %%s failed a database constraint\", table).Wrap(err)", errPkg)
	b.L("case %s.IsNotNullViolation(err):", dbxPkg)
	b.L("return %s.Invalid(\"a required %%s field was not given\", table).Wrap(err)", errPkg)
	b.L("default:")
	b.L("return %s.Internal(err, \"write %%s\", table)", errPkg)
	b.L("}")
	b.L("}")
	b.NL()
}
