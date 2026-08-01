package persistgo

import (
	"fmt"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// repositoryFile emits the interface and its pgx implementation.
func (e *emitter) repositoryFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)

	e.repositoryInterface(b, res)
	e.repositoryImpl(b, res)

	return artifact(naming.Snake(res.Name)+"_repository.gen.go", b)
}

func (e *emitter) repositoryInterface(b *gobuf.Buf, res *ir.Resource) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		optPkg  = b.Import(runtimeModule + "/readopt")
	)
	s := res.Storage

	b.Comment(res.Name + "Repository reads and writes " + s.Table + " rows.\n\n" +
		"Every method scopes to the caller's tenant. There is no way to ask it not " +
		"to from a request handler, which is the point.")
	b.L("type %sRepository interface {", res.Name)

	b.Comment("Get returns one row by identifier.\n\n" +
		"A lookup by primary key deliberately ignores the lifecycle filters: it " +
		"returns the row whether it is live, deleted, or a snapshot, because a " +
		"caller holding an identifier is usually asking about that exact row.")
	b.L("Get(ctx %s.Context, id %s.UUID, opts ...%s.Option) (*%s, error)",
		ctxPkg, uuidPkg, optPkg, res.Name)
	b.NL()

	b.Comment("List returns matching rows and the total ignoring pagination.")
	b.L("List(ctx %s.Context, q %sQuery, opts ...%s.Option) ([]*%s, int64, error)",
		ctxPkg, res.Name, optPkg, res.Name)
	b.NL()

	b.Comment("Create inserts a row, stamping the identifier, tenant and audit columns.")
	b.L("Create(ctx %s.Context, in *%sCreate) (*%s, error)", ctxPkg, res.Name, res.Name)
	b.NL()

	if s.IsSnapshotable() {
		b.Comment("Update changes a row, writing a snapshot of the previous version first.")
	} else {
		b.Comment("Update changes the fields the input mentions and leaves the rest alone.")
	}
	b.L("Update(ctx %s.Context, id %s.UUID, in *%sUpdate) (*%s, error)",
		ctxPkg, uuidPkg, res.Name, res.Name)
	b.NL()

	if s.IsSoftDeletable() {
		b.Comment("Delete retires a row by stamping its deletion time. It is idempotent.")
	} else {
		b.Comment("Delete removes a row.")
	}
	b.L("Delete(ctx %s.Context, in %sDelete) error", ctxPkg, res.Name)

	if s.IsSoftDeletable() {
		b.NL()
		b.Comment("Restore brings a deleted row back, if it is still inside the window.")
		b.L("Restore(ctx %s.Context, id %s.UUID) (*%s, error)", ctxPkg, uuidPkg, res.Name)
		b.NL()
		b.Comment("ListDeleted returns retired rows still inside the restore window.")
		b.L("ListDeleted(ctx %s.Context, q %sQuery) ([]*%s, int64, error)", ctxPkg, res.Name, res.Name)
	}

	if s.IsSnapshotable() {
		b.NL()
		b.Comment("ListSnapshots returns a row's previous versions, newest first.")
		b.L("ListSnapshots(ctx %s.Context, id %s.UUID) ([]*%s, error)", ctxPkg, uuidPkg, res.Name)
	}

	b.L("}")
	b.NL()
}

// repo is the generated implementation's receiver name.
const repo = "r"

func (e *emitter) repositoryImpl(b *gobuf.Buf, res *ir.Resource) {
	typeName := naming.New(naming.Config{}).GoUnexported(res.Name) + "Repo"

	b.L("type %s struct {", typeName)
	b.L("db  *Store")
	b.L("}")
	b.NL()

	b.L("var _ %sRepository = (*%s)(nil)", res.Name, typeName)
	b.NL()

	e.scanHelpers(b, res, typeName)
	e.getMethod(b, res, typeName)
	e.listMethod(b, res, typeName)
	e.createMethod(b, res, typeName)
	e.updateMethod(b, res, typeName)
	e.deleteMethod(b, res, typeName)

	if res.Storage.IsSoftDeletable() {
		e.restoreMethod(b, res, typeName)
		e.listDeletedMethod(b, res, typeName)
	}
	if res.Storage.IsSnapshotable() {
		e.listSnapshotsMethod(b, res, typeName)
	}
}

