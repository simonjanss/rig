package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/outbox"
)

// The delivery half, and most of it is about concurrency rather than about mail.
//
// Every replica runs a dispatcher and the operator's cron runs another, so ten
// claimants on one row is normal operation here rather than an edge. The
// guarantee has to be stated rather than inferred, and the obvious way to state
// it is wrong.
//
// **`SELECT … FOR UPDATE SKIP LOCKED` inside the transaction that sends** is
// correct and unusable: a row lock lives as long as its transaction, so it would
// be held across a call to SMTP or APNs. One slow provider then holds a pool
// connection per message in flight, and a provider that hangs holds them until
// the statement timeout — a notification backlog that takes the API down with it.
//
// **So the claim is a lease, and a send is three short transactions.** Claim,
// with no transaction open across the second; send; mark. `SKIP LOCKED` is what
// makes the claim itself contention-free — a second claimant walks past the rows
// the first is taking instead of queueing behind them, so throughput rises with
// replicas rather than flattening.
//
// **Which buys at-least-once, and this package uses those words.** A process
// that handed a message to a provider and died before its third transaction will
// hand it over again when the lease expires. No arrangement of one database
// prevents that: the send and the bookkeeping are two systems and no transaction
// spans both. What rig does instead is make the duplicate survivable, in two
// halves worth separating because one is much stronger than the other. The inbox
// cannot duplicate, unconditionally, because of the unique index on
// (notification_id, account_id). A channel gets a stable [Delivery.ID] and has
// to hand it to its provider as an idempotency key — which rig cannot enforce,
// and which is why the field says so at length.

// DefaultClaimTTL is how long a claim is honoured before another dispatcher may
// take the row.
//
// Five minutes, which is the wrong number for somebody and has to be some
// number. It is chosen for the slowest channel most applications have, which is
// mail. The relationship that actually matters is to the channel's own timeout:
// set this shorter than that and every message is claimed twice under ordinary
// load — at-least-once stops being a crash-recovery property and becomes an
// everyday one.
const DefaultClaimTTL = 5 * time.Minute

// MinClaimTTL is the shortest lease [NewEngine] will accept, and it panics
// rather than starting under it. Refused at boot rather than discovered as
// duplicate mail six weeks later.
const MinClaimTTL = time.Minute

// Defaults for the retry arithmetic, and the three of them are one decision.
//
// Fourteen attempts, doubling from a minute and capped at an hour, span about
// eight hours: 1m, 2m, 4m, 8m, 16m, 32m, and hourly after that. The number that
// matters is the total, because the thing being outlasted is an outage — and an
// outage is measured in hours by everyone who has had one. Five attempts at a
// doubling minute, which is what this package shipped with, span thirty-one
// minutes. That outlasts a blip and nothing else: a provider down for a morning
// permanently failed every message rig had for it, and the row said Failed
// rather than "was never really tried".
//
// The cap is what makes more attempts safe to have. Doubling fourteen times from
// a minute is five days, so without a ceiling the late attempts are not retries,
// they are a row nobody will look at again. Capped at an hour, the tail is seven
// hourly knocks, which is the shape somebody watching a provider's status page
// would choose by hand.
//
// The trade is stated because it is a real one, and it is the reverse of the old
// default's. Eight hours of attempts means a genuinely undeliverable message
// occupies a queue for eight hours. [Permanent] is the answer to that — a
// provider that knows the recipient is wrong can say so and skip the whole
// schedule — and it is why these numbers could be raised at all.
const (
	DefaultMaxAttempts = 14
	DefaultBackoffBase = time.Minute
	DefaultBackoffCap  = time.Hour
)

// DefaultSendTimeout bounds one call into a channel.
//
// Thirty seconds, which is the number rigclient already chose for a whole
// request, and it is here because a [Sender] is the one seam in rig where the
// code on the other side belongs to somebody else. Every other outbound call rig
// makes bounds itself — three seconds for the breach check, ten for a token
// exchange — and this one could not, because rig does not make it.
//
// What it prevents is not a slow provider, which the backoff already handles. It
// is a provider that never answers at all: an http.Client with no Timeout, which
// is Go's default, dialling a host that black-holes the packets. A pass is a
// single goroutine, so one such call used to stop this replica dispatching
// anything ever again — on every channel, and the inbox with it, since a pass
// resolves before it dispatches.
const DefaultSendTimeout = 30 * time.Second

