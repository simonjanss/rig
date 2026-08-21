package persistgo

import (
	"fmt"
	"slices"
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
	e.conditionBuilder(b, res)
	e.repositoryImpl(b, res)

	return artifact(naming.Snake(res.Name)+"_repository.gen.go", b)
}

func (e *emitter) repositoryInterface(b *gobuf.Buf, res *ir.Resource) {
	entity := e.entity(b, res)

	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		optPkg  = b.Import(runtimeModule + "/readopt")
		hookPkg = b.Import(runtimeModule + "/dbhook")
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
		ctxPkg, uuidPkg, optPkg, entity)
	b.NL()

	b.Comment("List returns matching rows and the total ignoring pagination.\n\n" +
		"The filter says which rows and the page says how many of them, in what " +
		"order. They are separate arguments because they arrive separately: the " +
		"filter is a request body, the page is two query parameters.")
	b.L("List(ctx %s.Context, f %sFilter, page %sPage, opts ...%s.Option) ([]*%s, int64, error)",
		ctxPkg, entity, entity, optPkg, entity)
	b.NL()

	b.Comment("Create inserts a row, stamping the identifier, tenant and audit " +
		"columns.\n\n" +
		"It takes an envelope rather than the input alone: the rules that check " +
		"the input and the callbacks that run around the write belong to the " +
		"same unit of work as the write itself.")
	b.L("Create(ctx %s.Context, in %s.Create[%sCreateInput, %s]) (*%s, error)",
		ctxPkg, hookPkg, entity, entity, entity)
	b.NL()

	if s.IsSnapshotable() {
		b.Comment("Update changes a row, writing a snapshot of the previous version first.")
	} else {
		b.Comment("Update changes the fields the input mentions and leaves the rest alone.")
	}
	b.L("Update(ctx %s.Context, id %s.UUID, in %s.Update[%sUpdateInput, %s]) (*%s, error)",
		ctxPkg, uuidPkg, hookPkg, entity, entity, entity)
	b.NL()

	if s.IsSoftDeletable() {
		b.Comment("Delete retires a row by stamping its deletion time. It is idempotent.")
	} else {
		b.Comment("Delete removes a row.")
	}
	b.L("Delete(ctx %s.Context, in %s.Delete[%sDeleteInput, %s]) error",
		ctxPkg, hookPkg, entity, entity)

	if s.IsSoftDeletable() {
		b.NL()
		b.Comment("Restore brings a deleted row back, if it is still inside the " +
			"window.\n\n" +
			"It takes the same input an update does, because the world may have " +
			"moved on while the row was retired: a unique value can have been taken " +
			"by something created since, and the only way back is to bring the row " +
			"back under a different one. An empty input restores it as it was.")
		b.L("Restore(ctx %s.Context, id %s.UUID, in %s.Restore[%sUpdateInput, %s]) (*%s, error)",
			ctxPkg, uuidPkg, hookPkg, entity, entity, entity)
		b.NL()
		b.Comment("ListDeleted returns retired rows still inside the restore window.")
		b.L("ListDeleted(ctx %s.Context, f %sFilter, page %sPage, opts ...%s.Option) ([]*%s, int64, error)",
			ctxPkg, entity, entity, optPkg, entity)
	}

	if s.IsSnapshotable() {
		b.NL()
		b.Comment("ListSnapshots returns a row's previous versions, newest first.")
		b.L("ListSnapshots(ctx %s.Context, id %s.UUID) ([]*%s, error)", ctxPkg, uuidPkg, entity)
		b.NL()
		b.Comment("Revert replays one of a row's previous versions onto it.\n\n" +
			"The values come from the version and the hooks come from the caller, " +
			"because a revert is an update: it goes through the same path, so the " +
			"state it replaces is snapshotted first and every rule an update runs " +
			"still runs. Writing the old values straight over the row would skip " +
			"both, and a revert that cannot itself be reverted is not much of a " +
			"safety net.")
		b.L("Revert(ctx %s.Context, id, versionID %s.UUID, hooks %s.UpdateHooks[%sUpdateInput, %s]) (*%s, error)",
			ctxPkg, uuidPkg, hookPkg, entity, entity, entity)
	}

	b.L("}")
	b.NL()
}

// keyWritesAllowed reports whether a request carrying an API key may change this
// table.
//
// The rule is that a key must be no less traceable than a person: if a table
// records who changed a row, it has to record which key a change came through as
// well, or a write from an integration is the one write nobody can attribute. A
// table that records nobody — a lookup table, a join table — is as traceable for
// a key as it is for anybody, so it is left alone.
//
// The alternative would be stamping nothing and hoping the auth log is enough.
// It is not: the log says a key was used, not which rows it touched.
func keyWritesAllowed(res *ir.Resource) bool {
	s := res.Storage
	if s == nil || s.Audit == nil {
		return true
	}

	for _, pair := range []struct{ actor, key *ir.ColumnRef }{
		{s.Audit.CreatedBy, s.Audit.CreatedByAPIKey},
		{s.Audit.UpdatedBy, s.Audit.UpdatedByAPIKey},
		{s.Audit.DeletedBy, s.Audit.DeletedByAPIKey},
	} {
		if pair.actor != nil && pair.key == nil {
			return false
		}
	}
	return true
}

// refuseKeyWrites emits the guard, for a table that cannot say which key changed
// it.
//
// In the repository rather than in a handler or a hook, because this is the floor
// every write stands on: a custom endpoint reaching for the writer, a service
// calling itself, and the generated handler all pass through here. A check in the
// router would cover the router.
func (e *emitter) refuseKeyWrites(b *gobuf.Buf, res *ir.Resource, fail string) {
	if keyWritesAllowed(res) || e.doc.Table("rig_api_key") == nil {
		return
	}

	errPkg := b.Import(runtimeModule + "/rigerr")
	b.Comment("This table records who changed a row and not which key it came " +
		"through, so a change made with an API key could not be attributed to " +
		"one. Add created_by_api_key_id and its update and delete counterparts " +
		"to allow it.")
	b.L("if claims.APIKeyID != nil {")
	b.L("return %s%s.Forbidden(\"a %s cannot be changed with an API key: the table "+
		"does not record which key made a change\")", fail, errPkg, res.Name)
	b.L("}")
	b.NL()
}

// repo is the generated implementation's receiver name.
const repo = "r"