// scanHelpers emit the select list and the row scanner, so no method spells
// out the column order twice.
func (e *emitter) scanHelpers(b *gobuf.Buf, res *ir.Resource, typeName string) {
	pgxPkg := b.Import("github.com/jackc/pgx/v5")
	t := e.table(res)

	columns := make([]string, 0, len(t.Columns))
	for i := range t.Columns {
		columns = append(columns, t.Columns[i].Name)
	}

	b.L("const %sSelect = %s", typeName, gobuf.Quote(strings.Join(columns, ", ")))
	b.NL()

	b.Comment("scan" + res.Name + " reads one row in the order " + typeName + "Select lists.")
	b.L("func scan%s(row %s.Row) (*%s, error) {", res.Name, pgxPkg, res.Name)
	b.L("var m %s", res.Name)
	b.P("if err := row.Scan(")
	for i := range t.Columns {
		if i > 0 {
			b.P(", ")
		}
		field := e.fieldForColumn(res, t.Columns[i].Name)
		if field == "" {
			// A column excluded from the API still has to be scanned, because
			// the select list is the table's. It goes nowhere.
			b.P("new(%s)", e.scanPlaceholder(b, &t.Columns[i]))
			continue
		}
		b.P("&m.%s", field)
	}
	b.L("); err != nil {")
	b.L("return nil, err")
	b.L("}")
	b.L("return &m, nil")
	b.L("}")
	b.NL()
}

// fieldForColumn returns the model field a column maps to, or empty when the
// column is not exposed.
func (e *emitter) fieldForColumn(res *ir.Resource, column string) string {
	for _, f := range res.Fields {
		if f.Column != nil && f.Column.Name == column {
			return f.Name
		}
	}
	return ""
}

// scanPlaceholder is a type wide enough to receive a column nobody reads.
func (e *emitter) scanPlaceholder(b *gobuf.Buf, c *ir.Column) string {
	_ = c
	return "any"
}

// tenantFilter renders the predicate every read and write is scoped by.
func (e *emitter) tenantFilter(res *ir.Resource) (column string, ok bool) {
	if res.Storage.Tenant == nil {
		return "", false
	}
	return res.Storage.Tenant.Name, true
}

func (e *emitter) getMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		optPkg  = b.Import(runtimeModule + "/readopt")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		fmtPkg  = b.Import("fmt")
	)

	b.Comment("Get implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Get(ctx %s.Context, id %s.UUID, opts ...%s.Option) (*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, optPkg, res.Name)
	b.L("cfg, err := %s.Apply(opts)", optPkg)
	b.L("if err != nil { return nil, err }")
	b.NL()
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.NL()

	b.L("args := []any{id}")
	b.L("where := \"id = $1\"")
	if column, ok := e.tenantFilter(res); ok {
		b.L("if !cfg.SkipTenantScope {")
		b.L("args = append(args, claims.TenantID)")
		b.L("where += %s", gobuf.Quote(" AND "+column+" = $2"))
		b.L("}")
	}
	b.NL()

	b.L("sql := %s.Sprintf(\"SELECT %%s FROM %s WHERE %%s\", %sSelect, where)",
		fmtPkg, res.Storage.Table, typeName)
	b.L("m, err := scan%s(%s.db.conn().QueryRow(ctx, sql, args...))", res.Name, repo)
	b.L("if %s.IsNoRows(err) {", dbxPkg)
	b.L("return nil, %s.NotFound(\"no %s with id %%s\", id)", errPkg, res.Name)
	b.L("}")
	b.L("if err != nil {")
	b.L("return nil, %s.Internal(err, \"read %s\")", errPkg, res.Storage.Table)
	b.L("}")
	b.L("return m, nil")
	b.L("}")
	b.NL()
}

func (e *emitter) listMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	e.listLike(b, res, typeName, "List", "", false)
}

func (e *emitter) listDeletedMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	e.listLike(b, res, typeName, "ListDeleted",
		"ListDeleted returns retired rows still inside the restore window.", true)
}

