//go:build docker

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
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

// A provider that refuses the recipient is believed, on the first attempt.
//
// The counterpart to TestMaxAttemptsStopsAPermanentFailure above, and the
// difference between them is the whole point of notify.Permanent: that one takes
// four passes to give up on an address that does not exist, this one takes a
// single call. What made raising max_attempts from five to fourteen affordable is
// exactly this — an eight-hour schedule is only reasonable if a provider that
// knows the answer can skip it.
func TestAPermanentRefusalIsBelievedImmediately(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)
	w.keepOne(t)

	var calls int
	refuse := w.engineWith(t, 14, notify.SenderFunc(func(context.Context, notify.Message) error {
		calls++
		return notify.Permanent(errors.New("no mailbox by that name"))
	}))

	report, err := refuse.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Errorf("the sender was called %d times, want 1", calls)
	}
	if report.Rejected == 0 {
		t.Error("a permanent refusal was not counted as rejected")
	}
	if report.Retrying != 0 {
		t.Errorf("%d rows were scheduled for a retry the provider ruled out", report.Retrying)
	}
	if got := w.countState(t, "Failed"); got == 0 {
		t.Error("a permanent refusal did not reach Failed")
	}
	if got := w.countState(t, "Pending"); got != 0 {
		t.Errorf("%d rows are still Pending after a permanent refusal, want 0", got)
	}
	// One attempt, not fourteen. The budget is the thing being saved.
	if got := w.attemptsOf(t, "Failed"); got != 1 {
		t.Errorf("a permanent refusal spent %d attempts, want 1", got)
	}
}

// A bare error still means the ordinary schedule, which is what makes classifying
// optional rather than a migration every Sender has to do.
func TestABareErrorStillRetries(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)
	w.keepOne(t)

	ordinary := w.engineWith(t, 14, notify.SenderFunc(func(context.Context, notify.Message) error {
		return errors.New("503 from the provider")
	}))

	report, err := ordinary.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Retrying == 0 {
		t.Error("a bare error was not scheduled for a retry")
	}
	if report.Rejected != 0 {
		t.Errorf("%d rows were rejected on a bare error", report.Rejected)
	}
	if got := w.countState(t, "Pending"); got == 0 {
		t.Error("nothing is Pending after a retryable failure")
	}
}

// A provider that names the time to come back is honoured, and honoured as a
// floor: the row is never claimable before it asked, and never much later.
func TestARetryAfterMovesTheRowOut(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)
	w.keepOne(t)

	const asked = 10 * time.Minute
	busy := w.engineWith(t, 14, notify.SenderFunc(func(context.Context, notify.Message) error {
		return notify.RetryAfter(errors.New("429 slow down"), asked)
	}))

	before := time.Now().UTC()
	report, err := busy.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Counted apart from Retrying, because "the provider says slow down" and
	// "the provider is down" are different problems and only one of them is
	// fixed by waiting.
	if report.Deferred == 0 {
		t.Error("a Retry-After was not counted as deferred")
	}
	if report.Retrying != 0 {
		t.Errorf("%d rows were counted as retrying rather than deferred", report.Retrying)
	}

	got := w.earliestDeliverAt(t)
	if min := before.Add(asked); got.Before(min) {
		t.Errorf("deliver_at is %s, which is before the %s the provider asked for",
			got, asked)
	}
	// Spread upward by at most half, so a boundary stays a boundary rather than
	// becoming a guess.
	if max := before.Add(asked + asked/2 + time.Minute); got.After(max) {
		t.Errorf("deliver_at is %s, well past the %s asked for", got, asked)
	}
	if w.countDue(t) != 0 {
		t.Error("a deferred row is still due, so the next pass would ignore the request")
	}
}