// claimBatch bounds how many deliveries one pass takes.
const claimBatch = 100

// DispatchReport is what one dispatch pass did.
//
// Every count, including the zeros, for the reason the file sweeper's report
// gives: a pass that sent nothing is the ordinary case and still worth seeing,
// because the absence of a line cannot be told from the job not running.
type DispatchReport struct {
	// Claimed is how many rows this pass took, Sent how many a channel
	// accepted, and Failed how many reached max_attempts and stopped.
	Claimed int
	Sent    int
	Failed  int
	// Retrying is how many failed this time and will be tried again.
	Retrying int
	// Rejected is how many a channel refused permanently, with [Permanent], and
	// which were failed on this attempt rather than spending the rest of the
	// schedule.
	//
	// It is worth telling apart from Failed because the two mean opposite things
	// about the provider. A rising Failed is rig giving up after eight hours of
	// trying; a rising Rejected is the provider answering immediately and
	// definitively, which usually means a list of addresses has gone stale
	// rather than that anything is wrong.
	Rejected int
	// Deferred is how many were rescheduled at a channel's own request, with
	// [RetryAfter], rather than on the computed backoff.
	//
	// A steady non-zero one is a provider telling this project it is sending too
	// fast, which is a different problem from a provider being down and is not
	// visible in Retrying, where it would otherwise be counted.
	Deferred int
	// Held is how many were outside somebody's window and moved to its next
	// opening rather than being sent or dropped.
	Held int
	// Digested is how many were folded into a message with others.
	Digested int
	// Released is how many claims a clean shutdown gave back rather than
	// leaving to expire.
	Released int
	// Abandoned is how many the pass did not reach because the lease ran out
	// before it got to them.
	//
	// It is the number that says a channel is slow enough to be a problem. Zero
	// is the ordinary case and a steady non-zero one means the batch cannot be
	// sent inside a claim — either the provider got slower or claim_ttl and
	// send_timeout no longer fit each other. Those rows are Pending again, with
	// the attempt the claim charged them given back, so nothing is lost; what is
	// lost is the assumption that a pass drains what it claims.
	Abandoned int
}

// String is the one line a pass is worth in a log: every count, zeros included.
func (r DispatchReport) String() string {
	return fmt.Sprintf(
		"notify: claimed %d, sent %d, failed %d, rejected %d, retrying %d, "+
			"deferred %d, held %d, digested %d, released %d, abandoned %d",
		r.Claimed, r.Sent, r.Failed, r.Rejected, r.Retrying, r.Deferred, r.Held,
		r.Digested, r.Released, r.Abandoned)
}

