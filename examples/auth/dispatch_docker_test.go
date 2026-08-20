//go:build docker

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/internal/store"
	"github.com/simonjanss/rig/examples/auth/services/note"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The concurrency claims, first, because they are the ones a reader will not
// otherwise believe and the ones a bug in would be invisible in production until
// it was expensive.
//
// Every replica runs a dispatcher and the operator's cron runs another, so ten
// claimants on one row is normal operation here rather than an edge.
func TestNDispatchersSendEachMessageOnce(t *testing.T) {
	w := newDeliveryWorld(t)

	const notes = 6
	for range notes {
		w.owe(t)
	}
	// One copy per person per note. How many people are in the seeded tenant is
	// the seed's business, so the expectation is read rather than assumed.
	owed := w.countDue(t)
	if owed == 0 {
		t.Fatal("the fixture owes nothing, so this proves nothing about claiming it")
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		sent    = map[uuid.UUID]int{}
		engines = make([]*notify.Engine, 10)
	)
	for i := range engines {
		engines[i] = w.engine(t, notify.SenderFunc(func(_ context.Context, m notify.Message) error {
			mu.Lock()
			defer mu.Unlock()
			for _, d := range m.Deliveries {
				sent[d.ID]++
			}
			return nil
		}))
	}

	// Ten goroutines on one pool, claiming from the same due set. Without SKIP
	// LOCKED this is where they would queue behind each other, and the lease
	// would be a comment.
	//
	// Several rounds, because a pass is bounded and draining is not what this
	// is about: what it is about is that nothing was sent twice on the way.
	for range 12 {
		for _, e := range engines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.Dispatch(context.Background()); err != nil {
					t.Errorf("dispatch: %v", err)
				}
			}()
		}
		wg.Wait()
		if w.countState(t, "Pending") == 0 {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()

	// The assertion this test exists for: ten claimants on one due set, and
	// nothing went out twice. Without SKIP LOCKED and the lease, this is where
	// somebody gets two of every mail.
	for id, n := range sent {
		if n != 1 {
			t.Errorf("delivery %s sent %d times, want once", id, n)
		}
	}
	// And its other half: they all went out. A lease that never expired, or a
	// claim that took rows it then dropped, would show up here as a backlog
	// that ten dispatchers could not drain.
	if left := w.countDue(t); left != 0 {
		t.Errorf("%d of %d due deliveries are still owed after ten dispatchers drained", left, owed)
	}
	if got := w.countState(t, "Sent"); got != len(sent) {
		t.Errorf("%d rows are Sent but %d were handed to a channel", got, len(sent))
	}
}

// A crashed claimant is recovered, and not before it should be.
//
// Asserted with its inverse in the same test, because a lease that expires too
// eagerly is the failure that sends everything twice — and it would look exactly
// like this test passing.
func TestACrashedClaimantIsRecoveredAfterItsLease(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)
	// One row, so "taken" and "not taken" are unambiguous.
	w.keepOne(t)

	// A process that claimed and died leaves the row Pending with a claim on
	// it. Written directly rather than acted out, because what this test is
	// about is what the *next* dispatcher does with that state.
	w.claimBySomebodyElse(t, time.Now().UTC())

	// Inside the lease, nobody else may take it.
	var taken int
	count := notify.SenderFunc(func(_ context.Context, m notify.Message) error {
		taken += len(m.Deliveries)
		return nil
	})
	if _, err := w.engine(t, count).Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if taken != 0 {
		t.Fatalf("%d deliveries taken inside the lease; a lease that expires eagerly "+
			"sends everything twice", taken)
	}

	// Past it, the next dispatcher does — and the attempt count carries
	// forward, so max_attempts still terminates rather than starting over.
	w.claimBySomebodyElse(t, time.Now().Add(-2*notify.DefaultClaimTTL).UTC())

	var attempts int
	record := notify.SenderFunc(func(_ context.Context, m notify.Message) error {
		for _, d := range m.Deliveries {
			attempts = d.Attempts
		}
		return nil
	})
	if _, err := w.engine(t, record).Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d on the recovered claim, want at least 2: the count has to "+
			"carry forward or a permanently broken address never reaches the cap", attempts)
	}
}

// A clean shutdown gives the work back rather than leaving it to expire.
//
// The TTL is for crashes. A process that knows it is going has no excuse for
// being slow about saying so: leaving them turns every ordinary rollout into a
// delivery delay, and a rollout that replaces every pod turns it into that delay
// repeatedly.
func TestACleanShutdownGivesTheWorkBack(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)
	w.keepOne(t)

	// A sender that fails leaves the row claimed and pending, which is the
	// state a shutdown has to hand back.
	failing := w.engine(t, notify.SenderFunc(func(context.Context, notify.Message) error {
		return errors.New("the provider is slow today")
	}))
	if _, err := failing.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Claim it again and stop before marking, so there is a lease to release.
	held := w.engine(t, nil)
	claimed, err := held.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = claimed

	if _, err := held.ReleaseClaims(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.claimedRows(t); got != 0 {
		t.Errorf("%d rows still claimed after a clean shutdown, want 0", got)
	}
}