// readPreamble emits the checks every read starts with.
//
// The values are bound only when the rest of the method uses them. A table with
// no tenant column, no soft delete, and no snapshots has nothing to scope by —
// but both checks still run, because a nonsensical combination of read options
// and a request carrying no claims are worth refusing whether or not this
// particular table would have used them.
func (e *emitter) readPreamble(b *gobuf.Buf, res *ir.Resource, optPkg, tenPkg, fail string, usesOptions, usesClaims bool) {
	if usesOptions {
		b.L("cfg, err := %s.Apply(opts)", optPkg)
		b.L("if err != nil { return %s }", fail)
	} else {
		b.L("if _, err := %s.Apply(opts); err != nil { return %s }", optPkg, fail)
	}
	b.NL()

	if usesClaims {
		b.L("claims, err := %s.FromContext(ctx)", tenPkg)
		b.L("if err != nil { return %s }", fail)
	} else {
		b.L("if _, err := %s.FromContext(ctx); err != nil { return %s }", tenPkg, fail)
	}
	b.NL()
}

// usesClaims reports whether a read scopes by anything the caller's identity
// supplies.
func (e *emitter) usesClaims(res *ir.Resource) bool {
	if _, ok := e.tenantFilter(res); ok {
		return true
	}
	_, ok := e.ownerFilter(res)
	return ok
}

// filterNeedsClaims reports whether building this table's filter needs the
// caller's identity.
//
// Its own tenant column is the obvious case. The other one is a filter that
// reaches across a relation into a table that has one: the subquery scopes the
// far side by tenant, so a table with no tenant column of its own still needs
// the claims to ask about a related row.
func (e *emitter) filterNeedsClaims(res *ir.Resource) bool {
	if e.usesClaims(res) {
		return true
	}
	for _, r := range e.relationFilters(res) {
		if r.target.Storage.Tenant != nil {
			return true
		}
	}
	return false
}

// usesReadOptions reports whether any read option changes what a query-based
// read returns for this table.
//
// Get is not one of those reads: it fetches by primary key and deliberately
// returns the row whatever its lifecycle state, so the only options it can act
// on are the two scopes.
func (e *emitter) usesReadOptions(res *ir.Resource) bool {
	return e.usesClaims(res) || res.Storage.IsSoftDeletable() || res.Storage.IsSnapshotable()
}

// repoTypeName is the unexported implementation's type.
//
// It goes through the namer rather than lowercasing the first letter, because
// lowercasing the first letter of APIKey produces aPIKey — which compiles, and
// which nobody would ever type on purpose.
func repoTypeName(res *ir.Resource) string {
	return naming.New(naming.Config{}).GoUnexported(res.Name) + "Repo"
}

func (e *emitter) repositoryImpl(b *gobuf.Buf, res *ir.Resource) {
	typeName := repoTypeName(res)

	b.L("type %s struct {", typeName)
	b.L("db  *Store")
	b.L("}")
	b.NL()

	b.L("var _ %sRepository = (*%s)(nil)", res.Name, typeName)
	b.NL()

	e.traceHelper(b, typeName)
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
		e.revertMethod(b, res, typeName)
	}
}

// scanHelpers emit the select list and the row scanner, so no method spells
// out the column order twice.
func (e *emitter) scanHelpers(b *gobuf.Buf, res *ir.Resource, typeName string) {
	pgxPkg := b.Import("github.com/jackc/pgx/v5")
	t := e.table(res)

	// Qualified, because a statement that joins has two of some of these names
	// and Postgres will not guess which was meant.
	columns := make([]string, 0, len(t.Columns))
	for i := range t.Columns {
		columns = append(columns, res.Storage.Table+"."+t.Columns[i].Name)
	}

	b.L("const %sSelect = %s", typeName, gobuf.Quote(strings.Join(columns, ", ")))
	b.NL()

	b.Comment("scan" + res.Name + " reads one row in the order " + typeName + "Select lists.")
	b.L("func scan%s(row %s.Row) (*%s, error) {", res.Name, pgxPkg, e.entity(b, res))
	b.L("var m %s", e.entity(b, res))
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
	e.normalizeInstants(b, res)
	b.L("return &m, nil")
	b.L("}")
	b.NL()
}

