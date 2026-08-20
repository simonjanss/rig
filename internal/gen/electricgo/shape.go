package electricgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// shapeFile emits one resource's shape endpoint.
func (e *emitter) shapeFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.paramsType(b, res)
	e.scopeType(b, res)
	e.handler(b, res)

	return artifact(naming.Snake(res.Name)+"_shape.gen.go", b, gen.Overwrite)
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

// scopeType declares the hook an application implements.
func (e *emitter) scopeType(b *gobuf.Buf, res *ir.Resource) {
	var (
		ctxPkg  = b.Import("context")
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
	)

	b.Comment(res.Name + "Scope narrows the shape further.\n\n" +
		"It receives a filter that already carries the tenant and lifecycle " +
		"conditions, and can only add to it — every condition is joined with AND, " +
		"so there is nothing a scope can write that widens what the subscriber " +
		"sees. Add conditions through the Where methods rather than as text: they " +
		"bind their values, and a shape filter built by string concatenation is an " +
		"injection point with a streaming response attached.\n\n" +
		"Returning an error refuses the subscription.")
	b.L("type %sScope func(ctx %s.Context, r *%s.Request, claims %s.Claims, p %sShapeParams, w *%s.Where) error",
		res.Name, ctxPkg, httpPkg, tenPkg, res.Name, elecPkg)
	b.NL()
}

// handler emits the endpoint.
func (e *emitter) handler(b *gobuf.Buf, res *ir.Resource) {
	var (
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		storage = res.Storage
		admin   = res.Electric.Auth == ir.ElectricAuthAdmin
	)

	b.Comment("handle" + res.Name + "Shape serves GET " + res.Electric.Path + ".")
	b.L("func handle%sShape(s Server, scope %sScope) %s.HandlerFunc {", res.Name, res.Name, httpPkg)
	b.L("return func(w %s.ResponseWriter, r *%s.Request) {", httpPkg, httpPkg)
	b.L("claims, where, ok := prepare(s, w, r, %t)", admin)
	b.L("if !ok { return }")
	b.NL()

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
	if storage.IsSoftDeletable() {
		b.Comment("A retired row stops appearing in reads, so it stops appearing " +
			"in a live stream too.")
		b.L("where.IsNull(%s)", gobuf.Quote(storage.SoftDelete.Column.Name))
	}
	if storage.IsSnapshotable() {
		b.Comment("Snapshots are prior versions. A subscriber wants the live row.")
		b.L("where.Eq(%s, %s)", gobuf.Quote(storage.Snapshot.VersionType.Name),
			gobuf.Quote(e.versionOriginal(res)))
	}
	b.NL()

	b.L("params, err := parse%sShapeParams(r)", res.Name)
	b.L("if err != nil {")
	b.L("fail(s, w, r, err)")
	b.L("return")
	b.L("}")
	b.NL()

	b.L("if scope != nil {")
	b.L("if err := scope(r.Context(), r, claims, params, where); err != nil {")
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

	e.columns(b, res)
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

// versionOriginal is the Postgres label a live row carries.
//
// The wire value, not the Go constant: this ends up as a bound parameter in a
// filter the sync service runs against the database, where the label is what
// exists.
func (e *emitter) versionOriginal(res *ir.Resource) string {
	col := e.doc.Resolve(res.Storage.Snapshot.VersionType)
	if col == nil || col.EnumType == "" {
		return "Original"
	}
	for _, enum := range e.doc.API.Enums {
		if enum.PgType != col.EnumType {
			continue
		}
		for _, v := range enum.Values {
			if v.Wire == "Original" || v.Name == "Original" {
				return v.Wire
			}
		}
	}
	return "Original"
}
