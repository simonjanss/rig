package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The mail queue, and most of it is about not storing a secret.
//
// Every link this package mints goes out through [Notifier], and until this file
// existed that call was made inline in the request that caused it: no timeout, no
// retry, no queue. A provider having a bad minute failed the caller's request,
// spent their rate-limit budget, and killed a token that had already been minted
// — so somebody who asked to reset their password got an error, and asking again
// cost them another attempt against the limiter.
//
// The queue is the answer, and the obvious shape of it is wrong.
//
// **A row cannot carry the token.** [Service.mintVerification] stores only a
// sha256 of it, and returns the one and only plaintext copy; that copy is in
// memory and then in the mail and nowhere else. A queue row with a `token` column
// would put plaintext bearer tokens at rest for the length of its retention, and
// encrypting them is worse rather than better — rig has no key-management seam,
// and inventing one for three mail templates ends with a key in an environment
// variable beside the database URL.
//
// **So the row carries intent, and the dispatcher mints.** A delivery says "a
// reset link is owed to the person behind this verification". The secret is
// generated immediately before the send, which keeps the property the inline path
// had: the plaintext exists in memory and in the mail. It also fixes the dead
// token, because a retry mints a live one, and it starts the link's own expiry
// when the mail goes out rather than when the request was made.
//
// **And it rotates rather than inserting.** The delivery owns exactly one
// verification row, written at enqueue time with a null hash, and every attempt
// rotates a fresh hash into that row. Inserting a new verification per attempt
// was the first design and it breaks two things that are easy to miss:
// [Store.PendingInvitations] lists live invitation rows, so N attempts list the
// same person N times — and worse, [Store.RevokeVerification] cancels one row, so
// "withdraw this invitation" would leave the others live.
//
// The cost of rotating is worth stating: a mail that arrived but whose third
// transaction failed has its link killed by the retry. The person gets two mails
// and the newest one works. That is better than two live reset tokens for one
// request, and it is what keeps the invitation listing and the revocation honest.

// DeliveryState is where one queued link is in its life.
type DeliveryState string

const (
	// DeliveryPending is owed and not yet claimed, or claimed by a process that
	// has not marked it.
	DeliveryPending DeliveryState = "Pending"
	// DeliverySent means the notifier accepted it, which is not the same as it
	// arriving — and rig does not pretend to know the difference.
	DeliverySent DeliveryState = "Sent"
	// DeliveryFailed is past MaxAttempts, or refused permanently, and stops
	// being claimed.
	DeliveryFailed DeliveryState = "Failed"
	// DeliverySkipped is a link that was consumed or withdrawn between being
	// queued and being sent.
	//
	// It is the state this queue buys that the inline path could not have: today
	// a withdrawn invitation cannot un-send a mail that is already in flight,
	// and here the rotate step finds the row already settled and sends nothing.
	DeliverySkipped DeliveryState = "Skipped"
)

// Delivery is one queued link, on its way.
type Delivery struct {
	ID uuid.UUID
	// VerificationID is the link this is the delivery of. Its token is rotated
	// before every attempt, so exactly one is live at a time.
	VerificationID uuid.UUID
	// Kind is which [Notifier] method to call, copied from the verification so a
	// claim needs no join.
	Kind VerificationKind

	State     DeliveryState
	DeliverAt time.Time
	// Attempts is how many times this has been claimed, including this one.
	Attempts int
}

// MailOptions is what the queue runs on, and every field is optional.
//
// The four numbers are the same four notify's engine takes, with the same
// defaults and the same refusals, because they are the same decision made twice
// in two modules that cannot import each other — rig/auth depends on rig/runtime
// and not on rig/notify. Where notify states the reason at length, this file
// names the file it is agreeing with.
type MailOptions struct {
	// ClaimTTL is how long this process's claim is honoured before another
	// dispatcher may take the row. Zero means [DefaultMailClaimTTL]; under
	// [MinMailClaimTTL] is refused.
	ClaimTTL time.Duration
	// SendTimeout bounds one call into a [Notifier]. Zero means
	// [DefaultMailSendTimeout]; ClaimTTL or longer is refused.
	//
	// It is the deadline on the context the notifier is handed, and it is
	// cooperative: a notifier that ignores it hangs its dispatcher anyway.
	SendTimeout time.Duration

	// MaxAttempts, BackoffBase and BackoffCap are the retry arithmetic. Zero
	// means the defaults, which span about eight hours.
	MaxAttempts int
	BackoffBase time.Duration
	BackoffCap  time.Duration

	// Jitter is where the spread on a retry comes from: given n it returns
	// something in [0, n). It exists to be replaced in a test and for nothing
	// else. Nil means math/rand/v2's.
	Jitter func(int64) int64
}