// max_attempts terminates, rather than consuming a lease and a log line forever.
func TestMaxAttemptsStopsAPermanentFailure(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)
	w.keepOne(t)

	always := w.engineWith(t, 2, notify.SenderFunc(func(context.Context, notify.Message) error {
		return errors.New("this address does not exist")
	}))

	for range 4 {
		w.undeliver(t)
		w.stampClaim(t, time.Now().Add(-2*notify.MinClaimTTL).UTC())
		if _, err := always.Dispatch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if got := w.countState(t, "Failed"); got == 0 {
		t.Error("nothing reached Failed after passing max_attempts")
	}
	// And it stops being claimed at all.
	if got := w.countState(t, "Pending"); got != 0 {
		t.Errorf("%d rows are still Pending, want 0", got)
	}
}

// An Hourly account gets one message containing several; an Immediate account
// beside it in the same pass gets one each.
//
// Asserted together, because the interesting failure is one setting leaking into
// the other's batch.
func TestADigestIsOneMessageAndImmediateIsSeveral(t *testing.T) {
	w := newDeliveryWorld(t)

	digested := w.reader(t, "digested")
	immediate := w.reader(t, "immediate")
	w.setting(t, digested, notify.DigestHourly)
	w.setting(t, immediate, notify.DigestImmediate)

	for range 3 {
		w.owe(t)
	}

	// Only these two, so the batching is about them rather than about whoever
	// else the seeded tenant has accumulated.
	w.onlyFor(t, digested, immediate)

	// The digested account's copies are due at the window's close, so bring
	// that forward rather than waiting an hour.
	w.makeDue(t)

	var (
		mu    sync.Mutex
		sizes = map[uuid.UUID][]int{}
	)
	e := w.engine(t, notify.SenderFunc(func(_ context.Context, m notify.Message) error {
		mu.Lock()
		defer mu.Unlock()
		sizes[m.AccountID] = append(sizes[m.AccountID], len(m.Deliveries))
		return nil
	}))
	if _, err := e.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := sizes[digested]; len(got) != 1 || got[0] < 2 {
		t.Errorf("the Hourly account got %v messages, want one containing several", got)
	}
	// Three messages of one rather than one of three: "tell me as things
	// happen" and "give me a summary" are different requests, and answering
	// them the same way is the failure this test exists for.
	if got := sizes[immediate]; len(got) != 3 {
		t.Errorf("the Immediate account got %v, want one message per notification", got)
	}
}

// deliveryWorld is a tenant with one author, one reader and a note they are told
// about, over a real database.
type deliveryWorld struct {
	*server
	tenant  uuid.UUID
	author  uuid.UUID
	readers []uuid.UUID
	ctx     context.Context
}

func newDeliveryWorld(t *testing.T) *deliveryWorld {
	t.Helper()

	s := newServer(t)
	tenant := s.seed(t)
	author := s.accountID(t, tenant, SeedEmail)

	w := &deliveryWorld{
		server: s,
		tenant: tenant,
		author: author,
		ctx: tenancy.NewContext(context.Background(), tenancy.Claims{
			TenantID: tenant, AccountID: author, Subject: tenancy.SubjectAccount,
		}),
	}
	w.readers = append(w.readers, s.addAccount(t, tenant, "told"))

	// The seeded tenant is shared between the tests in this file and the
	// database is not thrown away between runs, so each world starts from an
	// empty inbox. Counting is most of what these tests do, and a count over
	// somebody else's rows proves nothing.
	w.empty(t)
	return w
}