// normalizeInstants settles every timestamp into UTC on the way out.
//
// pgx decodes a timestamptz into time.Local, so without this the location on a
// scanned time is the host's rather than the row's — the instant is correct, and
// everything that formats it is not. It happens here, in the only place a row is
// read, rather than being left to whoever built the pool.
func (e *emitter) normalizeInstants(b *gobuf.Buf, res *ir.Resource) {
	var fields []ir.ResourceField
	for _, f := range res.Fields {
		if f.Type == ir.TypeTimestamp && f.Column != nil {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return
	}

	dbxPkg := b.Import(runtimeModule + "/dbx")
	for _, f := range fields {
		switch {
		case f.IsArray():
			// An array is nil-able whether or not the column is, so one call
			// covers both: a null array arrives as a nil slice.
			b.L("m.%s = %s.UTCSlice(m.%s)", f.Name, dbxPkg, f.Name)
		case f.IsNullable():
			b.L("m.%s = %s.UTCPtr(m.%s)", f.Name, dbxPkg, f.Name)
		default:
			b.L("m.%s = %s.UTC(m.%s)", f.Name, dbxPkg, f.Name)
		}
	}
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

// ownerFilter renders the predicate a narrow read is scoped by, on a table that
// asked to be owner-scoped.
//
// It sits beside the tenant filter rather than being layered on top of it in the
// service, because this is the floor: a hook reaching for the repository, a
// custom endpoint, and the generated handler all pass through here, and a
// narrowing that only the generated read path applied would be a narrowing a
// custom endpoint silently drops.
func (e *emitter) ownerFilter(res *ir.Resource) (column string, ok bool) {
	if !res.Storage.IsOwnerScoped() {
		return "", false
	}
	return res.Storage.Owner.Name, true
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
		repo, typeName, ctxPkg, uuidPkg, optPkg, e.entity(b, res))
	e.methodSpan(b, res, "Get")
	e.readPreamble(b, res, optPkg, tenPkg, "nil, err", e.usesClaims(res), e.usesClaims(res))

	b.L("args := []any{id}")
	b.L("where := \"id = $1\"")

	// Two optional predicates can each be dropped at run time, so the second
	// one's placeholder number is not knowable here. One of them alone always
	// lands at $2, and the literal reads better than a format string, so the
	// number is only computed when it can actually move.
	ownerColumn, ownerScoped := e.ownerFilter(res)
	placeholder := func(column string) string {
		if !ownerScoped {
			return gobuf.Quote(" AND " + column + " = $2")
		}
		return fmtPkg + ".Sprintf(" + gobuf.Quote(" AND "+column+" = $%d") + ", len(args))"
	}

	if column, ok := e.tenantFilter(res); ok {
		b.L("if !cfg.SkipTenantScope {")
		b.L("args = append(args, claims.TenantID)")
		b.L("where += %s", placeholder(column))
		b.L("}")
	}
	if ownerScoped {
		b.Comment("A lookup by identifier is where every write starts, so this is " +
			"also what stops one caller changing another's row: the read comes back " +
			"empty and the write is a 404. Not a 403 — a 403 would confirm the row " +
			"exists to somebody who cannot see it.")
		b.L("if !cfg.SkipOwnerScope {")
		b.L("args = append(args, claims.AccountID)")
		b.L("where += %s", placeholder(ownerColumn))
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
	entity := e.entity(b, res)

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
		b.L("func (%s *%s) %s(ctx %s.Context, f %sFilter, page %sPage, opts ...%s.Option) ([]*%s, int64, error) {",
			repo, typeName, method, ctxPkg, entity, entity, optPkg, entity)
		e.methodSpan(b, res, method)
		b.Comment("The lifecycle option is forced and the caller's are kept: which " +
			"rows the trash holds is not up for discussion, and how wide a view of " +
			"it the caller gets still is.")
		b.L("return %s.list(ctx, f, page, append([]%s.Option{%s.WithOnlyDeleted()}, opts...))",
			repo, optPkg, optPkg)
		b.L("}")
		b.NL()
		return
	}

	b.L("func (%s *%s) %s(ctx %s.Context, f %sFilter, page %sPage, opts ...%s.Option) ([]*%s, int64, error) {",
		repo, typeName, method, ctxPkg, entity, entity, optPkg, entity)
	e.methodSpan(b, res, method)
	b.L("return %s.list(ctx, f, page, opts)", repo)
	b.L("}")
	b.NL()

	// The shared implementation.
	b.Comment("list is the body of every read that takes a filter.")
	b.L("func (%s *%s) list(ctx %s.Context, f %sFilter, page %sPage, opts []%s.Option) ([]*%s, int64, error) {",
		repo, typeName, ctxPkg, entity, entity, optPkg, entity)
	e.readPreamble(b, res, optPkg, tenPkg, "nil, 0, err", e.usesReadOptions(res), e.filterNeedsClaims(res))

	// A filter that scopes nothing by the caller's identity is built from an
	// empty set of claims rather than from claims the preamble did not bind.
	scopeClaims := "claims"
	if !e.filterNeedsClaims(res) {
		scopeClaims = tenPkg + ".Claims{}"
	}
	b.Comment("One scope for the whole statement: it says who is asking, and it " +
		"qualifies every column with the table it belongs to. The qualification " +
		"is not decoration — an ordering that reaches a related table brings a " +
		"second tenant_id into scope, and an unqualified one is then ambiguous.")
	b.L("sc := newFilterScope(%s, %s)", scopeClaims, gobuf.Quote(res.Storage.Table))
	b.NL()
	b.L("scope := %s.Group{}", queryPk)
	e.lifecycleFilters(b, res, queryPk)
	b.NL()
	b.L("group, err := %s(f, sc)", queryFuncName(res))
	b.L("if err != nil { return nil, 0, err }")
	b.L("scope.Nest(group)")
	b.NL()

	b.Comment("The count has its own arguments and no joins. A left join cannot " +
		"change how many rows there are, and a statement must be given exactly " +
		"the parameters its text refers to.")
	b.L("countArgs := %s.NewArgs()", queryPk)
	b.L("countWhere := scope.SQL(countArgs)")
	b.L("if countWhere != \"\" { countWhere = \" WHERE \" + countWhere }")
	b.L("countSQL := %s.Sprintf(\"SELECT count(*) FROM %s%%s\", countWhere)", fmtPkg, res.Storage.Table)
	b.L("var total int64")
	b.L("if err := %s.db.conn().QueryRow(ctx, countSQL, countArgs.Values()...).Scan(&total); err != nil {", repo)
	b.L("return nil, 0, %s.Internal(err, \"count %s\")", errPkg, res.Storage.Table)
	b.L("}")
	b.NL()

	b.L("order, joins, err := %s(page.OrderBy, sc)", orderFuncName(res))
	b.L("if err != nil { return nil, 0, err }")
	b.L("if len(order) == 0 { order = %sDefaultOrder }", res.Name)
	b.L("window := %s.Page{Limit: page.Limit, Offset: page.Offset}.Clamp(DefaultLimit, MaxLimit)", queryPk)
	b.NL()

	b.Comment("The joins render first, because Postgres numbers placeholders in " +
		"the order the statement mentions them and a join stands before the " +
		"conditions do.")
	b.L("args := %s.NewArgs()", queryPk)
	b.L("joinSQL := %s.JoinSQL(joins, args)", queryPk)
	b.L("where := scope.SQL(args)")
	b.L("if where != \"\" { where = \" WHERE \" + where }")
	b.NL()

	b.L("listSQL := %s.Sprintf(\"SELECT %%s FROM %s%%s%%s%%s%%s\", %sSelect, joinSQL, where, %s.OrderSQL(order), window.SQL(args))",
		fmtPkg, res.Storage.Table, typeName, queryPk)
	b.L("rows, err := %s.db.conn().Query(ctx, listSQL, args.Values()...)", repo)
	b.L("if err != nil {")
	b.L("return nil, 0, %s.Internal(err, \"list %s\")", errPkg, res.Storage.Table)
	b.L("}")
	b.L("defer rows.Close()")
	b.NL()
	b.L("out := make([]*%s, 0, window.Limit)", e.entity(b, res))
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
		b.L("scope.Add(sc.at(%s.Eq(%s, claims.TenantID)))", queryPk, gobuf.Quote(column))
		b.L("}")
	}

	if column, ok := e.ownerFilter(res); ok {
		b.Comment("The table asked to be read narrowly, so this is what happens " +
			"when nobody says otherwise. Widening it is a parameter the caller has " +
			"to pass and a permission it has to hold.\n\n" +
			"The column is nullable, so a row created by a migration or by a service " +
			"— with no account behind it — matches nobody and is invisible to a " +
			"narrow read. That is the right answer and a surprising one: ask for the " +
			"wide scope to see those rows.")
		b.L("if !cfg.SkipOwnerScope {")
		b.L("scope.Add(sc.at(%s.Eq(%s, claims.AccountID)))", queryPk, gobuf.Quote(column))
		b.L("}")
	}

	if s.IsSoftDeletable() {
		column := gobuf.Quote(s.SoftDelete.Column.Name)
		b.L("switch {")
		b.L("case cfg.OnlyDeleted:")
		b.L("scope.Add(sc.at(%s.NotNull(%s)))", queryPk, column)
		b.Comment("A row past the restore window is gone as far as anyone is " +
			"concerned, so it does not appear in the trash either.")
		b.L("scope.Add(sc.at(%s.Gte(%s, %sRestoreCutoff())))", queryPk, column, res.Name)
		b.L("case !cfg.IncludeDeleted:")
		b.L("scope.Add(sc.at(%s.IsNull(%s)))", queryPk, column)
		b.L("}")
	}

	if s.IsSnapshotable() {
		b.L("if !cfg.IncludeSnapshots {")
		b.L("scope.Add(sc.at(%s.Eq(%s, %s)))", queryPk,
			gobuf.Quote(s.Snapshot.VersionType.Name), e.versionOriginal(b, res))
		b.L("}")
	}
}

