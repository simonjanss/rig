package persistgo

import (
	"fmt"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/ir"
)

// caching reports whether anything in this document holds rows between
// requests.
//
// Both halves are required and neither implies the other: the block owns the
// channel a withdrawal travels on, and a table's `cache: true` is the promise
// that every write to it goes through here. The compiler refuses the second
// without the first, so this is belt and braces — and it is what keeps
// runtime/cache out of the go.mod of every project that did not ask.
func (e *emitter) caching() bool {
	c := e.doc.API.Cache
	if c == nil || !c.Enabled {
		return false
	}
	for _, res := range e.resources() {
		if res.Cached {
			return true
		}
	}
	return false
}

// cached reports whether this resource's Get is held.
func (e *emitter) cached(res *ir.Resource) bool {
	return e.caching() && res.Cached
}

// cachedResources are the resources holding rows, in document order.
func (e *emitter) cachedResources() []*ir.Resource {
	if !e.caching() {
		return nil
	}
	var out []*ir.Resource
	for _, res := range e.resources() {
		if res.Cached {
			out = append(out, res)
		}
	}
	return out
}

// cacheField is the name of the store field holding one resource's rows.
func cacheField(res *ir.Resource) string {
	return naming.New(naming.Config{}).GoUnexported(res.Name) + "Cache"
}

// cacheTopic is the name a resource's withdrawals travel under.
//
// The table, because that is the one name both halves of a deployment agree on
// without either of them being generated from the other: two replicas of the
// same service compiled from the same schema derive the same topic, and a
// resource somebody renamed in configuration does not silently stop hearing its
// own invalidations. It may not hold a colon, which a table name cannot.
func cacheTopic(res *ir.Resource) string { return res.Storage.Table }

// storeCacheHeld emits the store's cache fields, one per cached resource.
func (e *emitter) storeCacheHeld(b *gobuf.Buf) {
	for _, res := range e.cachedResources() {
		b.L("%s *%s.RowCache[*%s]", cacheField(res),
			b.Import(runtimeModule+"/cache"), e.entity(b, res))
	}
}

// storeCacheBus emits the bus the store owns, already listening.
//
// Built, served and started inside New rather than handed in, which is the same
// departure `server-go` makes for the authentication caches and for the same
// reason: a generated constructor that a main.go has to remember to Start is one
// more thing to forget, and forgetting it is invisible — the cache goes on
// answering and nothing withdraws anything. There is no Serve to call and no
// order to get right.
//
// A second bus beside the one inside auth.New, when a project has both. Two
// LISTEN connections rather than one, and the alternative is worse: threading
// auth's bus into a store that is built before auth exists, with a nil to handle
// at every step. They can share a channel because a bus ignores a topic it does
// not hold — see cache.Bus.deliver — which is the same property that lets
// replicas of different services share one.
func (e *emitter) storeCacheBus(b *gobuf.Buf) {
	cached := e.cachedResources()
	if len(cached) == 0 {
		return
	}

	c := e.doc.API.Cache
	cachePkg := b.Import(runtimeModule + "/cache")

	b.Comment("The invalidation channel, listening from here on. Every held row " +
		"is withdrawn over it by the transaction that changed the row, so this is " +
		"what makes the lifetime a backstop rather than a promise about staleness.")
	b.L("s.cacheBus = %s.NewBus(%s.BusConfig{", cachePkg, cachePkg)
	b.L("Pool: pool,")
	b.L("Channel: %s,", gobuf.Quote(c.Channel))
	b.Comment("The one line anybody reads when this stops working. Losing the " +
		"channel is correct and silent — the caches go dead and every read is a " +
		"query again — so nothing else about the process would say so.")
	b.L("Logger: cfg.Logger,")
	b.L("})")
	b.NL()

	for _, res := range cached {
		b.L("s.%s = %s.NewRowCache[*%s](%s.RowCacheConfig{",
			cacheField(res), cachePkg, e.entity(b, res), cachePkg)
		b.L("Topic: %s,", gobuf.Quote(cacheTopic(res)))
		b.L("TTL: %s,", goDurationSeconds(b, c.TTLSeconds))
		b.L("MaxEntries: %d,", c.MaxEntries)
		b.L("})")
		b.L("s.%s.Serve(s.cacheBus)", cacheField(res))
	}
	b.NL()

	b.Comment("After the caches are attached, so nothing can be delivered a " +
		"notification for a topic that is not registered yet.")
	b.L("s.cacheBus.Start()")
	b.NL()
}