// listLike emits List and its deleted-only variant, which differ only in the
// options they force.
func (e *emitter) listLike(b *gobuf.Buf, res *ir.Resource, typeName, method, doc string, onlyDeleted bool) {
	var (
		ctxPkg  = b.Import("context")
		optPkg  = b.Import(runtimeModule + "/readopt")
		queryPk = b.Import(runtimeModule + "/query")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		fmtPkg  = b.Import("fmt")
	)

	if doc == "" {
		doc = method + " implements " + res.Name + "Repository."
	}
	b.Comment(doc)

	if onlyDeleted {
		b.L("func (%s *%s) %s(ctx %s.Context, q %sQuery) ([]*%s, int64, error) {",
			repo, typeName, method, ctxPkg, res.Name, res.Name)
		b.L("return %s.list(ctx, q, []%s.Option{%s.WithOnlyDeleted()})", repo, optPkg, optPkg)
		b.L("}")
		b.NL()
		return
	}

	b.L("func (%s *%s) %s(ctx %s.Context, q %sQuery, opts ...%s.Option) ([]*%s, int64, error) {",
		repo, typeName, method, ctxPkg, res.Name, optPkg, res.Name)
	b.L("return %s.list(ctx, q, opts)", repo)
	b.L("}")
	b.NL()

	// The shared implementation.
	b.Comment("list is the body of every read that takes a query.")
	b.L("func (%s *%s) list(ctx %s.Context, q %sQuery, opts []%s.Option) ([]*%s, int64, error) {",
		repo, typeName, ctxPkg, res.Name, optPkg, res.Name)
	b.L("cfg, err := %s.Apply(opts)", optPkg)
	b.L("if err != nil { return nil, 0, err }")
	b.NL()
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, 0, err }")
	b.NL()

	b.L("args := %s.NewArgs()", queryPk)
	b.L("scope := %s.Group{}", queryPk)
	e.lifecycleFilters(b, res, queryPk)
	b.NL()
	b.L("scope.Nest(q.group())")
	b.NL()
	b.L("where := scope.SQL(args)")
	b.L("if where != \"\" { where = \" WHERE \" + where }")
	b.NL()

	b.L("countSQL := %s.Sprintf(\"SELECT count(*) FROM %s%%s\", where)", fmtPkg, res.Storage.Table)
	b.L("var total int64")
	b.L("if err := %s.db.conn().QueryRow(ctx, countSQL, args.Values()...).Scan(&total); err != nil {", repo)
	b.L("return nil, 0, %s.Internal(err, \"count %s\")", errPkg, res.Storage.Table)
	b.L("}")
	b.NL()

	b.L("order := q.OrderBy")
	b.L("if len(order) == 0 { order = %sDefaultOrder }", res.Name)
	b.L("page := %s.Page{Limit: q.Limit, Offset: q.Offset}.Clamp(DefaultLimit, MaxLimit)", queryPk)
	b.NL()

	b.L("listSQL := %s.Sprintf(\"SELECT %%s FROM %s%%s%%s%%s\", %sSelect, where, %s.OrderSQL(order), page.SQL(args))",
		fmtPkg, res.Storage.Table, typeName, queryPk)
	b.L("rows, err := %s.db.conn().Query(ctx, listSQL, args.Values()...)", repo)
	b.L("if err != nil {")
	b.L("return nil, 0, %s.Internal(err, \"list %s\")", errPkg, res.Storage.Table)
	b.L("}")
	b.L("defer rows.Close()")
	b.NL()
	b.L("out := make([]*%s, 0, page.Limit)", res.Name)
	b.L("for rows.Next() {")
	b.L("m, err := scan%s(rows)", res.Name)
	b.L("if err != nil {")
	b.L("return nil, 0, %s.Internal(err, \"read %s\")", errPkg, res.Storage.Table)
	b.L("}")
	b.L("out = append(out, m)")
	b.L("}")
	b.L("if err := rows.Err(); err != nil {")
	b.L("return nil, 0, %s.Internal(err, \"list %s\")", errPkg, res.Storage.Table)
	b.L("}")
	b.L("return out, total, nil")
	b.L("}")
	b.NL()

	e.defaultOrder(b, res, queryPk)
}