// versionOriginal is the expression for the live-version enum value.
func (e *emitter) versionOriginal(b *gobuf.Buf, res *ir.Resource) string {
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
				return e.model(b) + "." + enum.Name + v.Name
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
		b.P("{Table: %s, Column: %s, Desc: %t}",
			gobuf.Quote(res.Storage.Table), gobuf.Quote(t.Column), t.Desc)
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
		// Beside it rather than instead of it: the account says whose change this
		// is, the key says which credential it came through, and a request from a
		// person leaves the key null.
		if s.Audit.CreatedByAPIKey != nil {
			out = append(out, insertColumn{
				Column: s.Audit.CreatedByAPIKey.Name, Expr: "claims.ActorKey()",
			})
		}
	}
	if s.IsSnapshotable() {
		out = append(out, insertColumn{
			Column: s.Snapshot.VersionType.Name,
			Expr:   "versionType",
		})
	}

	for _, f := range writableFields(res, ir.FieldOpCreate) {
		out = append(out, insertColumn{Column: f.Column.Name, Expr: "in.Input." + f.Name})
	}
	return out
}

type insertColumn struct {
	Column string
	Expr   string
}

func (e *emitter) createMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	entity := e.entity(b, res)

	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		hookPkg = b.Import(runtimeModule + "/dbhook")
		fmtPkg  = b.Import("fmt")
		timePkg = b.Import("time")
	)
	s := res.Storage

	b.Comment("Create implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Create(ctx %s.Context, in %s.Create[%sCreateInput, %s]) (*%s, error) {",
		repo, typeName, ctxPkg, hookPkg, entity, entity, entity)
	e.methodSpan(b, res, "Create")
	b.Comment("Who is asking, first of all. A write from a request carrying no " +
		"identity is refused here — before a rule runs, before a column notices " +
		"— which is what lets every hook below take the claims as a value rather " +
		"than something to look up and check.")
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.L("_ = claims")
	b.NL()
	e.refuseKeyWrites(b, res, "nil, ")

	b.Comment("Normalize then validate, before anything is written. A service " +
		"that forgets to call these cannot write a row that would fail them, " +
		"which is the only way a rule the schema declares is actually a rule.")
	b.L("in.Input.Normalize()")
	b.L("if err := in.Input.Validate(); err != nil { return nil, err }")
	b.NL()
	b.Comment("Then the rules the schema cannot express, if the service wrote " +
		"any. They run before the transaction opens: a rule that reaches another " +
		"service should not be holding a row lock while it waits.\n\n" +
		"They run before the create hook rather than after it, which is the one " +
		"place the order goes this way: an update validates inside the " +
		"transaction that read the row, so its hook can run first and have what " +
		"it sets judged. Here there is no row to read and no transaction yet, " +
		"and opening one to hold across a rule that may call out to another " +
		"service would be a worse trade than the one it buys.")
	b.L("if in.Hooks.Validator != nil {")
	e.hookCall(b, res, "in.Hooks.Validator.RunCreate(ctx, claims, &in.Input)", "nil, ", "Create", "Validator")
	b.L("}")
	b.NL()

	b.Comment("A version 7 identifier sorts by creation time, so an index on the " +
		"primary key stays dense as rows are added.")
	b.L("id, err := %s.NewV7()", uuidPkg)
	b.L("if err != nil { return nil, %s.Internal(err, \"generate an identifier\") }", errPkg)
	b.L("now := %s.Now().UTC()", timePkg)
	b.L("_ = now")
	if s.IsSnapshotable() {
		b.L("versionType := %s", e.versionOriginal(b, res))
	}
	b.NL()

	if e.needsReferenceCheck(res, ir.FieldOpCreate) {
		b.Comment("A single INSERT is already atomic, so the transaction is opened " +
			"only when a hook needs to be able to undo it — or, here, when a " +
			"reference has to be checked first. A check on one connection and an " +
			"insert on another leaves a window where the row it approved is deleted " +
			"before the row that points at it lands.")
		b.L("needsTx := true")
	} else {
		b.Comment("A single INSERT is already atomic, so the transaction is opened " +
			"only when a hook needs to be able to undo it. Two round trips is not a " +
			"price to pay for nothing.")
		b.L("needsTx := in.Hooks.Before != nil || in.Hooks.After != nil")
	}
	b.NL()
	b.L("var m *%s", entity)
	b.L("err = %s.InTxIf(ctx, %s.db.pool, %s.db.conn(), needsTx, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, repo, ctxPkg, dbxPkg)
	b.L("if in.Hooks.Before != nil {")
	e.hookCall(b, res, "in.Hooks.Before(ctx, claims, &in.Input)", "", "Create", "Before")
	b.L("}")
	b.NL()

	// After the hook, because the hook is allowed to change which row this
	// points at, and checking the value it replaced would be checking a value
	// nothing is going to write.
	e.createReferenceChecks(b, res)

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
	b.L("created, err := scan%s(tx.QueryRow(ctx, sql, values...))", res.Name)
	b.L("if err != nil {")
	b.L("return writeError(err, %s)", gobuf.Quote(res.Storage.Table))
	b.L("}")
	b.L("m = created")
	b.NL()
	b.L("if in.Hooks.After != nil {")
	e.hookCall(b, res, "in.Hooks.After(ctx, claims, m)", "", "Create", "After")
	b.L("}")
	b.L("return nil")
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.NL()

	e.afterCommit(b, res, "Create", "m", "")
	b.L("return m, nil")
	b.L("}")
	b.NL()
}

