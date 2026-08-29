package compile

import (
	"cmp"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/pkg/ir"
)

// Meta describes the run that produced a document.
type Meta struct {
	// Tool identifies the version of rig that generated it.
	Tool string
	// Permissions is whether authorization checks are derived. The zero value
	// derives them, so a caller that forgets to pass it gets the protected shape
	// rather than the open one.
	Permissions project.PermissionMode
}

// Freeze produces the final document.
//
// Three things happen here and nowhere else. Types are resolved once, so no
// generator has to work out whether "LessonStatus" is an enum or an object.
// Routes are computed once, so the router, the specification, and the client
// cannot disagree about a path. And every column reference is checked against
// the schema it claims to describe — the check that makes the document's two
// views structurally unable to drift apart.
//
// A failure from the last of those is a bug in rig, not in the project, and
// says so.
func Freeze(api ir.API, schema ir.Schema, meta Meta) (*ir.Document, diag.List) {
	var diags diag.List

	doc := &ir.Document{
		IRVersion: ir.CurrentVersion,
		Tool:      meta.Tool,
		API:       api,
		Schema:    schema,
	}

	sortDocument(doc)
	doc.Reindex()

	diags.Append(resolveTypes(doc))
	diags.Append(computeRoutes(doc))
	diags.Append(computePermissions(doc, meta.Permissions))
	diags.Append(verifyColumnRefs(doc))

	doc.Valid = !diags.HasErrors()
	doc.Reindex()

	return doc, diags
}