// lifecycleFilters emit the predicates every query-based read carries whether
// or not it asked for them.
func (e *emitter) lifecycleFilters(b *gobuf.Buf, res *ir.Resource, queryPk string) {
	s := res.Storage

	if column, ok := e.tenantFilter(res); ok {
		b.L("if !cfg.SkipTenantScope {")
		b.L("scope.Add(%s.Eq(%s, claims.TenantID))", queryPk, gobuf.Quote(column))
		b.L("}")
	}

	if s.IsSoftDeletable() {
		column := gobuf.Quote(s.SoftDelete.Column.Name)
		b.L("switch {")
		b.L("case cfg.OnlyDeleted:")
		b.L("scope.Add(%s.NotNull(%s))", queryPk, column)
		b.Comment("A row past the restore window is gone as far as anyone is " +
			"concerned, so it does not appear in the trash either.")
		b.L("scope.Add(%s.Gte(%s, %sRestoreCutoff()))", queryPk, column, res.Name)
		b.L("case !cfg.IncludeDeleted:")
		b.L("scope.Add(%s.IsNull(%s))", queryPk, column)
		b.L("}")
	}

	if s.IsSnapshotable() {
		b.L("if !cfg.IncludeSnapshots {")
		b.L("scope.Add(%s.Eq(%s, %s))", queryPk,
			gobuf.Quote(s.Snapshot.VersionType.Name), e.versionOriginal(res))
		b.L("}")
	}
}

// versionOriginal is the expression for the live-version enum value.
func (e *emitter) versionOriginal(res *ir.Resource) string {
	col := e.doc.Resolve(res.Storage.Snapshot.VersionType)
	if col == nil || col.EnumType == "" {
		return gobuf.Quote("Original")
	}
	for _, enum := range e.doc.API.Enums {
		if enum.PgType != col.EnumType {
			continue
		}
		for _, v := range enum.Values {
			if v.Wire == "Original" {
				return enum.Name + v.Name
			}
		}
	}
	return gobuf.Quote("Original")
}

func (e *emitter) defaultOrder(b *gobuf.Buf, res *ir.Resource, queryPk string) {
	b.Comment(res.Name + "DefaultOrder is the ordering used when a query asks for none.\n\n" +
		"It always ends with the primary key, so the order is total and a page " +
		"boundary cannot repeat or skip a row.")
	b.P("var %sDefaultOrder = []%s.Order{", res.Name, queryPk)
	terms := res.Storage.DefaultOrder
	for i, t := range terms {
		if i > 0 {
			b.P(", ")
		}
		b.P("{Column: %s, Desc: %t}", gobuf.Quote(t.Column), t.Desc)
	}
	b.L("}")
	b.NL()
}

// insertColumns are the columns a create writes, in a stable order.
func (e *emitter) insertColumns(res *ir.Resource) []insertColumn {
	s := res.Storage
	var out []insertColumn

	out = append(out, insertColumn{Column: "id", Expr: "id"})
	if s.Tenant != nil {
		out = append(out, insertColumn{Column: s.Tenant.Name, Expr: "claims.TenantID"})
	}
	if s.Audit != nil {
		if s.Audit.CreatedAt != nil {
			out = append(out, insertColumn{Column: s.Audit.CreatedAt.Name, Expr: "now"})
		}
		if s.Audit.CreatedBy != nil {
			out = append(out, insertColumn{Column: s.Audit.CreatedBy.Name, Expr: "claims.Actor()"})
		}
	}
	if s.IsSnapshotable() {
		out = append(out, insertColumn{
			Column: s.Snapshot.VersionType.Name,
			Expr:   "versionType",
		})
	}

	for _, f := range writableFields(res, ir.FieldOpCreate) {
		out = append(out, insertColumn{Column: f.Column.Name, Expr: "in." + f.Name})
	}
	return out
}

type insertColumn struct {
	Column string
	Expr   string
}

