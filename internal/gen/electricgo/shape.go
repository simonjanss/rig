package electricgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The version labels a snapshotable table's enum carries. They are the
// fallbacks: versionLabel prefers whatever the compiled enum actually says.
const (
	versionOriginal = "Original"
	versionSnapshot = "Snapshot"
)

// shapeKind distinguishes the routes a table's live-sync surface has.
type shapeKind int

const (
	// shapeLive streams the rows an ordinary read returns.
	shapeLive shapeKind = iota
	// shapeDeleted streams the retired ones — the trash.
	shapeDeleted
	// shapeVersions streams one row's prior versions — its history.
	shapeVersions
)

// shape is one route of a table's live-sync surface.
type shape struct {
	kind shapeKind
	// name is the Go stem for this shape's scope type and handler.
	name string
	// path is the route, for example "/api/v1/lesson/_deleted/_stream".
	path string
}

// shapesFor is the routes a resource exposes.
//
// Nothing configures this. A table that retires its rows has a trash to stream,
// and a table that keeps its previous versions has a history — the schema
// already answers both questions, and answering them again in a configuration
// key would only create a way for the two answers to disagree.
//
// It is the columns alone, and deliberately not the resource's operations. The
// API asks for both — listDeleted needs List and versions needs Get — but live
// sync is its own read surface and has never been gated on the CRUD set: a table
// with no operations at all still gets a live shape, which is how an unexposed
// table like rig_notification_recipient is subscribed to. Reading the operations
// here and not there would mean a table whose only read surface is a shape could
// never have a trash, so what the two rules have in common is the columns and
// that is what is asked.
func (e *emitter) shapesFor(res *ir.Resource) []shape {
	shapes := []shape{{kind: shapeLive, name: res.Name, path: res.Electric.Path}}
	if res.Storage.IsSoftDeletable() {
		shapes = append(shapes, shape{
			kind: shapeDeleted,
			name: res.Name + "Deleted",
			path: res.Electric.DeletedPath(),
		})
	}
	if res.Storage.IsSnapshotable() {
		shapes = append(shapes, shape{
			kind: shapeVersions,
			name: res.Name + "Versions",
			path: res.Electric.VersionsPath(),
		})
	}
	return shapes
}

// shapeFile emits one resource's shape endpoints.
func (e *emitter) shapeFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	// The declared parameters and the readable columns mean the same thing on
	// every one of a table's shapes, so both are emitted once and shared.
	e.paramsType(b, res)
	for _, sh := range e.shapesFor(res) {
		e.scopeType(b, res, sh)
		e.handler(b, res, sh)
		if sh.kind == shapeVersions {
			e.versionsAdapter(b, res)
		}
	}
	e.columns(b, res)

	return artifact(naming.Snake(res.Name)+"_shape.gen.go", b, gen.Overwrite)
}

// versionsAdapter lets the history shape inherit the live shape's scope.
//
// The two signatures differ by the row id, and a live scope has no argument for
// one because it does not need one: the history shape bound the row before the
// scope ran, so the id is already in the filter this receives. Dropping it is
// the whole adaptation.
func (e *emitter) versionsAdapter(b *gobuf.Buf, res *ir.Resource) {
	var (
		ctxPkg  = b.Import("context")
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		uuidPkg = b.Import("github.com/google/uuid")
	)

	b.Comment("versionsFromLive" + res.Name + " is the live scope as a history scope.\n\n" +
		"Nil stays nil rather than becoming a function that calls one: a scope " +
		"nobody wrote is not a refusal.")
	b.L("func versionsFromLive%s(live %sScope) %sVersionsScope {", res.Name, res.Name, res.Name)
	b.L("if live == nil { return nil }")
	b.L("return func(ctx %s.Context, r *%s.Request, claims %s.Claims, _ %s.UUID, p %sShapeParams, w *%s.Where) error {",
		ctxPkg, httpPkg, tenPkg, uuidPkg, res.Name, elecPkg)
	b.L("return live(ctx, r, claims, p, w)")
	b.L("}")
	b.L("}")
	b.NL()
}

