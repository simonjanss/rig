package account_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/throttle"
)

// The queue, over the memory double. What is proved here is the rules; that two
// dispatchers racing over one row behave is a question for a database, and the
// Docker suite asks it.

// queued is a fixture with mail queueing on, and a notifier that can be told how
// to fail.
type queued struct {
	*fixture
	outbox *account.MemoryOutbox
	sender *scriptedNotifier
}

// scriptedNotifier records every token it was handed and answers with whatever
// the test put in `answers`, one per call, repeating the last.
type scriptedNotifier struct {
	tokens  []string
	answers []error
	calls   int
}

func (n *scriptedNotifier) answer(token string) error {
	n.tokens = append(n.tokens, token)
	n.calls++
	if len(n.answers) == 0 {
		return nil
	}
	if n.calls > len(n.answers) {
		return n.answers[len(n.answers)-1]
	}
	return n.answers[n.calls-1]
}

func (n *scriptedNotifier) SendPasswordReset(_ context.Context, _ *account.Identity, token string) error {
	return n.answer(token)
}

func (n *scriptedNotifier) SendEmailVerification(_ context.Context, _ *account.Identity, token string) error {
	return n.answer(token)
}

func (n *scriptedNotifier) SendInvitation(_ context.Context, _ *account.Identity, _ *account.Account, token string) error {
	return n.answer(token)
}

func setupQueued(t *testing.T, answers ...error) *queued {
	t.Helper()
	return setupQueuedWith(t, nil, answers...)
}

// setupQueuedWith is the same with one chance to edit the mail options, for the
// tests that have an opinion about a number.
func setupQueuedWith(t *testing.T, edit func(*account.Config), answers ...error) *queued {
	t.Helper()

	var outbox *account.MemoryOutbox
	sender := &scriptedNotifier{answers: answers}
	f := setupWith(t, func(cfg *account.Config) {
		outbox = account.NewMemoryOutbox(cfg.Store.(*account.MemoryStore))
		cfg.Outbox = outbox
		cfg.Notifier = sender
		// A millisecond rather than a minute, so a retry test does not wait one
		// out. The arithmetic is the same; the number is the only thing a test
		// has an opinion about.
		cfg.Mail.BackoffBase = time.Millisecond
		cfg.Mail.BackoffCap = time.Second
		if edit != nil {
			edit(cfg)
		}
	})
	return &queued{fixture: f, outbox: outbox, sender: sender}
}