// afterCommit emits the hand-off to the callback that runs once the work has
// actually landed.
//
// It is registered rather than called: when this repository method was invoked
// inside somebody else's transaction, the commit that matters is theirs, and
// announcing the change before it happens is exactly the bug the hook exists to
// avoid.
func (e *emitter) afterCommit(b *gobuf.Buf, res *ir.Resource, op string, args ...string) {
	dbxPkg := b.Import(runtimeModule + "/dbx")

	var passed []string
	for _, a := range args {
		if a != "" {
			passed = append(passed, a)
		}
	}

	b.L("if in.Hooks.AfterCommit != nil {")
	b.Comment("The claims are captured with the rest: the request may be over " +
		"by the time this runs, and reaching into a context for them then is " +
		"reaching into one that has been cancelled.")
	b.L("done, who := in.Hooks.AfterCommit, claims")

	if !e.tracing() {
		b.L("%s.AfterCommit(ctx, func() { done(ctx, who, %s) })", dbxPkg, strings.Join(passed, ", "))
		b.L("}")
		b.NL()
		return
	}

	// The span is opened inside the callback rather than around the
	// registration, because by the time this runs the method that registered it
	// has returned and its own span is closed. What it hangs from is the
	// transaction's context, which is what the callback was handed, so the work
	// still lands under the request that caused it.
	b.L("%s.AfterCommit(ctx, func() {", dbxPkg)
	e.afterCommitSpan(b, res, op, "AfterCommit")
	b.L("done(ctx, who, %s)", strings.Join(passed, ", "))
	b.L("})")
	b.L("}")
	b.NL()
}

func (e *emitter) updateMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	entity := e.entity(b, res)

	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		hookPkg = b.Import(runtimeModule + "/dbhook")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		fmtPkg  = b.Import("fmt")
		timePkg = b.Import("time")
	)
	s := res.Storage

	b.Comment("Update implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Update(ctx %s.Context, id %s.UUID, in %s.Update[%sUpdateInput, %s]) (*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, hookPkg, entity, entity, entity)
	e.methodSpan(b, res, "Update")
	b.L("in.Input.Normalize()")
	b.NL()
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.L("_ = claims")
	b.NL()
	e.refuseKeyWrites(b, res, "nil, ")

	b.L("var updated, prev *%s", e.entity(b, res))
	b.L("err = %s.InTx(ctx, %s.db.pool, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, ctxPkg, dbxPkg)

	b.L("prev, err = %s.Get(ctx, id)", repo)
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
		b.L("if prev.%s != %s {", versionField, e.versionOriginal(b, res))
		b.L("return %s.Conflict(\"%s %%s is a snapshot and cannot be changed\", id)", errPkg, res.Name)
		b.L("}")
	}
	b.NL()

	b.Comment("The hook shapes the input, so a field it sets is a field that " +
		"gets written — and one the rules below then judge. It runs first for " +
		"exactly that reason: a hook that ran after validation could write a " +
		"value nothing had checked.")
	b.L("if in.Hooks.Before != nil {")
	e.hookCall(b, res, "in.Hooks.Before(ctx, claims, &in.Input, prev)", "", "Update", "Before")
	b.L("}")
	b.NL()

	b.Comment("Validation runs against the row this update would produce, not " +
		"against the request — a rule about two fields needs both, and only one " +
		"of them may have been sent. It happens inside the transaction that " +
		"read prev, so nothing can change underneath it.")
	b.L("if err := in.Input.Validate(prev); err != nil { return err }")
	b.NL()
	b.L("if in.Hooks.Validator != nil {")
	e.hookCall(b, res, "in.Hooks.Validator.RunUpdate(ctx, claims, &in.Input, prev)", "", "Update", "Validator")
	b.L("}")
	b.NL()

	// After the hook and the rules, for the same reason the create checks there:
	// the value that gets checked has to be the value that gets written. Before
	// the snapshot, so a refused update leaves no copy behind.
	e.updateReferenceChecks(b, res)

	b.L("columns := []string{}")
	b.L("values := []any{}")
	b.NL()

	for _, f := range writableFields(res, ir.FieldOpUpdate) {
		column := gobuf.Quote(f.Column.Name)

		if f.IsNullable() {
			// Touched covers a value and an explicit clear alike: both are
			// things the caller asked for, and both belong in the statement.
			b.L("if in.Input.%s.Touched() {", f.Name)
			b.L("columns = append(columns, %s)", column)
			// A nil pointer is exactly how the driver writes a NULL, so the
			// clear needs no branch of its own.
			b.L("values = append(values, in.Input.%s.Ptr())", f.Name)
		} else {
			// There is no null to guard against. The type cannot hold one, so
			// the check this used to make is now the compiler's.
			b.L("if v, ok := in.Input.%s.Get(); ok {", f.Name)
			b.L("columns = append(columns, %s)", column)
			b.L("values = append(values, v)")
		}

		b.L("}")
	}
	b.NL()

	b.L("if len(columns) == 0 {")
	b.Comment("Nothing was asked for, so nothing happens — no stamp, and no " +
		"snapshot either. An update that changes no column did not happen, and " +
		"a history full of copies of an unchanged row is a history nobody can " +
		"read.")
	b.L("updated = prev")
	b.L("return nil")
	b.L("}")
	b.NL()

	if s.IsSnapshotable() {
		b.Comment("The copy is taken now that there is something to copy for.")
		b.L("if err := %s.writeSnapshot(ctx, tx, prev); err != nil { return err }", repo)
		b.NL()
	}
	if s.Audit != nil && s.Audit.UpdatedAt != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedAt.Name))
		b.L("values = append(values, %s.Now().UTC())", timePkg)
	}
	if s.Audit != nil && s.Audit.UpdatedBy != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedBy.Name))
		b.L("values = append(values, claims.Actor())")
	}
	if s.Audit != nil && s.Audit.UpdatedByAPIKey != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedByAPIKey.Name))
		b.L("values = append(values, claims.ActorKey())")
	}
	b.NL()

	b.L("values = append(values, id)")
	b.L("sql := %s.Sprintf(\"UPDATE %s SET %%s WHERE id = $%%d RETURNING %%s\", "+
		"assignments(columns), len(values), %sSelect)",
		fmtPkg, res.Storage.Table, typeName)
	b.NL()
	b.L("updated, err = scan%s(tx.QueryRow(ctx, sql, values...))", res.Name)
	b.L("if err != nil { return writeError(err, %s) }", gobuf.Quote(res.Storage.Table))
	b.NL()
	b.L("if in.Hooks.After != nil {")
	e.hookCall(b, res, "in.Hooks.After(ctx, claims, updated, prev)", "", "Update", "After")
	b.L("}")
	b.L("return nil")
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.NL()

	e.afterCommit(b, res, "Update", "updated", "prev")
	b.L("return updated, nil")
	b.L("}")
	b.NL()

	if s.IsSnapshotable() {
		e.snapshotWriter(b, res, typeName)
	}
}