// sortDocument puts everything in a deterministic order. Two runs over the same
// database must produce the same bytes, or the committed document becomes a
// source of spurious diffs.
func sortDocument(doc *ir.Document) {
	slices.SortFunc(doc.Schema.Tables, func(a, b ir.Table) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(doc.Schema.Enums, func(a, b ir.PgEnum) int { return cmp.Compare(a.Name, b.Name) })

	slices.SortFunc(doc.API.Enums, func(a, b ir.Enum) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(doc.API.Objects, func(a, b ir.Object) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(doc.API.Resources, func(a, b ir.Resource) int { return cmp.Compare(a.Name, b.Name) })

	for i := range doc.API.Resources {
		r := &doc.API.Resources[i]
		// Endpoints sort by route rather than by name so that reading the
		// document top to bottom reads like the routing table.
		slices.SortStableFunc(r.Endpoints, func(a, b ir.Endpoint) int {
			return cmp.Or(
				cmp.Compare(a.Path, b.Path),
				cmp.Compare(a.Method, b.Method),
				cmp.Compare(a.Name, b.Name),
			)
		})
		for j := range r.Endpoints {
			slices.Sort(r.Endpoints[j].Errors)
			slices.SortStableFunc(r.Endpoints[j].Responses, func(a, b ir.EndpointResponse) int {
				return cmp.Compare(a.StatusCode, b.StatusCode)
			})
		}
		if r.Storage != nil {
			slices.SortFunc(r.Storage.Relations, func(a, b ir.Relation) int {
				return cmp.Compare(a.Name, b.Name)
			})
		}
	}
}

// resolveTypes stamps every field with what its type name refers to.
func resolveTypes(doc *ir.Document) diag.List {
	var diags diag.List

	resolve := func(f *ir.Field, where string) {
		if f.Type == "" {
			return
		}
		kind, ok := doc.TypeKindOf(f.Type)
		if !ok {
			diags.Add(diag.CodeUnresolvedType, diag.At(where),
				"field %s has type %q, which is not a primitive and not declared anywhere",
				f.Name, f.Type)
			return
		}
		f.TypeKind = kind
	}

	for i := range doc.API.Objects {
		o := &doc.API.Objects[i]
		for j := range o.Fields {
			resolve(&o.Fields[j], o.Name+"."+o.Fields[j].Name)
		}
	}

	for i := range doc.API.Resources {
		r := &doc.API.Resources[i]
		for j := range r.Fields {
			resolve(&r.Fields[j].Field, r.Name+"."+r.Fields[j].Name)
		}
		for j := range r.Endpoints {
			e := &r.Endpoints[j]
			where := r.Name + "." + e.Name
			for k := range e.Request.PathParams {
				resolve(&e.Request.PathParams[k], where)
			}
			for k := range e.Request.QueryParams {
				resolve(&e.Request.QueryParams[k], where)
			}
			for k := range e.Request.BodyParams {
				resolve(&e.Request.BodyParams[k], where)
			}
			for k := range e.Responses {
				for l := range e.Responses[k].BodyFields {
					resolve(&e.Responses[k].BodyFields[l], where)
				}
			}
		}
	}

	return diags
}

// computeRoutes fills in the full route and operation identifier for every
// endpoint, and the full route for every live-sync shape, once.
//
// Both are expanded against the same base path, because both are served by the
// same mux, and both land in one namespace that is checked for collisions.
func computeRoutes(doc *ir.Document) diag.List {
	var diags diag.List

	base := strings.TrimRight(doc.API.BasePath, "/")
	n := naming.New(naming.Config{})

	seen := make(map[string]string)

	for i := range doc.API.Resources {
		r := &doc.API.Resources[i]
		prefix := base + "/" + r.PathSegment

		for j := range r.Endpoints {
			e := &r.Endpoints[j]

			path := prefix + e.Path
			e.Pattern = e.Method + " " + path

			// An alias is written relative to the resource, the same way the
			// endpoint's own path is, and expanded here so the two forms cannot
			// drift.
			for k, alias := range e.AliasPatterns {
				method, rel, ok := strings.Cut(alias, " ")
				if !ok {
					diags.Add(diag.CodeInternal, diag.At(r.Name+"."+e.Name),
						"alias %q is not in the form \"METHOD /path\"", alias)
					continue
				}
				e.AliasPatterns[k] = method + " " + prefix + rel
			}

			// The handler name already carries the right cardinality — ListTodos
			// against GetTodo — so deriving the operation id from it keeps the
			// two in step instead of inventing a second pluralization rule.
			if e.OperationID == "" {
				switch {
				case e.Impl.HandlerName != "":
					e.OperationID = n.GoUnexported(e.Impl.HandlerName)
				default:
					e.OperationID = n.GoUnexported(e.Name + r.Name)
				}
			}

			for _, pattern := range append([]string{e.Pattern}, e.AliasPatterns...) {
				if prev, dup := seen[pattern]; dup {
					diags.Add(diag.CodeInvalidEndpoint, diag.At(r.Name+"."+e.Name),
						"route %q is already served by %s", pattern, prev)
					continue
				}
				seen[pattern] = r.Name + "." + e.Name
			}
		}
	}

	// A live-sync shape's route is expanded the same way and against the same
	// base, because it is served by the same mux: api.Register mounts the shape
	// routes beside the REST ones. The second pass is what lets it be checked
	// against every REST route rather than only the ones ahead of it.
	//
	// The check is the live shape's route alone. The trash and the history sit
	// on longer paths under the same stem and are composed by the generator that
	// mounts them; the shape's own route is the one an endpoint can land on by
	// accident, and it is the one that would take the mux down at startup.
	for i := range doc.API.Resources {
		r := &doc.API.Resources[i]
		if r.Electric == nil {
			continue
		}
		r.Electric.Path = base + r.Electric.Path

		pattern := "GET " + r.Electric.Path
		if prev, dup := seen[pattern]; dup {
			diags.Add(diag.CodeInvalidEndpoint, diag.At(r.Name+".electric"),
				"%s streams on %q, which is already served by %s", r.Name, pattern, prev)
			continue
		}
		seen[pattern] = r.Name + ".electric"
	}

	return diags
}

// verifyColumnRefs checks the denormalized copies against the schema.
//
// Every API field backed by storage carries a small copy of its column's facts,
// because that is what generators need inline. This is the check that keeps the
// copy honest: if it ever disagrees with the schema, a generator emitting SQL
// and one emitting JSON would be working from different truths.
func verifyColumnRefs(doc *ir.Document) diag.List {
	var diags diag.List

	check := func(ref *ir.ColumnRef, where string) {
		if ref == nil {
			return
		}
		col := doc.Column(ref.Table, ref.Name)
		if col == nil {
			diags.Add(diag.CodeColumnRefMismatch, diag.At(where),
				"%s references column %s.%s, which is not in the schema",
				where, ref.Table, ref.Name)
			return
		}
		if ref.SQLType != col.SQLType || ref.Nullable != col.Nullable {
			diags.Add(diag.CodeColumnRefMismatch, diag.At(where),
				"%s describes %s.%s as %s (nullable %t) but the schema says %s (nullable %t)",
				where, ref.Table, ref.Name, ref.SQLType, ref.Nullable, col.SQLType, col.Nullable)
		}
	}

	for i := range doc.API.Resources {
		r := &doc.API.Resources[i]
		for j := range r.Fields {
			check(r.Fields[j].Column, r.Name+"."+r.Fields[j].Name)
		}
		s := r.Storage
		if s == nil {
			continue
		}
		check(s.Tenant, r.Name+".storage.tenant")
		if s.Audit != nil {
			check(s.Audit.CreatedAt, r.Name+".storage.audit.created_at")
			check(s.Audit.CreatedBy, r.Name+".storage.audit.created_by")
			check(s.Audit.UpdatedAt, r.Name+".storage.audit.updated_at")
			check(s.Audit.UpdatedBy, r.Name+".storage.audit.updated_by")
			check(s.Audit.DeletedAt, r.Name+".storage.audit.deleted_at")
			check(s.Audit.DeletedBy, r.Name+".storage.audit.deleted_by")
		}
		if s.SoftDelete != nil {
			check(s.SoftDelete.Column, r.Name+".storage.soft_delete")
			check(s.SoftDelete.Actor, r.Name+".storage.soft_delete.actor")
		}
		if s.Snapshot != nil {
			check(s.Snapshot.VersionType, r.Name+".storage.snapshot.version_type")
			check(s.Snapshot.FromID, r.Name+".storage.snapshot.from_id")
			check(s.Snapshot.FromAt, r.Name+".storage.snapshot.from_at")
		}
	}

	for i := range doc.API.Objects {
		o := &doc.API.Objects[i]
		for j := range o.Fields {
			check(o.Fields[j].Column, o.Name+"."+o.Fields[j].Name)
		}
	}

	return diags
}
