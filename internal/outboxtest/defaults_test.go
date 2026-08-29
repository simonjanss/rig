// Package outboxtest holds the one assertion neither queue can make about itself.
//
// rig has two of them — notify's delivery table and auth's mail outbox — and their
// retry numbers are deliberately identical, so that an operator tuning one
// dispatcher does not have to learn a second set of arithmetic for the other. Both
// sets of doc comments say so.
//
// Neither module can check it. `rig/auth` cannot import `rig/notify` and the
// reverse, which is the boundary the whole arrangement rests on. So the check
// lives in the root module, which requires both — and it is the reason the
// constants themselves were *not* moved into `runtime/outbox`: the names are
// published API on two modules, the refusal messages differ on purpose (one names
// rig.yaml keys, the other names Go fields), and a shared declaration would have
// bought this assertion at the cost of both.
package outboxtest_test

import (
	"testing"
	"time"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/notify"
)

func TestTheTwoOutboxesShareTheirDefaults(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		notify, mail   time.Duration
		notifyN, mailN int
		durations      bool
	}{
		{name: "claim TTL", notify: notify.DefaultClaimTTL, mail: account.DefaultMailClaimTTL, durations: true},
		{name: "minimum claim TTL", notify: notify.MinClaimTTL, mail: account.MinMailClaimTTL, durations: true},
		{name: "send timeout", notify: notify.DefaultSendTimeout, mail: account.DefaultMailSendTimeout, durations: true},
		{name: "backoff base", notify: notify.DefaultBackoffBase, mail: account.DefaultMailBackoffBase, durations: true},
		{name: "backoff cap", notify: notify.DefaultBackoffCap, mail: account.DefaultMailBackoffCap, durations: true},
		{name: "max attempts", notifyN: notify.DefaultMaxAttempts, mailN: account.DefaultMailMaxAttempts},
	} {
		if tc.durations {
			if tc.notify != tc.mail {
				t.Errorf("%s: notify says %s, the mail outbox says %s — the two are "+
					"documented as the same number", tc.name, tc.notify, tc.mail)
			}
			continue
		}
		if tc.notifyN != tc.mailN {
			t.Errorf("%s: notify says %d, the mail outbox says %d", tc.name, tc.notifyN, tc.mailN)
		}
	}
}

// And the schedule those numbers add up to, which is the claim both sets of doc
// comments actually make: "about eight hours". Asserted here as well as in each
// module, because this is the only place the two can be seen to agree.
func TestBothSchedulesSpanAboutEightHours(t *testing.T) {
	t.Parallel()

	span := func(base, ceiling time.Duration, attempts int) time.Duration {
		var total, wait time.Duration
		for i := 1; i < attempts; i++ {
			wait = base
			for range i - 1 {
				if wait >= ceiling {
					break
				}
				wait *= 2
			}
			total += min(wait, ceiling)
		}
		return total
	}

	want := 8*time.Hour + 3*time.Minute
	got := span(notify.DefaultBackoffBase, notify.DefaultBackoffCap, notify.DefaultMaxAttempts)
	if got != want {
		t.Errorf("notify's schedule spans %s, want %s", got, want)
	}
	got = span(account.DefaultMailBackoffBase, account.DefaultMailBackoffCap,
		account.DefaultMailMaxAttempts)
	if got != want {
		t.Errorf("the mail outbox's schedule spans %s, want %s", got, want)
	}
}