// storeCacheHeldBus is the bus field on the Store.
func (e *emitter) storeCacheHeldBus(b *gobuf.Buf) {
	if len(e.cachedResources()) == 0 {
		return
	}
	b.L("cacheBus *%s.Bus", b.Import(runtimeModule+"/cache"))
}

// storeCacheClose emits the shutdown.
//
// Safe to leave out of a main.go, which is why it is the only application-facing
// surface this feature has: a listener that is not running reports itself as not
// live, and a cache that is not live reads through. Skipping it costs a Postgres
// connection held until the process exits, not correctness.
func (e *emitter) storeCacheClose(b *gobuf.Buf) {
	if len(e.cachedResources()) == 0 {
		return
	}

	ctxPkg := b.Import("context")

	b.Comment("Close stops listening for invalidations.\n\n" +
		"Worth registering beside the rest of a program's shutdown:\n\n" +
		"\tapp.CloseWithin(\"store\", 5*time.Second, repos.Close)\n\n" +
		"and safe to leave out. A bus that has stopped is a bus that is not live, " +
		"and a cache that is not live reads through and holds nothing — so " +
		"forgetting this costs a connection held until the process exits rather " +
		"than a row somebody cannot withdraw.")
	b.L("func (s *Store) Close(ctx %s.Context) error {", ctxPkg)
	b.L("return s.cacheBus.Close(ctx)")
	b.L("}")
	b.NL()
}

// goDurationSeconds renders a float count of seconds as a Go duration.
func goDurationSeconds(b *gobuf.Buf, seconds float64) string {
	timePkg := b.Import("time")
	if seconds <= 0 {
		return "0"
	}
	ms := int64(seconds * 1000)
	if ms%1000 == 0 {
		return fmt.Sprintf("%d * %s.Second", ms/1000, timePkg)
	}
	return fmt.Sprintf("%d * %s.Millisecond", ms, timePkg)
}

// cloneFuncName is the copy a cached read hands out.
func cloneFuncName(res *ir.Resource) string { return "clone" + res.Name }

// cloneFunc emits the copy every cached read returns.
//
// Cloning on the way *out* rather than in the loader, which is the opposite of
// what runtime/cache recommends and for a reason the package cannot know. Its
// advice is right when a cached value has one shape and many readers; here the
// value is a row, and what a row was before this cache existed is a fresh
// allocation per caller — scanTodo runs once per read and nobody shares
// anything. A cache that handed the same row to every request in a window would
// have changed something other than where the read happened, and the thing it
// changed is that one caller editing a field is editing what the others are
// about to encode.
//
// The copy goes exactly as deep as a scan did: the struct, and its own storage
// for every pointer and every slice on it. It does not go deeper, because a
// scan does not either — and the one shape where that is not enough, a jsonb
// column with a `go_type` of its own, is refused at compile time rather than
// half-copied here.
func (e *emitter) cloneFunc(b *gobuf.Buf, res *ir.Resource) {
	if !e.cached(res) {
		return
	}

	entity := e.entity(b, res)

	b.Comment(cloneFuncName(res) + " is a caller's own copy of a held row.\n\n" +
		"A cached read hands one of these back rather than the row it holds, so " +
		"that a caller which writes to what it was given is writing to its own " +
		"copy. Nil in, nil out: a miss is an error rather than a nil row, but a " +
		"helper that says so at the top is one less thing for a call site to be " +
		"careful about.")
	b.L("func %s(m *%s) *%s {", cloneFuncName(res), entity, entity)
	b.L("if m == nil { return nil }")
	b.L("cp := *m")

	for _, f := range storedFields(res) {
		switch cloneKindOf(f) {
		case cloneSlice:
			b.L("cp.%s = %s.Clone(m.%s)", f.Name, b.Import("slices"), f.Name)
		case cloneNumeric:
			bigPkg := b.Import("math/big")
			b.L("if cp.%s.Int != nil { cp.%s.Int = new(%s.Int).Set(cp.%s.Int) }",
				f.Name, f.Name, bigPkg, f.Name)
		case clonePointerNumeric:
			bigPkg := b.Import("math/big")
			b.L("if m.%s != nil {", f.Name)
			b.L("v := *m.%s", f.Name)
			b.Comment("A Numeric carries a big.Int, which is a pointer. A scan " +
				"allocated one per row and so does this.")
			b.L("if v.Int != nil { v.Int = new(%s.Int).Set(v.Int) }", bigPkg)
			b.L("cp.%s = &v", f.Name)
			b.L("}")
		case clonePointer:
			b.L("if m.%s != nil { v := *m.%s; cp.%s = &v }", f.Name, f.Name, f.Name)
		}
	}

	b.L("return &cp")
	b.L("}")
	b.NL()
}

