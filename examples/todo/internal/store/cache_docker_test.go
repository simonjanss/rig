//go:build docker

package store_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/model"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/examples/todo/services/todo"
	"github.com/simonjanss/rig/files"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/readopt"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// `todo` sets `cache: true`, so a Get of one row is answered out of memory until
// a write to that row publishes the withdrawal. This suite is about the two
// halves of that being true at once, and every test in it works the same way:
// change the row *behind rig's back* with raw SQL, then read it through the
// repository. What comes back says whether the answer was held.
//
// That is also the hazard the block documents, demonstrated. A write rig cannot
// see is a write rig cannot withdraw.

// newStore is newRepo's sibling, for the tests that need the store itself: two
// of them for the cross-replica case, and Pool for writing behind rig's back.
func newStore(t *testing.T) (*store.Store, context.Context, tenancy.Claims) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55440/rig?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	t.Cleanup(pool.Close)

	s := store.New(pool, store.Config{})
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Close(closing)
	})

	claims := tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}
	return s, tenancy.NewContext(ctx, claims), claims
}

// awaitCaching waits until the store is actually holding rows.
//
// Nothing is held until the invalidation channel is up, which is a listener
// connecting to Postgres on a goroutine New started — so for the first moments
// of a process every read is a query. That is the design and not a wrinkle to
// work around: a cache that held rows before it could be told to forget them
// would be a lifetime over the application's own data. It does mean a test that
// asserts a hit has to wait for one to be possible.
//
// The probe is the same trick every test here uses, on a row of its own: read it,
// move it underneath the cache, read again. A stale answer means the cache is
// live.
func awaitCaching(t *testing.T, s *store.Store, ctx context.Context) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		held := uniq("Probe")
		probe := mustCreate(t, s.Todos, ctx, held)
		mustGet(t, s.Todos, ctx, probe.ID)
		retitle(t, s, probe.ID, uniq("Probe moved"))

		if mustGet(t, s.Todos, ctx, probe.ID).Title == held {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the invalidation channel to come up; " +
		"without it nothing is held, which is correct and makes this suite untestable")
}

// uniq makes a title nothing else in the database has. `todo` has a unique index
// over the titles of live rows, and these rows outlive the test that made them —
// and the database outlives the run.
func uniq(what string) string { return what + " " + uuid.NewString() }

// retitle changes a row without going through the repository, which is the one
// thing a project that sets `cache: true` promises not to do. Here it is the
// instrument: it moves the database without publishing anything, so the next
// read tells us whether it came from memory.
func retitle(t *testing.T, s *store.Store, id uuid.UUID, title string) {
	t.Helper()

	if _, err := s.Pool().Exec(context.Background(),
		"UPDATE todo SET title = $1 WHERE id = $2", title, id); err != nil {
		t.Fatal(err)
	}
}

