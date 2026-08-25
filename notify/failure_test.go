package notify_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/simonjanss/rig/notify"
)

var errProvider = errors.New("the provider said no")

// The one that would have been a bug rather than a nuisance: [Engine.Dispatch]
// decides a send succeeded by testing its error against nil, so a helper that
// turned a nil into a non-nil wrapper would mark every delivered message as a
// permanent failure. Both helpers are handed nil here because both are reachable
// with one — a Sender that classifies its provider's answer before checking
// whether there was one is an easy thing to write.
func TestClassifyingNothingIsStillNothing(t *testing.T) {
	t.Parallel()

	if err := notify.Permanent(nil); err != nil {
		t.Errorf("Permanent(nil) is %v, and a successful send would be failed", err)
	}
	if err := notify.RetryAfter(nil, time.Minute); err != nil {
		t.Errorf("RetryAfter(nil, 1m) is %v, and a successful send would be retried", err)
	}
}

// A Sender annotates before it returns — fmt.Errorf with the recipient in it, most
// often — so the classification has to survive being wrapped, in both directions.
func TestClassificationSurvivesWrapping(t *testing.T) {
	t.Parallel()

	if !notify.IsPermanent(fmt.Errorf("sending to nobody@example.com: %w",
		notify.Permanent(errProvider))) {
		t.Error("a Permanent wrapped in fmt.Errorf stopped reading as permanent")
	}

	// And the provider's own error is still reachable, which is what makes
	// classifying a sentinel free rather than a trade.
	if !errors.Is(notify.Permanent(errProvider), errProvider) {
		t.Error("Permanent hid the error it was given from errors.Is")
	}
	if !errors.Is(notify.RetryAfter(errProvider, time.Minute), errProvider) {
		t.Error("RetryAfter hid the error it was given from errors.Is")
	}
}

// "Do not retry" and "retry at this time" cannot both be honoured, so one of them
// has to win in the code rather than in whichever order a Sender happened to
// wrap. Refusing to retry is the stronger claim, and it wins from either side —
// otherwise a Sender that wrapped both would keep a delivery alive that it had
// already said was hopeless.
func TestPermanentBeatsRetryAfterFromEitherSide(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"permanent outermost", notify.Permanent(notify.RetryAfter(errProvider, time.Hour))},
		{"retry-after outermost", notify.RetryAfter(notify.Permanent(errProvider), time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !notify.IsPermanent(tc.err) {
				t.Error("the permanent half was lost")
			}
			if d, ok := notify.RetryAfterOf(tc.err); ok {
				t.Errorf("a permanent error asked to be retried after %s", d)
			}
		})
	}
}

func TestRetryAfterOf(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		err      error
		want     time.Duration
		wantAsks bool
	}{
		{"a bare error asks for nothing", errProvider, 0, false},
		{"an interval is carried", notify.RetryAfter(errProvider, 10*time.Minute),
			10 * time.Minute, true},
		{"and survives annotation",
			fmt.Errorf("429 from the provider: %w", notify.RetryAfter(errProvider, time.Hour)),
			time.Hour, true},
		// A provider that sent `Retry-After: 0` is saying "now", and "now" for a
		// queue is "the next pass" — which is what the ordinary backoff already
		// means. So a non-positive interval is a plain wrap rather than a
		// deliver_at in the past that every pass would reclaim immediately.
		{"zero is not an interval", notify.RetryAfter(errProvider, 0), 0, false},
		{"nor is a negative one", notify.RetryAfter(errProvider, -time.Minute), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, asks := notify.RetryAfterOf(tc.err)
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
//
// Stated as "it is the error, and it is not wrapped" rather than as an identity
// comparison, which is the same claim for an errors.New sentinel and does not
// read as the mistake errorlint is looking for.
func TestANonPositiveIntervalDoesNotWrap(t *testing.T) {
	t.Parallel()

	got := notify.RetryAfter(errProvider, 0)
	if !errors.Is(got, errProvider) {
		t.Errorf("RetryAfter(err, 0) = %v, want the error it was given", got)
	}
	if inner := errors.Unwrap(got); inner != nil {
		t.Errorf("RetryAfter(err, 0) wrapped it around %v", inner)
	}
}

// A bare error is what every Sender written before these helpers returns, and it
// has to keep meaning "the ordinary schedule".
func TestABareErrorIsNeitherThing(t *testing.T) {
	t.Parallel()

	if notify.IsPermanent(errProvider) {
		t.Error("a bare error read as permanent, so every Sender written before " +
			"this file would stop retrying")
	}
	if _, ok := notify.RetryAfterOf(errProvider); ok {
		t.Error("a bare error read as asking for an interval")
	}
}