// empty removes every notification in this tenant, and everything hanging off
// one.
func (w *deliveryWorld) empty(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM rig_notification_delivery WHERE tenant_id = $1`,
		`DELETE FROM rig_notification_recipient WHERE tenant_id = $1`,
		`DELETE FROM note_notification WHERE tenant_id = $1`,
		`DELETE FROM rig_notification WHERE tenant_id = $1`,
	} {
		if _, err := w.pool.Exec(context.Background(), q, w.tenant); err != nil {
			t.Fatal(err)
		}
	}
}

// reader adds somebody else who will be told.
func (w *deliveryWorld) reader(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := w.addAccount(t, w.tenant, name)
	w.readers = append(w.readers, id)
	return id
}

// owe writes a note, announces it, and resolves it — which is what puts a
// delivery row on the table for every reader.
func (w *deliveryWorld) owe(t *testing.T) {
	t.Helper()

	published := time.Now().Add(-time.Minute).UTC()
	w.note(t, w.ctx, "Something worth sending", &published)

	// Resolved by an engine that has a channel registered, because that is
	// what writes the delivery rows: a channel nothing can send on gets no
	// copies at all, which is the right answer and the reason this cannot be
	// the bare engine.
	resolver := w.engine(t, notify.SenderFunc(func(context.Context, notify.Message) error {
		return nil
	}))
	if _, err := resolver.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// engine builds one with a sender registered for Email, which is the channel
// that needs no device rows.
func (w *deliveryWorld) engine(t *testing.T, sender notify.Sender) *notify.Engine {
	t.Helper()
	return w.engineWith(t, 0, sender)
}

func (w *deliveryWorld) engineWith(t *testing.T, maxAttempts int, sender notify.Sender) *notify.Engine {
	t.Helper()

	senders := map[notify.Channel]notify.Sender{}
	if sender != nil {
		senders[notify.ChannelEmail] = sender
	}

	// The same object graph main.go builds, and in the same order.
	repos := store.New(w.pool, store.Config{})
	reg := notify.NewRegistry()
	inbox := api.NewNotifications(w.pool, reg)
	notes := note.New(repos.Notes, inbox, w.pool)
	reg.Register(api.NewNoteSubject(notes))

	return notify.NewEngine(notify.EngineConfig{
		Config: notify.Config{DB: w.pool, Registry: reg},
		Links:  api.NotificationLinks(),

		Senders:     senders,
		MaxAttempts: maxAttempts,
		// A millisecond rather than a minute, so a retry test does not wait one
		// out. The arithmetic is the same; the number is the only thing a test
		// has an opinion about.
		BackoffBase: time.Millisecond,
	})
}

func (w *deliveryWorld) countState(t *testing.T, state string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_notification_delivery WHERE tenant_id = $1 AND state = $2`,
		w.tenant, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *deliveryWorld) claimedRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_notification_delivery
		 WHERE tenant_id = $1 AND claimed_at IS NOT NULL AND state = 'Pending'`,
		w.tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stampClaim moves every claim to a chosen moment, which is how a test ages a
// lease without sleeping. A lease test that slept for real would be slow, flaky,
// and deleted within a year.
func (w *deliveryWorld) stampClaim(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(),
		`UPDATE rig_notification_delivery SET claimed_at = $2
		 WHERE tenant_id = $1 AND claimed_at IS NOT NULL`, w.tenant, at); err != nil {
		t.Fatal(err)
	}
}

// undeliver brings a retry's deliver_at forward, so a test does not wait out a
// backoff either.
func (w *deliveryWorld) undeliver(t *testing.T) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(),
		`UPDATE rig_notification_delivery SET deliver_at = now() - interval '1 minute'
		 WHERE tenant_id = $1 AND state = 'Pending'`, w.tenant); err != nil {
		t.Fatal(err)
	}
}

// makeDue brings a digest's window close forward.
func (w *deliveryWorld) makeDue(t *testing.T) { w.undeliver(t) }

// claimBySomebodyElse writes the state a process that claimed and died leaves
// behind: Pending, with a claim on it and one attempt spent.
//
// The clock is moved rather than waited out. A lease test that slept for real
// would be slow, flaky, and deleted within a year.
func (w *deliveryWorld) claimBySomebodyElse(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), `
		UPDATE rig_notification_delivery SET
			state = 'Pending', claimed_at = $2, claimed_by = $3,
			attempts = 1, deliver_at = now() - interval '1 minute'
		WHERE tenant_id = $1`, w.tenant, at, uuid.New()); err != nil {
		t.Fatal(err)
	}
}

// countDue is what a dispatcher would actually claim: pending, and its time has
// come. A row held into somebody's window is neither sent nor a backlog.
func (w *deliveryWorld) countDue(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM rig_notification_delivery
		WHERE tenant_id = $1 AND state = 'Pending' AND deliver_at <= now()`,
		w.tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// onlyFor narrows the fixture to the accounts a test is about.
func (w *deliveryWorld) onlyFor(t *testing.T, accounts ...uuid.UUID) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(),
		`DELETE FROM rig_notification_delivery WHERE tenant_id = $1 AND account_id <> ALL($2)`,
		w.tenant, accounts); err != nil {
		t.Fatal(err)
	}
}

// keepOne narrows the fixture to a single owed delivery, so a test about one
// row's lease is about one row.
func (w *deliveryWorld) keepOne(t *testing.T) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), `
		DELETE FROM rig_notification_delivery
		WHERE tenant_id = $1 AND id <> (
			SELECT id FROM rig_notification_delivery WHERE tenant_id = $1 LIMIT 1)`,
		w.tenant); err != nil {
		t.Fatal(err)
	}
}

// setting writes one account's answer for the mail channel.
func (w *deliveryWorld) setting(t *testing.T, account uuid.UUID, digest notify.Digest) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), `
		INSERT INTO rig_notification_setting (id, tenant_id, account_id, channel, digest)
		VALUES ($1, $2, $3, 'Email', $4)
		ON CONFLICT (account_id, channel) WHERE kind IS NULL
		DO UPDATE SET digest = excluded.digest`,
		uuid.New(), w.tenant, account, string(digest)); err != nil {
		t.Fatal(err)
	}
}
