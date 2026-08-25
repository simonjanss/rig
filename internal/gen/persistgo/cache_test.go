package persistgo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The two cached fixtures. One is tenant-scoped with a history, which is the
// shape most tables have; the other is owner-scoped, which is the one where the
// key has to name the account as well.
const (
	cachedIR      = "rowcache.ir.json"
	cachedOwnedIR = "rowcacheowned.ir.json"
)

func cachedRepository(t *testing.T, fixture string) string {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())

	var out strings.Builder
	for _, a := range artifacts {
		out.Write(a.Content)
	}
	return out.String()
}

func TestCachedGolden(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ dir, fixture string }{
		{"rowcache", cachedIR},
		{"rowcacheowned", cachedOwnedIR},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			t.Parallel()

			doc := gentest.LoadDocument(t, filepath.Join("testdata", tc.fixture))
			artifacts := gentest.Run(t, persistgo.New(), doc, opts())
			gentest.Golden(t, filepath.Join("testdata", tc.dir), artifacts, *update)
		})
	}
}

// TestCachedCodeCompiles is the check the golden files cannot make. A cache
// wired into four write methods and a generic helper is a lot of new references
// for a golden to agree with and a compiler to reject.
func TestCachedCodeCompiles(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{cachedIR, cachedOwnedIR} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
			gentest.MustCompileAll(t,
				gentest.Package{
					Dir: "model",
					Artifacts: gentest.Run(t, modelgo.New(), doc,
						gen.Options{Raw: map[string]any{"package": "model"}}),
				},
				gentest.Package{Dir: pkg, Artifacts: gentest.Run(t, persistgo.New(), doc, opts())},
			)
		})
	}
}

// TestAReadInsideATransactionIsNeverCached is the property the whole design
// rests on.
//
// Every generated write opens a transaction and reads the row it is about to
// change through Get — to snapshot the previous version, and to judge the
// change against it. A held row there would put a version that never existed
// into the history and show a transition rule a row somebody had already moved
// past, so the branch that sends those reads to the database has to be the first
// thing Get does.
func TestAReadInsideATransactionIsNeverCached(t *testing.T) {
	t.Parallel()

	src := cachedRepository(t, cachedIR)

	if !strings.Contains(src, "if _, inTx := dbx.Tx(ctx); inTx {") {
		t.Error("Get does not check for a transaction before consulting the cache")
	}

	// The read method exists and the writes reach it through Get, which is what
	// makes the branch above cover them. A write calling readLesson directly
	// would be a write that bypassed the check rather than passing it.
	if !strings.Contains(src, "func (r *lessonRepo) readLesson(") {
		t.Error("the uncached row read was not emitted")
	}
	if strings.Count(src, "prev, err = r.Get(ctx,") == 0 {
		t.Error("the write paths no longer read the previous row through Get")
	}
}

// TestTheCacheKeyCarriesEveryScopeTheReadApplied is the property that keeps a
// held row from reaching a caller the query would have refused.
//
// The scope is in the key rather than checked after the read, because the
// alternative is a second implementation of the tenant predicate that has to go
// on agreeing with the one in the statement — and the failure when they stop
// agreeing is a cross-tenant read.
func TestTheCacheKeyCarriesEveryScopeTheReadApplied(t *testing.T) {
	t.Parallel()

	tenanted := cachedRepository(t, cachedIR)
	if !strings.Contains(tenanted, "func lessonCacheKey(tenantID, id uuid.UUID) string") {
		t.Error("a tenant-scoped table's key does not name the tenant")
	}
	if !strings.Contains(tenanted, "lessonCacheKey(claims.TenantID, id)") {
		t.Error("the read does not key by the tenant it filtered on")
	}

	owned := cachedRepository(t, cachedOwnedIR)
	if !strings.Contains(owned, "func memoCacheKey(tenantID, ownerID, id uuid.UUID) string") {
		t.Error("an owner-scoped table's key does not name the account")
	}
	if !strings.Contains(owned, "memoCacheKey(claims.TenantID, claims.AccountID, id)") {
		t.Error("the read does not key by the account it filtered on")
	}
}