// The test the whole spread exists for, and the one a unit test cannot make
// honestly: one provider refusing one pass of many rows must not put them all
// back at the same instant.
//
// Without it, every replica retries every row on the same schedule, so a provider
// having a bad minute meets the entire backlog again a minute later — which is
// the load that turns a bad minute into a bad afternoon.
//
// The engine's clock is frozen, and that is what makes the assertion mean
// anything. mark runs once per message, so with a real clock every row picks up
// its own microsecond of wall time and the timestamps differ whether or not
// anything spread them — a version of this test that read a real clock passed
// with the jitter removed. Frozen, an unspread schedule puts every row on one
// instant exactly, and only a spread can separate them.
func TestAFailedPassDoesNotRetryInLockstep(t *testing.T) {
	w := newDeliveryWorld(t)
	// Enough rows that one shared timestamp would be unmistakable, and few
	// enough to stay inside a single claim batch.
	for i := range 24 {
		w.reader(t, fmt.Sprintf("crowd-%d", i))
	}
	w.owe(t)

	// A base big enough that half of it is measurable. engineOf's millisecond is
	// there so retry tests do not wait; here the wait is never served, only read
	// off the row.
	const base = 10 * time.Minute
	// Ahead of the rows' own deliver_at, so a frozen clock still claims them.
	frozen := time.Now().Add(time.Minute).UTC()

	refuse := w.engineAt(t, frozen, base,
		notify.SenderFunc(func(context.Context, notify.Message) error {
			return errors.New("the provider is having a moment")
		}))

	report, err := refuse.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Retrying < 20 {
		t.Fatalf("only %d rows were retried, so this proves nothing", report.Retrying)
	}

	// Only the rows this pass touched. The seeded tenant is shared and its
	// accounts accumulate across runs, so a query over every row would pick up
	// deliveries no dispatcher claimed and digests waiting out their windows —
	// which is how the first version of this test came to pass without a spread.
	times := w.retriedDeliverAts(t)
	if len(times) < 20 {
		t.Fatalf("only %d retried rows were readable, so this proves nothing", len(times))
	}

	distinct := map[time.Time]bool{}
	first, last := times[0], times[0]
	for _, at := range times {
		distinct[at] = true
		if at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}

	// The failure this catches is one shared timestamp across the batch, which is
	// exactly what a frozen clock and no spread produce.
	if len(distinct) < len(times)/2 {
		t.Errorf("%d rows landed on %d distinct times, so the batch retries in "+
			"lockstep", len(times), len(distinct))
	}
	// And over a real share of the window rather than a rounding artefact. The
	// spread is up to half the wait, so a fifth of it is a floor a correct
	// implementation clears comfortably and an absent one cannot reach.
	if spread := last.Sub(first); spread < base/5 {
		t.Errorf("the batch is spread over %s of a %s base, which is not a spread",
			spread, base)
	}
	// Never earlier than the schedule said, because the spread only ever adds.
	if earliest := frozen.Add(base); first.Before(earliest) {
		t.Errorf("a row came back at %s, before the %s the schedule allows",
			first, earliest)
	}
}

// A channel that never answers must not take the dispatcher with it.
//
// This is the failure the send timeout exists for, and it is not the same as a
// slow channel: the backoff handles slow. A provider that accepts the connection
// and then says nothing — an http.Client with no Timeout of its own, which is
// Go's default, against a host that black-holes packets — used to park the pass
// forever. A pass resolves before it dispatches and both run in one goroutine,
// so that stopped this replica writing inbox lines and sending on every channel,
// not just the wedged one, until somebody restarted it.
func TestAHangingSenderDoesNotWedgeThePass(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)

	var deadlines int
	var mu sync.Mutex
	hangs := notify.SenderFunc(func(ctx context.Context, _ notify.Message) error {
		// Exactly what a well-written channel does and a hung one does not:
		// waits on the context it was given. That it returns at all is the
		// assertion; the deadline is what makes it return.
		<-ctx.Done()
		mu.Lock()
		deadlines++
		mu.Unlock()
		return ctx.Err()
	})

	engine := w.engineOf(t,
		map[notify.Channel]notify.Sender{notify.ChannelEmail: hangs}, 10, 50*time.Millisecond, 0, 0)

	// Bounded from the outside too. If the deadline does not work, the pass
	// never returns, and a test that simply called Dispatch would hang the
	// suite rather than failing it.
	done := make(chan error, 1)
	go func() {
		_, err := engine.Dispatch(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the pass did not return, so the send timeout is not bounding the sender")
	}

	mu.Lock()
	saw := deadlines
	mu.Unlock()
	if saw == 0 {
		t.Error("no send saw its deadline expire, so this proved nothing")
	}

	// And the rows are owed again rather than stranded: the lease came back, so
	// another dispatcher takes them now instead of after claim_ttl.
	if got := w.claimedRows(t); got != 0 {
		t.Errorf("%d rows are still claimed after a timed-out pass, want 0", got)
	}
	if got := w.countState(t, "Pending"); got == 0 {
		t.Error("a timed-out send left nothing Pending, so the delivery was lost")
	}
}