func (e *emitter) createMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		fmtPkg  = b.Import("fmt")
		timePkg = b.Import("time")
	)
	s := res.Storage

	b.Comment("Create implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Create(ctx %s.Context, in *%sCreate) (*%s, error) {",
		repo, typeName, ctxPkg, res.Name, res.Name)
	b.L("if in == nil { return nil, %s.BadRequest(\"no %s to create\") }", errPkg, res.Name)
	b.NL()
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.NL()

	b.Comment("A version 7 identifier sorts by creation time, so an index on the " +
		"primary key stays dense as rows are added.")
	b.L("id, err := %s.NewV7()", uuidPkg)
	b.L("if err != nil { return nil, %s.Internal(err, \"generate an identifier\") }", errPkg)
	b.L("now := %s.Now().UTC()", timePkg)
	b.L("_ = now")
	if s.IsSnapshotable() {
		b.L("versionType := %s", e.versionOriginal(res))
	}
	b.NL()

	cols := e.insertColumns(res)
	b.P("columns := []string{")
	for i, c := range cols {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s", gobuf.Quote(c.Column))
	}
	b.L("}")

	b.P("values := []any{")
	for i, c := range cols {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s", c.Expr)
	}
	b.L("}")
	b.NL()

	b.L("sql := %s.Sprintf(\"INSERT INTO %s (%%s) VALUES (%%s) RETURNING %%s\", "+
		"joinColumns(columns), placeholders(len(values)), %sSelect)",
		fmtPkg, res.Storage.Table, typeName)
	b.NL()
	b.L("m, err := scan%s(%s.db.conn().QueryRow(ctx, sql, values...))", res.Name, repo)
	b.L("if err != nil {")
	b.L("return nil, writeError(err, %s)", gobuf.Quote(res.Storage.Table))
	b.L("}")
	b.NL()

	if s.AuditLog {
		e.recordAudit(b, res, "Create", "m", nil)
	}
	b.L("return m, nil")
	b.L("}")
	b.NL()
}

func (e *emitter) updateMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		fmtPkg  = b.Import("fmt")
		timePkg = b.Import("time")
	)
	s := res.Storage

	b.Comment("Update implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Update(ctx %s.Context, id %s.UUID, in *%sUpdate) (*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, res.Name, res.Name)
	b.L("if in == nil { return nil, %s.BadRequest(\"no changes given\") }", errPkg)
	b.NL()
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.L("_ = claims")
	b.NL()

	b.L("var (")
	b.L("updated *%s", res.Name)
	b.L("changes []auditChange")
	b.L(")")
	b.L("err = %s.InTx(ctx, %s.db.pool, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, ctxPkg, dbxPkg)

	b.L("prev, err := %s.Get(ctx, id)", repo)
	b.L("if err != nil { return err }")
	b.NL()

	if s.IsSoftDeletable() {
		b.L("if prev.%s != nil {", e.fieldForColumn(res, s.SoftDelete.Column.Name))
		b.L("return %s.Conflict(\"%s %%s is deleted; restore it first\", id)", errPkg, res.Name)
		b.L("}")
	}
	if s.IsSnapshotable() {
		versionField := e.fieldForColumn(res, s.Snapshot.VersionType.Name)
		b.Comment("A snapshot is a record of what was; changing one would make " +
			"the history a lie.")
		b.L("if prev.%s != %s {", versionField, e.versionOriginal(res))
		b.L("return %s.Conflict(\"%s %%s is a snapshot and cannot be changed\", id)", errPkg, res.Name)
		b.L("}")
		b.NL()
		b.L("if err := %s.writeSnapshot(ctx, tx, prev); err != nil { return err }", repo)
	}
	b.NL()

	b.L("columns := []string{}")
	b.L("values := []any{}")
	if s.Audit != nil && s.Audit.UpdatedAt != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedAt.Name))
		b.L("values = append(values, %s.Now().UTC())", timePkg)
	}
	if s.Audit != nil && s.Audit.UpdatedBy != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedBy.Name))
		b.L("values = append(values, claims.Actor())")
	}
	b.NL()

	b.L("changes = changes[:0]")
	for _, f := range writableFields(res, ir.FieldOpUpdate) {
		column := gobuf.Quote(f.Column.Name)
		b.L("if in.%s.Touched() {", f.Name)
		b.L("columns = append(columns, %s)", column)
		if f.IsNullable() || f.Column.Nullable {
			b.L("if v, ok := in.%s.Get(); ok { values = append(values, v) } else { values = append(values, nil) }", f.Name)
		} else {
			b.L("v, ok := in.%s.Get()", f.Name)
			b.L("if !ok { return %s.Invalid(\"%s cannot be null\") }", errPkg, f.Name)
			b.L("values = append(values, v)")
		}
		b.L("changes = append(changes, auditChange{Column: %s, Type: %s, Old: render(prev.%s), New: render(in.%s)})",
			column, gobuf.Quote(f.Type), f.Name, f.Name)
		b.L("}")
	}
	b.NL()

	b.L("if len(changes) == 0 {")
	b.Comment("Nothing was asked for, so nothing is written. An update that " +
		"changes no columns should not bump the audit trail.")
	b.L("updated = prev")
	b.L("return nil")
	b.L("}")
	b.NL()

	b.L("values = append(values, id)")
	b.L("sql := %s.Sprintf(\"UPDATE %s SET %%s WHERE id = $%%d RETURNING %%s\", "+
		"assignments(columns), len(values), %sSelect)",
		fmtPkg, res.Storage.Table, typeName)
	b.NL()
	b.L("updated, err = scan%s(tx.QueryRow(ctx, sql, values...))", res.Name)
	b.L("if err != nil { return writeError(err, %s) }", gobuf.Quote(res.Storage.Table))
	b.L("return nil")
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.NL()

	if s.AuditLog {
		e.recordAudit(b, res, "Update", "updated", []string{"changes..."})
	}
	b.L("return updated, nil")
	b.L("}")
	b.NL()

	if s.IsSnapshotable() {
		e.snapshotWriter(b, res, typeName)
	}
}