// Defaults for the mail queue. Every one of them is notify's number, and the
// pairing is deliberate: an operator tuning one dispatcher should not have to
// learn a second set of arithmetic for the other.
const (
	// DefaultMailClaimTTL is five minutes, chosen for the slowest channel this
	// queue has, which is mail.
	DefaultMailClaimTTL = 5 * time.Minute
	// MinMailClaimTTL is the shortest lease [Service] will accept. Under it,
	// every message a slow provider is still sending is claimed a second time
	// under ordinary load, and at-least-once stops being a crash property.
	MinMailClaimTTL = time.Minute
	// DefaultMailSendTimeout bounds one call into a notifier.
	DefaultMailSendTimeout = 30 * time.Second

	// DefaultMailMaxAttempts, DefaultMailBackoffBase and DefaultMailBackoffCap
	// span about eight hours: 1m, 2m, 4m, 8m, 16m, 32m, then hourly. An outage
	// is measured in hours by everyone who has had one.
	DefaultMailMaxAttempts = 14
	DefaultMailBackoffBase = time.Minute
	DefaultMailBackoffCap  = time.Hour
)

// mailBatch bounds how many deliveries one pass takes.
const mailBatch = 100

// Outbox is the queue's persistence, and it is the same kind of short adapter
// over generated repositories that [Store] is.
//
// Setting one on [Config] is what turns queueing on. Nil is the inline path this
// package shipped with, unchanged — see [Config.Outbox] for why that is the
// default and has to be.
type Outbox interface {
	// Enqueue writes one delivery.
	//
	// It has to join the caller's transaction when there is one. The verification
	// row and this row are written together, and a verification without its
	// delivery is an orphan link nobody will ever mail — invisible, except as an
	// invitation in a listing that was never sent.
	Enqueue(ctx context.Context, d *Delivery) error

	// Claim takes up to limit due rows and stamps this process's lease on them,
	// charging each an attempt.
	//
	// The lease is what makes a crashed dispatcher recoverable: a row is still
	// Pending with a stale claim, and the next pass past it takes it. It must
	// skip rows another claimant is taking rather than queueing behind them.
	Claim(ctx context.Context, by uuid.UUID, now time.Time, ttl time.Duration, limit int) ([]Delivery, error)

	// RotateToken writes a fresh hash and expiry onto the verification a delivery
	// owns, and reports whether it changed anything.
	//
	// False means the link was consumed or withdrawn between being queued and
	// being sent, and the delivery is Skipped rather than sent or failed. It must
	// not touch a row that is already settled — that check and this write are one
	// statement, or two requests racing can both believe they hold the live
	// token.
	RotateToken(ctx context.Context, verificationID uuid.UUID, hash []byte, expiresAt time.Time) (bool, error)

	// MarkSent records that a notifier accepted a delivery.
	MarkSent(ctx context.Context, id uuid.UUID, at time.Time) error
	// MarkFailed records that a delivery is done being tried, with what the
	// notifier said last.
	MarkFailed(ctx context.Context, id uuid.UUID, reason string, at time.Time) error
	// MarkSkipped records that there was nothing left to send.
	MarkSkipped(ctx context.Context, id uuid.UUID, reason string, at time.Time) error
	// Retry puts a delivery back with a later deliver_at, keeping what the
	// notifier said so a pattern is visible before the cap is reached.
	Retry(ctx context.Context, id uuid.UUID, at time.Time, reason string, now time.Time) error

	// Abandon gives back the claim and the attempt for a row a pass never
	// reached. It must only affect rows this process still holds.
	Abandon(ctx context.Context, ids []uuid.UUID, by uuid.UUID, now time.Time) error
	// ReleaseClaims gives back every lease this process holds, keeping the
	// attempts — a shutdown gives up sends it made, not sends it never made.
	ReleaseClaims(ctx context.Context, by uuid.UUID, now time.Time) (int, error)
}

// MailReport is what one dispatch pass did.
//
// Every count, including the zeros, for the reason notify's report gives: a pass
// that sent nothing is the ordinary case and still worth seeing, because the
// absence of a line cannot be told from the job not running.
type MailReport struct {
	// Claimed is how many rows this pass took, and Sent how many a notifier
	// accepted.
	Claimed int
	Sent    int
	// Failed is how many reached MaxAttempts and stopped.
	Failed int
	// Rejected is how many a notifier refused permanently, with
	// [PermanentMailError], and which stopped on this attempt with the rest of
	// their schedule unspent.
	Rejected int
	// Retrying is how many failed this time and will be tried again.
	Retrying int
	// Deferred is how many were rescheduled at a notifier's own request rather
	// than on the computed backoff.
	Deferred int
	// Skipped is how many had nothing left to send: a link consumed, withdrawn,
	// or belonging to somebody who has since been deactivated.
	//
	// A steady non-zero one is not a problem. It is invitations being accepted
	// or withdrawn faster than the dispatcher runs, which is the queue working.
	Skipped int
	// Released is how many claims a clean shutdown gave back rather than leaving
	// to expire, and Abandoned how many the pass could not reach inside its
	// lease.
	Released  int
	Abandoned int
}