// isSnapshotClearedColumn reports whether a column is written as NULL on a
// snapshot rather than copied.
//
// The lifecycle stamps of the row itself: a snapshot is a copy that has never
// been updated and never been deleted, which is exactly what the scaffolded
// CHECK constraint asserts. The actor who wrote the version is deliberately not
// in this set — it is the one piece of that bookkeeping the copy is the right
// place for, and it pairs with snapshot_from_<table>_at.
func (e *emitter) isSnapshotClearedColumn(res *ir.Resource, column string) bool {
	s := res.Storage
	if s == nil {
		return false
	}

	cleared := []*ir.ColumnRef{}
	if s.Audit != nil {
		cleared = append(cleared, s.Audit.UpdatedAt, s.Audit.DeletedAt, s.Audit.DeletedBy,
			s.Audit.DeletedByAPIKey)
	}
	if s.SoftDelete != nil {
		cleared = append(cleared, s.SoftDelete.Column, s.SoftDelete.Actor, s.SoftDelete.ActorKey)
	}

	for _, c := range cleared {
		if c != nil && c.Name == column {
			return true
		}
	}
	return false
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
		repo, typeName, ctxPkg, dbxPkg, e.entity(b, res))
	e.methodSpan(b, res, "writeSnapshot")
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
			columns, exprs = append(columns, c.Name), append(exprs, e.versionSnapshot(b, res))
		case s.Snapshot.FromID.Name:
			columns, exprs = append(columns, c.Name), append(exprs, "prev.ID")
		case s.Snapshot.FromAt.Name:
			columns, exprs = append(columns, c.Name), append(exprs, "sourceVersion")
		default:
			// A snapshot has never been updated and has never been deleted:
			// those columns describe what happened to *this* row, and nothing
			// has. Copying them across is also what the CHECK constraint
			// refuses — it is the constraint that makes a snapshot immutable,
			// so a copy that trips it is the copy being wrong.
			//
			// Nothing is lost. Which version this is, is snapshot_from_<t>_at;
			// who wrote it is the actor column, which is kept and reads
			// alongside it.
			if e.isSnapshotClearedColumn(res, c.Name) {
				columns, exprs = append(columns, c.Name), append(exprs, "nil")
				continue
			}

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

func (e *emitter) versionSnapshot(b *gobuf.Buf, res *ir.Resource) string {
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
				return e.model(b) + "." + enum.Name + v.Name
			}
		}
	}
	return gobuf.Quote("Snapshot")
}

func (e *emitter) deleteMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		hookPkg = b.Import(runtimeModule + "/dbhook")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		timePkg = b.Import("time")
		fmtPkg  = b.Import("fmt")
	)
	s := res.Storage

	b.Comment("Delete implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Delete(ctx %s.Context, in %s.Delete[%sDeleteInput, %s]) error {",
		repo, typeName, ctxPkg, hookPkg, e.entity(b, res), e.entity(b, res))
	e.methodSpan(b, res, "Delete")
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return err }")
	b.L("_ = claims")
	b.NL()
	e.refuseKeyWrites(b, res, "")

	b.L("var prev *%s", e.entity(b, res))
	b.L("err = %s.InTx(ctx, %s.db.pool, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, ctxPkg, dbxPkg)
	b.L("prev, err = %s.Get(ctx, in.Input.ID)", repo)
	b.L("if err != nil { return err }")
	b.NL()
	e.enterDelete(b, res)

	if !s.IsSoftDeletable() {
		e.beforeDelete(b, res)
		e.childrenDeleting(b, res)
		b.L("if _, err := tx.Exec(ctx, \"DELETE FROM %s WHERE id = $1\", in.Input.ID); err != nil {", s.Table)
		b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
		b.L("}")
		e.afterDelete(b, res)
		e.childrenDeleted(b, res)
		b.L("return nil")
		b.L("})")
		b.L("if err != nil { return err }")
		b.NL()
		e.afterCommit(b, res, "Delete", "prev")
		b.L("return nil")
		b.L("}")
		b.NL()
		b.L("var _ = %s.Now", timePkg)
		b.L("var _ = %s.Sprintf", fmtPkg)
		b.L("var _ = %s.NotFound", errPkg)
		b.NL()
		return
	}

	deletedField := e.fieldForColumn(res, s.SoftDelete.Column.Name)

	b.Comment("Deleting an already-deleted row is not an error: the caller " +
		"asked for it to be gone, and it is. Nothing happens, so no hook runs " +
		"either — a hook that fires on a no-op is a notification about nothing.")
	b.L("if prev.%s != nil && !in.Input.Hard { return nil }", deletedField)
	b.NL()

	e.beforeDelete(b, res)
	e.childrenDeleting(b, res)

	b.L("if in.Input.Hard {")
	if s.IsSnapshotable() {
		b.Comment("The snapshots reference this row, so they go first.")
		b.L("if _, err := tx.Exec(ctx, \"DELETE FROM %s WHERE %s = $1\", in.Input.ID); err != nil {",
			s.Table, s.Snapshot.FromID.Name)
		b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
		b.L("}")
	}
	b.L("if _, err := tx.Exec(ctx, \"DELETE FROM %s WHERE id = $1\", in.Input.ID); err != nil {", s.Table)
	b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
	b.L("}")
	e.afterDelete(b, res)
	e.childrenDeleted(b, res)
	b.L("return nil")
	b.L("}")
	b.NL()

	b.L("columns := []string{%s}", gobuf.Quote(s.SoftDelete.Column.Name))
	b.L("values := []any{%s.Now().UTC()}", timePkg)
	if s.SoftDelete.Actor != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.SoftDelete.Actor.Name))
		b.L("values = append(values, claims.Actor())")
	}
	if s.SoftDelete.ActorKey != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.SoftDelete.ActorKey.Name))
		b.L("values = append(values, claims.ActorKey())")
	}
	b.L("values = append(values, in.Input.ID)")
	b.NL()

	b.Comment("The retirement is written directly rather than through Update, " +
		"so it neither bumps updated_at nor takes a snapshot of a row nobody " +
		"changed.")
	b.L("sql := %s.Sprintf(\"UPDATE %s SET %%s WHERE id = $%%d\", assignments(columns), len(values))",
		fmtPkg, s.Table)
	b.L("if _, err := tx.Exec(ctx, sql, values...); err != nil {")
	b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
	b.L("}")
	e.afterDelete(b, res)
	e.childrenDeleted(b, res)
	b.L("return nil")
	b.L("})")
	b.L("if err != nil { return err }")
	b.NL()

	e.afterCommit(b, res, "Delete", "prev")
	b.L("return nil")
	b.L("}")
	b.NL()
}

