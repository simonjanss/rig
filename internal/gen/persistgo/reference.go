package persistgo

import (
	"sort"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// The reference check closes a hole that has nothing to do with any one
// feature:
//
//	A generated write may not reference a row the caller could not have read.
//
// A nested route resolves its parent through the repository before it does
// anything, so it cannot be aimed at a row the caller cannot see. An ordinary
// create on a child table has no such step: the parent's identifier arrives in
// the body as a column value, and tenancy is the only thing standing behind it.
// Inside one tenant, nothing stops a caller naming a parent an `access: { scope:
// own }` rule makes invisible to them — attach a row to a stranger's parent,
// then read it back through your own child.
//
// This is true of every child table rig generates. The answer is not to
// special-case the tables where it is easiest to notice, which would make one
// kind of table behave unlike every other for reasons nobody could infer from
// the schema.
//
// It belongs in the repository for the reason [emitter.ownerFilter] already
// gives: a hook reaching for the repository, a custom endpoint and the generated
// handler all pass through here, and a narrowing that only the generated read
// path applied is a narrowing a custom endpoint silently drops.

// checkedRef is one foreign key a write has to justify.
type checkedRef struct {
	// Field is the input field carrying the identifier.
	Field string
	// Target is the resource the key points at, whose read scope the check
	// borrows.
	Target *ir.Resource
	// Nullable means the column can be cleared, so there may be no identifier to
	// check.
	Nullable bool
}

// checkedRefs are the references a write of this operation has to justify.
//
// Only foreign keys to exposed resources: an unexposed table has no repository
// and no reader, so there is no predicate to borrow. Those are covered by the
// tenant being inside the key itself, or not at all.
func (e *emitter) checkedRefs(res *ir.Resource, op string) []checkedRef {
	if res.Storage == nil {
		return nil
	}

	// A column is only worth checking if this operation can write it. An
	// immutable key is checked on create and never again.
	fields := writableFields(res, op)
	writable := make(map[string]*ir.ResourceField, len(fields))
	for i := range fields {
		if fields[i].Column != nil {
			writable[fields[i].Column.Name] = &fields[i]
		}
	}

	var out []checkedRef
	for _, rel := range res.Storage.Relations {
		if rel.Kind != ir.RelationBelongsTo || rel.LocalColumn == "" {
			continue
		}
		f, ok := writable[rel.LocalColumn]
		if !ok {
			continue
		}
		target := e.resource(rel.Target)
		if target == nil || target.Unexposed || target.Storage == nil {
			continue
		}
		// A target nothing narrows at all is one where the check could only ever
		// say yes: the foreign key constraint has already proved the row exists,
		// and there is no scope for it to be outside of.
		if !e.narrowsReads(target) {
			continue
		}
		out = append(out, checkedRef{
			Field:    f.Name,
			Target:   target,
			Nullable: f.IsNullable(),
		})
	}
	return out
}

// narrowsReads reports whether there is any way for a row of this resource to
// be one the caller could not have read.
//
// Tenancy is in the list, and it is the case that is easy to argue away and
// should not be. A single-column foreign key to another table proves only that
// the row exists — nothing in `references team (id)` says the team is in the
// caller's tenant, so a create can point a row in tenant A at a row in tenant B
// and the database will take it. A composite key carrying the tenant would stop
// that, and rig recommends one where it matters, but a check that only fired for
// tables that had already been written the careful way would be a check that
// never fires where it is needed.
//
// Owner scope is the case the invariant was written for. The lifecycle
// predicates come with it because a relation reaching into the trash or into the
// history is one the read side would never have produced: List excludes both by
// default, and `original` says as much where it is rendered — "excludes the
// history, which is never what a condition on a relation means".
func (e *emitter) narrowsReads(res *ir.Resource) bool {
	s := res.Storage
	return s.Tenant != nil || s.IsOwnerScoped() || s.IsSoftDeletable() || s.IsSnapshotable()
}

// resource finds a resource by name.
func (e *emitter) resource(name string) *ir.Resource {
	for i := range e.doc.API.Resources {
		if e.doc.API.Resources[i].Name == name {
			return &e.doc.API.Resources[i]
		}
	}
	return nil
}

// referenceTargets are the resources some write in this document has to check
// against, so the store file emits a visibility helper for each and no more.
func (e *emitter) referenceTargets() []*ir.Resource {
	seen := map[string]*ir.Resource{}
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		for _, op := range []string{ir.FieldOpCreate, ir.FieldOpUpdate} {
			for _, ref := range e.checkedRefs(res, op) {
				seen[ref.Target.Name] = ref.Target
			}
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]*ir.Resource, 0, len(names))
	for _, name := range names {
		out = append(out, seen[name])
	}
	return out
}

// needsReferenceCheck reports whether any write on this resource performs one,
// which is what decides whether a create has to open a transaction it would
// otherwise not need.
func (e *emitter) needsReferenceCheck(res *ir.Resource, op string) bool {
	return len(e.checkedRefs(res, op)) > 0
}

