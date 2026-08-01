package persistgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

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
		poolPkg  = b.Import("github.com/jackc/pgx/v5/pgxpool")
		dbxPkg   = b.Import(runtimeModule + "/dbx")
		auditPkg = b.Import(runtimeModule + "/audit")
		ctxPkg   = b.Import("context")
	)

	b.Comment("Pagination limits. A read without a limit is a production " +
		"incident waiting for the table to grow, so one is always applied.")
	b.L("const (")
	b.L("DefaultLimit = 50")
	b.L("MaxLimit     = 500")
	b.L(")")
	b.NL()

	b.Comment("Config is what a Store needs beyond a connection.")
	b.L("type Config struct {")
	b.L("// Audit receives the change log. Leave it nil to record nothing.")
	b.L("Audit %s.Log", auditPkg)
	b.L("}")
	b.NL()

	b.Comment("Store holds the connection pool and hands out repositories.")
	b.L("type Store struct {")
	b.L("pool  *%s.Pool", poolPkg)
	b.L("audit %s.Log", auditPkg)
	b.NL()
	for _, res := range e.resources() {
		b.L("%s %sRepository", res.Plural, res.Name)
	}
	b.L("}")
	b.NL()

	b.Comment("New builds a store over a connection pool.")
	b.L("func New(pool *%s.Pool, cfg Config) *Store {", poolPkg)
	b.L("s := &Store{pool: pool, audit: cfg.Audit}")
	b.L("if s.audit == nil { s.audit = %s.Noop{} }", auditPkg)
	for _, res := range e.resources() {
		b.L("s.%s = &%sRepo{db: s}", res.Plural, unexported(res.Name))
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

func unexported(name string) string {
	if name == "" {
		return name
	}
	return string(name[0]|0x20) + name[1:]
}

// helpers emit the small shared functions the repositories call, so the same
// four lines are not written into every method.
func (e *emitter) helpers(b *gobuf.Buf) {
	var (
		strPkg     = b.Import("strings")
		strcPkg    = b.Import("strconv")
		fmtPkg     = b.Import("fmt")
		reflectPkg = b.Import("reflect")
		errPkg     = b.Import(runtimeModule + "/rigerr")
		dbxPkg     = b.Import(runtimeModule + "/dbx")
		auditPkg   = b.Import(runtimeModule + "/audit")
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

	b.Comment("auditChange is one column's before and after, collected while an " +
		"update builds its statement.")
	b.L("type auditChange struct {")
	b.L("Column string")
	b.L("Type   string")
	b.L("Old    *string")
	b.L("New    *string")
	b.L("}")
	b.NL()

	b.L("func auditValues(changes ...auditChange) []%s.Value {", auditPkg)
	b.L("out := make([]%s.Value, 0, len(changes))", auditPkg)
	b.L("for _, c := range changes {")
	b.L("out = append(out, %s.Value{Column: c.Column, Type: c.Type, Old: c.Old, New: c.New})", auditPkg)
	b.L("}")
	b.L("return out")
	b.L("}")
	b.NL()

	b.Comment("render turns a value into the string the audit log stores.\n\n" +
		"Audit rows outlive the Go types that produced them, so a value nobody " +
		"can read back is not a record of anything.")
	b.L("func render(v any) *string {")
	b.L("if v == nil { return nil }")
	b.Comment("A nil *string arrives as a non-nil interface holding a nil pointer, " +
		"so a plain nil check misses it and the audit row would record the text " +
		"\"<nil>\" where it means no value at all.")
	b.L("rv := %s.ValueOf(v)", reflectPkg)
	b.L("switch rv.Kind() {")
	b.L("case %s.Pointer, %s.Slice, %s.Map:", reflectPkg, reflectPkg, reflectPkg)
	b.L("if rv.IsNil() { return nil }")
	b.L("if rv.Kind() == %s.Pointer { v = rv.Elem().Interface() }", reflectPkg)
	b.L("}")
	b.L("s := %s.Sprintf(\"%%v\", v)", fmtPkg)
	b.L("return &s")
	b.L("}")
	b.NL()
}