// Dispatch is one pass: claim what is due, send it, mark it.
//
// A pass that fails part way through is a pass. The rows it did not reach are
// still Pending, the rows it claimed and did not mark come back when their lease
// expires, and the next pass takes both.
func (e *Engine) Dispatch(ctx context.Context) (DispatchReport, error) {
	var report DispatchReport
	if len(e.senders) == 0 {
		// No channel is registered, so there is nothing to claim. The inbox
		// still works: it is not a channel.
		return report, nil
	}

	// The pass gets the lease's worth of time and no more. Bounding each send is
	// not enough on its own: a batch of a hundred against a channel that takes
	// the full timeout every time is a hundred timeouts long, which outlives the
	// claim by a wide margin, and every row in it is then claimed a second time
	// by a replica that was right to think the lease had expired.
	//
	// Started before the claim rather than after it, because the lease starts
	// when the claim stamps it. A budget begun after the claim and the addressing
	// queries would end that much later than the lease it is standing in for,
	// which is the one thing it exists to prevent.
	pass, endPass := context.WithTimeout(ctx, e.claimTTL)
	defer endPass()

	claimed, err := e.claim(ctx, claimBatch)
	if err != nil {
		return report, err
	}
	report.Claimed = len(claimed)
	if len(claimed) == 0 {
		return report, nil
	}

	e.hold(claimed)
	defer e.release(ctx, &report)

	// The pass stops when what is left of the lease cannot fit another send, and
	// the rows it did not reach are handed back rather than left claimed until
	// the TTL. Bounding the pass rather than shrinking the batch is what keeps a
	// healthy channel fast: a provider answering in a hundred milliseconds still
	// gets all hundred in one pass.
	var abandoned []Delivery

	for _, m := range e.messages(ctx, claimed, &report) {
		if !e.budgetFor(pass) {
			// Not enough lease left to send inside it. Whatever is left is owed
			// again and the next pass takes it, which is the same sentence as
			// every other partial pass here.
			abandoned = append(abandoned, m.Deliveries...)
			continue
		}

		sender, ok := e.senders[m.Channel]
		if !ok || sender == nil {
			// A channel that had a sender when these rows were written and does
			// not now — a deploy that dropped one from the map, most often. The
			// rows exist and nothing can take them, so they retry and then fail
			// like any other undeliverable copy.
			//
			// Guarded rather than indexed straight into, because a nil Sender is
			// a panic in a goroutine with no recover above it, which takes the
			// process rather than the delivery.
			err := fmt.Errorf("%w: %s", ErrNoSender, m.Channel)
			if markErr := e.mark(ctx, m, err, &report); markErr != nil {
				return report, markErr
			}
			continue
		}

		// The send, with no transaction open at all. This is the whole reason
		// the claim is a lease: a provider that hangs holds nothing but its own
		// lease, and the pool is untouched.
		//
		// Bounded, because "holds nothing but its own lease" was not true of the
		// goroutine doing the holding. A Sender that ignores this deadline hangs
		// anyway — Go cannot prevent that, and [Sender] says so.
		send, endSend := context.WithTimeout(pass, e.sendTimeout)
		err := sender.Send(send, m)
		endSend()

		// Marked on ctx and not on pass: the pass deadline is for the provider,
		// and a write that recorded a successful send is not something to abandon
		// because the lease ran out while it was being made.
		if markErr := e.mark(ctx, m, err, &report); markErr != nil {
			return report, markErr
		}
	}

	if err := e.abandon(ctx, abandoned, &report); err != nil {
		return report, err
	}
	return report, nil
}

// budgetFor reports whether the pass has room for one whole send.
//
// The question is whether the *next* send fits, not whether the budget has
// already run out: a send started with a millisecond left runs to the pass
// deadline and no further, which is a call still in flight as the lease expires —
// the case [NewEngine] refuses a configuration over. So a pass ends with the
// send timeout unspent, and that is the point.
func (e *Engine) budgetFor(pass context.Context) bool {
	return outbox.RoomFor(pass, e.sendTimeout)
}

// abandon gives back a row the pass never reached: the claim and the attempt
// both.
//
// The attempt is the part worth spelling out. `claim` charges every row in the
// batch an attempt before anything is sent, so a row released without a send
// would have paid for one it never got — and max_attempts of those, on a channel
// slow enough to abandon the tail of every batch, would Fail a delivery that no
// channel had ever been asked about. This is the one thing [Engine.ReleaseClaims]
// deliberately does not do, because the send it gives up on was made.
//
// `claimed_by` is in the WHERE clause for the same reason: past a lease that
// expired anyway, the row may be somebody else's now, and the attempt to give
// back would be theirs.
func (e *Engine) abandon(ctx context.Context, abandoned []Delivery, report *DispatchReport) error {
	if len(abandoned) == 0 {
		return nil
	}

	ids := idsOf(abandoned)

	const q = `UPDATE ` + DeliveryTable + ` SET
			attempts = greatest(attempts - 1, 0), claimed_at = NULL, updated_at = now()
		WHERE id = ANY($1) AND claimed_by = $2 AND state = 'Pending'`
	if _, err := e.store.conn(ctx).Exec(ctx, q, ids, e.claimedBy); err != nil {
		return fmt.Errorf("notify: abandon deliveries: %w", err)
	}

	// Forgotten only now, so that a failure above leaves them to the deferred
	// release: a lease given back late is better than one left to expire.
	for _, id := range ids {
		e.forget(id)
	}
	report.Abandoned += len(ids)
	return nil
}

