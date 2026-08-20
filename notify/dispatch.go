package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/dbx"
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

// Defaults for the retry arithmetic. Five attempts at a doubling minute span
// about half an hour, which outlasts the ordinary provider blip and does not
// outlast anybody's patience.
const (
	DefaultMaxAttempts = 5
	DefaultBackoffBase = time.Minute
)

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
	// Held is how many were outside somebody's window and moved to its next
	// opening rather than being sent or dropped.
	Held int
	// Digested is how many were folded into a message with others.
	Digested int
	// Released is how many claims a clean shutdown gave back rather than
	// leaving to expire.
	Released int
}

// String is the one line a pass is worth in a log: every count, zeros included.
func (r DispatchReport) String() string {
	return fmt.Sprintf(
		"notify: claimed %d, sent %d, failed %d, retrying %d, held %d, digested %d, released %d",
		r.Claimed, r.Sent, r.Failed, r.Retrying, r.Held, r.Digested, r.Released)
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

	for _, m := range e.messages(ctx, claimed, &report) {
		// The send, with no transaction open at all. This is the whole reason
		// the claim is a lease: a provider that hangs holds nothing but its own
		// lease, and the pool is untouched.
		err := e.senders[m.Channel].Send(ctx, m)
		if markErr := e.mark(ctx, m, err, &report); markErr != nil {
			return report, markErr
		}
	}
	return report, nil
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

	// Past the cap it is Failed and stops being claimed. Without one, a
	// permanently broken address consumes a lease and a log line forever.
	if attempts >= e.maxAttempts {
		const q = `UPDATE ` + DeliveryTable + ` SET
				state = 'Failed', failed_reason = $2, claimed_at = NULL, updated_at = now()
			WHERE id = ANY($1)`
		if _, err := e.store.conn(ctx).Exec(ctx, q, ids, sendErr.Error()); err != nil {
			return fmt.Errorf("notify: mark failed: %w", err)
		}
		report.Failed += len(ids)
		return nil
	}

	const q = `UPDATE ` + DeliveryTable + ` SET
			deliver_at = $2, failed_reason = $3, claimed_at = NULL, updated_at = now()
		WHERE id = ANY($1)`
	if _, err := e.store.conn(ctx).Exec(ctx, q, ids, e.backoff(attempts), sendErr.Error()); err != nil {
		return fmt.Errorf("notify: schedule a retry: %w", err)
	}
	report.Retrying += len(ids)
	return nil
}

// backoff doubles: one minute, two, four, eight, sixteen.
func (e *Engine) backoff(attempts int) time.Time {
	delay := e.backoffBase
	for range attempts - 1 {
		delay *= 2
	}
	return e.cfg.now().Add(delay)
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
	held := e.heldIDs()
	if len(held) == 0 {
		return 0, nil
	}

	const q = `UPDATE ` + DeliveryTable + ` SET claimed_at = NULL, updated_at = now()
		WHERE id = ANY($1) AND state = 'Pending'`
	tag, err := e.store.conn(ctx).Exec(ctx, q, held)
	if err != nil {
		return 0, fmt.Errorf("notify: release claims: %w", err)
	}
	e.forgetAll()
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

var _ = dbx.IsNoRows