// snapshotWriter emits the copy taken before an update.
func (e *emitter) snapshotWriter(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		fmtPkg  = b.Import("fmt")
	)
	s := res.Storage
	t := e.table(res)

	b.Comment("writeSnapshot copies a row before it changes.\n\n" +
		"The copy records the source row's last-updated time rather than the " +
		"moment of copying: that identifies the version captured, which is what " +
		"a reader of the history is asking about.")
	b.L("func (%s *%s) writeSnapshot(ctx %s.Context, tx %s.Conn, prev *%s) error {",
		repo, typeName, ctxPkg, dbxPkg, res.Name)
	b.L("snapshotID, err := %s.NewV7()", uuidPkg)
	b.L("if err != nil { return %s.Internal(err, \"generate an identifier\") }", errPkg)
	b.NL()

	// Every column is copied except the ones that identify the snapshot itself.
	var (
		columns []string
		exprs   []string
	)
	for i := range t.Columns {
		c := &t.Columns[i]
		switch c.Name {
		case "id":
			columns, exprs = append(columns, c.Name), append(exprs, "snapshotID")
		case s.Snapshot.VersionType.Name:
			columns, exprs = append(columns, c.Name), append(exprs, e.versionSnapshot(res))
		case s.Snapshot.FromID.Name:
			columns, exprs = append(columns, c.Name), append(exprs, "prev.ID")
		case s.Snapshot.FromAt.Name:
			columns, exprs = append(columns, c.Name), append(exprs, "sourceVersion")
		default:
			field := e.fieldForColumn(res, c.Name)
			if field == "" || c.Generated {
				continue
			}
			columns, exprs = append(columns, c.Name), append(exprs, "prev."+field)
		}
	}

	// The version identity is the source's updated_at, falling back to its
	// creation time for a row that has never changed.
	if s.Audit != nil && s.Audit.UpdatedAt != nil && s.Audit.CreatedAt != nil {
		updatedField := e.fieldForColumn(res, s.Audit.UpdatedAt.Name)
		createdField := e.fieldForColumn(res, s.Audit.CreatedAt.Name)
		b.L("sourceVersion := prev.%s", createdField)
		b.L("if prev.%s != nil { sourceVersion = *prev.%s }", updatedField, updatedField)
	} else {
		timePkg := b.Import("time")
		b.L("sourceVersion := %s.Now().UTC()", timePkg)
	}
	b.NL()

	b.P("columns := []string{")
	for i, c := range columns {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s", gobuf.Quote(c))
	}
	b.L("}")
	b.P("values := []any{")
	for i, expr := range exprs {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s", expr)
	}
	b.L("}")
	b.NL()

	b.L("sql := %s.Sprintf(\"INSERT INTO %s (%%s) VALUES (%%s)\", joinColumns(columns), placeholders(len(values)))",
		fmtPkg, res.Storage.Table)
	b.L("if _, err := tx.Exec(ctx, sql, values...); err != nil {")
	b.L("return writeError(err, %s)", gobuf.Quote(res.Storage.Table))
	b.L("}")
	b.L("return nil")
	b.L("}")
	b.NL()
}

func (e *emitter) versionSnapshot(res *ir.Resource) string {
	col := e.doc.Resolve(res.Storage.Snapshot.VersionType)
	if col == nil || col.EnumType == "" {
		return gobuf.Quote("Snapshot")
	}
	for _, enum := range e.doc.API.Enums {
		if enum.PgType != col.EnumType {
			continue
		}
		for _, v := range enum.Values {
			if v.Wire == "Snapshot" {
				return enum.Name + v.Name
			}
		}
	}
	return gobuf.Quote("Snapshot")
}