// String is the one line a pass is worth in a log: every count, zeros included.
func (r MailReport) String() string {
	return fmt.Sprintf(
		"account: claimed %d, sent %d, failed %d, rejected %d, retrying %d, "+
			"deferred %d, skipped %d, released %d, abandoned %d",
		r.Claimed, r.Sent, r.Failed, r.Rejected, r.Retrying, r.Deferred, r.Skipped,
		r.Released, r.Abandoned)
}

// What a Notifier can say about a failure beyond the fact of it.
//
// The same four helpers notify ships, declared again because rig/auth cannot
// import rig/notify. Two copies of four small functions is the price of that
// boundary, and it is cheaper than the alternative, which is a queue package in
// runtime/ that every rig application imports.

// PermanentMailError wraps an error a retry cannot fix, so the delivery stops on
// this attempt rather than spending the rest of its budget.
//
// The case it is for is a provider refusing the recipient rather than the
// request: an address that does not exist, a mailbox that is full for good, a
// message the provider will never accept. It is also what makes an eight-hour
// schedule affordable — without a way to say "this address is wrong", every dead
// mailbox would occupy a row for a working day.
//
// Use it only when the answer is about the recipient. A 500, a timeout and a 429
// are all about the provider, and wrapping one of those turns a bad ten minutes
// into a password nobody can reset.
//
// PermanentMailError(nil) is nil. That is not a courtesy: a pass decides a send
// succeeded by testing the error against nil, so a helper that manufactured a
// non-nil wrapper from a nil error would record every delivered mail as a
// permanent failure. This is notify.Permanent's twin.
func PermanentMailError(err error) error {
	if err == nil {
		return nil
	}
	return &permanentMailError{err: err}
}

// RetryMailAfter wraps an error with the earliest a retry is worth making, which
// is most often a 429 carrying a Retry-After header.
//
// The interval replaces the computed backoff for this attempt only, so a provider
// asking for ten minutes once does not reset where the doubling had got to. It is
// honoured as a floor rather than exactly, because a batch of rows carrying one
// provider's boundary cannot all return at that boundary without rebuilding the
// herd it was asked to prevent. It does not extend the attempt budget.
//
// RetryMailAfter(nil, d) is nil, and a non-positive d is a plain wrap. This is
// notify.RetryAfter's twin.
func RetryMailAfter(err error, d time.Duration) error {
	if err == nil {
		return nil
	}
	if d <= 0 {
		return err
	}
	return &retryMailAfterError{err: err, after: d}
}

// IsPermanentMailError reports whether err asks not to be retried. It reads
// through wrapping.
func IsPermanentMailError(err error) bool {
	var p *permanentMailError
	return errors.As(err, &p)
}

// MailRetryAfterOf is the interval err asks to be retried after, and whether it
// asked at all.
//
// A permanent error answers false even when [RetryMailAfter] was also applied:
// "do not retry" and "retry then" cannot both be honoured, and refusing is the
// stronger of the two. Reading them the other way round would let a notifier that
// wrapped both keep alive a delivery it had already called hopeless.
func MailRetryAfterOf(err error) (time.Duration, bool) {
	if IsPermanentMailError(err) {
		return 0, false
	}
	var r *retryMailAfterError
	if errors.As(err, &r) {
		return r.after, true
	}
	return 0, false
}

type permanentMailError struct{ err error }

func (e *permanentMailError) Error() string { return e.err.Error() }
func (e *permanentMailError) Unwrap() error { return e.err }

type retryMailAfterError struct {
	err   error
	after time.Duration
}

func (e *retryMailAfterError) Error() string { return e.err.Error() }
func (e *retryMailAfterError) Unwrap() error { return e.err }

// errMailSkipped is why a delivery was skipped, recorded in failed_reason so a
// Skipped row says which of the several reasons it was.
var (
	errLinkSettled      = errors.New("account: the link was consumed or withdrawn before it was sent")
	errIdentityInactive = errors.New("account: the person this link is for is no longer active")
	errVerificationGone = errors.New("account: the link this delivery was for no longer exists")
)