// A pass that cannot finish its batch inside the lease hands the rest back,
// attempt included.
//
// The lease is what stops two dispatchers sending the same message, and a pass
// that kept going past it would have every remaining row claimed by a replica
// that was right to think it had expired. So the pass stops while a whole send
// still fits, and the rows it did not reach are owed again — with the attempt the
// claim charged them given back, because a row no channel was ever asked about
// has not used one of its five. Without that, a channel slow enough to abandon
// the tail of every batch would Fail a delivery that had never been sent.
func TestAPassHandsBackWhatItCannotSendInsideTheLease(t *testing.T) {
	w := newDeliveryWorld(t)
	w.reader(t, "also told")
	w.reader(t, "told as well")
	w.owe(t)

	// Three rows and no more: the seeded tenant has other accounts of its own,
	// and a count this test states in whole numbers has to be about the three
	// readers it made.
	w.onlyFor(t, w.readers...)

	// Three Immediate rows are three messages, and the arithmetic is chosen so
	// that only the first one fits: a four-second send against a lease of a
	// minute leaves fifty-six seconds, and a send may not start with less than
	// its own timeout left. The margin is the four seconds the claim and the
	// addressing queries would have to take to make the *first* send skip too.
	var sends int
	var mu sync.Mutex
	slow := notify.SenderFunc(func(context.Context, notify.Message) error {
		mu.Lock()
		sends++
		mu.Unlock()
		time.Sleep(4 * time.Second)
		return nil
	})

	engine := w.engineOf(t, map[notify.Channel]notify.Sender{notify.ChannelEmail: slow},
		5, 56*time.Second, time.Minute, 0)

	report, err := engine.Dispatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	saw := sends
	mu.Unlock()
	if saw != 1 {
		t.Fatalf("%d sends were made, want 1: the pass did not stop at its budget", saw)
	}
	if report.Sent != 1 || report.Abandoned != 2 {
		t.Errorf("report says %d sent and %d abandoned, want 1 and 2: %s",
			report.Sent, report.Abandoned, report)
	}

	// Owed again rather than stranded, and owed with a full retry budget: the
	// claim's attempt is given back to a row nothing was tried on.
	if got := w.claimedRows(t); got != 0 {
		t.Errorf("%d rows are still claimed after the pass, want 0", got)
	}
	if got := w.countState(t, "Pending"); got != 2 {
		t.Errorf("%d rows are Pending after the pass, want 2", got)
	}
	if got := w.attemptsOf(t, "Pending"); got != 0 {
		t.Errorf("an abandoned row has %d attempts against it, want 0", got)
	}
}

// A channel whose sender went away fails its rows rather than taking the
// process.
//
// The rows exist because the sender existed when they were written — a deploy
// that dropped one from the map is the ordinary way to get here. Indexing
// straight into the map yields a nil Sender, and calling a method on it panics
// in a goroutine with no recover above it, which loses the process rather than
// the delivery. That this test completes at all is the assertion; a panic would
// take the whole run down.
func TestAChannelWhoseSenderWentAwayFailsRatherThanPanicking(t *testing.T) {
	w := newDeliveryWorld(t)
	w.owe(t)
	w.keepOne(t)

	// Email rows on the table, and an engine that can only send Mobile. Not an
	// empty map, which Dispatch already short-circuits: the case worth covering
	// is a map with the wrong channel in it.
	gone := w.engineOf(t, map[notify.Channel]notify.Sender{
		notify.ChannelMobile: notify.SenderFunc(func(context.Context, notify.Message) error {
			return nil
		}),
	}, 1, 0, 0, 0)

	if _, err := gone.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}

	// max_attempts is 1, so the first refusal is terminal.
	if got := w.countState(t, "Failed"); got == 0 {
		t.Error("a delivery with no sender was not failed")
	}
	// And it says why, because "Failed" with no reason is a row nobody can act
	// on.
	var reason string
	if err := w.pool.QueryRow(context.Background(),
		`SELECT coalesce(failed_reason, '') FROM rig_notification_delivery
		 WHERE tenant_id = $1 AND state = 'Failed' LIMIT 1`, w.tenant).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("no reason was recorded")
	}
	if !strings.Contains(reason, notify.ErrNoSender.Error()) {
		t.Errorf("failed_reason does not say the channel had no sender: %q", reason)
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
	return w.engineOf(t, senders, maxAttempts, 0, 0, 0)
}