// recordAudit emits the audit call, which never fails the operation it
// describes.
func (e *emitter) recordAudit(b *gobuf.Buf, res *ir.Resource, op, row string, extra []string) {
	auditPkg := b.Import(runtimeModule + "/audit")

	values := "nil"
	if len(extra) > 0 {
		values = "auditValues(" + strings.Join(extra, ", ") + ")"
	}

	b.L("_ = %s.Record(ctx, %s.db.audit, %s.Entry{", auditPkg, repo, auditPkg)
	b.L("TenantID:  claims.TenantID,")
	b.L("AccountID: claims.Actor(),")
	b.L("Operation: %s.Operation%s,", auditPkg, op)
	b.L("Entity:    %s,", gobuf.Quote(res.Storage.Table))
	b.L("EntityID:  %s.ID,", row)
	b.L("Values:    %s,", values)
	b.L("})")
}

func (e *emitter) deleteMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		timePkg = b.Import("time")
		fmtPkg  = b.Import("fmt")
	)
	s := res.Storage

	b.Comment("Delete implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Delete(ctx %s.Context, in %sDelete) error {", repo, typeName, ctxPkg, res.Name)
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return err }")
	b.L("_ = claims")
	b.NL()

	b.L("return %s.InTx(ctx, %s.db.pool, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, ctxPkg, dbxPkg)
	b.L("prev, err := %s.Get(ctx, in.ID)", repo)
	b.L("if err != nil { return err }")
	b.NL()

	if !s.IsSoftDeletable() {
		b.L("if _, err := tx.Exec(ctx, \"DELETE FROM %s WHERE id = $1\", in.ID); err != nil {", s.Table)
		b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
		b.L("}")
		if s.AuditLog {
			e.recordAudit(b, res, "Delete", "prev", nil)
		}
		b.L("return nil")
		b.L("})")
		b.L("}")
		b.NL()
		b.L("var _ = %s.Now", timePkg)
		b.L("var _ = %s.Sprintf", fmtPkg)
		b.L("var _ = %s.NotFound", errPkg)
		b.NL()
		return
	}

	deletedField := e.fieldForColumn(res, s.SoftDelete.Column.Name)

	b.L("if in.Hard {")
	if s.IsSnapshotable() {
		b.Comment("The snapshots reference this row, so they go first.")
		b.L("if _, err := tx.Exec(ctx, \"DELETE FROM %s WHERE %s = $1\", in.ID); err != nil {",
			s.Table, s.Snapshot.FromID.Name)
		b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
		b.L("}")
	}
	b.L("if _, err := tx.Exec(ctx, \"DELETE FROM %s WHERE id = $1\", in.ID); err != nil {", s.Table)
	b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
	b.L("}")
	if s.AuditLog {
		e.recordAudit(b, res, "Delete", "prev", nil)
	}
	b.L("return nil")
	b.L("}")
	b.NL()

	b.Comment("Deleting an already-deleted row is not an error: the caller " +
		"asked for it to be gone, and it is.")
	b.L("if prev.%s != nil { return nil }", deletedField)
	b.NL()

	b.L("columns := []string{%s}", gobuf.Quote(s.SoftDelete.Column.Name))
	b.L("values := []any{%s.Now().UTC()}", timePkg)
	if s.SoftDelete.Actor != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.SoftDelete.Actor.Name))
		b.L("values = append(values, claims.Actor())")
	}
	b.L("values = append(values, in.ID)")
	b.NL()

	b.Comment("The retirement is written directly rather than through Update, " +
		"so it neither bumps updated_at nor takes a snapshot of a row nobody " +
		"changed.")
	b.L("sql := %s.Sprintf(\"UPDATE %s SET %%s WHERE id = $%%d\", assignments(columns), len(values))",
		fmtPkg, s.Table)
	b.L("if _, err := tx.Exec(ctx, sql, values...); err != nil {")
	b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
	b.L("}")
	if s.AuditLog {
		e.recordAudit(b, res, "Delete", "prev", nil)
	}
	b.L("return nil")
	b.L("})")
	b.L("}")
	b.NL()
}