// buildQueuedService builds a service and returns only whether it was accepted,
// which is all the refusal tests are asking.
//
// It assembles its own configuration rather than borrowing the fixture's, so that
// these can run in parallel: the fixture is a whole object graph and none of it
// is read by a constructor that refuses before it builds anything.
func buildQueuedService(t *testing.T, mail account.MailOptions, n account.Notifier) error {
	t.Helper()

	tokens := session.NewMemoryStore()
	sessions, err := session.New(session.Config{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := session.NewIdentity(session.IdentityConfig{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	store := account.NewMemoryStore()

	_, err = account.New(account.Config{
		Store:      store,
		Sessions:   sessions,
		Identities: identities,
		Limiter:    throttle.New(throttle.NewMemory()),
		Notifier:   n,
		Outbox:     account.NewMemoryOutbox(store),
		Mail:       mail,
	})
	return err
}

// dispatch runs one pass and fails the test if it errors.
func (q *queued) dispatch(t *testing.T) account.MailReport {
	t.Helper()
	report, err := q.svc.DispatchMail(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// undeliver brings every pending retry forward, so a test does not wait out a
// backoff.
func (q *queued) undeliver(t *testing.T) {
	t.Helper()
	for _, d := range q.outbox.Deliveries() {
		if d.State == account.DeliveryPending {
			_ = q.outbox.Retry(context.Background(), d.ID,
				q.clock.now().Add(-time.Minute), "", q.clock.now())
		}
	}
}

func (q *queued) states(t *testing.T) map[account.DeliveryState]int {
	t.Helper()
	out := map[account.DeliveryState]int{}
	for _, d := range q.outbox.Deliveries() {
		out[d.State]++
	}
	return out
}

// The security assertion this whole design exists for, and the first test in the
// file for that reason.
//
// A queued row cannot carry its token — the plaintext is never written down — so
// the dispatcher mints one per attempt and rotates it into the link the delivery
// owns. Which means the token in the first mail stops working when the second
// goes out, and only the newest one redeems. Anything else would mean either a
// secret at rest or several live reset tokens for one request.
func TestEveryAttemptCarriesAFreshTokenAndOnlyTheNewestWorks(t *testing.T) {
	q := setupQueued(t, errors.New("503 from the provider"), nil)

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	// Nothing has been sent, and the request did not fail on the way out.
	if q.sender.calls != 0 {
		t.Fatalf("the notifier was called %d times from the request path", q.sender.calls)
	}

	q.dispatch(t) // fails
	q.undeliver(t)
	q.dispatch(t) // succeeds

	if len(q.sender.tokens) != 2 {
		t.Fatalf("the notifier saw %d tokens, want 2", len(q.sender.tokens))
	}
	first, second := q.sender.tokens[0], q.sender.tokens[1]
	if first == second {
		t.Error("both attempts carried the same token, so a retry would resend a " +
			"link the rotate had already killed")
	}

	// The old one is dead and the new one works, in that order — confirming the
	// second before the first would consume the link and prove nothing.
	err := q.svc.ConfirmPasswordReset(context.Background(), first, "a whole new password", "203.0.113.5")
	if err == nil {
		t.Error("the token from the first attempt still redeems, so a retry leaves " +
			"two live reset links for one request")
	}
	if err := q.svc.ConfirmPasswordReset(context.Background(), second, "a whole new password", "203.0.113.5"); err != nil {
		t.Errorf("the token from the last attempt does not redeem: %v", err)
	}
}

// The other half of rotate-over-mint, and the reason it is rotate at all.
//
// Minting a new verification per attempt was the obvious design and it breaks two
// things: PendingInvitations lists live invitation rows, so N attempts list one
// person N times, and RevokeVerification cancels one row, so withdrawing an
// invitation would leave the others live. One delivery, one link, however many
// attempts.
func TestRetryingWritesNoExtraLinks(t *testing.T) {
	q := setupQueued(t, errors.New("still down"))

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	before := q.store.CountVerifications()

	for range 4 {
		q.dispatch(t)
		q.undeliver(t)
	}

	if got := q.store.CountVerifications(); got != before {
		t.Errorf("four attempts wrote %d link rows, want the %d there were after one",
			got, before)
	}
	if got := len(q.outbox.Deliveries()); got != 1 {
		t.Errorf("four attempts wrote %d delivery rows, want 1", got)
	}
}

// A provider having a bad minute no longer fails the caller's request, which is
// the whole point of the queue.
func TestAFailedSendLeavesTheRowForTheNextPass(t *testing.T) {
	q := setupQueued(t, errors.New("503 from the provider"))

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}

	report := q.dispatch(t)
	if report.Retrying != 1 {
		t.Errorf("retrying = %d, want 1 (%s)", report.Retrying, report)
	}
	if got := q.states(t)[account.DeliveryPending]; got != 1 {
		t.Errorf("%d rows are Pending after a failed send, want 1", got)
	}

	// And the attempt was charged, so the cap is reachable.
	if got := q.outbox.Deliveries()[0].Attempts; got != 1 {
		t.Errorf("attempts = %d after one pass, want 1", got)
	}
}

// Past the cap it stops, so a permanently broken address does not consume a lease
// and a log line forever.
func TestMaxAttemptsStopsAMailNobodyCanDeliver(t *testing.T) {
	// Three attempts rather than fourteen, so the test does not run the whole
	// default schedule to prove a rule that does not depend on its length.
	q := setupQueuedWith(t, func(cfg *account.Config) { cfg.Mail.MaxAttempts = 3 },
		errors.New("no such mailbox"))

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}

	for range 5 {
		q.dispatch(t)
		q.undeliver(t)
	}

	if got := q.states(t)[account.DeliveryFailed]; got != 1 {
		t.Errorf("%d rows are Failed after passing max_attempts, want 1", got)
	}
	// And it stops being claimed at all.
	if report := q.dispatch(t); report.Claimed != 0 {
		t.Errorf("a Failed row was claimed again: %s", report)
	}
}

// A provider that refuses the recipient is believed on the first attempt, and it
// is what makes an eight-hour schedule affordable.
func TestAPermanentRefusalStopsOnTheFirstAttempt(t *testing.T) {
	q := setupQueued(t, account.PermanentMailError(errors.New("no mailbox by that name")))

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}

	report := q.dispatch(t)
	if report.Rejected != 1 {
		t.Errorf("rejected = %d, want 1 (%s)", report.Rejected, report)
	}
	if q.sender.calls != 1 {
		t.Errorf("the notifier was called %d times, want 1", q.sender.calls)
	}
	if got := q.states(t)[account.DeliveryFailed]; got != 1 {
		t.Errorf("%d rows are Failed, want 1", got)
	}
}

// The thing this queue buys that the inline path could not have at all: today a
// withdrawn invitation cannot un-send a mail already in flight.
func TestALinkWithdrawnBeforeItWentOutIsNeverSent(t *testing.T) {
	q := setupQueued(t)

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	// Withdrawn between being queued and being sent.
	link := q.outbox.Deliveries()[0]
	if _, err := q.store.RevokeVerification(context.Background(), link.VerificationID, q.clock.now()); err != nil {
		t.Fatal(err)
	}

	report := q.dispatch(t)
	if report.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (%s)", report.Skipped, report)
	}
	if q.sender.calls != 0 {
		t.Error("a withdrawn link was mailed anyway")
	}
	if got := q.states(t)[account.DeliverySkipped]; got != 1 {
		t.Errorf("%d rows are Skipped, want 1", got)
	}
}

// The same for a link somebody redeemed by another route before the cron ran.
func TestALinkConsumedBeforeItWentOutIsNeverSent(t *testing.T) {
	q := setupQueued(t)

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	link := q.outbox.Deliveries()[0]
	if _, err := q.store.ConsumeVerification(context.Background(), link.VerificationID, q.clock.now()); err != nil {
		t.Fatal(err)
	}

	if report := q.dispatch(t); report.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (%s)", report.Skipped, report)
	}
	if q.sender.calls != 0 {
		t.Error("a consumed link was mailed anyway")
	}
}

// Mailing somebody a working reset link for an account that has since been
// switched off is the one outcome here nobody wants.
func TestADeactivatedPersonIsNotMailed(t *testing.T) {
	q := setupQueued(t)

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	q.store.DeactivateIdentity(q.ident.ID)

	if report := q.dispatch(t); report.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (%s)", report.Skipped, report)
	}
	if q.sender.calls != 0 {
		t.Error("a deactivated person was mailed a working link")
	}
}