func mustCreate(t *testing.T, repo store.TodoRepository, ctx context.Context, title string) *model.Todo {
	t.Helper()

	row, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
		Input: model.TodoCreateInput{Title: title},
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func mustGet(t *testing.T, repo store.TodoRepository, ctx context.Context, id uuid.UUID, opts ...readopt.Option) *model.Todo {
	t.Helper()

	row, err := repo.Get(ctx, id, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// The first read is a query and the second is not. Nothing here can watch the
// wire, so the row is moved underneath the cache instead: a second read that
// still says the old title is a second read that never reached Postgres.
func TestASecondReadOfOneRowIsAnsweredFromMemory(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	held := uniq("Water the plants")
	row := mustCreate(t, repo, ctx, held)
	if got := mustGet(t, repo, ctx, row.ID).Title; got != held {
		t.Fatalf("Title = %q before anything was cached", got)
	}

	retitle(t, s, row.ID, uniq("Moved underneath the cache"))

	if got := mustGet(t, repo, ctx, row.ID).Title; got != held {
		t.Errorf("Title = %q; the second read should have been the held one", got)
	}
}

// And what the caller gets is its own copy. Without the cache every read
// produced a fresh row out of a scan, so a caller writing to what it was handed
// was writing to something nobody else had — and a cache that changed that would
// have changed something other than where the read happened.
func TestEachCallerGetsItsOwnCopyOfAHeldRow(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	shared := uniq("Shared")
	row := mustCreate(t, repo, ctx, shared)
	first := mustGet(t, repo, ctx, row.ID)
	first.Title = "Written by the first caller"

	second := mustGet(t, repo, ctx, row.ID)
	if second.Title != shared {
		t.Errorf("Title = %q; one caller's write reached another's row", second.Title)
	}
	if first == second {
		t.Error("two reads returned the same pointer")
	}
}

// A write through the repository withdraws the row it changed, and it does so in
// time for the request that made the write to read it back.
//
// This is the property the notification alone cannot provide. It travels out
// through Postgres and returns on the listener's own connection, which takes
// moments that belong to the caller who just wrote — so the local drop is
// registered on the commit, where it runs before the write returns. Somebody who
// saves a change and is then shown the old value has been told their write did
// not happen.
func TestAWriteWithdrawsTheRowItChanged(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	row := mustCreate(t, repo, ctx, uniq("Before"))
	mustGet(t, repo, ctx, row.ID)

	after := uniq("After")
	if _, err := repo.Update(ctx, row.ID, dbhook.Update[model.TodoUpdateInput, model.Todo]{
		Input: model.TodoUpdateInput{Title: patch.NewOptional(after)},
	}); err != nil {
		t.Fatal(err)
	}

	if got := mustGet(t, repo, ctx, row.ID).Title; got != after {
		t.Errorf("Title = %q; the write did not withdraw the row it changed", got)
	}
}

// The property the whole design rests on, and the one worth breaking on purpose
// to see fail.
//
// Update reads the previous row to snapshot it and to judge the change against
// it, inside the transaction that then writes. If that read came from memory the
// history would record a version that never existed — so Get sends every read
// inside a transaction to the database, and this is what says so. The row is
// moved underneath the cache first, so a held answer and a real one differ.
func TestAWriteSnapshotsTheRowAsItActuallyWas(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	row := mustCreate(t, repo, ctx, uniq("First"))

	// Held, and now wrong.
	mustGet(t, repo, ctx, row.ID)
	second := uniq("Second")
	retitle(t, s, row.ID, second)

	if _, err := repo.Update(ctx, row.ID, dbhook.Update[model.TodoUpdateInput, model.Todo]{
		Input: model.TodoUpdateInput{Title: patch.NewOptional(uniq("Third"))},
	}); err != nil {
		t.Fatal(err)
	}

	versions, err := repo.ListSnapshots(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("the update took no snapshot")
	}
	if got := versions[0].Title; got != second {
		t.Errorf("the snapshot says %q; the row was %q when the update read it, and a "+
			"history that records a version nobody ever saw is worse than no history", got, second)
	}
}

// A hook runs inside the write's transaction, so the row it is handed is the row
// the transaction sees. It is covered by the same branch without being mentioned
// in it, which is the point of asking dbx.Tx rather than counting call sites.
func TestAHookSeesTheRowTheTransactionSees(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	row := mustCreate(t, repo, ctx, uniq("First"))
	mustGet(t, repo, ctx, row.ID)
	second := uniq("Second")
	retitle(t, s, row.ID, second)

	var seen string
	if _, err := repo.Update(ctx, row.ID, dbhook.Update[model.TodoUpdateInput, model.Todo]{
		Input: model.TodoUpdateInput{Title: patch.NewOptional(uniq("Third"))},
		Hooks: dbhook.UpdateHooks[model.TodoUpdateInput, model.Todo]{
			Before: func(hookCtx context.Context, _ tenancy.Claims, _ *model.TodoUpdateInput, prev *model.Todo) error {
				seen = prev.Title
				// And a read the hook makes for itself is inside the same
				// transaction, so it is not the held one either.
				fresh, err := repo.Get(hookCtx, row.ID)
				if err != nil {
					return err
				}
				if fresh.Title != second {
					t.Errorf("a hook's own Get returned %q, not the row in its transaction", fresh.Title)
				}
				return nil
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if seen != second {
		t.Errorf("the hook was handed %q, not the row its transaction was about to change", seen)
	}
}

// A read that widened its scope is not the read the key describes, so it is
// never answered from memory. These are the administrative ones, and reading
// through is one query on a path that is rare and privileged.
func TestAReadAcrossTenantsIsNotAnsweredFromMemory(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	scoped := uniq("Scoped")
	row := mustCreate(t, repo, ctx, scoped)
	mustGet(t, repo, ctx, row.ID)
	moved := uniq("Moved underneath the cache")
	retitle(t, s, row.ID, moved)

	wide := mustGet(t, repo, ctx, row.ID, readopt.WithoutTenantScope())
	if wide.Title != moved {
		t.Errorf("a read across tenants returned %q out of a key that does not describe it", wide.Title)
	}

	// And the narrow answer is still held, which is what makes the two separate
	// rather than one having replaced the other.
	if got := mustGet(t, repo, ctx, row.ID).Title; got != scoped {
		t.Errorf("the narrow read returned %q; the wide one should not have stored anything", got)
	}
}

// A miss is never held. The identifier half of a read is caller-supplied, so
// caching "no such row" would let anybody fill the map with invented ones — and
// it is also what makes a create have nothing to publish.
func TestAMissIsNotHeld(t *testing.T) {
	s, ctx, claims := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	missing, arrived := uuid.New(), uniq("Arrived later")
	if _, err := repo.Get(ctx, missing); err == nil {
		t.Fatal("a row that does not exist was found")
	}

	// Inserted behind rig's back under the identifier that just missed. A held
	// miss would go on answering not-found.
	if _, err := s.Pool().Exec(context.Background(),
		"INSERT INTO todo (id, tenant_id, title) VALUES ($1, $2, $3)",
		missing, claims.TenantID, arrived); err != nil {
		t.Fatal(err)
	}

	if got := mustGet(t, repo, ctx, missing).Title; got != arrived {
		t.Errorf("Title = %q; the not-found was held", got)
	}
}

// Two stores over one pool are two replicas as far as this mechanism is
// concerned: separate maps, separate listeners, one channel. A write on either
// has to reach the other, which is the whole reason the invalidation is a NOTIFY
// rather than a lifetime.
func TestAWriteOnOneReplicaWithdrawsOnTheOther(t *testing.T) {
	first, ctx, claims := newStore(t)

	second := store.New(first.Pool(), store.Config{})
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = second.Close(closing)
	})
	_ = claims

	awaitCaching(t, first, ctx)
	awaitCaching(t, second, ctx)

	row := mustCreate(t, first.Todos, ctx, uniq("Before"))

	// Held on both.
	mustGet(t, first.Todos, ctx, row.ID)
	mustGet(t, second.Todos, ctx, row.ID)

	after := uniq("After")
	if _, err := second.Todos.Update(ctx, row.ID, dbhook.Update[model.TodoUpdateInput, model.Todo]{
		Input: model.TodoUpdateInput{Title: patch.NewOptional(after)},
	}); err != nil {
		t.Fatal(err)
	}

	// Delivery is a notification travelling through Postgres, so it is not
	// instant. A deadline rather than a sleep: what is being asserted is that it
	// arrives, not how fast.
	waitFor(t, "the other replica to forget", func() bool {
		return mustGet(t, first.Todos, ctx, row.ID).Title == after
	})
}

// A hard delete removes the row's history as well as the row, and each of those
// snapshots is a row somebody may be holding — reachable by an identifier of its
// own through the versions endpoint. Their identifiers are knowable only from the
// statement that removes them, which is why it says RETURNING.
func TestAHardDeleteWithdrawsTheSnapshotsItRemoved(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	row := mustCreate(t, repo, ctx, uniq("First"))
	if _, err := repo.Update(ctx, row.ID, dbhook.Update[model.TodoUpdateInput, model.Todo]{
		Input: model.TodoUpdateInput{Title: patch.NewOptional(uniq("Second"))},
	}); err != nil {
		t.Fatal(err)
	}

	versions, err := repo.ListSnapshots(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("the update took no snapshot")
	}
	version := versions[0].ID

	// Held, under a key of its own.
	if _, err := repo.Get(ctx, version); err != nil {
		t.Fatal(err)
	}

	if err := repo.Delete(ctx, dbhook.Delete[model.TodoDeleteInput, model.Todo]{
		Input: model.TodoDeleteInput{ID: row.ID, Hard: true},
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the removed snapshot to be forgotten", func() bool {
		_, err := repo.Get(ctx, version)
		return err != nil
	})
}

// A retirement changes what a held row says rather than whether there is one:
// Get returns a row whatever its lifecycle state, so a soft delete has to
// withdraw too.
func TestASoftDeleteWithdrawsTheRowItRetired(t *testing.T) {
	s, ctx, _ := newStore(t)
	repo := s.Todos

	awaitCaching(t, s, ctx)

	row := mustCreate(t, repo, ctx, uniq("Retire me"))
	if got := mustGet(t, repo, ctx, row.ID); got.DeletedAt != nil {
		t.Fatal("a new row is not deleted")
	}

	if err := repo.Delete(ctx, dbhook.Delete[model.TodoDeleteInput, model.Todo]{
		Input: model.TodoDeleteInput{ID: row.ID},
	}); err != nil {
		t.Fatal(err)
	}

	if got := mustGet(t, repo, ctx, row.ID); got.DeletedAt == nil {
		t.Error("the held row still says it is live")
	}
}

// The one write rig makes to this table that is not a repository write.
//
// files.Service sets cover_file_id with a statement of its own, inside the
// transaction that finalizes the upload — it has to be that transaction, so it
// cannot go through Update. Left to itself that is the whole hazard this table's
// `cache: true` documents, except committed rather than warned about: the held row
// would go on saying there is no cover, and DownloadCoverFile compares the path's
// file against the row it reads, so it would answer not-found for a file that had
// just been uploaded successfully.
//
// So this is the test that does *not* work like the rest of the suite. Nothing is
// moved behind rig's back; the upload is an ordinary one through the generated
// service, and what is asserted is that rig withdrew the row it wrote.
func TestUploadingAFileWithdrawsTheRowItWasAttachedTo(t *testing.T) {
	s, ctx, _ := newStore(t)

	awaitCaching(t, s, ctx)

	svc := todo.New(s.Todos, api.NewFiles(s.Pool()), nil, nil, s.Pool(), nil)

	row := mustCreate(t, s.Todos, ctx, uniq("Needs a cover"))

	// Held, and holding the state before the upload: no cover.
	if got := mustGet(t, s.Todos, ctx, row.ID); got.CoverFileID != nil {
		t.Fatal("a new row already has a cover")
	}

	claims, err := tenancy.FromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}

	f, err := svc.UploadCoverFile(ctx, api.Request[api.TodoUploadCoverFilePath, struct{}, files.Upload]{
		Claims: claims,
		Path:   api.TodoUploadCoverFilePath{ID: row.ID},
		Body: files.Upload{
			Name:         "cover.png",
			DeclaredType: "image/png",
			Body:         bytes.NewReader(onePixelPNG),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	after := mustGet(t, s.Todos, ctx, row.ID)
	if after.CoverFileID == nil {
		t.Fatal("the held row still says there is no cover; the upload withdrew nothing")
	}
	if *after.CoverFileID != f.ID {
		t.Errorf("CoverFileID = %s, want %s", *after.CoverFileID, f.ID)
	}

	// The endpoint a caller would reach for next, which is where the stale row
	// surfaces as a 404 rather than as a wrong field.
	content, err := svc.DownloadCoverFile(ctx, api.Request[api.TodoDownloadCoverFilePath, struct{}, struct{}]{
		Claims: claims,
		Path:   api.TodoDownloadCoverFilePath{ID: row.ID, FileID: f.ID, Filename: "cover.png"},
	})
	if err != nil {
		t.Fatalf("downloading the cover that was just uploaded: %v", err)
	}
	if err := content.Body.Close(); err != nil {
		t.Error(err)
	}

	// And deleting it goes the same way round: the column goes back to null in a
	// transaction the repository did not open.
	if err := svc.DeleteCoverFile(ctx, api.Request[api.TodoDeleteCoverFilePath, struct{}, struct{}]{
		Claims: claims,
		Path:   api.TodoDeleteCoverFilePath{ID: row.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s.Todos, ctx, row.ID); got.CoverFileID != nil {
		t.Error("the held row still points at the cover that was detached")
	}
}

// onePixelPNG is the smallest upload that sniffs as an image, because the file
// service records the sniffed type rather than the declared one.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
	0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
	0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// waitFor polls until something becomes true, or fails saying what did not
// happen. Notifications travel through Postgres, so what is worth asserting is
// that one arrives rather than how quickly.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
