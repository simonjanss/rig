package account

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The sending half, and it is four short transactions rather than notify's three.
//
// Claim, rotate, send, mark. Everything but the rotate is lifted in shape from
// notify/dispatch.go, whose header states the argument in full: a claim is a
// lease rather than a row lock, because a lock lives as long as its transaction
// and would be held across a call to somebody's mail provider — one slow provider
// then holds a pool connection per message in flight, and a provider that hangs
// holds them until the statement timeout.
//
// The extra transaction is the secret. It has to be before the send and outside
// it, and the crash window it opens is worth naming because the shape invites the
// reader to worry about it: a process that dies between rotating and sending
// leaves a live token in the database whose plaintext nobody holds — not the
// person, not the provider, not rig. The link is unusable rather than leaked, and
// the retry rotates over it. That is the good outcome; the alternative orderings
// are worse.
//
// At-least-once, in the same words notify uses: a process that handed a mail over
// and died before its last transaction will hand it over again when the lease
// expires. Here the duplicate is a second mail whose link works and a first whose
// link does not, which is why [Notifier] says not to give a provider an
// idempotency key for these.

// DispatchMail is one pass: claim what is due, send it, mark it.
//
// A pass that fails part way through is a pass. The rows it did not reach are
// still Pending, the rows it claimed and did not mark come back when their lease
// expires, and the next pass takes both.
//
// It answers an empty report and no error for a service with no [Config.Outbox],
// so a project on the inline path can register the task and change nothing else.
//
// The report is a named result because [Service.releaseMail] runs from a defer
// and adds to it: an unnamed one would be copied before the defer ran, and the
// count of leases a failed pass handed back would be zero in every log line that
// could carry it.
func (s *Service) DispatchMail(ctx context.Context) (report MailReport, err error) {
	if s.cfg.Outbox == nil {
		return report, nil
	}
	// A drain that has stopped this service claiming is honoured here, because
	// this is the only place that claims. notify checks the same flag in the loop
	// that drives its dispatcher; this queue is a task somebody's cron runs, so
	// there is no loop to check it in and the pass has to.
	if !s.claimingMail() {
		return report, nil
	}

	// Bounded before the claim, not after: the lease starts when the claim
	// stamps it, so a budget measured from any later moment is a budget that
	// outlives what it is protecting.
	pass, endPass := context.WithTimeout(ctx, s.mail.ClaimTTL)
	defer endPass()

	claimed, err := s.cfg.Outbox.Claim(ctx, s.mailClaimedBy, s.now(), s.mail.ClaimTTL, mailBatch)
	if err != nil {
		return report, err
	}
	report.Claimed = len(claimed)
	if len(claimed) == 0 {
		return report, nil
	}

	s.holdMail(claimed)
	defer s.releaseMail(ctx, &report)

	var abandoned []uuid.UUID
	for _, d := range claimed {
		if !s.mailBudgetFor(pass) {
			// Not enough lease left to send inside it. Whatever is left is owed
			// again and the next pass takes it.
			abandoned = append(abandoned, d.ID)
			continue
		}

		if err := s.deliverOne(ctx, pass, d, &report); err != nil {
			return report, err
		}
	}

	if len(abandoned) > 0 {
		if err := s.cfg.Outbox.Abandon(ctx, abandoned, s.mailClaimedBy, s.now()); err != nil {
			return report, fmt.Errorf("account: abandon deliveries: %w", err)
		}
		for _, id := range abandoned {
			s.forgetMail(id)
		}
		report.Abandoned += len(abandoned)
	}
	return report, nil
}