// paramsType emits the declared query parameters, typed.
func (e *emitter) paramsType(b *gobuf.Buf, res *ir.Resource) {
	params := res.Electric.Params

	b.Comment(res.Name + "ShapeParams are the query parameters this shape accepts.\n\n" +
		"They are the application's, not the protocol's: a subscriber uses them to " +
		"ask for less, and the scoping function turns them into conditions. Nothing " +
		"here can ask for more.")
	b.L("type %sShapeParams struct {", res.Name)
	if len(params) == 0 {
		b.Comment("This shape declares none.")
	}
	for _, p := range params {
		if p.Description != "" {
			b.Comment(p.Description)
		}
		b.L("%s %s", p.Field, e.paramType(b, p))
		if p.Optional {
			b.Comment("Has" + p.Field + " reports whether it was given, so a zero " +
				"value can be told from an absent one.")
			b.L("Has%s bool", p.Field)
		}
		b.NL()
	}
	b.L("}")
	b.NL()

	e.parseParams(b, res)
}

// paramType is the Go type one declared parameter takes.
func (e *emitter) paramType(b *gobuf.Buf, p ir.ElectricParam) string {
	switch p.Type {
	case ir.TypeBool:
		return "bool"
	case ir.TypeInt:
		return "int"
	case ir.TypeInt64:
		return "int64"
	case ir.TypeFloat64:
		return "float64"
	case ir.TypeUUID:
		return b.Import("github.com/google/uuid") + ".UUID"
	case ir.TypeDate, ir.TypeTime, ir.TypeTimestamp:
		return b.Import("time") + ".Time"
	default:
		return "string"
	}
}

// parse names the helper that reads one parameter's type.
func parseCall(p ir.ElectricParam) string {
	switch p.Type {
	case ir.TypeBool:
		return "parseBool"
	case ir.TypeInt:
		return "parseInt"
	case ir.TypeInt64:
		return "parseInt64"
	case ir.TypeFloat64:
		return "parseFloat"
	case ir.TypeUUID:
		return "parseUUID"
	case ir.TypeDate, ir.TypeTime, ir.TypeTimestamp:
		return "parseTime"
	default:
		return ""
	}
}

func (e *emitter) parseParams(b *gobuf.Buf, res *ir.Resource) {
	httpPkg := b.Import("net/http")

	b.Comment("parse" + res.Name + "ShapeParams reads the declared parameters.")
	b.L("func parse%sShapeParams(r *%s.Request) (%sShapeParams, error) {",
		res.Name, httpPkg, res.Name)
	b.L("var p %sShapeParams", res.Name)

	for _, param := range res.Electric.Params {
		b.NL()
		quoted := gobuf.Quote(param.Name)
		call := parseCall(param)

		if param.Optional {
			b.L("if raw, ok := optional(r, %s); ok {", quoted)
			if call == "" {
				b.L("p.%s = raw", param.Field)
			} else {
				b.L("v, err := %s(%s, raw)", call, quoted)
				b.L("if err != nil { return p, err }")
				b.L("p.%s = v", param.Field)
			}
			b.L("p.Has%s = true", param.Field)
			b.L("}")
			continue
		}

		b.L("raw%s, err := required(r, %s)", param.Field, quoted)
		b.L("if err != nil { return p, err }")
		if call == "" {
			b.L("p.%s = raw%s", param.Field, param.Field)
		} else {
			b.L("v%s, err := %s(%s, raw%s)", param.Field, call, quoted, param.Field)
			b.L("if err != nil { return p, err }")
			b.L("p.%s = v%s", param.Field, param.Field)
		}
	}

	b.NL()
	b.L("return p, nil")
	b.L("}")
	b.NL()
}

// scopeDoc is what a scope type says about itself.
//
// The paragraphs after the first are the same for every shape, because what
// they describe is: the filter arrives built, AND is the only way to join to
// it, and a value that reaches SQL as text is an injection point. None of that
// depends on which rows the shape carries.
func scopeDoc(sh shape) string {
	subject := "the shape"
	switch sh.kind {
	case shapeDeleted:
		subject = "the trash shape"
	case shapeVersions:
		subject = "the history shape"
	}

	doc := sh.name + "Scope narrows " + subject + " further.\n\n"
	if sh.kind == shapeVersions {
		doc += "The id is the row whose history this is, parsed before the filter was " +
			"built because the filter is made of it. A scope that wants to refuse " +
			"some rows outright can answer on it, rather than on what comes back.\n\n"
	}
	return doc + "It receives a filter that already carries the tenant and lifecycle " +
		"conditions, and can only add to it — every condition is joined with AND, " +
		"so there is nothing a scope can write that widens what the subscriber " +
		"sees. Add conditions through the Where methods rather than as text: they " +
		"bind their values, and a shape filter built by string concatenation is an " +
		"injection point with a streaming response attached.\n\n" +
		"Returning an error refuses the subscription."
}