// cloneKind is how much of a field a copy of the struct leaves behind.
type cloneKind int

const (
	// cloneValue needs nothing: assigning the struct copied it.
	cloneValue cloneKind = iota
	// clonePointer needs its own allocation, holding a value.
	clonePointer
	// cloneSlice needs its own backing array.
	cloneSlice
	// cloneNumeric is a pgtype.Numeric, which is a value around a *big.Int.
	cloneNumeric
	// clonePointerNumeric is a pointer to one.
	clonePointerNumeric
	// cloneUnknown is a type this generator will not guess about.
	cloneUnknown
)

// copyableGoTypes are the types a generated clone can hand out its own copy of.
//
// It is the list rig's own type mapping produces — see internal/pgtypes — and it
// is exhaustive on purpose rather than a default: a type nobody has thought about
// gets refused rather than shallow-copied, because a shallow copy of something
// with a map inside it is a cache two requests share a field of.
//
// netip.Addr and netip.Prefix are in it and hold an internal pointer, which is
// the one entry worth justifying: both are documented as immutable comparable
// values, so no caller has a way to write through one.
var copyableGoTypes = map[string]bool{
	"bool": true, "float32": true, "float64": true,
	"int": true, "int16": true, "int64": true, "int32": true,
	"string": true, "time.Time": true, "uuid.UUID": true,
	"netip.Addr": true, "netip.Prefix": true,
}

// cloneKindOf classifies one stored field.
//
// It reads the resolved kind rather than the spelling of the Go type, because
// the two things it most needs to tell apart look identical written down: a
// generated enum is a string behind a bare identifier, and a jsonb column's
// `go_type` is an application struct behind one. The first copies as a value and
// the second is exactly what this refuses to guess about.
func cloneKindOf(f ir.ResourceField) cloneKind {
	if elem, ok := sliceElement(f.GoType); ok {
		if !copyableElement(f, elem) {
			return cloneUnknown
		}
		return cloneSlice
	}

	switch {
	case f.GoType == "pgtype.Numeric":
		return cloneNumeric
	case f.GoType == "*pgtype.Numeric":
		return clonePointerNumeric
	case f.TypeKind == ir.TypeKindEnum:
		if strings.HasPrefix(f.GoType, "*") {
			return clonePointer
		}
		return cloneValue
	case f.TypeKind != ir.TypeKindPrimitive:
		// An object or a resource. rig does not know what is inside it.
		return cloneUnknown
	}

	base := strings.TrimPrefix(f.GoType, "*")
	if !copyableGoTypes[base] {
		return cloneUnknown
	}
	if strings.HasPrefix(f.GoType, "*") {
		return clonePointer
	}
	return cloneValue
}

// uncopyableFields are a cached resource's fields a clone cannot give a caller
// its own copy of.
//
// Two shapes reach here. A jsonb column with a `go_type` of its own is the
// obvious one: the struct belongs to the application, so a copy of the row shares
// whatever map or slice is inside it. The other is an array whose elements are
// not themselves copyable — `numeric[]`, `bytea[]`, `jsonb[]` — where cloning the
// backing array leaves every element pointing at what it pointed at before.
//
// Either way two requests in one window would be reading a field either of them
// can write. Refused rather than half-copied — the whole promise of the clone is
// that a cached read is indistinguishable from a fresh one.
func uncopyableFields(res *ir.Resource) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range storedFields(res) {
		if cloneKindOf(f) == cloneUnknown {
			out = append(out, f)
		}
	}
	return out
}

// sliceElement is what one element of a field's storage is, for storage that is
// its own backing array — so a copy of the struct would share it.
//
// json.RawMessage is the one that does not look like it: it is a []byte behind a
// name, so the syntax says nothing and the answer is still yes. Its elements are
// bytes, which is why naming them is worth the line.
func sliceElement(goType string) (elem string, ok bool) {
	if goType == "json.RawMessage" {
		return "byte", true
	}
	return strings.CutPrefix(goType, "[]")
}