// deliverOne is the rotate, the send and the mark for one row.
func (s *Service) deliverOne(ctx context.Context, pass context.Context, d Delivery, report *MailReport) error {
	v, err := s.cfg.Store.VerificationByID(ctx, d.VerificationID)
	if err != nil {
		return err
	}
	if v == nil {
		// The link is gone, so there is nothing to send and nothing to retry.
		// Only reachable if something deleted the row out from under the cascade.
		return s.skipMail(ctx, d, errVerificationGone, report)
	}

	ident, err := s.cfg.Store.FindIdentityByID(ctx, v.IdentityID)
	if err != nil {
		return err
	}
	if ident == nil || !ident.IsActive {
		// Deactivated between being queued and being sent. Mailing somebody a
		// working reset link for an account that has been switched off is the
		// one outcome here nobody wants.
		return s.skipMail(ctx, d, errIdentityInactive, report)
	}

	var acct *Account
	if d.Kind == KindInvitation {
		if v.InvitedToTenantID == nil {
			return s.skipMail(ctx, d, errVerificationGone, report)
		}
		acct, err = s.cfg.Store.AccountForIdentity(ctx, *v.InvitedToTenantID, ident.ID)
		if err != nil {
			return err
		}
		if acct == nil || !acct.IsActive {
			return s.skipMail(ctx, d, errIdentityInactive, report)
		}
	}

	// The secret, generated now and never written down. Its expiry runs from
	// here rather than from the request, which is the right answer and a small
	// improvement on the inline path: a link that waited out an outage is not a
	// link that spent its whole window waiting.
	token, hash, err := s.mintToken()
	if err != nil {
		return err
	}

	rotated, err := s.cfg.Outbox.RotateToken(ctx, v.ID, hash, s.now().Add(s.ttlFor(d.Kind)))
	if err != nil {
		return err
	}
	if !rotated {
		// Consumed or withdrawn while this pass was running. Nothing is sent,
		// and this is the outcome the inline path could not produce at all.
		return s.skipMail(ctx, d, errLinkSettled, report)
	}

	// The send, with no transaction open. This is the whole reason the claim is
	// a lease: a provider that hangs holds nothing but its own lease, and the
	// pool is untouched.
	send, endSend := context.WithTimeout(pass, s.mail.SendTimeout)
	sendErr := s.notify(send, d.Kind, ident, acct, token)
	endSend()

	// Marked on ctx and not on pass, for notify's reason: the pass deadline is
	// for the provider, and a write that records a successful send is not
	// something to abandon because the lease ran out while it was being made.
	return s.markMail(ctx, d, sendErr, report)
}

// notify is the one place the three Notifier methods are told apart.
func (s *Service) notify(ctx context.Context, kind VerificationKind, ident *Identity, acct *Account, token string) error {
	switch kind {
	case KindPasswordReset:
		return s.cfg.Notifier.SendPasswordReset(ctx, ident, token)
	case KindEmailVerification:
		return s.cfg.Notifier.SendEmailVerification(ctx, ident, token)
	case KindInvitation:
		return s.cfg.Notifier.SendInvitation(ctx, ident, acct, token)
	default:
		// A kind this build does not know, which is a queued row written by a
		// newer one. Permanent, because another pass will not know it either.
		return PermanentMailError(fmt.Errorf("account: no notifier for %q", kind))
	}
}

// ttlFor is how long the link this delivery carries is good for, read at send
// time so that a configuration change reaches rows that are already queued.
func (s *Service) ttlFor(kind VerificationKind) time.Duration {
	switch kind {
	case KindPasswordReset:
		return s.cfg.ResetTTL
	case KindEmailVerification:
		return s.cfg.VerificationTTL
	default:
		return s.cfg.InvitationTTL
	}
}

// markMail is the last transaction: what happened, and when to try again.
func (s *Service) markMail(ctx context.Context, d Delivery, sendErr error, report *MailReport) error {
	s.forgetMail(d.ID)
	now := s.now()

	if sendErr == nil {
		if err := s.cfg.Outbox.MarkSent(ctx, d.ID, now); err != nil {
			return fmt.Errorf("account: mark sent: %w", err)
		}
		report.Sent++
		return nil
	}

	// A notifier that answered permanently is taken at its word on this attempt,
	// with the rest of the schedule unspent. Checked before the attempt cap
	// because the two agree on the outcome and disagree on the count, and
	// Rejected is the more useful of the two to see.
	if IsPermanentMailError(sendErr) {
		if err := s.cfg.Outbox.MarkFailed(ctx, d.ID, sendErr.Error(), now); err != nil {
			return fmt.Errorf("account: mark rejected: %w", err)
		}
		report.Rejected++
		return nil
	}

	// Past the cap it stops being claimed. Without one, a permanently broken
	// address consumes a lease and a log line forever.
	if d.Attempts >= s.mail.MaxAttempts {
		if err := s.cfg.Outbox.MarkFailed(ctx, d.ID, sendErr.Error(), now); err != nil {
			return fmt.Errorf("account: mark failed: %w", err)
		}
		report.Failed++
		return nil
	}

	asked, deferred := MailRetryAfterOf(sendErr)
	next := s.nextMailAttemptAt(d.Attempts, asked)
	if err := s.cfg.Outbox.Retry(ctx, d.ID, next, sendErr.Error(), now); err != nil {
		return fmt.Errorf("account: schedule a retry: %w", err)
	}
	if deferred {
		report.Deferred++
	} else {
		report.Retrying++
	}
	return nil
}