// claim takes the due rows, in the one statement that makes this contention-free.
//
// The inner SELECT is what does the work: `SKIP LOCKED` walks past whatever
// another claimant is taking rather than queueing behind it, and the outer
// UPDATE stamps the lease. `claimed_at IS NULL OR claimed_at < now() - ttl` is
// what makes a crashed process recoverable at all — the row is still Pending
// with a stale claim, and the next dispatcher past it takes it.
func (e *Engine) claim(ctx context.Context, limit int) ([]Delivery, error) {
	const q = `
		UPDATE ` + DeliveryTable + ` SET
			claimed_at = $1, claimed_by = $2, attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM ` + DeliveryTable + `
			WHERE state = 'Pending'
			  AND deliver_at <= $1
			  AND (claimed_at IS NULL OR claimed_at < $3)
			ORDER BY deliver_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, account_id, recipient_id, channel, kind, digest, attempts, deliver_at`

	now := e.cfg.now()
	rows, err := e.store.conn(ctx).Query(ctx, q, now, e.claimedBy, now.Add(-e.claimTTL), limit)
	if err != nil {
		return nil, fmt.Errorf("notify: claim deliveries: %w", err)
	}
	defer rows.Close()

	var out []Delivery
	for rows.Next() {
		var (
			d               Delivery
			channel, digest string
		)
		if err := rows.Scan(&d.ID, &d.TenantID, &d.AccountID, &d.RecipientID,
			&channel, &d.Kind, &digest, &d.Attempts, &d.DeliverAt); err != nil {
			return nil, fmt.Errorf("notify: scan claim: %w", err)
		}
		d.Channel = Channel(channel)
		d.Digest = Digest(digest)
		out = append(out, d)
	}
	return out, rows.Err()
}

// mark is the third transaction: what happened, and when to try again.
func (e *Engine) mark(ctx context.Context, m Message, sendErr error, report *DispatchReport) error {
	ids := make([]uuid.UUID, 0, len(m.Deliveries))
	attempts := 0
	for _, d := range m.Deliveries {
		ids = append(ids, d.ID)
		e.forget(d.ID)
		attempts = max(attempts, d.Attempts)
	}

	if sendErr == nil {
		const q = `UPDATE ` + DeliveryTable + ` SET
				state = 'Sent', sent_at = $2, claimed_at = NULL, updated_at = now()
			WHERE id = ANY($1)`
		if _, err := e.store.conn(ctx).Exec(ctx, q, ids, e.cfg.now()); err != nil {
			return fmt.Errorf("notify: mark sent: %w", err)
		}
		report.Sent += len(ids)
		return nil
	}

	const failQ = `UPDATE ` + DeliveryTable + ` SET
			state = 'Failed', failed_reason = $2, claimed_at = NULL, updated_at = now()
		WHERE id = ANY($1)`

	// A channel that answered permanently is taken at its word, on this attempt,
	// with the rest of the schedule unspent. The alternative is spending eight
	// hours asking a provider a question it has already answered — and the
	// provider is the only party here that can tell a wrong address from its own
	// bad afternoon, which is the whole argument for [Permanent] existing.
	//
	// Checked before the attempt cap rather than after: the two agree on the
	// outcome and disagree on the count, and Rejected is the more useful of the
	// two to see, because it says the provider refused rather than that rig gave
	// up waiting for it.
	if IsPermanent(sendErr) {
		if _, err := e.store.conn(ctx).Exec(ctx, failQ, ids, sendErr.Error()); err != nil {
			return fmt.Errorf("notify: mark rejected: %w", err)
		}
		report.Rejected += len(ids)
		return nil
	}

	// Past the cap it is Failed and stops being claimed. Without one, a
	// permanently broken address consumes a lease and a log line forever.
	if attempts >= e.maxAttempts {
		if _, err := e.store.conn(ctx).Exec(ctx, failQ, ids, sendErr.Error()); err != nil {
			return fmt.Errorf("notify: mark failed: %w", err)
		}
		report.Failed += len(ids)
		return nil
	}

	// A channel's own answer about when to come back, and whether it gave one.
	// Counted separately below because a provider saying "slow down" and a
	// provider being down are different problems, and the second one is the only
	// one Retrying used to be able to mean.
	asked, deferred := RetryAfterOf(sendErr)

	const q = `UPDATE ` + DeliveryTable + ` SET
			deliver_at = $2, failed_reason = $3, claimed_at = NULL, updated_at = now()
		WHERE id = ANY($1)`
	next := e.nextAttemptAt(attempts, asked)
	if _, err := e.store.conn(ctx).Exec(ctx, q, ids, next, sendErr.Error()); err != nil {
		return fmt.Errorf("notify: schedule a retry: %w", err)
	}
	if deferred {
		report.Deferred += len(ids)
	} else {
		report.Retrying += len(ids)
	}
	return nil
}