// copyableElement reports whether slices.Clone is the whole of the copy.
//
// It is only the whole of it when nothing inside an element can be written
// through, and the element is where the exhaustive list has to be applied a
// second time: a Postgres array reaches rig as `[]` in front of its element's Go
// type, so `numeric[]` is []pgtype.Numeric — elements each holding the *big.Int
// that the two scalar Numeric cases deliberately deep-copy — and `bytea[]` is
// [][]byte, whose inner slices slices.Clone would leave shared. Both are two
// requests sharing a field, which is the one thing the clone exists to prevent,
// so both are refused rather than half-copied.
//
// An enum array is the exception and needs nothing: a generated enum is a string
// behind a name.
func copyableElement(f ir.ResourceField, elem string) bool {
	if f.TypeKind == ir.TypeKindEnum {
		return true
	}
	// byte for json.RawMessage and for bytea, which is []byte and so has bytes
	// under it rather than a type the list would ever name.
	return elem == "byte" || copyableGoTypes[elem]
}

// cacheKeyFuncName is the key a resource's rows are held under.
func cacheKeyFuncName(res *ir.Resource) string {
	return naming.New(naming.Config{}).GoUnexported(res.Name) + "CacheKey"
}

// cacheKeyFunc emits the one place a key is spelled.
//
// One function and not two, which matters more here than it looks. Two things
// need this key and they hold different material: a read has the caller's claims
// and the identifier it was asked for, and a write has the row. If each of them
// formatted its own key they would agree until somebody edited one, and the
// failure that follows is a write that publishes a withdrawal nothing is holding
// — a cache that silently never invalidates, which is indistinguishable from one
// that works right up until it matters. So the format lives here and both sides
// pass parts in.
//
// The scope is in the key rather than checked after the read, and that is why a
// held row cannot reach the wrong caller. The alternative is a second
// implementation of the tenant predicate, in Go, that has to go on agreeing with
// the one in the statement; one of those two disagreeing is a cross-tenant read.
// So the cache is keyed by exactly what the statement filtered on.
//
// Every part is a uuid, so no part can contain the separator, and the identifier
// is last so that what varies is at the end.
func (e *emitter) cacheKeyFunc(b *gobuf.Buf, res *ir.Resource) {
	if !e.cached(res) {
		return
	}

	uuidPkg := b.Import("github.com/google/uuid")

	params, parts := []string{}, []string{}
	if _, ok := e.tenantFilter(res); ok {
		params = append(params, "tenantID")
		parts = append(parts, "tenantID.String()")
	}
	if _, ok := e.ownerFilter(res); ok {
		params = append(params, "ownerID")
		parts = append(parts, "ownerID.String()")
	}
	params = append(params, "id")
	parts = append(parts, "id.String()")

	b.Comment(cacheKeyFuncName(res) + " is what one held row is keyed by.\n\n" +
		"Every scope the read applied is in it, because a cached answer is only " +
		"reusable by a caller the same filters would have answered the same way. A " +
		"read that widened either scope is not held at all — see Get — so there is " +
		"no wide answer here for a narrow key to collide with.")
	b.L("func %s(%s %s.UUID) string {",
		cacheKeyFuncName(res), strings.Join(params, ", "), uuidPkg)
	b.L("return %s", strings.Join(parts, ` + "/" + `))
	b.L("}")
	b.NL()
}

// cacheKeyForRead is the key expression a read uses, from the claims it was made
// under.
func (e *emitter) cacheKeyForRead(res *ir.Resource, id string) string {
	return e.cacheKeyExpr(res, "claims.TenantID", "claims.AccountID", id)
}

// cacheKeyForRow is the key expression a write uses, from the row it is
// changing.
//
// The row rather than the writer's claims, and that is not a stylistic choice.
// The entry being withdrawn was stored by whoever *read* the row, keyed by the
// scope their read matched — so the values in that key are the row's own tenant
// and the row's own owner, which is exactly what the filters compared against.
// Taking them from the writer would be right only for as long as the writer and
// the reader are the same person, and an administrative write is where that
// stops being true.
//
// A row is reached by the identifier passed in rather than by its own ID field,
// because a hard delete withdraws its snapshots too and those are rows this one
// has never seen.
func (e *emitter) cacheKeyForRow(res *ir.Resource, row, id string) string {
	tenant := ""
	if column, ok := e.tenantFilter(res); ok {
		tenant = row + "." + e.fieldForColumn(res, column)
	}
	return e.cacheKeyExpr(res, tenant, "ownerOf"+res.Name+"("+row+")", id)
}