// engineAt is for the one test that reads a wait off a row rather than serving
// it. It needs two things no other test here does: a base big enough that a
// share of it is measurable, and a clock that does not move, so that rows which
// were not spread apart are identical rather than merely close.
func (w *deliveryWorld) engineAt(
	t *testing.T, now time.Time, base time.Duration, sender notify.Sender,
) *notify.Engine {
	t.Helper()
	return w.engineFrom(t, map[notify.Channel]notify.Sender{notify.ChannelEmail: sender},
		14, 0, 0, base, func() time.Time { return now })
}

// engineOf is the same builder with the four things a timeout or retry test has
// an opinion about: which channels have senders at all, how long one call may
// take, how long the lease that call has to fit inside is, and how long the first
// retry waits. The clock is the real one, which is what all but one test wants.
func (w *deliveryWorld) engineOf(
	t *testing.T, senders map[notify.Channel]notify.Sender,
	maxAttempts int, sendTimeout, claimTTL, backoffBase time.Duration,
) *notify.Engine {
	t.Helper()
	return w.engineFrom(t, senders, maxAttempts, sendTimeout, claimTTL, backoffBase, nil)
}

// engineFrom is engineOf with the clock as well. Nil means time.Now.
func (w *deliveryWorld) engineFrom(
	t *testing.T, senders map[notify.Channel]notify.Sender,
	maxAttempts int, sendTimeout, claimTTL, backoffBase time.Duration,
	now func() time.Time,
) *notify.Engine {
	t.Helper()

	// The same object graph main.go builds, and in the same order.
	repos := store.New(w.pool, store.Config{})
	reg := notify.NewRegistry()
	inbox := api.NewNotifications(w.pool, reg)
	notes := note.New(repos.Notes, inbox, w.pool)
	reg.Register(api.NewNoteSubject(notes))

	return notify.NewEngine(notify.EngineConfig{
		Config: notify.Config{DB: w.pool, Registry: reg, Now: now},
		Links:  api.NotificationLinks(),

		Senders:     senders,
		MaxAttempts: maxAttempts,
		SendTimeout: sendTimeout,
		ClaimTTL:    claimTTL,
		// A millisecond rather than a minute by default, so a retry test does not
		// wait one out. The arithmetic is the same; the number is the only thing
		// a test has an opinion about.
		BackoffBase: cmp.Or(backoffBase, time.Millisecond),
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

// attemptsOf is the highest attempt count against any row in a state, which is
// what a test asserting an attempt was given back has to ask: one row is enough
// to have charged for a send nobody made.
func (w *deliveryWorld) attemptsOf(t *testing.T, state string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT coalesce(max(attempts), 0) FROM rig_notification_delivery
		 WHERE tenant_id = $1 AND state = $2`, w.tenant, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// deliverAts is every pending row's next attempt, which is what a test about the
// spread has to read: the assertion is about the shape of the set, not about any
// one row.
func (w *deliveryWorld) deliverAts(t *testing.T) []time.Time {
	t.Helper()
	rows, err := w.pool.Query(context.Background(),
		`SELECT deliver_at FROM rig_notification_delivery
		 WHERE tenant_id = $1 AND state = 'Pending'`, w.tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, at.UTC())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// retriedDeliverAts is the next attempt of every row a pass actually claimed and
// failed, which is the only set a spread can be read off.
//
// attempts > 0 is what distinguishes them. The seeded tenant is shared and its
// accounts accumulate across runs, so the table also holds rows no dispatcher
// reached and digests waiting out a window — and including those is how a spread
// assertion comes to pass without a spread.
func (w *deliveryWorld) retriedDeliverAts(t *testing.T) []time.Time {
	t.Helper()
	rows, err := w.pool.Query(context.Background(),
		`SELECT deliver_at FROM rig_notification_delivery
		 WHERE tenant_id = $1 AND state = 'Pending' AND attempts > 0`, w.tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, at.UTC())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// earliestDeliverAt is the soonest any row could be claimed again, which is the
// only one a floor can be checked against: a Retry-After is honoured if nothing
// comes back before it.
func (w *deliveryWorld) earliestDeliverAt(t *testing.T) time.Time {
	t.Helper()
	times := w.deliverAts(t)
	if len(times) == 0 {
		t.Fatal("no pending rows, so there is no deliver_at to read")
	}
	earliest := times[0]
	for _, at := range times {
		if at.Before(earliest) {
			earliest = at
		}
	}
	return earliest
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