// nextAttemptAt is when to try again: the doubling, capped, spread.
//
// `asked` is what a channel requested with [RetryAfter], and zero when it did
// not ask. A request replaces the computed wait for this attempt and does not
// move where the doubling had got to — a provider asking for ten minutes once is
// not the same as the outage having lasted ten minutes.
//
// **The spread is added on top of the wait rather than taken out of it**, which
// is the reverse of what rigclient does with the same problem. There a caller is
// blocked on the answer, so a spread that could only lengthen the wait is a cost
// somebody sits through, and half the window is given up to buy the other half's
// randomness. Here nobody is waiting: the row is in a table and the next pass is
// a minute away regardless. So the nominal schedule is a floor, backoff_base
// keeps meaning what its documentation says, and the arithmetic that turns
// max_attempts into "about eight hours" stays arithmetic instead of becoming an
// average.
//
// What the spread is for is the case this package had no answer to: one provider
// refusing one pass of a hundred rows, on every replica at once. Without it all
// hundred come back at the same instant, on the same schedule, for all fourteen
// attempts — so a provider having a bad minute meets a hundred simultaneous
// retries a minute later, which is the load that turns a bad minute into a bad
// afternoon. Spread over half the wait, those hundred arrive over thirty seconds
// on the first retry and over half an hour by the last.
//
// A Retry-After is spread the same way, and for the same reason rather than in
// spite of being a boundary a provider named. One call can honour a boundary
// exactly; a hundred rows carrying one provider's boundary cannot all honour it
// exactly without rebuilding the herd at the instant it asked everybody back.
// Adding is what keeps it a boundary — the wait is never shorter than what was
// asked for, only later.
func (e *Engine) nextAttemptAt(attempts int, asked time.Duration) time.Time {
	return outbox.Backoff{
		Base:   e.backoffBase,
		Cap:    e.backoffCap,
		Jitter: e.jitter,
	}.Next(e.cfg.now(), attempts, asked)
}

// ReleaseClaims gives back every lease this process still holds.
//
// **A clean shutdown must not cost a lease.** The TTL exists for crashes, and a
// process that knows it is going has no excuse for being slow about saying so:
// leaving them to expire turns every ordinary rollout into a delivery delay, and
// a rollout that replaces every pod turns it into that delay repeatedly.
//
// `attempts` is left where it is. A send that outlived the close budget is
// abandoned rather than cancelled — the provider may still deliver it — so the
// retry after it is the at-least-once case again, which is the same case and not
// a new one.
func (e *Engine) ReleaseClaims(ctx context.Context) (int, error) {
	held := e.leases.IDs()
	if len(held) == 0 {
		return 0, nil
	}

	const q = `UPDATE ` + DeliveryTable + ` SET claimed_at = NULL, updated_at = now()
		WHERE id = ANY($1) AND state = 'Pending'`
	tag, err := e.store.conn(ctx).Exec(ctx, q, held)
	if err != nil {
		return 0, fmt.Errorf("notify: release claims: %w", err)
	}
	// Drop what this call released rather than clearing everything, because the
	// statement above is scoped by id: it released the snapshot taken at the top
	// and nothing else. Two passes can run at once — the in-process goroutine and
	// the cron task both call Dispatch — and clearing here would forget the other
	// one's claims without releasing them, leaving rows held until a TTL meant for
	// crashes ran out. auth's release is scoped by claimant, so there Clear is
	// right.
	e.leases.Drop(held...)
	return int(tag.RowsAffected()), nil
}

func (e *Engine) release(ctx context.Context, report *DispatchReport) {
	n, err := e.ReleaseClaims(ctx)
	if err == nil {
		report.Released += n
	}
}

// ErrNoSender is what a channel with nothing registered answers with.
var ErrNoSender = errors.New("notify: no sender for this channel")