// cacheKeyExpr is the call to the key function, given expressions for the parts
// this resource's key happens to have.
//
// The three callers hold their material in three different shapes — a read has
// claims, a write has the row, and a hard delete has columns it has just scanned
// out of the statement that removed them — and only this decides which of those
// parts the key names. Passing an expression a resource has no place for is
// allowed and ignored, which is what lets a caller build all three without asking
// what the table looks like.
func (e *emitter) cacheKeyExpr(res *ir.Resource, tenant, owner, id string) string {
	args := []string{}
	if _, ok := e.tenantFilter(res); ok {
		args = append(args, tenant)
	}
	if _, ok := e.ownerFilter(res); ok {
		args = append(args, owner)
	}
	args = append(args, id)
	return fmt.Sprintf("%s(%s)", cacheKeyFuncName(res), strings.Join(args, ", "))
}

// ownerFunc emits the row's owner as a plain uuid.
//
// The column is nullable, and a null one is a row no narrow read ever matched:
// the filter compares it to the caller's account, and nothing equals null. So
// the zero uuid here is a key that was never held, and withdrawing a key nothing
// holds is explicitly not an error — a bus delivers every replica the same
// notification and most of them will not have it.
func (e *emitter) ownerFunc(b *gobuf.Buf, res *ir.Resource) {
	column, ok := e.ownerFilter(res)
	if !e.cached(res) || !ok {
		return
	}

	var (
		uuidPkg = b.Import("github.com/google/uuid")
		field   = e.fieldForColumn(res, column)
		entity  = e.entity(b, res)
	)

	b.Comment("ownerOf" + res.Name + " is the account a row belongs to, for the " +
		"key a withdrawal is published under.")
	b.L("func ownerOf%s(m *%s) %s.UUID {", res.Name, entity, uuidPkg)
	if strings.HasPrefix(e.goTypeForColumn(res, column), "*") {
		b.L("if m.%s == nil { return %s.Nil }", field, uuidPkg)
		b.L("return *m.%s", field)
	} else {
		b.L("return m.%s", field)
	}
	b.L("}")
	b.NL()
}

// goTypeForColumn is a stored field's Go type.
func (e *emitter) goTypeForColumn(res *ir.Resource, column string) string {
	for _, f := range storedFields(res) {
		if f.Column != nil && f.Column.Name == column {
			return f.GoType
		}
	}
	return ""
}

// widenedExpr is the condition under which a read is too wide to be answered
// from memory, as an expression over `cfg`.
//
// Empty when nothing about the options can widen the read, which is a table with
// neither a tenant column nor an owner column: there is one answer per row and
// every caller gets it.
func (e *emitter) widenedExpr(res *ir.Resource) string {
	var terms []string
	if _, ok := e.tenantFilter(res); ok {
		terms = append(terms, "cfg.SkipTenantScope")
	}
	if _, ok := e.ownerFilter(res); ok {
		terms = append(terms, "cfg.SkipOwnerScope")
	}
	return strings.Join(terms, " || ")
}