// TestAWidenedReadIsNotCached covers the administrative reads.
//
// Reading across tenants, or across a table's owners, answers something the key
// does not describe. Those reads are rare and privileged, and giving them a key
// of their own would be a second entry per row that nothing else ever hits.
func TestAWidenedReadIsNotCached(t *testing.T) {
	t.Parallel()

	if src := cachedRepository(t, cachedIR); !strings.Contains(src, "if cfg.SkipTenantScope {") {
		t.Error("a read across tenants is answered from the cache")
	}
	if src := cachedRepository(t, cachedOwnedIR); !strings.Contains(src,
		"if cfg.SkipTenantScope || cfg.SkipOwnerScope {") {
		t.Error("a read across a table's owners is answered from the cache")
	}
}

// TestEveryWriteWithdrawsTheRowItChanged is the other half of the contract.
//
// rig caches what it can withdraw, so every generated write to a cached table
// has to publish — and it has to publish on the transaction doing the writing,
// which is what makes the invalidation atomic with the change and discarded by a
// rollback. A create publishes nothing, and that is not an omission: a miss is
// an error, and runtime/cache never stores what a failing loader returned.
func TestEveryWriteWithdrawsTheRowItChanged(t *testing.T) {
	t.Parallel()

	src := cachedRepository(t, cachedIR)

	// Update, Restore, and both halves of Delete — the soft stamp and the hard
	// removal. Four, plus one per snapshot inside the hard branch.
	if got := strings.Count(src, "r.db.lessonCache.forget(ctx,"); got < 5 {
		t.Errorf("only %d withdrawals were published; every write to a held row needs one", got)
	}

	for _, method := range []string{"func (r *lessonRepo) Update(", "func (r *lessonRepo) Delete(",
		"func (r *lessonRepo) Restore("} {
		body := methodBody(t, src, method)
		if !strings.Contains(body, "lessonCache.forget(ctx,") {
			t.Errorf("%s writes the row and withdraws nothing", method)
		}
	}

	if body := methodBody(t, src, "func (r *lessonRepo) Create("); strings.Contains(body, "lessonCache") {
		t.Error("Create publishes a withdrawal; a miss is never held, so there is nothing to withdraw")
	}
}

// TestAHardDeleteWithdrawsTheSnapshotsItRemoved is the case a key alone cannot
// cover.
//
// A hard delete removes the row's history as well as the row, and each of those
// snapshots is reachable by an identifier of its own — through the versions
// endpoint, and through a revert — so each may be held. Their identifiers are
// knowable only from the statement that removes them, which is why it has to say
// RETURNING. This is the same shape as auth's Store.RevokeFamily, which returns
// the tokens it ended rather than how many there were.
func TestAHardDeleteWithdrawsTheSnapshotsItRemoved(t *testing.T) {
	t.Parallel()

	src := cachedRepository(t, cachedIR)

	if !strings.Contains(src, "RETURNING id\", in.Input.ID)") {
		t.Error("the snapshot delete does not return the identifiers it removed")
	}
	if !strings.Contains(src, "for _, version := range versions {") {
		t.Error("the identifiers a hard delete removed are not withdrawn")
	}
	if !strings.Contains(src, "lessonCacheKey(prev.TenantID, version)") {
		t.Error("a removed snapshot is withdrawn under the wrong key")
	}
}