// visibilityFunc emits the predicate one target's rows are visible under.
//
// It is a package-level function rather than a repository method because the
// caller is another table's repository, and because there is nothing about it
// worth letting anybody override.
func (e *emitter) visibilityFunc(b *gobuf.Buf, res *ir.Resource) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		fmtPkg  = b.Import("fmt")
	)
	s := res.Storage

	b.Comment("visible" + res.Name + " reports whether this caller could have read " +
		"the " + res.Name + " row named, which is what a write pointing at it has " +
		"to be able to say.\n\n" +
		"It takes the transaction rather than the pool so the answer and the write " +
		"that depends on it are the same unit of work.\n\n" +
		"There is no readopt here on purpose. SkipTenantScope and SkipOwnerScope " +
		"exist so a background job can read past the boundary; a request handler " +
		"reaching a foreign key through them would be the boundary having an " +
		"opt-out, which is the thing this closes.")
	b.L("func visible%s(ctx %s.Context, tx %s.Conn, claims %s.Claims, id %s.UUID) (bool, error) {",
		res.Name, ctxPkg, dbxPkg, tenPkg, uuidPkg)

	b.L("args := []any{id}")
	b.L("where := \"id = $1\"")
	if s.Tenant == nil && !s.IsOwnerScoped() {
		b.L("_ = claims")
	}
	b.NL()

	if s.Tenant != nil {
		b.L("args = append(args, claims.TenantID)")
		b.L("where += %s.Sprintf(\" AND %s = $%%d\", len(args))", fmtPkg, s.Tenant.Name)
	}
	if s.IsOwnerScoped() {
		b.Comment("The reason this function exists: a row inside the tenant that " +
			"this caller was never allowed to know about.")
		b.L("args = append(args, claims.AccountID)")
		b.L("where += %s.Sprintf(\" AND %s = $%%d\", len(args))", fmtPkg, s.Owner.Name)
	}
	if s.IsSoftDeletable() {
		b.Comment("A row in the trash is not something to point new rows at: the " +
			"sweeper is coming for it, and the reference would outlive it.")
		b.L("where += %s", gobuf.Quote(" AND "+s.SoftDelete.Column.Name+" IS NULL"))
	}
	if s.IsSnapshotable() {
		b.Comment("A snapshot is a copy of a past state. A key pointing at one " +
			"names a version rather than the thing, which is never what a relation " +
			"means.")
		b.L("args = append(args, %s)", e.versionOriginal(b, res))
		b.L("where += %s.Sprintf(\" AND %s = $%%d\", len(args))", fmtPkg, s.Snapshot.VersionType.Name)
	}
	b.NL()

	b.L("var one int")
	b.L("err := tx.QueryRow(ctx, \"SELECT 1 FROM %s WHERE \"+where, args...).Scan(&one)",
		s.Table)
	b.L("if %s.IsNoRows(err) { return false, nil }", dbxPkg)
	b.L("if err != nil {")
	b.L("return false, %s.Internal(err, \"check that %s %%s can be referenced\", id)", errPkg, res.Name)
	b.L("}")
	b.L("return true, nil")
	b.L("}")
	b.NL()
}

// refused emits the field error one failed check answers with.
//
// It is a field error rather than a 403, and deliberately the same field error
// as an identifier that names nothing at all. Telling the two apart would
// confirm the row exists to somebody who cannot see it, which is exactly what an
// owner-scoped table is trying not to say.
func (e *emitter) refused(b *gobuf.Buf, ref checkedRef, errType, idExpr string) {
	var (
		modelPkg = e.model(b)
		errPkg   = b.Import(runtimeModule + "/rigerr")
	)

	b.L("ok, err := visible%s(ctx, tx, claims, %s)", ref.Target.Name, idExpr)
	b.L("if err != nil { return err }")
	b.L("if !ok {")
	b.L("return &%s.%s{%s: %s.NewFieldError(%s.FieldCodeNotFound, \"no %s with id %%s\", %s)}",
		modelPkg, errType, ref.Field, errPkg, errPkg, ref.Target.Name, idExpr)
	b.L("}")
}

// createReferenceChecks emits the checks for a create, where every writable
// field is a plain value that is always being written.
func (e *emitter) createReferenceChecks(b *gobuf.Buf, res *ir.Resource) {
	refs := e.checkedRefs(res, ir.FieldOpCreate)
	if len(refs) == 0 {
		return
	}

	b.Comment("Every identifier this row would store has to name a row the caller " +
		"could have read. One indexed lookup per key, inside the transaction that " +
		"is about to do the write.")
	errType := res.Name + "CreateInputError"
	for _, ref := range refs {
		expr := "in.Input." + ref.Field
		if ref.Nullable {
			// A key left null points at nothing, and nothing is always allowed.
			b.L("if %s != nil {", expr)
			expr = "*" + expr
		} else {
			// A block of its own, so a second key on the same row can declare the
			// same two names without the first having to be spelled differently.
			b.L("{")
		}
		e.refused(b, ref, errType, expr)
		b.L("}")
		b.NL()
	}
}

// updateReferenceChecks emits the checks for an update, where a field is only
// written when the caller touched it.
//
// A key the request did not mention keeps whatever it already held, and
// re-checking that would mean an update to a caption failing because somebody
// else's row moved out of scope in the meantime.
func (e *emitter) updateReferenceChecks(b *gobuf.Buf, res *ir.Resource) {
	refs := e.checkedRefs(res, ir.FieldOpUpdate)
	if len(refs) == 0 {
		return
	}

	b.Comment("Every identifier this update would store has to name a row the " +
		"caller could have read — the ones it actually sends, and no others.")
	errType := res.Name + "UpdateInputError"
	for _, ref := range refs {
		if ref.Nullable {
			// Touched covers a value and an explicit clear alike. Only the first
			// names anything, and a clear is always allowed.
			b.L("if v := in.Input.%s.Ptr(); in.Input.%s.Touched() && v != nil {",
				ref.Field, ref.Field)
			e.refused(b, ref, errType, "*v")
		} else {
			b.L("if v, sent := in.Input.%s.Get(); sent {", ref.Field)
			e.refused(b, ref, errType, "v")
		}
		b.L("}")
		b.NL()
	}
}