// beforeDelete and afterDelete emit the two hooks that bracket a deletion.
func (e *emitter) beforeDelete(b *gobuf.Buf, res *ir.Resource) {
	b.L("if in.Hooks.Before != nil {")
	e.hookCall(b, res, "in.Hooks.Before(ctx, claims, &in.Input, prev)", "", "Delete", "Before")
	b.L("}")
	b.NL()
}

// childrenDeleting emits the second step of a delete: every table pointing at
// this one, in the order the document settled.
//
// After the row's own Before and before the row is touched. The parent's own
// veto coming first is deliberate — "this team may not be deleted while the
// season is open" is the cheapest and most specific rule in the building, and it
// should not require running every child's cleanup before it gets to say so.
//
// A child that deletes its own rows by calling its own Delete triggers their
// children the same way, so depth is the call stack. Termination is the visited
// set and the depth cap on the context, which is why this is bracketed by
// [dbx.EnterDelete] rather than trusting the schema to be acyclic.
func (e *emitter) childrenDeleting(b *gobuf.Buf, res *ir.Resource) {
	if len(res.Children) == 0 {
		return
	}
	fmtPkg := b.Import("fmt")

	b.Comment("Every table that references this one, in the derived order. An " +
		"error from any of them unwinds the whole transaction, including whatever " +
		"the children before it already did.")
	b.L("for _, child := range in.Hooks.Children {")
	b.L("if child.Deleting == nil { continue }")
	b.L("if err := child.Deleting(ctx, claims, prev, in.Input); err != nil {")
	b.Comment("Named, because the whole reason this is better than a 23503 is " +
		"that the answer can say which relation refused.")
	b.L("return %s.Errorf(\"%%s: %%w\", child.Child, err)", fmtPkg)
	b.L("}")
	b.L("}")
	b.NL()
}

// childrenDeleted queues the after-commit half.
//
// Onto dbx.AfterCommit rather than run here, so it fires once the outermost
// transaction has landed and in the same order Deleting ran. It returns nothing
// and a panic in it is contained: the row is gone, and unwinding a request that
// succeeded would report a failure that did not occur.
func (e *emitter) childrenDeleted(b *gobuf.Buf, res *ir.Resource) {
	if len(res.Children) == 0 {
		return
	}
	dbxPkg := b.Import(runtimeModule + "/dbx")

	b.L("for _, child := range in.Hooks.Children {")
	b.L("if child.Deleted == nil { continue }")
	b.L("done, row, input := child.Deleted, prev, in.Input")
	b.L("%s.AfterCommit(ctx, func() { done(ctx, claims, row, input) })", dbxPkg)
	b.L("}")
	b.NL()
}

// enterDelete emits the cycle guard.
//
// A second visit to the same row inside one transaction is a no-op rather than
// an error: the row is going, which is what the caller asked for, and refusing
// would turn a legal schema into a runtime failure. Passing the depth cap is a
// different answer and is an error, because finishing halfway through a
// propagation would leave the transaction in a state nobody asked for.
func (e *emitter) enterDelete(b *gobuf.Buf, res *ir.Resource) {
	if len(res.Children) == 0 {
		return
	}
	dbxPkg := b.Import(runtimeModule + "/dbx")

	b.L("ctx, more, err := %s.EnterDelete(ctx, %s, in.Input.ID.String())",
		dbxPkg, gobuf.Quote(res.Storage.Table))
	b.L("if err != nil { return err }")
	b.L("if !more { return nil }")
	b.NL()
}

func (e *emitter) afterDelete(b *gobuf.Buf, res *ir.Resource) {
	b.L("if in.Hooks.After != nil {")
	e.hookCall(b, res, "in.Hooks.After(ctx, claims, prev)", "", "Delete", "After")
	b.L("}")
}