// scopeType declares the hook an application implements.
func (e *emitter) scopeType(b *gobuf.Buf, res *ir.Resource, sh shape) {
	var (
		ctxPkg  = b.Import("context")
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
	)

	b.Comment(scopeDoc(sh))
	if sh.kind == shapeVersions {
		uuidPkg := b.Import("github.com/google/uuid")
		b.L("type %sScope func(ctx %s.Context, r *%s.Request, claims %s.Claims, id %s.UUID, p %sShapeParams, w *%s.Where) error",
			sh.name, ctxPkg, httpPkg, tenPkg, uuidPkg, res.Name, elecPkg)
		b.NL()
		return
	}
	b.L("type %sScope func(ctx %s.Context, r *%s.Request, claims %s.Claims, p %sShapeParams, w *%s.Where) error",
		sh.name, ctxPkg, httpPkg, tenPkg, res.Name, elecPkg)
	b.NL()
}

// handler emits one endpoint.
func (e *emitter) handler(b *gobuf.Buf, res *ir.Resource, sh shape) {
	var (
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		storage = res.Storage
		admin   = res.Electric.Auth == ir.ElectricAuthAdmin
	)

	b.Comment("handle" + sh.name + "Shape serves GET " + sh.path + ".")
	b.L("func handle%sShape(s Server, scope %sScope) %s.HandlerFunc {", sh.name, sh.name, httpPkg)
	b.L("return func(w %s.ResponseWriter, r *%s.Request) {", httpPkg, httpPkg)
	b.L("claims, where, ok := prepare(s, w, r, %t)", admin)
	b.L("if !ok { return }")
	b.NL()

	if sh.kind == shapeVersions {
		b.Comment("Before the filter rather than after, because this one is part of it: " +
			"a history shape with no row to be the history of would be every version " +
			"of everything.")
		b.L("id, err := parseUUID(%s, r.PathValue(%s))", gobuf.Quote("id"), gobuf.Quote("id"))
		b.L("if err != nil {")
		b.L("fail(s, w, r, err)")
		b.L("return")
		b.L("}")
		b.NL()
	}

	// The filter, in this order and before anything the application does.
	if storage.Tenant != nil {
		b.Comment("Every row this shape can ever carry belongs to the caller's " +
			"tenant. It is the first condition, and nothing below can remove it.")
		b.L("where.Eq(%s, claims.TenantID.String())", gobuf.Quote(storage.Tenant.Name))
	}
	if storage.Owner != nil {
		errPkg := b.Import(runtimeModule + "/rigerr")
		uuidPkg := b.Import("github.com/google/uuid")

		b.Comment("This table is owner-scoped, so a subscriber sees its own rows " +
			"and nothing else. Here rather than in the scope function below, for " +
			"the reason the repository does not make anybody remember it: a " +
			"narrowing the application has to add by hand is a narrowing somebody " +
			"eventually does not.")
		b.Comment("A caller with no account is refused rather than narrowed. An " +
			"API key and a system credential both have a nil identifier, and " +
			"comparing against one matches nothing *silently* — which is the wrong " +
			"kind of correct, because a subscriber handed an empty stream cannot " +
			"tell it from having nothing to receive.")
		b.L("if claims.AccountID == %s.Nil {", uuidPkg)
		b.L("fail(s, w, r, %s.Forbidden(%s))", errPkg,
			gobuf.Quote("this shape is scoped to one account, and this credential is not one"))
		b.L("return")
		b.L("}")
		b.L("where.Eq(%s, claims.AccountID.String())", gobuf.Quote(storage.Owner.Name))
	}
	e.lifecycle(b, res, sh)
	b.NL()

	b.L("params, err := parse%sShapeParams(r)", res.Name)
	b.L("if err != nil {")
	b.L("fail(s, w, r, err)")
	b.L("return")
	b.L("}")
	b.NL()

	b.L("if scope != nil {")
	if sh.kind == shapeVersions {
		b.L("if err := scope(r.Context(), r, claims, id, params, where); err != nil {")
	} else {
		b.L("if err := scope(r.Context(), r, claims, params, where); err != nil {")
	}
	b.L("fail(s, w, r, err)")
	b.L("return")
	b.L("}")
	b.L("}")
	b.NL()

	b.L("s.Proxy.Serve(w, r, %s.Shape{", elecPkg)
	b.L("Table:   %s,", gobuf.Quote(storage.Table))
	b.L("Where:   where.SQL(),")
	b.L("Params:  where.Params(),")
	b.Comment("The readable columns, named rather than left to default. A shape " +
		"carries every column it names to every subscriber, and a column that is " +
		"not in the API has no business in a live stream either.")
	b.L("Columns: %sShapeColumns,", res.Name)
	b.L("})")

	b.L("}")
	b.L("}")
	b.NL()
}