// getCached emits the cache in front of a resource's row read.
//
// Two decisions are made here and the first one is the load-bearing one.
//
// A read inside a transaction is never answered from memory. Every write starts
// by reading the row it is about to change — an update reads it to snapshot it
// and to judge the transition, a delete reads it to find out whether it is
// already gone — and all of them do it through this method, inside the
// transaction that then writes. Those reads are the reason the update's own
// comment can say that nothing changes underneath it, and a held row would make
// that false: the history would record a version that never existed, and a rule
// about a transition would judge a row somebody had already moved on from. The
// predicate is dbx.Tx, which is the same one connFor uses to decide which
// connection to read on, so the cached path and the uncached path cannot
// disagree about what "in a transaction" means. It covers the hooks for free,
// since a hook runs inside the write's transaction.
//
// A read that widened its scope is not answered from memory either. Those are
// the administrative reads — across tenants, or across a table's owners — and
// what they return is not what the key describes. Reading through is one query
// on a path that is rare and privileged, which is a better trade than a second
// key nothing else would ever hit.
func (e *emitter) getCached(b *gobuf.Buf, res *ir.Resource, typeName string) {
	var (
		ctxPkg  = b.Import("context")
		uuidPkg = b.Import("github.com/google/uuid")
		optPkg  = b.Import(runtimeModule + "/readopt")
		dbxPkg  = b.Import(runtimeModule + "/dbx")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
	)

	e.cacheKeyFunc(b, res)
	e.ownerFunc(b, res)
	e.cloneFunc(b, res)

	widened := e.widenedExpr(res)

	b.Comment("Get implements " + res.Name + "Repository.\n\n" +
		"The row may come from memory: this table set `cache: true`, so a read of " +
		"it is held until a write to it publishes the withdrawal. " +
		e.neverHeldPhrase(res) + "\n\n" +
		"What comes back is always this caller's own copy.")
	b.L("func (%s *%s) Get(ctx %s.Context, id %s.UUID, opts ...%s.Option) (*%s, error) {",
		repo, typeName, ctxPkg, uuidPkg, optPkg, e.entity(b, res))
	e.methodSpan(b, res, "Get")
	e.readPreamble(b, res, optPkg, tenPkg, "nil, err", widened != "", e.usesClaims(res))

	b.Comment("A read inside a transaction is a read something is about to write " +
		"against, and it has to see the row as the transaction sees it. Every " +
		"generated write begins with one — to snapshot the previous version, and " +
		"to judge the change against it — so a held row here would put a version " +
		"that never existed into the history and show a transition rule a row " +
		"somebody had already moved past.\n\n" +
		"dbx.Tx is the same question connFor asks, which is what keeps the two " +
		"paths from disagreeing about the answer. A hook is covered by it without " +
		"being mentioned, because a hook runs inside the write's transaction.")
	b.L("if _, inTx := %s.Tx(ctx); inTx {", dbxPkg)
	b.L("return %s.read%s(ctx, id, opts...)", repo, res.Name)
	b.L("}")
	b.NL()

	if widened != "" {
		b.Comment("A read that widened a scope is not the read the key describes. " +
			"These are the administrative ones — " + e.widenedPhrase(res) + " — and " +
			"they are rare, privileged, and cheaper to answer with a query than to " +
			"give a second key nothing else would hit.")
		b.L("if %s {", widened)
		b.L("return %s.read%s(ctx, id, opts...)", repo, res.Name)
		b.L("}")
		b.NL()
	}

	key := e.cacheKeyForRead(res, "id")

	b.Comment("A miss is an error rather than a nil row, which is what keeps it " +
		"out of the map: runtime/cache never stores what a failing loader " +
		"returned. So nothing has to withdraw a not-found, and a create has " +
		"nothing to publish.")
	b.L("held, err := %s.db.%s.Load(%s, func() (*%s, error) {", repo, cacheField(res), key, e.entity(b, res))
	b.L("return %s.read%s(ctx, id, opts...)", repo, res.Name)
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.L("return %s(held), nil", cloneFuncName(res))
	b.L("}")
	b.NL()
}

// forgetCachedSignature emits the one cache method a caller outside this package
// can reach.
//
// It is on the interface because rig itself needs it. `files` writes a
// `<role>_file_id` column with a statement of its own — that write has to be in
// the transaction that finalizes the upload, so it cannot go through Update — and
// the generated service hands this to it as [files.Owner.Forget]. Without it,
// attaching a file to a held row leaves every replica saying there is no file
// there, which the download endpoint answers as a 404 for something that was just
// uploaded.
//
// Exported rather than hidden, and the doc says why: the same escape hatch is what
// a project needs on the day it has to write this table some other way. It is the
// difference between a promise that can be kept and one that can only be broken.
func (e *emitter) forgetCachedSignature(b *gobuf.Buf, res *ir.Resource) {
	if !e.cached(res) {
		return
	}

	ctxPkg := b.Import("context")

	b.NL()
	b.Comment(forgetCachedDoc(res))
	b.L("ForgetCached(ctx %s.Context, row *%s) error", ctxPkg, e.entity(b, res))
}

// forgetCachedDoc is the comment both the interface and the implementation carry.
func forgetCachedDoc(res *ir.Resource) string {
	return "ForgetCached withdraws whatever is held of one row, on the " +
		"transaction that changed it.\n\n" +
		"Nothing that goes through this repository needs to call it: every write " +
		"here publishes its own withdrawal. It is for the writes that cannot — the " +
		"file service setting a `<role>_file_id` inside the transaction that " +
		"finalizes an upload, which is where rig calls it, and whatever a project " +
		"has to do through Store.Pool or in raw SQL, which is where a project " +
		"would.\n\n" +
		"The row rather than an identifier, because the key names every scope a " +
		"read of it applied and those values are the row's own. Pass the row as it " +
		"was before the write, and call this inside the transaction that made the " +
		"change: the withdrawal is a notification Postgres delivers on commit and " +
		"discards on a rollback. Withdrawing something nothing holds is not an " +
		"error.\n\n" +
		"\tif err := repos." + res.Plural + ".ForgetCached(ctx, row); err != nil { return err }"
}