// A pass over an empty queue is the ordinary case, and it is not an error.
func TestAPassWithNothingToDoIsAPass(t *testing.T) {
	q := setupQueued(t)
	if report := q.dispatch(t); report.Claimed != 0 {
		t.Errorf("claimed %d from an empty queue: %s", report.Claimed, report)
	}
}

// A drain that has stopped this service claiming is honoured by the next pass,
// and the pass is the only place it can be honoured: notify reads the same flag
// in the loop that drives its dispatcher, and this queue has no loop.
func TestADrainedServiceClaimsNothing(t *testing.T) {
	q := setupQueued(t)

	if err := q.svc.RequestPasswordReset(context.Background(), q.tenant, q.ident.EmailAddress, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	q.svc.StopClaimingMail()

	if report := q.dispatch(t); report.Claimed != 0 {
		t.Errorf("a drained service claimed %d deliveries: %s", report.Claimed, report)
	}
	if q.sender.calls != 0 {
		t.Errorf("a drained service sent %d mails", q.sender.calls)
	}
	// And the row is untouched rather than spent, so whatever runs next takes it
	// with its whole budget.
	if got := q.states(t)[account.DeliveryPending]; got != 1 {
		t.Errorf("%d rows are Pending, want the 1 nobody claimed", got)
	}
	if got := q.outbox.Deliveries()[0].Attempts; got != 0 {
		t.Errorf("attempts = %d after a pass that claimed nothing, want 0", got)
	}
}

// A service with no Outbox answers an empty report rather than an error, so a
// project on the inline path can register the task and change nothing else.
func TestDispatchingWithNoOutboxIsHarmless(t *testing.T) {
	f := setup(t)
	report, err := f.svc.DispatchMail(context.Background())
	if err != nil {
		t.Fatalf("dispatching without a queue failed: %v", err)
	}
	if report != (account.MailReport{}) {
		t.Errorf("a service with no queue reported %s", report)
	}
}

// Every count, zeros included, for the reason the report itself gives.
func TestTheMailReportLineNamesEveryCount(t *testing.T) {
	t.Parallel()
	line := account.MailReport{}.String()
	for _, want := range []string{
		"claimed", "sent", "failed", "rejected", "retrying", "deferred", "skipped",
		"released", "abandoned",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the report line does not mention %q: %s", want, line)
		}
	}
}