// skipMail records that there was nothing left to send.
func (s *Service) skipMail(ctx context.Context, d Delivery, why error, report *MailReport) error {
	s.forgetMail(d.ID)
	if err := s.cfg.Outbox.MarkSkipped(ctx, d.ID, why.Error(), s.now()); err != nil {
		return fmt.Errorf("account: mark skipped: %w", err)
	}
	report.Skipped++
	return nil
}

// nextMailAttemptAt is when to try again: the doubling, capped, spread.
//
// notify's nextAttemptAt, to the line, and the note there explains why the spread
// is added on top of the wait rather than taken out of it — nobody is blocked on
// a queued mail, so the nominal schedule stays a floor and backoff_base keeps
// meaning what it says. What the spread prevents is one provider refusing one
// pass of a hundred rows and meeting all hundred again at the same instant, on
// every replica at once.
func (s *Service) nextMailAttemptAt(attempts int, asked time.Duration) time.Time {
	wait := asked
	if wait <= 0 {
		wait = s.mail.BackoffBase
		for range attempts - 1 {
			// Tested before doubling rather than clamped after, so a long
			// schedule cannot overflow past the ceiling on its way to being
			// clamped back under it.
			if wait >= s.mail.BackoffCap {
				break
			}
			wait *= 2
		}
		wait = min(wait, s.mail.BackoffCap)
	}
	// The +1 makes the bound positive for every wait, including one short enough
	// that half of it truncates to zero.
	return s.now().Add(wait + time.Duration(s.mail.Jitter(int64(wait/2)+1)))
}

// mailBudgetFor reports whether the pass has room for one whole send.
//
// The question is whether the *next* send fits, not whether the budget has
// already run out: a send started with a millisecond left runs to the pass
// deadline and no further, which is a call still in flight as the lease expires —
// the case [New] refuses a configuration over. So a pass ends with the send
// timeout unspent, and that is the point.
func (s *Service) mailBudgetFor(pass context.Context) bool {
	if pass.Err() != nil {
		return false
	}
	deadline, ok := pass.Deadline()
	return !ok || time.Until(deadline) >= s.mail.SendTimeout
}

// StopClaimingMail stops this service taking new deliveries, for a shutdown that
// wants the pass in flight to finish without a new one starting.
//
// It is read by [Service.DispatchMail] and nothing else, because claiming is the
// only thing it stops: a pass already past that point runs to its end, which is
// the point of calling this rather than closing the pool.
func (s *Service) StopClaimingMail() {
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	s.mailClaiming = false
}

// claimingMail is whether a pass may still take work.
func (s *Service) claimingMail() bool {
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	return s.mailClaiming
}

// ReleaseMailClaims gives back every lease this process still holds.
//
// The attempts are not given back, and that is the difference from Abandon: the
// sends this pass made were made, and a shutdown that un-charged them would let a
// process that crashed repeatedly retry forever.
func (s *Service) ReleaseMailClaims(ctx context.Context) (int, error) {
	if s.cfg.Outbox == nil {
		return 0, nil
	}
	n, err := s.cfg.Outbox.ReleaseClaims(ctx, s.mailClaimedBy, s.now())
	if err != nil {
		return 0, fmt.Errorf("account: release mail claims: %w", err)
	}
	s.mailMu.Lock()
	clear(s.mailHeld)
	s.mailMu.Unlock()
	return n, nil
}

// The lease bookkeeping a clean shutdown reads.

func (s *Service) holdMail(ds []Delivery) {
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	for _, d := range ds {
		s.mailHeld[d.ID] = true
	}
}

func (s *Service) forgetMail(id uuid.UUID) {
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	delete(s.mailHeld, id)
}

// releaseMail hands back whatever a pass still holds on its way out, so a lease
// is given back late rather than left to expire.
func (s *Service) releaseMail(ctx context.Context, report *MailReport) {
	s.mailMu.Lock()
	held := len(s.mailHeld)
	s.mailMu.Unlock()
	if held == 0 {
		return
	}

	n, err := s.ReleaseMailClaims(ctx)
	if err != nil {
		// Nothing to do about it here and nothing lost: the leases expire on
		// their own, which is what the TTL is for.
		return
	}
	report.Released += n
}