// lifecycle emits the conditions that decide which generation of a row this
// shape carries: live, retired, or historical.
//
// It is the only thing that differs between a table's shapes, and it is built
// here rather than left to the scope function for the same reason the tenant
// condition is. A subscriber asks for one of these by choosing a route, and the
// route is generated from the schema — so there is no query parameter, and no
// scope, that turns one shape into another.
func (e *emitter) lifecycle(b *gobuf.Buf, res *ir.Resource, sh shape) {
	storage := res.Storage

	switch sh.kind {
	case shapeLive:
		if storage.IsSoftDeletable() {
			b.Comment("A retired row stops appearing in reads, so it stops appearing " +
				"in a live stream too.")
			b.L("where.IsNull(%s)", gobuf.Quote(storage.SoftDelete.Column.Name))
		}
		if storage.IsSnapshotable() {
			b.Comment("Snapshots are prior versions. A subscriber wants the live row.")
			b.L("where.Eq(%s, %s)", gobuf.Quote(storage.Snapshot.VersionType.Name),
				gobuf.Quote(e.versionLabel(res, versionOriginal)))
		}

	case shapeDeleted:
		b.Comment("The trash, so this shape wants precisely what the live one " +
			"excludes. A row deleted while somebody is subscribed to both leaves " +
			"one stream and arrives in the other.")
		b.L("where.NotNull(%s)", gobuf.Quote(storage.SoftDelete.Column.Name))
		if storage.IsSnapshotable() {
			b.Comment("Still the live generation of the row, though. The trash is " +
				"what was deleted, not the history of what was deleted.")
			b.L("where.Eq(%s, %s)", gobuf.Quote(storage.Snapshot.VersionType.Name),
				gobuf.Quote(e.versionLabel(res, versionOriginal)))
		}

	case shapeVersions:
		b.Comment("One row's history: the copies taken before each update, and never " +
			"the row itself.")
		b.L("where.Eq(%s, %s)", gobuf.Quote(storage.Snapshot.VersionType.Name),
			gobuf.Quote(e.versionLabel(res, versionSnapshot)))
		b.L("where.Eq(%s, id.String())", gobuf.Quote(storage.Snapshot.FromID.Name))
		if storage.IsSoftDeletable() {
			b.Comment("A snapshot is written with no deletion stamp and the table's " +
				"check constraint keeps it that way — but the constraint is one the " +
				"schema has to carry, and this filter does not depend on somebody " +
				"else's migration having written it.")
			b.L("where.IsNull(%s)", gobuf.Quote(storage.SoftDelete.Column.Name))
		}
	}
}

// columns lists what the shape streams.
func (e *emitter) columns(b *gobuf.Buf, res *ir.Resource) {
	b.Comment(res.Name + "ShapeColumns are the columns this shape carries.\n\n" +
		"They are the resource's readable fields — the same set a GET returns — so " +
		"a column excluded from the API is excluded here without anybody having to " +
		"remember.")
	b.L("var %sShapeColumns = []string{", res.Name)
	for _, f := range res.Fields {
		if f.Column == nil || !f.In(ir.FieldOpRead) {
			continue
		}
		b.L("%s,", gobuf.Quote(f.Column.Name))
	}
	b.L("}")
	b.NL()
}

// versionLabel is the Postgres label a row of the given generation carries.
//
// The wire value, not the Go constant: this ends up as a bound parameter in a
// filter the sync service runs against the database, where the label is what
// exists.
func (e *emitter) versionLabel(res *ir.Resource, label string) string {
	col := e.doc.Resolve(res.Storage.Snapshot.VersionType)
	if col == nil || col.EnumType == "" {
		return label
	}
	for _, enum := range e.doc.API.Enums {
		if enum.PgType != col.EnumType {
			continue
		}
		for _, v := range enum.Values {
			if v.Wire == label || v.Name == label {
				return v.Wire
			}
		}
	}
	return label
}