func (e *emitter) restoreMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		fmtPkg  = b.Import("fmt")
		timePkg = b.Import("time")
	)
	s := res.Storage
	deletedField := e.fieldForColumn(res, s.SoftDelete.Column.Name)

	b.Comment(res.Name + "RestoreCutoff is the moment before which a deleted row " +
		"can no longer be brought back.")
	b.L("func %sRestoreCutoff() %s.Time {", res.Name, timePkg)
	b.L("return %s.Now().UTC().AddDate(0, 0, -%d)", timePkg, s.SoftDelete.RestoreWindowDays)
	b.L("}")
	b.NL()

	b.Comment("Restore implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Restore(ctx %s.Context, id %s.UUID) (*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, res.Name)
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.L("_ = claims")
	b.NL()

	b.L("var restored *%s", res.Name)
	b.L("err = %s.InTx(ctx, %s.db.pool, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, ctxPkg, dbxPkg)
	b.L("prev, err := %s.Get(ctx, id)", repo)
	b.L("if err != nil { return err }")
	b.NL()

	b.Comment("Restoring a live row is not an error; it is already in the state " +
		"the caller asked for.")
	b.L("if prev.%s == nil {", deletedField)
	b.L("restored = prev")
	b.L("return nil")
	b.L("}")
	b.NL()

	b.L("if prev.%s.Before(%sRestoreCutoff()) {", deletedField, res.Name)
	b.L("return %s.Conflict(\"%s %%s was deleted more than %d days ago and can no longer be restored\", id)",
		errPkg, res.Name, s.SoftDelete.RestoreWindowDays)
	b.L("}")
	b.NL()

	b.L("columns := []string{%s}", gobuf.Quote(s.SoftDelete.Column.Name))
	b.L("values := []any{nil}")
	if s.SoftDelete.Actor != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.SoftDelete.Actor.Name))
		b.L("values = append(values, nil)")
	}
	b.L("values = append(values, id)")
	b.NL()

	b.L("sql := %s.Sprintf(\"UPDATE %s SET %%s WHERE id = $%%d RETURNING %%s\", "+
		"assignments(columns), len(values), %sSelect)", fmtPkg, s.Table, typeName)
	b.L("restored, err = scan%s(tx.QueryRow(ctx, sql, values...))", res.Name)
	b.L("if err != nil { return writeError(err, %s) }", gobuf.Quote(s.Table))
	b.L("return nil")
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.NL()

	if s.AuditLog {
		e.recordAudit(b, res, "Restore", "restored", nil)
	}
	b.L("return restored, nil")
	b.L("}")
	b.NL()
}

func (e *emitter) listSnapshotsMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		fmtPkg  = b.Import("fmt")
	)
	s := res.Storage

	b.Comment("ListSnapshots implements " + res.Name + "Repository.")
	b.L("func (%s *%s) ListSnapshots(ctx %s.Context, id %s.UUID) ([]*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, res.Name)
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.NL()

	args := "id, claims.TenantID"
	where := s.Snapshot.FromID.Name + " = $1"
	if s.Tenant != nil {
		where += " AND " + s.Tenant.Name + " = $2"
	} else {
		args = "id"
	}

	b.L("sql := %s.Sprintf(\"SELECT %%s FROM %s WHERE %s ORDER BY %s DESC\", %sSelect)",
		fmtPkg, s.Table, where, s.Snapshot.FromAt.Name, typeName)
	b.L("rows, err := %s.db.conn().Query(ctx, sql, %s)", repo, args)
	b.L("if err != nil { return nil, %s.Internal(err, \"list snapshots of %s\") }", errPkg, s.Table)
	b.L("defer rows.Close()")
	b.NL()
	b.L("var out []*%s", res.Name)
	b.L("for rows.Next() {")
	b.L("m, err := scan%s(rows)", res.Name)
	b.L("if err != nil { return nil, %s.Internal(err, \"read %s\") }", errPkg, s.Table)
	b.L("out = append(out, m)")
	b.L("}")
	b.L("if err := rows.Err(); err != nil {")
	b.L("return nil, %s.Internal(err, \"list snapshots of %s\")", errPkg, s.Table)
	b.L("}")
	b.L("return out, nil")
	b.L("}")
	b.NL()
}

var _ = fmt.Sprintf
