package account_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/simonjanss/rig/auth/account"
)

// The classification, asserted directly. [account.Service.DispatchMail] reads it
// to decide between Failed and Retry, and until now the only test that touched it
// was TestAPermanentRefusalStopsOnTheFirstAttempt, which observes the outcome
// rather than the rule — so a helper that stopped classifying would show up as
// one wrong state on one row rather than as the thing that broke.
//
// notify's failure_test.go, for notify's twins of these four. The pair are kept
// asserted the same way on purpose: they are the same four decisions, and the
// tests are where that stops being a claim in a comment.

var errProvider = errors.New("the provider said no")

// The one that would have been a bug rather than a nuisance: a pass decides a
// send succeeded by testing its error against nil, so a helper that turned a nil
// into a non-nil wrapper would record every delivered mail as a permanent
// failure. Both are handed nil because both are reachable with one — a Notifier
// that classifies its provider's answer before checking whether there was one is
// an easy thing to write.
func TestClassifyingNothingIsStillNothing(t *testing.T) {
	t.Parallel()

	if err := account.PermanentMailError(nil); err != nil {
		t.Errorf("PermanentMailError(nil) is %v, and a delivered mail would be failed", err)
	}
	if err := account.RetryMailAfter(nil, time.Minute); err != nil {
		t.Errorf("RetryMailAfter(nil, 1m) is %v, and a delivered mail would be retried", err)
	}
}

// A Notifier annotates before it returns — fmt.Errorf with the address in it,
// most often — so the classification has to survive being wrapped, in both
// directions.
//
// This is the test that catches the failure with no other symptom: if the
// constructor and the predicate ever stop agreeing on one wrapper type, nothing
// fails to compile and every permanent refusal quietly becomes a fourteen-attempt
// eight-hour retry.
func TestClassificationSurvivesWrapping(t *testing.T) {
	t.Parallel()

	if !account.IsPermanentMailError(fmt.Errorf("sending to nobody@example.com: %w",
		account.PermanentMailError(errProvider))) {
		t.Error("a PermanentMailError wrapped in fmt.Errorf stopped reading as permanent")
	}

	// And the provider's own error is still reachable, which is what makes
	// classifying a sentinel free rather than a trade.
	if !errors.Is(account.PermanentMailError(errProvider), errProvider) {
		t.Error("PermanentMailError hid the error it was given from errors.Is")
	}
	if !errors.Is(account.RetryMailAfter(errProvider, time.Minute), errProvider) {
		t.Error("RetryMailAfter hid the error it was given from errors.Is")
	}
}

// "Do not retry" and "retry at this time" cannot both be honoured, so one has to
// win in the code rather than in whichever order a Notifier happened to wrap.
// Refusing to retry is the stronger claim and wins from either side — otherwise a
// Notifier that wrapped both would keep alive a delivery it had already called
// hopeless.
func TestPermanentBeatsRetryAfterFromEitherSide(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"permanent outermost",
			account.PermanentMailError(account.RetryMailAfter(errProvider, time.Hour))},
		{"retry-after outermost",
			account.RetryMailAfter(account.PermanentMailError(errProvider), time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !account.IsPermanentMailError(tc.err) {
				t.Error("the permanent half was lost")
			}
			if d, ok := account.MailRetryAfterOf(tc.err); ok {
				t.Errorf("a permanent error asked to be retried after %s", d)
			}
		})
	}
}

func TestMailRetryAfterOf(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		err      error
		want     time.Duration
		wantAsks bool
	}{
		{"a bare error asks for nothing", errProvider, 0, false},
		{"an interval is carried", account.RetryMailAfter(errProvider, 10*time.Minute),
			10 * time.Minute, true},
		{"and survives annotation",
			fmt.Errorf("429 from the provider: %w", account.RetryMailAfter(errProvider, time.Hour)),
			time.Hour, true},
		// A provider that sent `Retry-After: 0` is saying "now", and "now" for a
		// queue is "the next pass" — which is what the ordinary backoff already
		// means. So a non-positive interval is a plain wrap rather than a
		// deliver_at in the past that every pass would reclaim immediately.
		{"zero is not an interval", account.RetryMailAfter(errProvider, 0), 0, false},
		{"nor is a negative one", account.RetryMailAfter(errProvider, -time.Minute), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, asks := account.MailRetryAfterOf(tc.err)
			if asks != tc.wantAsks {
				t.Errorf("asked = %v, want %v", asks, tc.wantAsks)
			}
			if got != tc.want {
				t.Errorf("interval = %s, want %s", got, tc.want)
			}
		})
	}
}

// A non-positive interval adds no wrapper at all, rather than one carrying zero,
// so nothing downstream has to tell "did not ask" from "asked for nothing".
func TestANonPositiveIntervalDoesNotWrap(t *testing.T) {
	t.Parallel()

	got := account.RetryMailAfter(errProvider, 0)
	if !errors.Is(got, errProvider) {
		t.Errorf("RetryMailAfter(err, 0) = %v, want the error it was given", got)
	}
	if inner := errors.Unwrap(got); inner != nil {
		t.Errorf("RetryMailAfter(err, 0) wrapped it around %v", inner)
	}
}

// A bare error is what every Notifier written before these helpers returns, and
// it has to keep meaning "the ordinary schedule".
func TestABareErrorIsNeitherThing(t *testing.T) {
	t.Parallel()

	if account.IsPermanentMailError(errProvider) {
		t.Error("a bare error read as permanent, so every Notifier written before " +
			"these helpers would stop retrying")
	}
	if _, ok := account.MailRetryAfterOf(errProvider); ok {
		t.Error("a bare error read as asking for an interval")
	}
}