// The refusals, each naming both numbers because the fix is to change one of them.
func TestTheQueuesConfigurationIsRefusedWhenItCannotBeTrue(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		mail  account.MailOptions
		wants []string
	}{
		{
			"a send that may outlive its lease",
			account.MailOptions{ClaimTTL: 2 * time.Minute, SendTimeout: 5 * time.Minute},
			[]string{"SendTimeout", "ClaimTTL", "2m", "5m"},
		},
		{
			"a send that fills its whole lease",
			account.MailOptions{ClaimTTL: 2 * time.Minute, SendTimeout: 2 * time.Minute},
			[]string{"SendTimeout", "ClaimTTL"},
		},
		{
			"a lease too short to survive a provider",
			account.MailOptions{ClaimTTL: 30 * time.Second},
			[]string{"ClaimTTL", "30s", "1m"},
		},
		{
			"a ceiling under the floor",
			account.MailOptions{BackoffBase: 5 * time.Minute, BackoffCap: time.Minute},
			[]string{"BackoffCap", "BackoffBase", "5m", "1m"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := buildQueuedService(t, tc.mail, &scriptedNotifier{})
			if err == nil {
				t.Fatal("the configuration was accepted")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not mention %q: %s", want, err)
				}
			}
		})
	}
}

// A queue whose rows are written and then dropped is a table that grows forever
// behind mail that never goes, so it is refused rather than run.
func TestAQueueWithNoNotifierIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		n    account.Notifier
	}{
		{"nil", nil},
		{"NoNotifier", account.NoNotifier{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := buildQueuedService(t, account.MailOptions{}, tc.n)
			if err == nil {
				t.Fatal("a queue with nothing to send through was accepted")
			}
			if !strings.Contains(err.Error(), "Notifier") {
				t.Errorf("the error does not name the missing piece: %s", err)
			}
		})
	}
}

// And the defaults satisfy every refusal above, which is the case it would be
// worst to get wrong: a default set this package rejects is a failure on a
// configuration nobody wrote.
func TestTheQueuesDefaultsAreAcceptedByTheirOwnChecks(t *testing.T) {
	t.Parallel()
	if err := buildQueuedService(t, account.MailOptions{}, &scriptedNotifier{}); err != nil {
		t.Errorf("the default mail options were refused: %v", err)
	}
	if account.DefaultMailSendTimeout >= account.DefaultMailClaimTTL {
		t.Errorf("DefaultMailSendTimeout %s does not fit inside DefaultMailClaimTTL %s",
			account.DefaultMailSendTimeout, account.DefaultMailClaimTTL)
	}
}