// forgetCachedMethod emits the implementation.
func (e *emitter) forgetCachedMethod(b *gobuf.Buf, res *ir.Resource, typeName string) {
	if !e.cached(res) {
		return
	}

	ctxPkg := b.Import("context")

	b.Comment(forgetCachedDoc(res))
	b.L("func (%s *%s) ForgetCached(ctx %s.Context, row *%s) error {",
		repo, typeName, ctxPkg, e.entity(b, res))
	b.Comment("Nil is the state the caller asked for: there is no row, so there " +
		"is no key, and a helper that says so here is one less thing for a call " +
		"site to be careful about.")
	b.L("if row == nil { return nil }")
	b.L("return %s.db.%s.Forget(ctx, %s)",
		repo, cacheField(res), e.cacheKeyForRow(res, "row", "row.ID"))
	b.L("}")
	b.NL()
}

// cacheForget emits the withdrawal one write publishes.
//
// Inside the write's transaction, and after the statement that made the held row
// wrong. Postgres delivers a notification issued inside a transaction when that
// transaction commits and discards it if the transaction rolls back, so this is
// atomic with the write rather than merely close to it — and the whole cost of
// being wrong about the ordering would be a hole in the invalidation, so it is
// worth being deliberate: after the write, never before.
//
// row is the variable holding the row being changed, and id the identifier the
// key is built around. They are separate because a hard delete withdraws the
// snapshots it removed too, and each of those is an identifier this row has
// never carried.
func (e *emitter) cacheForget(b *gobuf.Buf, res *ir.Resource, row, id, why string) {
	if !e.cached(res) {
		return
	}

	b.Comment(why)
	b.L("if err := %s.db.%s.Forget(ctx, %s); err != nil { return err }",
		repo, cacheField(res), e.cacheKeyForRow(res, row, id))
	b.NL()
}

// deleteSnapshots emits the removal of a row's history, ahead of the row itself.
//
// Uncached, this is the one statement it has always been. Cached, it has to say
// RETURNING: a snapshot is a row with an identifier of its own, reachable
// through the history endpoint and through a revert, so somebody may be holding
// one — and the identifiers are knowable only here, at the moment the statement
// removes them. This is the same shape as auth's Store.RevokeFamily, which
// returns the tokens it ended rather than how many there were, and for the same
// reason: a write that invalidates several held things has to name them.
//
// Clearing the whole topic would be the alternative and is worse. It drops every
// tenant's rows to withdraw one row's history.
//
// The statement returns the owner column as well, on a table that has one, and
// that is the part worth being careful about. A snapshot carries the scope the row
// had when the snapshot was taken, which is the scope the read that held it
// matched — so on a table whose `access.owner` names a column somebody can change,
// an assignee or an addressee, the live row's owner is the wrong key for a version
// taken before the change. Withdrawing under it would leave the earlier reader
// holding a version of a row that no longer exists.
func (e *emitter) deleteSnapshots(b *gobuf.Buf, res *ir.Resource) {
	s := res.Storage

	if !e.cached(res) {
		b.Comment("The snapshots reference this row, so they go first.")
		b.L("if _, err := tx.Exec(ctx, \"DELETE FROM %s WHERE %s = $1\", in.Input.ID); err != nil {",
			s.Table, s.Snapshot.FromID.Name)
		b.L("return writeError(err, %s)", gobuf.Quote(s.Table))
		b.L("}")
		return
	}

	var (
		pgxPkg  = b.Import("github.com/jackc/pgx/v5")
		uuidPkg = b.Import("github.com/google/uuid")

		owner, owned = e.ownerFilter(res)
		fail         = gobuf.Quote(s.Table)

		returning = "id"
		doc       = "The snapshots reference this row, so they go first.\n\n" +
			"RETURNING because each of them is a row of its own — reachable " +
			"through the history and through a revert — so each may be held under a " +
			"key of its own, and this statement is the only place their identifiers " +
			"are ever known."
	)
	if owned {
		returning += ", " + owner
		doc += "\n\nThe owner comes back with them, because a snapshot carries the " +
			"owner the row had when it was taken and that is the scope the read " +
			"holding it matched. Keying these off the live row would withdraw " +
			"nothing on a table where the column has since changed hands."
	}

	b.Comment(doc)
	b.L("gone, err := tx.Query(ctx, \"DELETE FROM %s WHERE %s = $1 RETURNING %s\", in.Input.ID)",
		s.Table, s.Snapshot.FromID.Name, returning)
	b.L("if err != nil { return writeError(err, %s) }", fail)

	if !owned {
		b.L("versions, err := %s.CollectRows(gone, %s.RowTo[%s.UUID])", pgxPkg, pgxPkg, uuidPkg)
		b.L("if err != nil { return writeError(err, %s) }", fail)
		b.L("for _, version := range versions {")
		b.L("if err := %s.db.%s.Forget(ctx, %s); err != nil { return err }",
			repo, cacheField(res), e.cacheKeyForRow(res, "prev", "version"))
		b.L("}")
		b.NL()
		return
	}

	nullable := strings.HasPrefix(e.goTypeForColumn(res, owner), "*")

	b.L("keys, err := %s.CollectRows(gone, func(version %s.CollectableRow) (string, error) {",
		pgxPkg, pgxPkg)
	b.L("var id, ownerID %s.UUID", uuidPkg)
	if nullable {
		b.L("var held *%s.UUID", uuidPkg)
		b.L("if err := version.Scan(&id, &held); err != nil { return \"\", err }")
		b.Comment("A null owner is a row no narrow read ever matched, so the zero " +
			"uuid is a key nothing holds — and withdrawing one of those is not an " +
			"error. Same answer as ownerOf" + res.Name + ".")
		b.L("if held != nil { ownerID = *held }")
	} else {
		b.L("if err := version.Scan(&id, &ownerID); err != nil { return \"\", err }")
	}
	tenant := ""
	if column, ok := e.tenantFilter(res); ok {
		// The row's, not the snapshot's: a tenant column is a key column and no
		// write may move a row between tenants, so the two cannot differ.
		tenant = "prev." + e.fieldForColumn(res, column)
	}
	b.L("return %s, nil", e.cacheKeyExpr(res, tenant, "ownerID", "id"))
	b.L("})")
	b.L("if err != nil { return writeError(err, %s) }", fail)
	b.L("for _, key := range keys {")
	b.L("if err := %s.db.%s.Forget(ctx, key); err != nil { return err }", repo, cacheField(res))
	b.L("}")
	b.NL()
}