// TestAHardDeleteWithdrawsASnapshotUnderItsOwnOwner is the half of that a live
// row cannot answer for.
//
// `access.owner` may name a column somebody can change — an assignee, an
// addressee — and a snapshot carries the owner the row had when it was taken,
// which is exactly the scope the read that held it matched. Keying these
// withdrawals off the live row would publish under whoever owns it now and leave
// the earlier reader holding a version of a row that no longer exists, so the
// statement returns the column and each key is built from the version's own
// values.
//
// The fixture is the tenant-scoped one made owner-scoped here, because the two
// shapes that have to meet — a mutable owner in the key, and a history to delete
// — are the combination no committed fixture has.
func TestAHardDeleteWithdrawsASnapshotUnderItsOwnOwner(t *testing.T) {
	t.Parallel()

	doc := ownerScopedCachedDoc(t)
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())

	var b strings.Builder
	for _, a := range artifacts {
		b.Write(a.Content)
	}
	src := b.String()

	if !strings.Contains(src, "RETURNING id, created_by_account_id\", in.Input.ID)") {
		t.Error("the snapshot delete does not return the owner each version was held under")
	}
	if !strings.Contains(src, "return lessonCacheKey(prev.TenantID, ownerID, id), nil") {
		t.Error("a removed snapshot is not keyed by its own owner")
	}
	if strings.Contains(src, "ownerOfLesson(prev), version)") {
		t.Error("a removed snapshot is still keyed by the live row's owner")
	}

	// The tenant is the live row's on purpose: it is a key column, so no write
	// moves a row between tenants and the two cannot differ.
	if !strings.Contains(src, "var id, ownerID uuid.UUID") {
		t.Error("the scanned key parts were not emitted")
	}

	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{Dir: pkg, Artifacts: artifacts},
	)
}

// ownerScopedCachedDoc is the rowcache fixture with `access: scope: own` applied
// to it, which is what `internal/compile` does when it copies the audit column
// into Storage.Owner.
func ownerScopedCachedDoc(t *testing.T) *ir.Document {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", cachedIR))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("no Lesson in the fixture")
	}
	if res.Storage.Audit == nil || res.Storage.Audit.CreatedBy == nil {
		t.Fatal("the fixture has no audit column to own by")
	}
	owner := *res.Storage.Audit.CreatedBy
	res.Storage.Owner = &owner
	return doc
}

// TestAnUncachedTableIsUntouched is what makes this safe to add.
//
// Turning the cache on for one table must not rewrite the rest of a project, so
// a document with no opt-in has to emit exactly what it emitted before — no
// read method, no clone, and no import of runtime/cache anywhere.
func TestAnUncachedTableIsUntouched(t *testing.T) {
	t.Parallel()

	src := cachedRepository(t, "lifecycle.ir.json")

	for _, unwanted := range []string{"runtime/cache", "rowCache", "lessonCache", "readLesson", "cloneLesson"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("a project that caches nothing emitted %q", unwanted)
		}
	}
}

// TestACachedTableRefusesAFieldItCannotCopy is the bound on the clone.
//
// A cached read has to be indistinguishable from a fresh one, and the thing that
// makes it so is that every caller gets its own row — which is what a scan used
// to give them. A jsonb column with a `go_type` of its own is an application
// struct rig knows nothing about, so a copy of the row would share whatever map
// is inside it and two requests in one window would be reading a field either of
// them can write. Refused rather than half-copied.
func TestACachedTableRefusesAFieldItCannotCopy(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", cachedIR))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("no Lesson in the fixture")
	}

	for i := range res.Fields {
		f := &res.Fields[i]
		if f.Column == nil {
			continue
		}
		f.TypeKind = ir.TypeKindObject
		f.GoType = "ApplicationPayload"
		break
	}

	if _, err := persistgo.New().Generate(t.Context(), doc, opts()); err == nil {
		t.Fatal("a cached table with an uncopyable field was accepted")
	} else if !strings.Contains(err.Error(), "cache: true") {
		t.Errorf("the refusal does not say what to change: %v", err)
	}
}

// methodBody is one generated method, from its signature to the next
// top-level one.
func methodBody(t *testing.T, src, signature string) string {
	t.Helper()

	i := strings.Index(src, signature)
	if i < 0 {
		t.Fatalf("no %s in the generated source", signature)
	}
	rest := src[i+len(signature):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}