func (e *emitter) restoreMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		hookPkg = b.Import(runtimeModule + "/dbhook")
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

	uuidPkg := b.Import("github.com/google/uuid")

	b.Comment("Restore implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Restore(ctx %s.Context, id %s.UUID, in %s.Restore[%sUpdateInput, %s]) (*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, hookPkg, e.entity(b, res), e.entity(b, res), e.entity(b, res))
	e.methodSpan(b, res, "Restore")
	b.L("claims, err := %s.FromContext(ctx)", tenPkg)
	b.L("if err != nil { return nil, err }")
	b.L("_ = claims")
	b.NL()
	e.refuseKeyWrites(b, res, "nil, ")

	b.L("var restored, prev *%s", e.entity(b, res))
	b.L("err = %s.InTx(ctx, %s.db.pool, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, ctxPkg, dbxPkg)
	b.L("prev, err = %s.Get(ctx, id)", repo)
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

	b.Comment("The request carried no fields, so this hook is where they come " +
		"from. It is handed the row as it was retired and an empty input; " +
		"setting a field on that input writes it as the row comes back, which is " +
		"how a value the world has taken since gets changed on the way in. " +
		"Returning an error refuses the restore instead.")
	b.L("if in.Hooks.Before != nil {")
	e.hookCall(b, res, "in.Hooks.Before(ctx, claims, &in.Input, prev)", "", "Restore", "Before")
	b.L("}")
	b.NL()

	b.Comment("Then every rule against whatever the hook settled on, not only " +
		"the ones for fields it touched: the row was not live, so nothing about " +
		"it has been checked against the world it is returning to.")
	b.L("if in.Hooks.Validator != nil {")
	e.hookCall(b, res, "in.Hooks.Validator.RunRestore(ctx, claims, &in.Input, prev)", "", "Restore", "Validator")
	b.L("}")
	b.NL()

	b.L("columns := []string{%s}", gobuf.Quote(s.SoftDelete.Column.Name))
	b.L("values := []any{nil}")
	if s.SoftDelete.Actor != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.SoftDelete.Actor.Name))
		b.L("values = append(values, nil)")
	}
	if s.SoftDelete.ActorKey != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.SoftDelete.ActorKey.Name))
		b.L("values = append(values, nil)")
	}
	b.NL()

	b.Comment("Whatever the input changed goes in the same statement. No " +
		"snapshot is taken: a snapshot has to be a copy of a live row — the " +
		"CHECK constraint says so — and there was no live row to copy.")
	for _, f := range writableFields(res, ir.FieldOpUpdate) {
		column := gobuf.Quote(f.Column.Name)
		if f.IsNullable() {
			b.L("if in.Input.%s.Touched() {", f.Name)
			b.L("columns = append(columns, %s)", column)
			b.L("values = append(values, in.Input.%s.Ptr())", f.Name)
		} else {
			b.L("if v, ok := in.Input.%s.Get(); ok {", f.Name)
			b.L("columns = append(columns, %s)", column)
			b.L("values = append(values, v)")
		}
		b.L("}")
	}
	b.NL()

	if s.Audit != nil && s.Audit.UpdatedAt != nil {
		b.Comment("The row changed, so it is stamped as changed. A restore that " +
			"left the update columns alone would report the row as last touched " +
			"before it was deleted.")
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedAt.Name))
		b.L("values = append(values, %s.Now().UTC())", timePkg)
	}
	if s.Audit != nil && s.Audit.UpdatedBy != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedBy.Name))
		b.L("values = append(values, claims.Actor())")
	}
	if s.Audit != nil && s.Audit.UpdatedByAPIKey != nil {
		b.L("columns = append(columns, %s)", gobuf.Quote(s.Audit.UpdatedByAPIKey.Name))
		b.L("values = append(values, claims.ActorKey())")
	}
	b.L("values = append(values, id)")
	b.NL()

	b.L("sql := %s.Sprintf(\"UPDATE %s SET %%s WHERE id = $%%d RETURNING %%s\", "+
		"assignments(columns), len(values), %sSelect)", fmtPkg, s.Table, typeName)
	b.L("restored, err = scan%s(tx.QueryRow(ctx, sql, values...))", res.Name)
	b.L("if err != nil { return writeError(err, %s) }", gobuf.Quote(s.Table))
	b.NL()
	b.L("if in.Hooks.After != nil {")
	e.hookCall(b, res, "in.Hooks.After(ctx, claims, restored, prev)", "", "Restore", "After")
	b.L("}")
	b.L("return nil")
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.NL()

	e.afterCommit(b, res, "Restore", "restored", "prev")
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
		repo, typeName, ctxPkg, uuidPkg, e.entity(b, res))
	e.methodSpan(b, res, "ListSnapshots")
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
	b.L("var out []*%s", e.entity(b, res))
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

// revertMethod emits the replay of a previous version onto the live row.
//
// It is deliberately thin: find the version, turn it into an update input, and
// hand that to Update. Everything that makes a revert safe — the snapshot of
// what is being replaced, the validator, the hooks, the transaction — already
// lives there, and a second write path that reimplemented any of it would be a
// second place for those to be wrong.
func (e *emitter) revertMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		hookPkg = b.Import(runtimeModule + "/dbhook")
		patch   = b.Import(runtimeModule + "/patch")
	)
	s := res.Storage
	entity := e.entity(b, res)

	b.Comment("Revert implements " + res.Name + "Repository.")
	b.L("func (%s *%s) Revert(ctx %s.Context, id, versionID %s.UUID, hooks %s.UpdateHooks[%sUpdateInput, %s]) (*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, hookPkg, entity, entity, entity)
	e.methodSpan(b, res, "Revert")

	b.L("var reverted *%s", entity)
	b.L("err := %s.InTx(ctx, %s.db.pool, func(ctx %s.Context, tx %s.Conn) error {",
		dbxPkg, repo, ctxPkg, dbxPkg)

	b.L("version, err := %s.Get(ctx, versionID)", repo)
	b.L("if err != nil { return err }")
	b.NL()

	fromID := e.fieldForColumn(res, s.Snapshot.FromID.Name)
	versionField := e.fieldForColumn(res, s.Snapshot.VersionType.Name)

	b.Comment("A version identifier is a row identifier, so it can name a live " +
		"row or somebody else's history. Both answer the same way: naming a row " +
		"that exists and is not yours should not be distinguishable from naming " +
		"one that does not.")
	b.L("if version.%s != %s || version.%s == nil || *version.%s != id {",
		versionField, e.versionSnapshot(b, res), fromID, fromID)
	b.L("return %s.NotFound(\"%s %%s has no version %%s\", id, versionID)", errPkg, res.Name)
	b.L("}")
	b.NL()

	b.L("in := %sUpdateInput{", entity)
	for _, f := range writableFields(res, ir.FieldOpUpdate) {
		if f.Column == nil {
			continue
		}
		// snapshot_ignore says the live value wins on a replay. Leaving the
		// field absent is exactly that: the update never mentions the column.
		if slices.Contains(s.Snapshot.IgnoreColumns, f.Column.Name) {
			continue
		}
		// A nullable column is held as *T, except an array, where nil is
		// already the absence and there is no pointer to take.
		switch {
		case f.IsNullable() && f.IsArray():
			b.L("%s: %s.FromSlice(version.%s),", f.Name, patch, f.Name)
		case f.IsNullable():
			b.L("%s: %s.FromPtr(version.%s),", f.Name, patch, f.Name)
		default:
			b.L("%s: %s.NewOptional(version.%s),", f.Name, patch, f.Name)
		}
	}
	b.L("}")
	b.NL()

	b.Comment("Every field the version carried, including the ones that already " +
		"match: what is being asked for is that state, not a diff against the " +
		"current one.")
	b.L("reverted, err = %s.Update(ctx, id, %s.Update[%sUpdateInput, %s]{Input: in, Hooks: hooks})",
		repo, hookPkg, entity, entity)
	b.L("return err")
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.NL()
	b.L("return reverted, nil")
	b.L("}")
	b.NL()
}

var _ = fmt.Sprintf