// storeCacheLoggerField adds the one thing the bus needs that a pool does not
// supply.
//
// Nil is usable and resolves to slog.Default inside cache.NewBus, so a store
// built by a migration or a seed script does not have to know about any of this.
func (e *emitter) storeCacheLoggerField(b *gobuf.Buf) {
	if len(e.cachedResources()) == 0 {
		return
	}

	// Only when something precedes it. A blank line above the first field of a
	// struct is a blank line gofmt keeps.
	if e.tracing() {
		b.NL()
	}
	b.Comment("Logger is where the invalidation channel reports losing touch " +
		"with Postgres, which is the one thing about this cache worth hearing " +
		"about: the fallback is to read every row again, and that is correct and " +
		"silent. Nil takes slog.Default.\n\n" +
		"\tstore.New(pool, store.Config{Logger: app.Logger})")
	b.L("Logger *%s.Logger", b.Import("log/slog"))
}

// neverHeldPhrase names the reads this table's Get never answers from memory.
//
// Written from the resource rather than in general, because a comment that
// mentions an owner scope on a table that has none is a comment a reader has to
// go and check.
func (e *emitter) neverHeldPhrase(res *ir.Resource) string {
	_, tenanted := e.tenantFilter(res)
	_, owned := e.ownerFilter(res)

	switch {
	case tenanted && owned:
		return "Two kinds of read are never held — one inside a transaction, and " +
			"one that widened either scope — and the reasons are worth knowing, so " +
			"they are on the branch below."
	case tenanted || owned:
		return "Two kinds of read are never held — one inside a transaction, and " +
			"one that widened its scope — and the reasons are worth knowing, so " +
			"they are on the branch below."
	}
	return "A read inside a transaction is never held, for the reason on the " +
		"branch below."
}

// widenedPhrase names the scopes a read of this table can widen.
func (e *emitter) widenedPhrase(res *ir.Resource) string {
	_, tenanted := e.tenantFilter(res)
	_, owned := e.ownerFilter(res)

	switch {
	case tenanted && owned:
		return "across tenants, or across this table's owners"
	case owned:
		return "across this table's owners"
	}
	return "across tenants"
}
